package build

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.uber.org/fx"
	"gorm.io/gorm"

	"kit.getarcane.app"
	"kit.getarcane.app/acfs"
	acfstypes "kit.getarcane.app/acfs/types"
	"kit.getarcane.app/builds/api"
	"kit.getarcane.app/builds/pkg/contextsource"
	buildtypes "kit.getarcane.app/builds/types"
	"kit.getarcane.app/capture"
	"kit.getarcane.app/docker"
	gitkit "kit.getarcane.app/git"

	"getarcane.dev/arcane/v3/internal/build/children/railpack"
	"getarcane.dev/arcane/v3/internal/build/children/workspace"
	"getarcane.dev/arcane/v3/internal/common"
	"getarcane.dev/arcane/v3/internal/database"
	dockerInternal "getarcane.dev/arcane/v3/internal/docker"
	"getarcane.dev/arcane/v3/internal/event"
	"getarcane.dev/arcane/v3/internal/gitrepo"
	"getarcane.dev/arcane/v3/internal/middleware"
	"getarcane.dev/arcane/v3/internal/registry"
	"getarcane.dev/arcane/v3/internal/settings"
	"getarcane.dev/arcane/v3/pkg/authz"
	"getarcane.dev/arcane/v3/pkg/gitutil"
	"getarcane.dev/arcane/v3/pkg/handlerutil"
	"getarcane.dev/arcane/v3/pkg/httpx"
	"getarcane.dev/arcane/v3/pkg/pagination"
	"getarcane.dev/arcane/v3/pkg/timeouts"
	"getarcane.dev/types/v3"
)

type (
	BuildService struct {
		db              *database.DB
		settings        *settings.SettingsService
		dockerService   *dockerInternal.DockerClientService
		registryService *registry.ContainerRegistryService
		gitRepository   *gitrepo.GitRepositoryService
		eventService    *event.EventService
		workspace       *workspace.Service
		builder         buildtypes.Builder
		railpack        railpackBuilder
		gitProbeFn      func(context.Context, string, git.AuthConfig) error
		gitCloneFn      func(context.Context, string, string, git.AuthConfig, int) (string, error)
		gitCleanupFn    func(string) error
	}

	BuildServiceParams struct {
		fx.In

		DB              *database.DB
		LocalSettings   *settings.SettingsService
		DockerService   *dockerInternal.DockerClientService
		RegistryService *registry.ContainerRegistryService
		GitRepository   *gitrepo.GitRepositoryService
		EventService    *event.EventService
	}

	// RailpackBuildOptions describes a Railpack build of app source that has no Dockerfile.
	RailpackBuildOptions struct {
		// Previous is the last build's info; its package versions pin this build.
		Previous *types.RailpackBuildInfo
		Actor    *types.Actor
		Progress io.Writer
		// Env is passed to Railpack (RAILPACK_* settings) and mounted as build secrets.
		Env           map[string]string
		EnvironmentID string
		// Source identifies the repository revision in build history.
		Source       string
		ContextDir   string
		Image        string
		BuildCommand string
		StartCommand string
	}

	railpackBuilder interface {
		Preflight(ctx context.Context, image string) (railpack.Target, error)
		Build(ctx context.Context, req railpack.Request, progress io.Writer) (*types.RailpackBuildInfo, error)
	}

	recordedBuild struct {
		user          *types.Actor
		progress      io.Writer
		build         func(io.Writer) (*buildtypes.BuildResult, error)
		environmentID string
		serviceName   string
		req           buildtypes.BuildRequest
	}
)

const (
	buildHistoryOutputLimitBytes = 2 * 1024 * 1024
	deleteBuildsBatchSize        = 500
	railpackProvider             = "railpack"
)

func NewBuildService(p BuildServiceParams) (*BuildService, error) {
	svc := &BuildService{
		db:              p.DB,
		settings:        p.LocalSettings,
		dockerService:   p.DockerService,
		registryService: p.RegistryService,
		gitRepository:   p.GitRepository,
		eventService:    p.EventService,
		gitProbeFn:      p.GitRepository.ProbeRemote,
		gitCloneFn:      p.GitRepository.Clone,
		gitCleanupFn:    os.RemoveAll,
		workspace:       workspace.NewService(p.LocalSettings),
		railpack:        railpack.NewService(p.DockerService),
	}

	// registry.ContainerRegistryService already implements buildtypes.RegistryAuthProvider,
	// so the builder consumes it directly instead of through forwarding methods on BuildService.
	builder, err := api.NewService(api.Config{
		SettingsProvider:     svc,
		DockerClientProvider: p.DockerService,
		RegistryAuthProvider: p.RegistryService,
	})
	if err != nil {
		return nil, fmt.Errorf("create builder: %w", err)
	}
	svc.builder = builder

	return svc, nil
}

func (s *BuildService) BuildSettings() buildtypes.BuildSettings {
	localSettings := s.settings.GetSettingsConfig()
	return buildtypes.BuildSettings{
		DepotProjectID:   localSettings.DepotProjectId.Value,
		DepotToken:       localSettings.DepotToken.Value,
		BuildProvider:    localSettings.BuildProvider.Value,
		BuildTimeoutSecs: localSettings.BuildTimeout.AsInt(),
	}
}

func (
	s *BuildService,
) BuildImage(
	ctx context.Context,
	environmentID string,
	req buildtypes.BuildRequest,
	progressWriter io.Writer,
	serviceName string,
	user *types.Actor,
) (
	*buildtypes.BuildResult,
	error,
) {
	permissions, _ := middleware.PermissionsFromContext(ctx)
	for _, perm := range authz.BuildPushConditions.Required("push", strconv.FormatBool(req.Push)) {
		if !permissions.Allows(perm, environmentID) {
			return nil, common.Classify(common.ErrForbidden, errors.New("permission denied: "+perm))
		}
	}

	return s.recordBuild(ctx, recordedBuild{
		user:          user,
		progress:      progressWriter,
		environmentID: environmentID,
		serviceName:   serviceName,
		req:           req,
		build: func(writer io.Writer) (*buildtypes.BuildResult, error) {
			resolvedReq, cleanup, err := s.resolveBuildRequest(ctx, req, writer, serviceName)
			if err != nil {
				return nil, err
			}
			defer func() {
				if cleanupErr := cleanup(); cleanupErr != nil {
					slog.WarnContext(ctx, "failed to cleanup temporary git build context", "error", cleanupErr)
				}
			}()
			return s.builder.BuildImage(ctx, resolvedReq, writer, serviceName)
		},
	})
}

// BuildRailpackImage builds opts.ContextDir into opts.Image with Railpack, reusing the image when it already exists.
func (s *BuildService) BuildRailpackImage(ctx context.Context, opts RailpackBuildOptions) (*types.RailpackBuildInfo, error) {
	target, err := s.railpack.Preflight(ctx, opts.Image)
	if err != nil {
		return nil, err
	}
	if target.Exists {
		// The tag names the commit and build inputs, so the existing image is this build.
		reused := kit.FromPtr(opts.Previous)
		reused.Image = opts.Image
		writeBuildProgressStatus(ctx, opts.Progress, "", "reusing existing image "+opts.Image)
		return &reused, nil
	}

	timeout := timeouts.GetDuration(s.BuildSettings().BuildTimeoutSecs, timeouts.DefaultBuildTimeout)
	buildCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var info *types.RailpackBuildInfo
	_, err = s.recordBuild(ctx, recordedBuild{
		user:          opts.Actor,
		progress:      opts.Progress,
		environmentID: opts.EnvironmentID,
		req: buildtypes.BuildRequest{
			Provider:   railpackProvider,
			ContextDir: sanitizeBuildContextForEvent(opts.Source),
			Tags:       []string{opts.Image},
			Platforms:  []string{target.Platform},
			Load:       true,
		},
		build: func(writer io.Writer) (*buildtypes.BuildResult, error) {
			built, buildErr := s.railpack.Build(buildCtx, railpack.Request{
				Env:              opts.Env,
				PreviousVersions: kit.FromPtr(opts.Previous).Packages,
				ContextDir:       opts.ContextDir,
				Image:            opts.Image,
				Platform:         target.Platform,
				BuildCommand:     opts.BuildCommand,
				StartCommand:     opts.StartCommand,
			}, writer)
			if buildErr != nil && buildCtx.Err() != nil && ctx.Err() == nil {
				// The flow engine retries deadline errors; a build that outran the timeout would only time out again.
				return nil, fmt.Errorf("railpack build timed out after %s", timeout)
			}
			if buildErr != nil {
				return nil, buildErr
			}
			info = built
			return &buildtypes.BuildResult{Provider: railpackProvider, Tags: []string{opts.Image}}, nil
		},
	})
	return info, err
}

// recordBuild runs a build with its output captured, then records it in build history and publishes the outcome.
func (s *BuildService) recordBuild(ctx context.Context, run recordedBuild) (*buildtypes.BuildResult, error) {
	// The builder emits raw docker-CLI text. The log capture stores it verbatim
	// for build history; the progress writer gets it framed as {"log":...} lines.
	logCapture := capture.New(buildHistoryOutputLimitBytes)
	writer := io.Writer(logCapture)
	var logWriter io.WriteCloser
	if run.progress != nil {
		logWriter = docker.NewLogLineWriter(run.progress)
		writer = io.MultiWriter(logWriter, logCapture)
	}

	buildRecordID := ""
	if strings.TrimSpace(run.environmentID) != "" {
		if record, err := s.createBuildRecord(ctx, run.environmentID, run.req, run.user); err != nil {
			slog.WarnContext(ctx, "failed to create build history record", "error", err)
		} else {
			buildRecordID = record.ID
		}
	}

	startedAt := time.Now()
	result, err := run.build(writer)
	completedAt := time.Now()
	if logWriter != nil {
		_ = logWriter.Close()
	}

	if buildRecordID != "" {
		output := logCapture.String()
		var outputPtr *string
		if output != "" {
			outputPtr = &output
		}

		provider := s.effectiveBuildProvider(run.req.Provider)
		var digest *string
		if result != nil {
			if result.Provider != "" {
				provider = result.Provider
			}
			if result.Digest != "" {
				digest = &result.Digest
			}
		}

		status := ImageBuildStatusSuccess
		var errMsg *string
		if err != nil {
			status = ImageBuildStatusFailed
			errMsg = new(err.Error())
		}

		if updateErr := s.completeBuildRecord(
			ctx,
			buildRecordID,
			status,
			outputPtr,
			logCapture.Truncated(),
			errMsg,
			digest,
			provider,
			completedAt,
			new(
				completedAt.Sub(
					startedAt,
				).Milliseconds(),
			),
		); updateErr != nil {
			slog.WarnContext(ctx, "failed to update build history record", "error", updateErr)
		}
	}
	s.eventService.PublishChange(run.environmentID, "build")

	if err != nil {
		s.logBuildFailureEvent(ctx, run.environmentID, run.req, run.serviceName, buildRecordID, err, run.user)
	}

	return result, err
}

func (s *BuildService) logBuildFailureEvent(ctx context.Context, environmentID string, req buildtypes.BuildRequest, serviceName, buildRecordID string, err error, user *types.Actor) {
	if err == nil {
		return
	}

	metadata := database.JSON{
		"action":     "build",
		"provider":   s.effectiveBuildProvider(req.Provider),
		"contextDir": sanitizeBuildContextForEvent(req.ContextDir),
		"dockerfile": req.Dockerfile,
		"tags":       append([]string(nil), req.Tags...),
	}
	if service := strings.TrimSpace(serviceName); service != "" {
		metadata["serviceName"] = service
	}
	if buildRecordID != "" {
		metadata["buildRecordId"] = buildRecordID
	}

	s.eventService.LogErrorEvent(ctx, event.ResourceEvent{
		Type:          event.EventTypeImageError,
		ResourceType:  "image",
		ResourceName:  cmp.Or(cmp.Or(kit.TrimNonEmpty(req.Tags)...), strings.TrimSpace(serviceName), sanitizeBuildContextForEvent(req.ContextDir)),
		UserID:        kit.FromPtr(user).ID,
		Username:      kit.FromPtr(user).Username,
		EnvironmentID: environmentID,
		Metadata:      metadata,
	}, err)
}

func sanitizeBuildContextForEvent(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}

	address, fragment, hasFragment := strings.Cut(trimmed, "#")
	parsed, err := url.Parse(address)
	if err != nil {
		return kit.Ternary(strings.Contains(address, "@"), "[unparseable URL]", trimmed)
	}
	if parsed.User == nil {
		return trimmed
	}

	parsed.User = url.User("redacted")
	sanitized := parsed.String()
	if hasFragment {
		sanitized += "#" + fragment
	}
	return sanitized
}

func (s *BuildService) effectiveBuildProvider(provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider != "" {
		return provider
	}
	provider = strings.ToLower(strings.TrimSpace(s.settings.GetSettingsConfig().BuildProvider.Value))
	return kit.Ternary(provider == "", "local", provider)
}

func (s *BuildService) resolveBuildRequest(
	ctx context.Context,
	req buildtypes.BuildRequest,
	progressWriter io.Writer,
	serviceName string,
) (buildtypes.BuildRequest, func() error, error) {
	source, ok, err := contextsource.ParseGitBuildContextSource(req.ContextDir)
	if err != nil {
		return buildtypes.BuildRequest{}, func() error { return nil }, err
	}
	if !ok || source == nil {
		if contextsource.IsPotentialRemoteBuildContextSource(req.ContextDir) {
			return buildtypes.BuildRequest{}, func() error { return nil }, fmt.Errorf("unsupported remote build context source %q: only git repository URLs are supported", req.ContextDir)
		}
		return req, func() error { return nil }, nil
	}

	writeBuildProgressStatus(ctx, progressWriter, serviceName, "resolving remote git context "+source.RepositoryURL)

	authConfig, matchedRepository, err := s.resolveGitBuildAuth(ctx, source.RepositoryURL)
	if err != nil {
		return buildtypes.BuildRequest{}, func() error { return nil }, err
	}
	if matchedRepository {
		writeBuildProgressStatus(ctx, progressWriter, serviceName, "using saved git credentials for "+source.RepositoryURL)
	}
	if gitkit.RequiresRemoteProbe(source.RepositoryURL) {
		writeBuildProgressStatus(ctx, progressWriter, serviceName, "verifying remote git repository "+source.RepositoryURL)
		if probeGitContextErr := s.gitProbeFn(ctx, source.RepositoryURL, authConfig); probeGitContextErr != nil {
			return buildtypes.BuildRequest{}, func() error { return nil }, fmt.Errorf("failed to verify remote git repository %q: %w", source.RepositoryURL, probeGitContextErr)
		}
	}

	repoPath, err := s.gitCloneFn(ctx, source.RepositoryURL, source.Ref, authConfig, 0)
	if err != nil {
		return buildtypes.BuildRequest{}, func() error { return nil }, err
	}

	contextDir := repoPath
	if source.Subdir != "" {
		if validatePathErr := git.ValidatePath(repoPath, filepath.FromSlash(source.Subdir)); validatePathErr != nil {
			_ = s.cleanupGitContext(repoPath)
			return buildtypes.BuildRequest{}, func() error { return nil }, fmt.Errorf("invalid git build context subdir: %w", validatePathErr)
		}
		contextDir = filepath.Join(repoPath, filepath.FromSlash(source.Subdir))
	}

	entry, err := acfs.Root{Path: repoPath}.Stat(ctx, path.Join("/", source.Subdir), acfstypes.StatOptions{FollowFinal: true})
	if err != nil {
		_ = s.cleanupGitContext(repoPath)
		return buildtypes.BuildRequest{}, func() error { return nil }, fmt.Errorf("failed to stat resolved git build context: %w", err)
	}
	if !entry.IsDirectory {
		_ = s.cleanupGitContext(repoPath)
		return buildtypes.BuildRequest{}, func() error { return nil }, errors.New("resolved git build context is not a directory")
	}

	writeBuildProgressStatus(ctx, progressWriter, serviceName, "using remote build context "+source.Raw)

	resolvedReq := req
	resolvedReq.ContextDir = contextDir

	return resolvedReq, func() error { return s.cleanupGitContext(repoPath) }, nil
}

func (s *BuildService) resolveGitBuildAuth(ctx context.Context, rawURL string) (git.AuthConfig, bool, error) {
	repository, err := s.gitRepository.FindEnabledRepositoryByURL(ctx, rawURL)
	if err != nil {
		return git.AuthConfig{}, false, fmt.Errorf("failed to resolve git repository credentials: %w", err)
	}
	if repository == nil {
		return git.AuthConfig{}, false, nil
	}

	authConfig, err := s.gitRepository.GetAuthConfig(ctx, repository)
	if err != nil {
		return git.AuthConfig{}, true, fmt.Errorf("failed to load git repository credentials: %w", err)
	}

	return authConfig, true, nil
}

func (s *BuildService) cleanupGitContext(repoPath string) error {
	if repoPath == "" {
		return nil
	}
	return s.gitCleanupFn(repoPath)
}

func writeBuildProgressStatus(ctx context.Context, progressWriter io.Writer, serviceName, status string) {
	if progressWriter == nil || strings.TrimSpace(status) == "" {
		return
	}

	line := status
	if service := strings.TrimSpace(serviceName); service != "" {
		line = service + ": " + status
	}
	if _, err := io.WriteString(progressWriter, line+"\n"); err != nil {
		slog.DebugContext(ctx, "failed to write build progress status", "error", err)
	}
}

func (s *BuildService) ListImageBuildsByEnvironmentPaginated(ctx context.Context, environmentID string, params pagination.QueryParams) ([]types.ImageBuildRecord, pagination.Response, error) {
	var builds []ImageBuild
	// The list DTO never includes the build output (buildToRecord with
	// includeOutput=false), so skip reading the up-to-2-MiB output column.
	q := s.db.WithContext(ctx).Model(&ImageBuild{}).Omit("output").Where("environment_id = ?", environmentID)

	if term := strings.TrimSpace(params.Search); term != "" {
		searchPattern := "%" + term + "%"
		q = q.Where(
			"context_dir LIKE ? OR COALESCE(dockerfile, '') LIKE ? OR COALESCE(username, '') LIKE ? OR COALESCE(provider, '') LIKE ? OR COALESCE(error_message, '') LIKE ?",
			searchPattern, searchPattern, searchPattern, searchPattern, searchPattern,
		)
	}

	q = pagination.ApplyFilter(q, "status", params.Filters["status"])
	q = pagination.ApplyFilter(q, "provider", params.Filters["provider"])

	params.Sort = cmp.Or(params.Sort, "createdAt")

	paginationResp, err := pagination.PaginateAndSortDB(params, q, &builds)
	if err != nil {
		return nil, pagination.Response{}, fmt.Errorf("failed to paginate builds: %w", err)
	}

	records := make([]types.ImageBuildRecord, 0, len(builds))
	for _, build := range builds {
		records = append(records, buildToRecord(build, false))
	}

	return records, paginationResp, nil
}

func (s *BuildService) AggregateImageBuilds(ctx context.Context, params pagination.QueryParams) (types.Paginated[types.ImageBuildRecord], error) {
	collection, err := httpx.CollectEnvironments[types.Paginated[types.ImageBuildRecord]](ctx, url.Values{"start": {"0"}, "limit": {"-1"}})
	if err != nil {
		return types.Paginated[types.ImageBuildRecord]{}, err
	}

	pages := make([]pagination.EnvironmentPage[types.ImageBuildRecord], 0, len(collection.Results))
	for _, result := range collection.Results {
		pages = append(pages, pagination.EnvironmentPage[types.ImageBuildRecord]{
			Environment:    result.Environment,
			Items:          result.Value.Data,
			TotalAvailable: cmp.Or(result.Value.Pagination.GrandTotalItems, result.Value.Pagination.TotalItems),
		})
	}

	config := pagination.Config[types.ImageBuildRecord]{SortBindings: []pagination.SortBinding[types.ImageBuildRecord]{
		{
			Key: "createdAt",
			Fn: func(a, b types.ImageBuildRecord) int {
				return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), strings.Compare(a.ID, b.ID))
			},
			DescFn: func(a, b types.ImageBuildRecord) int {
				return cmp.Or(b.CreatedAt.Compare(a.CreatedAt), strings.Compare(a.ID, b.ID))
			},
		},
		{Key: "completedAt", Fn: func(a, b types.ImageBuildRecord) int {
			return kit.FromPtr(a.CompletedAt).Compare(kit.FromPtr(b.CompletedAt))
		}},
		{Key: "status", Fn: func(a, b types.ImageBuildRecord) int {
			return strings.Compare(a.Status, b.Status)
		}},
	}}
	data, resp := pagination.MergeEnvironmentPages(config, pages, params)

	return types.Paginated[types.ImageBuildRecord]{
		Success:            true,
		Data:               data,
		Pagination:         handlerutil.PaginationResponse(resp),
		FailedEnvironments: collection.Failures,
	}, nil
}

func (s *BuildService) GetImageBuildByID(ctx context.Context, environmentID, buildID string) (*types.ImageBuildRecord, error) {
	var build ImageBuild
	if err := s.db.WithContext(ctx).First(&build, "id = ? AND environment_id = ?", buildID, environmentID).Error; err != nil {
		return nil, err
	}

	return new(buildToRecord(build, true)), nil
}

func (s *BuildService) createBuildRecord(ctx context.Context, environmentID string, req buildtypes.BuildRequest, user *types.Actor) (*ImageBuild, error) {
	buildArgs := mapToJSON(req.BuildArgs)
	labels := mapToJSON(req.Labels)
	ulimits := mapToJSON(req.Ulimits)

	var userID *string
	var username *string
	if user != nil {
		userID = &user.ID
		username = &user.Username
	}

	record := &ImageBuild{
		EnvironmentID: environmentID,
		UserID:        userID,
		Username:      username,
		Status:        ImageBuildStatusRunning,
		Provider:      req.Provider,
		ContextDir:    req.ContextDir,
		Dockerfile:    req.Dockerfile,
		Target:        req.Target,
		Tags:          database.StringSlice(req.Tags),
		Platforms:     database.StringSlice(req.Platforms),
		BuildArgs:     buildArgs,
		Labels:        labels,
		CacheFrom:     database.StringSlice(req.CacheFrom),
		CacheTo:       database.StringSlice(req.CacheTo),
		NoCache:       req.NoCache,
		Pull:          req.Pull,
		BuildNetwork:  req.Network,
		Isolation:     req.Isolation,
		ShmSize:       req.ShmSize,
		Ulimits:       ulimits,
		Entitlements:  database.StringSlice(req.Entitlements),
		Privileged:    req.Privileged,
		ExtraHosts:    database.StringSlice(req.ExtraHosts),
		Push:          req.Push,
		Load:          req.Load,
		CreatedAt:     time.Now(),
	}
	if err := s.db.WithContext(ctx).Create(record).Error; err != nil {
		return nil, fmt.Errorf("failed to create build record: %w", err)
	}
	return record, nil
}

func (s *BuildService) completeBuildRecord(
	ctx context.Context,
	buildID string,
	status ImageBuildStatus,
	output *string,
	outputTruncated bool,
	errMsg *string,
	digest *string,
	provider string,
	completedAt time.Time,
	durationMs *int64,
) error {
	updates := map[string]any{
		"status":           status,
		"completed_at":     completedAt,
		"duration_ms":      durationMs,
		"output":           output,
		"output_truncated": outputTruncated,
		"error_message":    errMsg,
		"digest":           digest,
		"provider":         provider,
	}

	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&ImageBuild{}).Where("id = ?", buildID).Updates(updates)
		if result.Error != nil {
			return fmt.Errorf("failed to update build record: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			return errors.New("build record not found")
		}
		return nil
	})
}

func (s *BuildService) PruneHistory(ctx context.Context, retentionDays int) (int64, error) {
	if retentionDays <= 0 {
		return 0, nil
	}

	var buildIDs []string
	if err := s.db.WithContext(ctx).Model(&ImageBuild{}).
		Where("status IN ?", []ImageBuildStatus{ImageBuildStatusSuccess, ImageBuildStatusFailed}).
		Where("COALESCE(completed_at, updated_at, created_at) < ?", time.Now().Add(-time.Duration(retentionDays)*24*time.Hour)).
		Pluck("id", &buildIDs).Error; err != nil {
		return 0, fmt.Errorf("failed to find builds older than retention window: %w", err)
	}

	var deleted int64
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for batch := range slices.Chunk(buildIDs, deleteBuildsBatchSize) {
			result := tx.Where("id IN ?", batch).Delete(&ImageBuild{})
			if result.Error != nil {
				return fmt.Errorf("failed to delete builds: %w", result.Error)
			}
			deleted += result.RowsAffected
		}
		return nil
	})
	return deleted, err
}

func buildToRecord(build ImageBuild, includeOutput bool) types.ImageBuildRecord {
	buildArgs := jsonToStringMap(build.BuildArgs)
	labels := jsonToStringMap(build.Labels)
	ulimits := jsonToStringMap(build.Ulimits)

	var output *string
	if includeOutput {
		output = build.Output
	}

	return types.ImageBuildRecord{
		ID:              build.ID,
		EnvironmentID:   build.EnvironmentID,
		UserID:          build.UserID,
		Username:        build.Username,
		Status:          string(build.Status),
		Provider:        build.Provider,
		ContextDir:      build.ContextDir,
		Dockerfile:      build.Dockerfile,
		Target:          build.Target,
		Tags:            build.Tags,
		Platforms:       build.Platforms,
		BuildArgs:       buildArgs,
		Labels:          labels,
		CacheFrom:       build.CacheFrom,
		CacheTo:         build.CacheTo,
		NoCache:         build.NoCache,
		Pull:            build.Pull,
		Network:         build.BuildNetwork,
		Isolation:       build.Isolation,
		ShmSize:         build.ShmSize,
		Ulimits:         ulimits,
		Entitlements:    build.Entitlements,
		Privileged:      build.Privileged,
		ExtraHosts:      build.ExtraHosts,
		Push:            build.Push,
		Load:            build.Load,
		Digest:          build.Digest,
		ErrorMessage:    build.ErrorMessage,
		Output:          output,
		OutputTruncated: build.OutputTruncated,
		CompletedAt:     build.CompletedAt,
		DurationMs:      build.DurationMs,
		CreatedAt:       build.CreatedAt,
	}
}

func mapToJSON(input map[string]string) database.JSON {
	if len(input) == 0 {
		return nil
	}

	out := database.JSON{}
	for key, value := range input {
		out[key] = value
	}

	return kit.Ternary(len(out) == 0, nil, out)
}

func jsonToStringMap(input database.JSON) map[string]string {
	out := map[string]string{}
	for key, value := range input {
		out[key] = fmt.Sprint(value)
	}

	return kit.Ternary(len(out) == 0, nil, out)
}

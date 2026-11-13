package railpack

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	"github.com/distribution/reference"
	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/util/progress/progressui"
	"github.com/moby/moby/client"
	"github.com/railwayapp/railpack/buildkit"
	"github.com/railwayapp/railpack/core"
	"github.com/railwayapp/railpack/core/app"
	"github.com/railwayapp/railpack/core/plan"
	"golang.org/x/sync/errgroup"

	"kit.getarcane.app/acfs"
	"kit.getarcane.app/builds/pkg/docker"

	"getarcane.dev/arcane/v3/internal/common"
	dockerInternal "getarcane.dev/arcane/v3/internal/docker"
	"getarcane.dev/types/v3"
)

type (
	// Service builds images from app source with Railpack on the daemon's embedded BuildKit.
	Service struct {
		dockerService *dockerInternal.DockerClientService
		generatePlan  func(*app.App, *app.Environment, *core.GenerateBuildPlanOptions) (*core.BuildResult, error)
		solve         func(context.Context, *client.Client, solveRequest, io.Writer) error
		// Railpack installs mise into one fixed temp path, so plans are generated one at a time.
		planMu sync.Mutex
	}

	// Target is what a build needs to know about the daemon before it starts.
	Target struct {
		Platform string
		Exists   bool
	}

	// Request is one Railpack build of ContextDir into Image.
	Request struct {
		Env              map[string]string
		PreviousVersions map[string]string
		ContextDir       string
		Image            string
		Platform         string
		BuildCommand     string
		StartCommand     string
	}

	solveRequest struct {
		plan       *plan.BuildPlan
		env        map[string]string
		contextDir string
		image      string
		cacheKey   string
		platform   string
	}
)

func NewService(dockerService *dockerInternal.DockerClientService) *Service {
	return &Service{
		dockerService: dockerService,
		generatePlan:  core.GenerateBuildPlan,
		solve:         solveWithBuildKit,
	}
}

// Preflight requires the containerd image store, resolves the daemon platform and reports whether image already exists.
func (s *Service) Preflight(ctx context.Context, image string) (Target, error) {
	info, err := s.dockerService.Call(ctx, func(ctx context.Context, dockerClient *client.Client) (client.SystemInfoResult, error) {
		return dockerClient.Info(ctx, client.InfoOptions{})
	})
	if err != nil {
		return Target{}, fmt.Errorf("failed to inspect Docker: %w", err)
	}
	if !dockerInternal.UsesContainerdImageStore(info.Info) {
		return Target{}, common.ErrBuildRequiresContainerdImageStore
	}
	platform := platforms.Format(platforms.Normalize(platforms.Platform{OS: info.Info.OSType, Architecture: info.Info.Architecture}))

	_, err = s.dockerService.Call(ctx, func(ctx context.Context, dockerClient *client.Client) (client.ImageInspectResult, error) {
		return dockerClient.ImageInspect(ctx, image)
	})
	switch {
	case err == nil:
		return Target{Platform: platform, Exists: true}, nil
	case errdefs.IsNotFound(err):
		return Target{Platform: platform, Exists: false}, nil
	default:
		return Target{}, fmt.Errorf("failed to inspect image %s: %w", image, err)
	}
}

// Build plans the app with Railpack and builds it into the daemon's image store as req.Image.
func (s *Service) Build(ctx context.Context, req Request, progress io.Writer) (*types.RailpackBuildInfo, error) {
	named, err := reference.ParseNormalizedNamed(req.Image)
	if err != nil {
		return nil, fmt.Errorf("invalid image reference %q: %w", req.Image, err)
	}
	result, err := s.plan(req, progress)
	if err != nil {
		return nil, err
	}
	dockerClient, err := s.dockerService.GetClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Docker: %w", err)
	}
	if solveErr := s.solve(ctx, dockerClient, solveRequest{
		plan:       result.Plan,
		env:        req.Env,
		contextDir: req.ContextDir,
		image:      req.Image,
		cacheKey:   reference.TrimNamed(named).Name(),
		platform:   req.Platform,
	}, progress); solveErr != nil {
		return nil, solveErr
	}
	return &types.RailpackBuildInfo{
		Packages:        resolvedVersions(result.ResolvedPackages),
		Image:           req.Image,
		RailpackVersion: result.RailpackVersion,
		Providers:       result.DetectedProviders,
	}, nil
}

func (s *Service) plan(req Request, progress io.Writer) (*core.BuildResult, error) {
	source, err := app.NewApp(req.ContextDir)
	if err != nil {
		return nil, fmt.Errorf("failed to open app source: %w", err)
	}
	// Railpack may add variables to its environment; the caller's map stays the secret set.
	env := maps.Clone(req.Env)

	s.planMu.Lock()
	result, err := s.generatePlan(source, app.NewEnvironment(&env), &core.GenerateBuildPlanOptions{
		RailpackVersion:          railpackVersion(),
		BuildCommand:             req.BuildCommand,
		StartCommand:             req.StartCommand,
		PreviousVersions:         req.PreviousVersions,
		ErrorMissingStartCommand: true,
	})
	s.planMu.Unlock()

	if result != nil {
		_, _ = io.WriteString(progress, core.FormatBuildResult(result, core.PrintOptions{Version: railpackVersion()}))
	}
	if err != nil {
		return nil, common.Classify(common.ErrUnavailable, fmt.Errorf("railpack could not resolve package versions: %w", err))
	}
	if !result.Success {
		return nil, common.Classify(common.ErrValidation, fmt.Errorf("railpack could not plan the app: %s", planErrors(result.Logs)))
	}
	return result, nil
}

func solveWithBuildKit(ctx context.Context, dockerClient *client.Client, req solveRequest, progress io.Writer) error {
	planDir, err := acfs.Host{}.MkdirTemp(ctx, os.TempDir(), "arcane-railpack-plan")
	if err != nil {
		return fmt.Errorf("failed to create Railpack plan directory: %w", err)
	}
	defer func() {
		if cleanupErr := (acfs.Host{}).RemoveAll(context.WithoutCancel(ctx), planDir); cleanupErr != nil {
			slog.WarnContext(ctx, "Failed to remove Railpack plan directory", "directory", planDir, "error", cleanupErr)
		}
	}()
	planJSON, err := json.Marshal(req.plan, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("failed to encode Railpack plan: %w", err)
	}
	if writeErr := (acfs.Host{}).Write(ctx, filepath.Join(planDir, planFileName), planJSON, acfs.WriteOptions{Mode: 0o600}); writeErr != nil {
		return fmt.Errorf("failed to write Railpack plan: %w", writeErr)
	}
	solveOpt, err := buildSolveOpt(req, planDir)
	if err != nil {
		return err
	}

	bk, err := bkclient.New(ctx, "", docker.ClientOpts(dockerClient)...)
	if err != nil {
		return fmt.Errorf("failed to connect to Docker BuildKit: %w", err)
	}
	defer func() { _ = bk.Close() }()
	waitCtx, cancelWait := context.WithTimeout(ctx, 30*time.Second)
	err = bk.Wait(waitCtx)
	cancelWait()
	if err != nil {
		return fmt.Errorf("failed to wait for Docker BuildKit: %w", err)
	}

	display, err := progressui.NewDisplay(progress, progressui.PlainMode)
	if err != nil {
		return fmt.Errorf("failed to render build progress: %w", err)
	}
	statusCh := make(chan *bkclient.SolveStatus)
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		// Railpack's frontend runs in-process, so its version always matches the plan.
		_, buildErr := bk.Build(groupCtx, solveOpt, "arcane", buildkit.Build, statusCh)
		return buildErr
	})
	group.Go(func() error {
		// BuildKit closes the channel when the solve ends; draining it never blocks a failed build.
		_, displayErr := display.UpdateFrom(context.WithoutCancel(groupCtx), statusCh)
		return displayErr
	})
	return group.Wait()
}

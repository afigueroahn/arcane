package gitops

import (
	"cmp"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"kit.getarcane.app"

	"getarcane.dev/arcane/v3/internal/common"
	"getarcane.dev/arcane/v3/internal/project"
	"getarcane.dev/arcane/v3/pkg/projects"
	"getarcane.dev/types/v3"
)

func validateDeploySource(sync project.GitOpsSync) error {
	invalid := func(field, message string) error {
		return common.Classify(common.ErrValidation, &types.FieldError{Field: field, Err: errors.New(message)})
	}
	switch cmp.Or(strings.TrimSpace(sync.BuildStrategy), types.GitOpsBuildStrategyCompose) {
	case types.GitOpsBuildStrategyRailpack:
		return validateRailpackSource(sync)
	case types.GitOpsBuildStrategyCompose:
	default:
		return invalid("buildStrategy", fmt.Sprintf("Unsupported build strategy %q.", sync.BuildStrategy))
	}
	if strings.TrimSpace(sync.ComposePath) == "" {
		return invalid("composePath", "compose path is required")
	}
	if composePath := path.Clean(filepath.ToSlash(strings.TrimSpace(sync.ComposePath))); composePath == ".." || strings.HasPrefix(composePath, "../") {
		return invalid("composePath", "Compose path must stay inside the repository.")
	}
	overridePath := strings.TrimSpace(sync.ComposeOverridePath)
	cleanedOverride := path.Clean(filepath.ToSlash(overridePath))
	switch {
	case overridePath == "":
	case len(overridePath) > 512:
		return invalid("composeOverridePath", "Override path must be 512 characters or fewer.")
	case path.IsAbs(cleanedOverride) || cleanedOverride == ".." || strings.HasPrefix(cleanedOverride, "../"):
		return invalid("composeOverridePath", "Override path must be relative to the repository root.")
	case !slices.Contains([]string{".yml", ".yaml"}, strings.ToLower(path.Ext(cleanedOverride))):
		return invalid("composeOverridePath", "Override file must be a .yml or .yaml file.")
	case cleanedOverride == path.Clean(filepath.ToSlash(sync.ComposePath)):
		return invalid("composeOverridePath", "Override file must differ from the compose file.")
	case sync.TargetType != "swarm_stack" && sync.SyncDirectory:
		return invalid("composeOverridePath", "Project directory syncs only merge compose.override.yaml beside the compose file; rename it or list it in COMPOSE_FILE.")
	}
	if _, err := projects.ParseProjectEnvContent(kit.FromPtr(sync.EnvContent), nil); err != nil {
		return invalid("envContent", err.Error())
	}
	return nil
}

// validateRailpackSource keeps a Railpack sync to what its generated single-service project supports.
func validateRailpackSource(sync project.GitOpsSync) error {
	invalid := func(field, message string) error {
		return common.Classify(common.ErrValidation, &types.FieldError{Field: field, Err: errors.New(message)})
	}
	sourcePath := path.Clean(filepath.ToSlash(strings.TrimSpace(sync.SourcePath)))
	switch {
	case sync.TargetType == "swarm_stack":
		return invalid("buildStrategy", "Railpack builds deploy to a project, not a swarm stack.")
	case sync.SyncDirectory:
		return invalid("syncDirectory", "Railpack builds read the whole repository; turn off \"Sync entire directory\".")
	case strings.TrimSpace(sync.ComposeOverridePath) != "":
		return invalid("composeOverridePath", "Railpack builds generate their own compose file and take no override.")
	case sync.PullImageAfterSync:
		return invalid("pullImageAfterSync", "Railpack images are built locally and cannot be pulled.")
	case strings.TrimSpace(kit.FromPtr(sync.PreDeployScriptPath)) != "":
		return invalid("preDeployScriptPath", "Pre-deploy hooks are not supported for Railpack builds.")
	case len(sync.SourcePath) > 512:
		return invalid("sourcePath", "Source directory must be 512 characters or fewer.")
	case path.IsAbs(sourcePath) || sourcePath == ".." || strings.HasPrefix(sourcePath, "../"):
		return invalid("sourcePath", "Source directory must be relative to the repository root.")
	case sync.AppPort < 0 || sync.AppPort > 65535:
		return invalid("appPort", "App port must be between 1 and 65535, or 0 for the default.")
	case sync.HostPort < 0 || sync.HostPort > 65535:
		return invalid("hostPort", "Published port must be between 1 and 65535, or 0 to leave the app unpublished.")
	case len(sync.BuildCommand) > 4096:
		return invalid("buildCommand", "Build command must be 4096 characters or fewer.")
	case len(sync.StartCommand) > 4096:
		return invalid("startCommand", "Start command must be 4096 characters or fewer.")
	}
	if _, err := projects.ParseProjectEnvContent(kit.FromPtr(sync.EnvContent), nil); err != nil {
		return invalid("envContent", err.Error())
	}
	return nil
}

// hasRailpackConfig reports whether a request configures a Railpack build, which needs images:build.
func hasRailpackConfig(req types.GitOpsSync) bool {
	return req.BuildStrategy == types.GitOpsBuildStrategyRailpack ||
		req.SourcePath != "" || req.BuildCommand != "" || req.StartCommand != "" || req.AppPort != 0 || req.HostPort != 0
}

func normalizeSyncLimitSetting(value, defaultValue int) int {
	return kit.Ternary(value < 0, defaultValue, value)
}

func nullableTrimmedString(p *string) *string {
	trimmed := strings.TrimSpace(kit.FromPtr(p))
	return kit.Ternary(trimmed == "", nil, &trimmed)
}

func validateSyncLimits(maxFiles *int, maxTotalSize, maxBinarySize *int64) error {
	if kit.FromPtr(maxFiles) < 0 {
		return errors.New("maxSyncFiles must be non-negative")
	}
	if kit.FromPtr(maxTotalSize) < 0 {
		return errors.New("maxSyncTotalSize must be non-negative")
	}
	if kit.FromPtr(maxBinarySize) < 0 {
		return errors.New("maxSyncBinarySize must be non-negative")
	}
	return nil
}

func hasPreDeployConfig(submitted map[string]bool) bool {
	return slices.ContainsFunc([]string{
		"preDeployScriptPath",
		"preDeployRunnerImage",
		"preDeployEnv",
		"preDeployExtraMounts",
		"preDeployTimeoutSec",
		"preDeployNetworkMode",
	}, func(field string) bool {
		return submitted[field]
	})
}

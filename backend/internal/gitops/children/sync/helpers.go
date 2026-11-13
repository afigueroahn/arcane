package sync

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v4"

	"kit.getarcane.app"

	projectpkg "getarcane.dev/arcane/v3/internal/project"
	"getarcane.dev/arcane/v3/pkg/gitutil"
	"getarcane.dev/arcane/v3/pkg/projects"
)

const (
	railpackServiceName    = "app"
	defaultRailpackAppPort = 3000
)

func overrideFileNameFor(repoPath string) string {
	name := path.Base(filepath.ToSlash(repoPath))
	return kit.Ternary(slices.Contains(projects.ComposeOverrideFileCandidates, name), name, projects.DefaultComposeOverrideFileName)
}

// railpackImageRef tags a build with its commit and a hash of every other build input, so a new
// commit or setting builds a new image while a repeat sync reuses the one it already built.
func railpackImageRef(sync *projectpkg.GitOpsSync, commit string, env map[string]string) string {
	inputs := []string{sync.SourcePath, sync.BuildCommand, sync.StartCommand}
	for _, key := range slices.Sorted(maps.Keys(env)) {
		inputs = append(inputs, key+"="+env[key])
	}
	repository := projects.LocalImageRepository(sync.ID, sync.ProjectName, railpackServiceName)
	return repository + ":" + commit[:min(12, len(commit))] + "-" + kit.SHA256Hex(strings.Join(inputs, "\x00"))[:8]
}

// railpackCompose is the project compose file of a Railpack sync: one service that runs the built image.
func railpackCompose(image string, appPort, hostPort int) (string, error) {
	port := strconv.Itoa(cmp.Or(appPort, defaultRailpackAppPort))
	service := map[string]any{
		"image":       image,
		"pull_policy": "never",
		"restart":     "unless-stopped",
		// The project's effective .env carries the sync's variables plus any project overrides.
		"env_file":    []map[string]any{{"path": projects.EffectiveEnvFileName, "required": false}},
		"environment": map[string]string{"PORT": port},
	}
	if hostPort > 0 {
		service["ports"] = []string{strconv.Itoa(hostPort) + ":" + port}
	}
	content, err := yaml.Marshal(map[string]any{"services": map[string]any{railpackServiceName: service}})
	if err != nil {
		return "", fmt.Errorf("failed to encode compose file: %w", err)
	}
	return string(content), nil
}

// railpackContextDir resolves the app directory inside the clone. Symlinks are resolved first, so the
// build context can never reach files outside the repository.
func railpackContextDir(repoPath, sourcePath string) (string, error) {
	requested := filepath.FromSlash(strings.TrimSpace(sourcePath))
	if err := git.ValidatePath(repoPath, requested); err != nil {
		return "", fmt.Errorf("invalid source directory %q: %w", sourcePath, err)
	}
	root, err := filepath.EvalSymlinks(repoPath)
	if err != nil {
		return "", fmt.Errorf("failed to resolve repository path: %w", err)
	}
	dir, err := filepath.EvalSymlinks(filepath.Join(root, requested))
	if err != nil {
		return "", fmt.Errorf("source directory %q not found in the repository: %w", sourcePath, err)
	}
	if rel, relErr := filepath.Rel(root, dir); relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("source directory %q resolves outside the repository", sourcePath)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("failed to inspect source directory %q: %w", sourcePath, err)
	}
	if !info.IsDir() {
		return "", errors.New("source directory " + strconv.Quote(sourcePath) + " is not a directory")
	}
	return dir, nil
}

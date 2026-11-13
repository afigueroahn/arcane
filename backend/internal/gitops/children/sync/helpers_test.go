package sync

import (
	"os"
	"path/filepath"
	"testing"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"kit.getarcane.app"

	projectpkg "getarcane.dev/arcane/v3/internal/project"
	"getarcane.dev/arcane/v3/pkg/projects"
)

func TestRailpackCompose_RunsTheBuiltImageOnItsPort(t *testing.T) {
	const image = "arcane.local/web-12345678/app:0123456789ab-0f1e2d3c"
	content, err := railpackCompose(image, 8080, 18080)
	require.NoError(t, err)

	dir := t.TempDir()
	composePath := filepath.Join(dir, projects.DefaultComposeFileName)
	require.NoError(t, os.WriteFile(composePath, []byte(content), 0o600))
	project, err := projects.LoadComposeProject(t.Context(), projects.LoadOptions{ComposeFile: composePath, ProjectName: "web", ProjectsDirectory: dir})
	require.NoError(t, err)

	service, ok := project.Services[railpackServiceName]
	require.True(t, ok)
	assert.Equal(t, image, service.Image)
	assert.Equal(t, composetypes.PullPolicyNever, service.PullPolicy)
	assert.Equal(t, "8080", kit.FromPtr(service.Environment["PORT"]))
	require.Len(t, service.Ports, 1)
	assert.Equal(t, "18080", service.Ports[0].Published)
	assert.EqualValues(t, 8080, service.Ports[0].Target)
}

func TestRailpackCompose_DefaultsPortAndLeavesAppUnpublished(t *testing.T) {
	content, err := railpackCompose("arcane.local/web-12345678/app:abc", 0, 0)
	require.NoError(t, err)

	assert.Contains(t, content, "PORT: \"3000\"")
	assert.NotContains(t, content, "ports:")
}

func TestRailpackImageRef_TracksCommitAndBuildInputs(t *testing.T) {
	const commit = "0123456789abcdef0123456789abcdef01234567"
	sync := &projectpkg.GitOpsSync{ID: "1234567890ab", ProjectName: "Web App", SourcePath: "web", StartCommand: "node server.js"}
	env := map[string]string{"API_KEY": "a"}

	ref := railpackImageRef(sync, commit, env)

	assert.Regexp(t, `^arcane\.local/web-app-12345678/app:0123456789ab-[0-9a-f]{8}$`, ref)
	assert.Equal(t, ref, railpackImageRef(sync, commit, map[string]string{"API_KEY": "a"}))
	assert.NotEqual(t, ref, railpackImageRef(sync, "fedcba9876543210fedcba9876543210fedcba98", env))
	assert.NotEqual(t, ref, railpackImageRef(sync, commit, map[string]string{"API_KEY": "b"}))
	changed := *sync
	changed.StartCommand = "npm start"
	assert.NotEqual(t, ref, railpackImageRef(&changed, commit, env))
}

func TestRailpackContextDir_StaysInsideTheRepository(t *testing.T) {
	repo := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "apps", "web"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "README.md"), []byte("demo\n"), 0o600))
	require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(repo, "escape")))
	require.NoError(t, os.Symlink(filepath.Join(repo, "apps"), filepath.Join(repo, "linked")))
	root, err := filepath.EvalSymlinks(repo)
	require.NoError(t, err)

	dir, err := railpackContextDir(repo, "apps/web")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "apps", "web"), dir)

	dir, err = railpackContextDir(repo, "")
	require.NoError(t, err)
	assert.Equal(t, root, dir)

	dir, err = railpackContextDir(repo, "linked/web")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "apps", "web"), dir)

	for _, rejected := range []string{"../outside", "escape", "README.md", "missing"} {
		_, err = railpackContextDir(repo, rejected)
		assert.Error(t, err, rejected)
	}
}

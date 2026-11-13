package railpack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/libtnb/sqlite"
	"github.com/moby/moby/client"
	"github.com/railwayapp/railpack/core"
	"github.com/railwayapp/railpack/core/app"
	"github.com/railwayapp/railpack/core/logger"
	"github.com/railwayapp/railpack/core/resolver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"kit.getarcane.app/docker/streams/bus"

	"getarcane.dev/arcane/v3/internal/common"
	"getarcane.dev/arcane/v3/internal/config"
	"getarcane.dev/arcane/v3/internal/database"
	dockerInternal "getarcane.dev/arcane/v3/internal/docker"
	"getarcane.dev/arcane/v3/internal/settings"
)

const testImage = "arcane.local/demo-12345678/app:abc123-0f1e2d3c"

func TestPreflight_RequiresContainerdImageStore(t *testing.T) {
	svc := newTestService(t, fakeDockerAPI("overlay2", false))

	_, err := svc.Preflight(t.Context(), testImage)

	require.ErrorIs(t, err, common.ErrBuildRequiresContainerdImageStore)
}

func TestPreflight_ResolvesPlatformAndMissingImage(t *testing.T) {
	svc := newTestService(t, fakeDockerAPI("io.containerd.snapshotter.v1", false))

	target, err := svc.Preflight(t.Context(), testImage)

	require.NoError(t, err)
	assert.Equal(t, Target{Platform: "linux/amd64", Exists: false}, target)
}

func TestPreflight_ReportsExistingImage(t *testing.T) {
	svc := newTestService(t, fakeDockerAPI("io.containerd.snapshotter.v1", true))

	target, err := svc.Preflight(t.Context(), testImage)

	require.NoError(t, err)
	assert.True(t, target.Exists)
}

func TestBuild_PlanFailureIsValidationErrorAndSkipsSolve(t *testing.T) {
	svc := newTestService(t, fakeDockerAPI("io.containerd.snapshotter.v1", false))
	svc.generatePlan = func(*app.App, *app.Environment, *core.GenerateBuildPlanOptions) (*core.BuildResult, error) {
		return &core.BuildResult{Logs: []logger.Msg{{Level: logger.Error, Msg: "no start command was found"}}}, nil
	}
	svc.solve = func(context.Context, *client.Client, solveRequest, io.Writer) error {
		t.Fatal("solve must not run when planning fails")
		return nil
	}

	_, err := svc.Build(t.Context(), Request{ContextDir: t.TempDir(), Image: testImage}, io.Discard)

	require.ErrorIs(t, err, common.ErrValidation)
	assert.Contains(t, err.Error(), "no start command was found")
}

func TestBuild_TransientPlanErrorIsRetryable(t *testing.T) {
	svc := newTestService(t, fakeDockerAPI("io.containerd.snapshotter.v1", false))
	svc.generatePlan = func(*app.App, *app.Environment, *core.GenerateBuildPlanOptions) (*core.BuildResult, error) {
		return &core.BuildResult{}, errors.New("github rate limited the mise lookup")
	}

	_, err := svc.Build(t.Context(), Request{ContextDir: t.TempDir(), Image: testImage}, io.Discard)

	require.ErrorIs(t, err, common.ErrUnavailable)
}

func TestBuild_SolvesPlanAndReturnsBuildInfo(t *testing.T) {
	svc := newTestService(t, fakeDockerAPI("io.containerd.snapshotter.v1", false))
	req := Request{
		Env:              map[string]string{"API_KEY": "s3cret", "RAILPACK_NODE_VERSION": "22"},
		PreviousVersions: map[string]string{"node": "22.10.0"},
		ContextDir:       t.TempDir(),
		Image:            testImage,
		Platform:         "linux/amd64",
		BuildCommand:     "npm run build",
		StartCommand:     "node server.js",
	}
	var planOptions *core.GenerateBuildPlanOptions
	svc.generatePlan = func(_ *app.App, env *app.Environment, options *core.GenerateBuildPlanOptions) (*core.BuildResult, error) {
		planOptions = options
		assert.Equal(t, req.Env, env.Variables)
		env.SetVariable("ADDED_BY_RAILPACK", "1")
		return &core.BuildResult{
			RailpackVersion:   "0.43.0",
			Plan:              testBuildPlan(),
			ResolvedPackages:  map[string]*resolver.ResolvedPackage{"node": {Name: "node", ResolvedVersion: new("22.11.0")}},
			DetectedProviders: []string{"node"},
			Success:           true,
		}, nil
	}
	var solved solveRequest
	svc.solve = func(_ context.Context, _ *client.Client, solve solveRequest, _ io.Writer) error {
		solved = solve
		return nil
	}
	var progress bytes.Buffer

	info, err := svc.Build(t.Context(), req, &progress)

	require.NoError(t, err)
	assert.Equal(t, "npm run build", planOptions.BuildCommand)
	assert.Equal(t, "node server.js", planOptions.StartCommand)
	assert.Equal(t, req.PreviousVersions, planOptions.PreviousVersions)
	assert.True(t, planOptions.ErrorMissingStartCommand)
	assert.NotContains(t, req.Env, "ADDED_BY_RAILPACK")

	assert.Equal(t, req.Env, solved.env)
	assert.Equal(t, req.ContextDir, solved.contextDir)
	assert.Equal(t, testImage, solved.image)
	assert.Equal(t, "arcane.local/demo-12345678/app", solved.cacheKey)
	assert.Equal(t, "linux/amd64", solved.platform)
	assert.Equal(t, "npm start", solved.plan.Deploy.StartCmd)

	assert.Equal(t, "0.43.0", info.RailpackVersion)
	assert.Equal(t, []string{"node"}, info.Providers)
	assert.Equal(t, map[string]string{"node": "22.11.0"}, info.Packages)
	assert.Equal(t, testImage, info.Image)
	assert.Contains(t, progress.String(), "Railpack")
}

func TestBuild_RejectsInvalidImageReference(t *testing.T) {
	svc := newTestService(t, fakeDockerAPI("io.containerd.snapshotter.v1", false))

	_, err := svc.Build(t.Context(), Request{ContextDir: t.TempDir(), Image: "Not A Valid Ref"}, io.Discard)

	require.Error(t, err)
}

func fakeDockerAPI(driverType string, imageExists bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := strings.TrimPrefix(r.URL.Path, "/v1.41")
		switch {
		case r.Method == http.MethodGet && path == "/info":
			_, _ = fmt.Fprintf(w, `{"OSType":"linux","Architecture":"x86_64","DriverStatus":[["driver-type",%q]]}`, driverType)
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/images/") && imageExists:
			_, _ = w.Write([]byte(`{"Id":"sha256:0f1e2d3c"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))
		}
	}
}

func newTestService(t *testing.T, handler http.HandlerFunc) *Service {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	dockerClient, err := client.New(client.WithHost(server.URL), client.WithAPIVersion("1.41"), client.WithHTTPClient(server.Client()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = dockerClient.Close() })

	gormDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, gormDB.AutoMigrate(&settings.SettingVariable{}))
	db := &database.DB{DB: gormDB}
	settingsService, err := settings.NewSettingsService(settings.SettingsServiceParams{
		Ctx:    t.Context(),
		DB:     db,
		Config: &config.Config{},
		Env:    settings.Env{},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, settingsService.Stop(context.WithoutCancel(t.Context()))) })

	return NewService(dockerInternal.NewDockerClientService(t.Context(), db, settingsService, bus.Config{}).WithClient(dockerClient))
}

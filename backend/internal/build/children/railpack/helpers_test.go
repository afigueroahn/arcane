package railpack

import (
	"context"
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"path/filepath"
	"testing"

	"github.com/moby/buildkit/session/secrets"
	"github.com/railwayapp/railpack/core/logger"
	"github.com/railwayapp/railpack/core/plan"
	"github.com/railwayapp/railpack/core/resolver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSecretsHash_IgnoresMapOrderAndTracksValues(t *testing.T) {
	base := secretsHash(map[string]string{"A": "1", "B": "2"})

	assert.Equal(t, base, secretsHash(map[string]string{"B": "2", "A": "1"}))
	assert.NotEqual(t, base, secretsHash(map[string]string{"A": "1", "B": "3"}))
	assert.NotEqual(t, base, secretsHash(map[string]string{"A": "1", "B": "2", "C": "3"}))
}

func TestBuildSolveOpt_MountsPlanAndPassesEveryVariableAsSecret(t *testing.T) {
	req := solveRequest{
		env:        map[string]string{"API_KEY": "s3cret", "PORT": "3000"},
		contextDir: t.TempDir(),
		image:      "arcane.local/demo-12345678/app:abc123",
		cacheKey:   "arcane.local/demo-12345678/app",
		platform:   "linux/amd64",
	}

	opt, err := buildSolveOpt(req, t.TempDir())
	require.NoError(t, err)

	assert.Equal(t, map[string]string{
		"filename":               planFileName,
		"platform":               "linux/amd64",
		"build-arg:cache-key":    "arcane.local/demo-12345678/app",
		"build-arg:secrets-hash": secretsHash(req.env),
	}, opt.FrontendAttrs)
	assert.Contains(t, opt.LocalMounts, "context")
	assert.Contains(t, opt.LocalMounts, "dockerfile")
	require.Len(t, opt.Exports, 1)
	assert.Equal(t, "moby", opt.Exports[0].Type)
	assert.Equal(t, map[string]string{"name": req.image}, opt.Exports[0].Attrs)

	require.Len(t, opt.Session, 1)
	provider, ok := opt.Session[0].(interface {
		GetSecret(context.Context, *secrets.GetSecretRequest) (*secrets.GetSecretResponse, error)
	})
	require.True(t, ok)
	for key, value := range req.env {
		resp, secretErr := provider.GetSecret(t.Context(), &secrets.GetSecretRequest{ID: key})
		require.NoError(t, secretErr)
		assert.Equal(t, value, string(resp.Data))
	}
}

func TestBuildSolveOpt_RejectsMissingContext(t *testing.T) {
	_, err := buildSolveOpt(solveRequest{contextDir: filepath.Join(t.TempDir(), "missing")}, t.TempDir())

	require.Error(t, err)
}

// Railpack's frontend decodes the plan file with encoding/json v1.
func TestPlanJSON_DecodesWithRailpackFrontendDecoder(t *testing.T) {
	data, err := json.Marshal(testBuildPlan(), json.Deterministic(true))
	require.NoError(t, err)

	decoded := plan.NewBuildPlan()
	require.NoError(t, jsonv1.Unmarshal(data, decoded))
	reencoded, err := json.Marshal(decoded, json.Deterministic(true))
	require.NoError(t, err)

	assert.JSONEq(t, string(data), string(reencoded))
	assert.Equal(t, "npm start", decoded.Deploy.StartCmd)
	require.Len(t, decoded.Steps, 1)
	assert.Len(t, decoded.Steps[0].Commands, 3)
}

func TestPlanErrors(t *testing.T) {
	logs := []logger.Msg{
		{Level: logger.Info, Msg: "detected node"},
		{Level: logger.Error, Msg: "no start command found"},
		{Level: logger.Error, Msg: "missing package.json"},
	}

	assert.Equal(t, "no start command found; missing package.json", planErrors(logs))
	assert.Equal(t, "see the build output for details", planErrors(logs[:1]))
}

func TestResolvedVersions_SkipsUnresolvedPackages(t *testing.T) {
	packages := map[string]*resolver.ResolvedPackage{
		"node":   {Name: "node", ResolvedVersion: new("22.11.0")},
		"python": {Name: "python"},
	}

	assert.Equal(t, map[string]string{"node": "22.11.0"}, resolvedVersions(packages))
}

func testBuildPlan() *plan.BuildPlan {
	buildPlan := plan.NewBuildPlan()
	install := plan.NewStep("install")
	install.Inputs = []plan.Layer{plan.NewImageLayer("ghcr.io/railwayapp/railpack-builder:test"), plan.NewLocalLayer()}
	install.AddCommands([]plan.Command{
		plan.NewCopyCommand("package.json"),
		plan.NewExecShellCommand("npm ci"),
		plan.NewPathCommand("/app/node_modules/.bin"),
	})
	install.Secrets = []string{"*"}
	buildPlan.Steps = append(buildPlan.Steps, *install)
	buildPlan.Secrets = []string{"API_KEY"}
	buildPlan.Deploy.Base = plan.NewImageLayer("ghcr.io/railwayapp/railpack-runtime:test")
	buildPlan.Deploy.Inputs = []plan.Layer{plan.NewStepLayer("install", plan.NewIncludeFilter([]string{"/app"}))}
	buildPlan.Deploy.StartCmd = "npm start"
	return buildPlan
}

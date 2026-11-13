package railpack

import (
	"cmp"
	"fmt"
	"maps"
	"runtime/debug"
	"slices"
	"strings"
	"sync"

	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/secrets/secretsprovider"
	"github.com/railwayapp/railpack/core/logger"
	"github.com/railwayapp/railpack/core/resolver"
	"github.com/tonistiigi/fsutil"

	"kit.getarcane.app"
)

const (
	planFileName   = "railpack-plan.json"
	railpackModule = "github.com/railwayapp/railpack"
)

// railpackVersion is the linked Railpack module version, stamped into plans and images.
var railpackVersion = sync.OnceValue(func() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			if dep.Path == railpackModule {
				return strings.TrimPrefix(dep.Version, "v")
			}
		}
	}
	return "dev"
})

// buildSolveOpt mounts the source as "context" and the plan as "dockerfile", the names Railpack's frontend reads.
func buildSolveOpt(req solveRequest, planDir string) (bkclient.SolveOpt, error) {
	contextFS, err := fsutil.NewFS(req.contextDir)
	if err != nil {
		return bkclient.SolveOpt{}, fmt.Errorf("failed to open build context: %w", err)
	}
	planFS, err := fsutil.NewFS(planDir)
	if err != nil {
		return bkclient.SolveOpt{}, fmt.Errorf("failed to open Railpack plan directory: %w", err)
	}
	secrets := make(map[string][]byte, len(req.env))
	for key, value := range req.env {
		secrets[key] = []byte(value)
	}
	return bkclient.SolveOpt{
		Exports:     []bkclient.ExportEntry{{Type: "moby", Attrs: map[string]string{"name": req.image}}},
		LocalMounts: map[string]fsutil.FS{"context": contextFS, "dockerfile": planFS},
		FrontendAttrs: map[string]string{
			"filename":               planFileName,
			"platform":               req.platform,
			"build-arg:cache-key":    req.cacheKey,
			"build-arg:secrets-hash": secretsHash(req.env),
		},
		Session: []session.Attachable{secretsprovider.FromMap(secrets)},
	}, nil
}

// secretsHash fingerprints the build secrets so Railpack reruns the steps that use them when a value changes.
func secretsHash(env map[string]string) string {
	var lines strings.Builder
	for _, key := range slices.Sorted(maps.Keys(env)) {
		lines.WriteString(key + "=" + env[key] + "\n")
	}
	return kit.SHA256Hex(lines.String())
}

func planErrors(logs []logger.Msg) string {
	var messages []string
	for _, msg := range logs {
		if msg.Level == logger.Error {
			messages = append(messages, msg.Msg)
		}
	}
	return cmp.Or(strings.Join(messages, "; "), "see the build output for details")
}

func resolvedVersions(packages map[string]*resolver.ResolvedPackage) map[string]string {
	versions := make(map[string]string, len(packages))
	for name, pkg := range packages {
		if version := kit.FromPtr(pkg.ResolvedVersion); version != "" {
			versions[name] = version
		}
	}
	return versions
}

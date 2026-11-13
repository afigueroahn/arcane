package gitops

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"getarcane.dev/types/v3"
)

func TestHasRailpackConfig(t *testing.T) {
	tests := []struct {
		name string
		req  types.GitOpsSync
		want bool
	}{
		{name: "compose sync", req: types.GitOpsSync{BuildStrategy: types.GitOpsBuildStrategyCompose, ComposePath: "compose.yaml"}, want: false},
		{name: "default strategy", req: types.GitOpsSync{Branch: "main"}, want: false},
		{name: "railpack strategy", req: types.GitOpsSync{BuildStrategy: types.GitOpsBuildStrategyRailpack}, want: true},
		{name: "source path", req: types.GitOpsSync{SourcePath: "web"}, want: true},
		{name: "start command", req: types.GitOpsSync{StartCommand: "node server.js"}, want: true},
		{name: "published port", req: types.GitOpsSync{HostPort: 8080}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, hasRailpackConfig(tt.req))
		})
	}
}

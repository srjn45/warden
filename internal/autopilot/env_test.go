package autopilot

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// writeWorkflow drops a workflow file under <repo>/.github/workflows.
func writeWorkflow(t *testing.T, repo, name, body string) {
	t.Helper()
	dir := filepath.Join(repo, ".github", "workflows")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644))
}

func TestWorkflowsCoverPRs(t *testing.T) {
	const integration = "autopilot/integration"
	tests := []struct {
		name     string
		workflow string // "" ⇒ no workflows dir at all
		want     bool
	}{
		{
			name: "no workflows dir",
			want: false,
		},
		{
			name:     "pull_request scalar covers all targets",
			workflow: "on: pull_request\njobs: {}\n",
			want:     true,
		},
		{
			name:     "pull_request sequence covers all targets",
			workflow: "on: [push, pull_request]\njobs: {}\n",
			want:     true,
		},
		{
			name:     "pull_request with no branch filter covers all",
			workflow: "on:\n  pull_request: {}\njobs: {}\n",
			want:     true,
		},
		{
			name:     "branch filter matching the integration branch exactly",
			workflow: "on:\n  pull_request:\n    branches: [autopilot/integration]\njobs: {}\n",
			want:     true,
		},
		{
			name:     "branch glob covering the integration branch",
			workflow: "on:\n  pull_request:\n    branches: ['autopilot/**']\njobs: {}\n",
			want:     true,
		},
		{
			name:     "branch filter for main only does not cover integration",
			workflow: "on:\n  pull_request:\n    branches: [main]\njobs: {}\n",
			want:     false,
		},
		{
			name:     "push-only workflow does not count",
			workflow: "on:\n  push:\n    branches: [main]\njobs: {}\n",
			want:     false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			if tc.workflow != "" {
				writeWorkflow(t, repo, "ci.yml", tc.workflow)
			}
			got, err := execEnv{}.WorkflowsCoverPRs(context.Background(), repo, integration)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

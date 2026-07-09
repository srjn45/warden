package autopilot

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// --- writePlanIfAbsent ---

func TestWritePlanIfAbsent_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "autopilot.plan.yaml")
	var out bytes.Buffer
	require.NoError(t, writePlanIfAbsent(path, &out))
	require.FileExists(t, path)
	data, _ := os.ReadFile(path)
	require.Contains(t, string(data), "version: 1")
	require.Contains(t, out.String(), "✓")
}

func TestWritePlanIfAbsent_SkipsExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "autopilot.plan.yaml")
	require.NoError(t, os.WriteFile(path, []byte("version: 1\ngoal: existing\n"), 0o644))
	var out bytes.Buffer
	require.NoError(t, writePlanIfAbsent(path, &out))
	data, _ := os.ReadFile(path)
	require.Contains(t, string(data), "existing") // unchanged
	require.Contains(t, out.String(), "already exists")
}

// --- Init ---

func TestInit_CreatesIntegrationBranch(t *testing.T) {
	env := &fakeEnv{}
	dir := t.TempDir()
	var out bytes.Buffer
	err := Init(context.Background(), env, dir, InitConfig{
		PlanFile:          "plan.yaml",
		IntegrationBranch: "autopilot/integration",
	}, &out)
	require.NoError(t, err)
	require.Contains(t, env.created, dir+"|autopilot/integration|main")
	require.Contains(t, out.String(), "✓ created integration branch")
}

func TestInit_SkipsBranchIfExists(t *testing.T) {
	repo := t.TempDir()
	env := &fakeEnv{exists: map[string]bool{repo + "\x00autopilot/integration": true}}
	var out bytes.Buffer
	require.NoError(t, Init(context.Background(), env, repo, InitConfig{
		PlanFile:          "plan.yaml",
		IntegrationBranch: "autopilot/integration",
	}, &out))
	require.Empty(t, env.created)
	require.Contains(t, out.String(), "already exists")
}

func TestInit_DefaultsApplied(t *testing.T) {
	env := &fakeEnv{}
	dir := t.TempDir()
	var out bytes.Buffer
	require.NoError(t, Init(context.Background(), env, dir, InitConfig{}, &out))
	// default plan file
	require.FileExists(t, filepath.Join(dir, "autopilot.plan.yaml"))
	// default integration branch created
	require.Contains(t, env.created, dir+"|autopilot/integration|main")
}

func TestInit_ProtectedBranchSkipped(t *testing.T) {
	env := &fakeEnv{defaultBranch: "main"}
	dir := t.TempDir()
	var out bytes.Buffer
	err := Init(context.Background(), env, dir, InitConfig{
		IntegrationBranch: "main", // protected
	}, &out)
	require.NoError(t, err) // warning, not fatal
	require.Contains(t, out.String(), "warning: integration branch")
}

// --- ciCoversIntegration ---

func TestCICoversIntegration(t *testing.T) {
	tests := []struct {
		name     string
		workflow string
		branch   string
		want     bool
	}{
		{
			name:   "no workflows dir",
			branch: "autopilot/integration",
			want:   false,
		},
		{
			name: "scalar pull_request",
			workflow: `on: pull_request
jobs:
  test:
    runs-on: ubuntu-latest
    steps: []
`,
			branch: "autopilot/integration",
			want:   true,
		},
		{
			name: "sequence with pull_request",
			workflow: `on: [push, pull_request]
jobs: {}
`,
			branch: "autopilot/integration",
			want:   true,
		},
		{
			name: "mapping pull_request no branches filter",
			workflow: `on:
  pull_request:
jobs: {}
`,
			branch: "autopilot/integration",
			want:   true,
		},
		{
			name: "mapping pull_request with exact branch match",
			workflow: `on:
  pull_request:
    branches:
      - autopilot/integration
jobs: {}
`,
			branch: "autopilot/integration",
			want:   true,
		},
		{
			name: "mapping pull_request with wildcard",
			workflow: `on:
  pull_request:
    branches:
      - autopilot/**
jobs: {}
`,
			branch: "autopilot/integration",
			want:   true,
		},
		{
			name: "mapping pull_request different branch",
			workflow: `on:
  pull_request:
    branches:
      - main
      - develop
jobs: {}
`,
			branch: "autopilot/integration",
			want:   false,
		},
		{
			name: "push only — no pull_request",
			workflow: `on:
  push:
    branches: [main]
jobs: {}
`,
			branch: "autopilot/integration",
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := t.TempDir()
			if tt.workflow != "" {
				wfDir := filepath.Join(repo, ".github", "workflows")
				require.NoError(t, os.MkdirAll(wfDir, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(wfDir, "ci.yml"), []byte(tt.workflow), 0o644))
			}
			got := ciCoversIntegration(repo, tt.branch)
			require.Equal(t, tt.want, got, "ciCoversIntegration(repo, %q)", tt.branch)
		})
	}
}

// --- branchGlobMatches ---

func TestBranchGlobMatches(t *testing.T) {
	tests := []struct {
		pattern, branch string
		want            bool
	}{
		{"**", "autopilot/integration", true},
		{"*", "anything", true},
		{"autopilot/integration", "autopilot/integration", true},
		{"autopilot/**", "autopilot/integration", true},
		{"autopilot/**", "autopilot/foo/bar", true},
		{"autopilot/**", "main", false},
		{"autopilot/*", "autopilot/integration", true},
		{"autopilot/*", "autopilot/foo/bar", false},
		{"main", "autopilot/integration", false},
	}
	for _, tt := range tests {
		got := branchGlobMatches(tt.pattern, tt.branch)
		if got != tt.want {
			t.Errorf("branchGlobMatches(%q, %q) = %v, want %v", tt.pattern, tt.branch, got, tt.want)
		}
	}
}

// --- updateAutopilotConfig ---

func TestUpdateAutopilotConfig_SkipsMissingFile(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, updateAutopilotConfig("/nonexistent/config.yaml", "plan.yaml", nil, &out))
	require.Contains(t, out.String(), "not found")
}

func TestUpdateAutopilotConfig_UpdatesPlans(t *testing.T) {
	cfg := minimalAutopilotConfig()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(cfg), 0o644))

	var out bytes.Buffer
	require.NoError(t, updateAutopilotConfig(path, "autopilot.plan.yaml", nil, &out))
	require.Contains(t, out.String(), "✓")

	data, _ := os.ReadFile(path)
	require.Contains(t, string(data), "autopilot.plan.yaml")
}

func TestUpdateAutopilotConfig_IdempotentWithExistingPlans(t *testing.T) {
	cfg := `autopilot:
  plans:
    - file: existing.yaml
  brain:
    backends:
      free: []
      subscription: []
      pay_per_use: []
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(cfg), 0o644))

	var out bytes.Buffer
	require.NoError(t, updateAutopilotConfig(path, "new.yaml", nil, &out))
	require.Contains(t, out.String(), "already set")

	data, _ := os.ReadFile(path)
	require.True(t, strings.Contains(string(data), "existing.yaml"), "existing plan must not be overwritten")
	require.False(t, strings.Contains(string(data), "new.yaml"), "new plan must not be added")
}

func TestUpdateAutopilotConfig_SetsDetectedBackends(t *testing.T) {
	cfg := minimalAutopilotConfig()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(cfg), 0o644))

	var out bytes.Buffer
	require.NoError(t, updateAutopilotConfig(path, "plan.yaml", []string{"claude", "antigravity"}, &out))

	data, _ := os.ReadFile(path)
	s := string(data)
	require.Contains(t, s, "claude")
	require.Contains(t, s, "antigravity")
}

// minimalAutopilotConfig returns a YAML config with an empty autopilot block.
func minimalAutopilotConfig() string {
	return `autopilot:
  enabled: false
  plans: []
  brain:
    backends:
      free: []
      subscription: []
      pay_per_use: []
  merge:
    target_branch: autopilot/integration
`
}

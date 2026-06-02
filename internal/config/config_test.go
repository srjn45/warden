package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("AGENTCTL_ADDR", "")
	c := Load()
	require.Equal(t, "127.0.0.1:8765", c.Addr)
}

func TestLoadFromEnv(t *testing.T) {
	t.Setenv("AGENTCTL_ADDR", "127.0.0.1:9000")
	require.Equal(t, "127.0.0.1:9000", Load().Addr)
}

func TestDataDirDefault(t *testing.T) {
	t.Setenv("AGENTCTL_DATA_DIR", "")
	c := Load()
	require.True(t, strings.HasSuffix(c.DataDir, ".agentctl"), "got %q", c.DataDir)
}

func TestDataDirFromEnv(t *testing.T) {
	t.Setenv("AGENTCTL_DATA_DIR", "/tmp/agentctl-data")
	require.Equal(t, "/tmp/agentctl-data", Load().DataDir)
}

func TestWorkdirDefault(t *testing.T) {
	t.Setenv("AGENTCTL_WORKDIR", "")
	c := Load()
	require.True(t, strings.HasSuffix(c.Workdir, "agentctl-agents"), "got %q", c.Workdir)
}

func TestWorkdirFromEnv(t *testing.T) {
	t.Setenv("AGENTCTL_WORKDIR", "/tmp/agents")
	require.Equal(t, "/tmp/agents", Load().Workdir)
}

func TestClaudeProjectsDirDefault(t *testing.T) {
	t.Setenv("CLAUDE_PROJECTS_DIR", "")
	c := Load()
	require.True(t, strings.HasSuffix(c.ClaudeProjectsDir, ".claude/projects"), "got %q", c.ClaudeProjectsDir)
}

func TestClaudeProjectsDirFromEnv(t *testing.T) {
	t.Setenv("CLAUDE_PROJECTS_DIR", "/tmp/projects")
	require.Equal(t, "/tmp/projects", Load().ClaudeProjectsDir)
}

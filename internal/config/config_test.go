package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("AGENTCTL_ADDR", "")
	t.Setenv("AGENTCTL_MONGO_URI", "")
	t.Setenv("AGENTCTL_DB", "")
	c := Load()
	require.Equal(t, "127.0.0.1:8765", c.Addr)
	require.Equal(t, "mongodb://localhost:27017", c.MongoURI)
	require.Equal(t, "agentctl", c.DB)
}

func TestLoadFromEnv(t *testing.T) {
	t.Setenv("AGENTCTL_ADDR", "127.0.0.1:9000")
	t.Setenv("AGENTCTL_MONGO_URI", "mongodb://db:27017")
	t.Setenv("AGENTCTL_DB", "test")
	c := Load()
	require.Equal(t, "127.0.0.1:9000", c.Addr)
	require.Equal(t, "mongodb://db:27017", c.MongoURI)
	require.Equal(t, "test", c.DB)
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

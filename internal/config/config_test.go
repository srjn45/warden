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

func TestClaudeProjectsDirDefault(t *testing.T) {
	t.Setenv("CLAUDE_PROJECTS_DIR", "")
	c := Load()
	require.True(t, strings.HasSuffix(c.ClaudeProjectsDir, ".claude/projects"), "got %q", c.ClaudeProjectsDir)
}

func TestClaudeProjectsDirFromEnv(t *testing.T) {
	t.Setenv("CLAUDE_PROJECTS_DIR", "/tmp/projects")
	require.Equal(t, "/tmp/projects", Load().ClaudeProjectsDir)
}

func TestNotifyDisabledByDefault(t *testing.T) {
	t.Setenv("AGENTCTL_NOTIFY", "")
	require.False(t, Load().NotifyEnabled, "notifications off by default")
}

func TestNotifyEnabledFromEnv(t *testing.T) {
	for _, v := range []string{"1", "on", "true", "ON"} {
		t.Setenv("AGENTCTL_NOTIFY", v)
		require.True(t, Load().NotifyEnabled, "AGENTCTL_NOTIFY=%q enables", v)
	}
}

func TestApprovalsEnabledByDefault(t *testing.T) {
	t.Setenv("AGENTCTL_APPROVALS", "")
	require.True(t, Load().ApprovalsEnabled, "approvals on by default")

	t.Setenv("AGENTCTL_APPROVALS", "on")
	require.True(t, Load().ApprovalsEnabled)
}

func TestApprovalsDisableFromEnv(t *testing.T) {
	for _, v := range []string{"0", "off", "false", "OFF"} {
		t.Setenv("AGENTCTL_APPROVALS", v)
		require.False(t, Load().ApprovalsEnabled, "AGENTCTL_APPROVALS=%q should disable approvals", v)
	}
	t.Setenv("AGENTCTL_APPROVALS", "1")
	require.True(t, Load().ApprovalsEnabled, "AGENTCTL_APPROVALS=1 should enable approvals")
}

func TestSpawnGateDefaults(t *testing.T) {
	t.Setenv("AGENTCTL_SPAWN_GATE", "")
	t.Setenv("AGENTCTL_SPAWN_GATE_MAX_AGENTS", "")
	c := Load()
	if !c.SpawnGateEnabled {
		t.Error("spawn gate must default ON")
	}
	if c.SpawnGateMaxAgents != 5 {
		t.Errorf("max agents default = %d, want 5", c.SpawnGateMaxAgents)
	}
}

func TestSpawnGateDisable(t *testing.T) {
	for _, v := range []string{"0", "off", "false", "OFF"} {
		t.Setenv("AGENTCTL_SPAWN_GATE", v)
		if Load().SpawnGateEnabled {
			t.Errorf("AGENTCTL_SPAWN_GATE=%q should disable the gate", v)
		}
	}
	t.Setenv("AGENTCTL_SPAWN_GATE", "1")
	if !Load().SpawnGateEnabled {
		t.Error("AGENTCTL_SPAWN_GATE=1 should enable the gate")
	}
}

func TestSpawnGateMaxAgentsOverride(t *testing.T) {
	t.Setenv("AGENTCTL_SPAWN_GATE_MAX_AGENTS", "8")
	if Load().SpawnGateMaxAgents != 8 {
		t.Error("max agents override not applied")
	}
	t.Setenv("AGENTCTL_SPAWN_GATE_MAX_AGENTS", "garbage")
	if Load().SpawnGateMaxAgents != 5 {
		t.Error("unparseable max agents should fall back to 5")
	}
}

package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("WARDEN_ADDR", "")
	t.Setenv("AGENTCTL_ADDR", "")
	c := Load()
	require.Equal(t, "127.0.0.1:8765", c.Addr)
}

func TestLoadFromEnv(t *testing.T) {
	t.Setenv("WARDEN_ADDR", "127.0.0.1:9000")
	require.Equal(t, "127.0.0.1:9000", Load().Addr)
}

// TestConfigFallsBackToLegacyEnv confirms the legacy AGENTCTL_* env vars still
// resolve when the canonical WARDEN_* is unset.
func TestConfigFallsBackToLegacyEnv(t *testing.T) {
	t.Setenv("WARDEN_ADDR", "")
	t.Setenv("AGENTCTL_ADDR", "127.0.0.1:9100")
	require.Equal(t, "127.0.0.1:9100", Load().Addr)

	t.Setenv("WARDEN_DATA_DIR", "")
	t.Setenv("AGENTCTL_DATA_DIR", "/tmp/legacy-data")
	require.Equal(t, "/tmp/legacy-data", Load().DataDir)

	t.Setenv("WARDEN_APPROVALS", "")
	t.Setenv("AGENTCTL_APPROVALS", "off")
	require.False(t, Load().ApprovalsEnabled, "legacy AGENTCTL_APPROVALS=off should disable")
}

// TestConfigPrefersWardenOverLegacy confirms WARDEN_* wins when both are set.
func TestConfigPrefersWardenOverLegacy(t *testing.T) {
	t.Setenv("AGENTCTL_ADDR", "127.0.0.1:1111")
	t.Setenv("WARDEN_ADDR", "127.0.0.1:2222")
	require.Equal(t, "127.0.0.1:2222", Load().Addr)
}

func TestDataDirDefault(t *testing.T) {
	t.Setenv("WARDEN_DATA_DIR", "")
	t.Setenv("AGENTCTL_DATA_DIR", "")
	c := Load()
	require.True(t, strings.HasSuffix(c.DataDir, ".warden"), "got %q", c.DataDir)
}

func TestDataDirFromEnv(t *testing.T) {
	t.Setenv("WARDEN_DATA_DIR", "/tmp/warden-data")
	require.Equal(t, "/tmp/warden-data", Load().DataDir)
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
	t.Setenv("WARDEN_NOTIFY", "")
	t.Setenv("AGENTCTL_NOTIFY", "")
	require.False(t, Load().NotifyEnabled, "notifications off by default")
}

func TestNotifyEnabledFromEnv(t *testing.T) {
	t.Setenv("AGENTCTL_NOTIFY", "")
	for _, v := range []string{"1", "on", "true", "ON"} {
		t.Setenv("WARDEN_NOTIFY", v)
		require.True(t, Load().NotifyEnabled, "WARDEN_NOTIFY=%q enables", v)
	}
}

func TestApprovalsEnabledByDefault(t *testing.T) {
	t.Setenv("AGENTCTL_APPROVALS", "")
	t.Setenv("WARDEN_APPROVALS", "")
	require.True(t, Load().ApprovalsEnabled, "approvals on by default")

	t.Setenv("WARDEN_APPROVALS", "on")
	require.True(t, Load().ApprovalsEnabled)
}

func TestApprovalsDisableFromEnv(t *testing.T) {
	t.Setenv("AGENTCTL_APPROVALS", "")
	for _, v := range []string{"0", "off", "false", "OFF"} {
		t.Setenv("WARDEN_APPROVALS", v)
		require.False(t, Load().ApprovalsEnabled, "WARDEN_APPROVALS=%q should disable approvals", v)
	}
	t.Setenv("WARDEN_APPROVALS", "1")
	require.True(t, Load().ApprovalsEnabled, "WARDEN_APPROVALS=1 should enable approvals")
}

func TestSpawnGateDefaults(t *testing.T) {
	t.Setenv("WARDEN_SPAWN_GATE", "")
	t.Setenv("AGENTCTL_SPAWN_GATE", "")
	t.Setenv("WARDEN_SPAWN_GATE_MAX_AGENTS", "")
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
	t.Setenv("AGENTCTL_SPAWN_GATE", "")
	for _, v := range []string{"0", "off", "false", "OFF"} {
		t.Setenv("WARDEN_SPAWN_GATE", v)
		if Load().SpawnGateEnabled {
			t.Errorf("WARDEN_SPAWN_GATE=%q should disable the gate", v)
		}
	}
	t.Setenv("WARDEN_SPAWN_GATE", "1")
	if !Load().SpawnGateEnabled {
		t.Error("WARDEN_SPAWN_GATE=1 should enable the gate")
	}
}

func TestSpawnGateMaxAgentsOverride(t *testing.T) {
	t.Setenv("AGENTCTL_SPAWN_GATE_MAX_AGENTS", "")
	t.Setenv("WARDEN_SPAWN_GATE_MAX_AGENTS", "8")
	if Load().SpawnGateMaxAgents != 8 {
		t.Error("max agents override not applied")
	}
	t.Setenv("WARDEN_SPAWN_GATE_MAX_AGENTS", "garbage")
	if Load().SpawnGateMaxAgents != 5 {
		t.Error("unparseable max agents should fall back to 5")
	}
}

func TestMetricsEnabledDefaultsOn(t *testing.T) {
	t.Setenv("WARDEN_METRICS", "")
	t.Setenv("AGENTCTL_METRICS", "")
	if !Load().MetricsEnabled {
		t.Fatal("metrics should default ON")
	}
}

func TestMetricsEnabledOff(t *testing.T) {
	t.Setenv("WARDEN_METRICS", "off")
	if Load().MetricsEnabled {
		t.Fatal("WARDEN_METRICS=off should disable")
	}
}

func TestTokenGuardDefaults(t *testing.T) {
	for _, k := range []string{"WARDEN_TOKEN_GUARD", "WARDEN_TOKEN_WARN_ALERT", "WARDEN_TOKEN_AUTO_COMPACT", "WARDEN_TOKEN_WARN", "WARDEN_TOKEN_CRITICAL", "AGENTCTL_TOKEN_GUARD"} {
		t.Setenv(k, "")
	}
	c := Load()
	if !c.TokenGuard || !c.TokenWarnAlert || !c.TokenAutoCompact {
		t.Fatalf("guard=%v warnAlert=%v autoCompact=%v, want all true", c.TokenGuard, c.TokenWarnAlert, c.TokenAutoCompact)
	}
	if c.TokenWarn != 200000 || c.TokenCritical != 400000 {
		t.Fatalf("warn=%d crit=%d, want 200000/400000", c.TokenWarn, c.TokenCritical)
	}
}

func TestTokenGuardOverrides(t *testing.T) {
	t.Setenv("WARDEN_TOKEN_AUTO_COMPACT", "off")
	t.Setenv("WARDEN_TOKEN_WARN", "100000")
	t.Setenv("WARDEN_TOKEN_CRITICAL", "150000")
	c := Load()
	if c.TokenAutoCompact {
		t.Fatal("auto-compact should be off")
	}
	if c.TokenWarn != 100000 || c.TokenCritical != 150000 {
		t.Fatalf("warn=%d crit=%d", c.TokenWarn, c.TokenCritical)
	}
}

func TestTokenThresholdsFallBackWhenInverted(t *testing.T) {
	t.Setenv("WARDEN_TOKEN_WARN", "500000")
	t.Setenv("WARDEN_TOKEN_CRITICAL", "400000") // crit <= warn → defaults
	c := Load()
	if c.TokenWarn != 200000 || c.TokenCritical != 400000 {
		t.Fatalf("inverted config not reset: warn=%d crit=%d", c.TokenWarn, c.TokenCritical)
	}
}

func TestIsLoopbackHost(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8765":   true,
		"localhost:8765":   true,
		"[::1]:8765":       true,
		"127.0.0.1":        true,  // bare host, no port
		":8765":            false, // empty host = all interfaces
		"0.0.0.0:8765":     false,
		"192.168.1.5:8765": false,
		"example.com:8765": false, // unresolved hostname → fail safe
	}
	for addr, want := range cases {
		if got := IsLoopbackHost(addr); got != want {
			t.Fatalf("IsLoopbackHost(%q)=%v want %v", addr, got, want)
		}
	}
}

func TestAllowNonLoopbackFlag(t *testing.T) {
	t.Setenv("WARDEN_ALLOW_NONLOOPBACK", "")
	t.Setenv("AGENTCTL_ALLOW_NONLOOPBACK", "")
	if Load().AllowNonLoopback {
		t.Fatal("should default OFF")
	}
	t.Setenv("WARDEN_ALLOW_NONLOOPBACK", "1")
	if !Load().AllowNonLoopback {
		t.Fatal("WARDEN_ALLOW_NONLOOPBACK=1 should enable")
	}
}

func TestAutoApproveEnabled(t *testing.T) {
	tests := []struct {
		name string
		val  string
		want bool
	}{
		{"empty", "", false},
		{"0", "0", false},
		{"off", "off", false},
		{"OFF", "OFF", false},
		{"false", "false", false},
		{"FALSE", "FALSE", false},
		{"1", "1", true},
		{"on", "on", true},
		{"ON", "ON", true},
		{"true", "true", true},
		{"TRUE", "TRUE", true},
		{"junk", "junk", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("WARDEN_AUTO_APPROVE", tt.val)
			cfg := Load()
			if cfg.AutoApproveEnabled != tt.want {
				t.Errorf("AutoApproveEnabled = %v, want %v", cfg.AutoApproveEnabled, tt.want)
			}
		})
	}
}

func TestAutoApproveEnabledLegacy(t *testing.T) {
	t.Setenv("AGENTCTL_AUTO_APPROVE", "1")
	cfg := Load()
	if !cfg.AutoApproveEnabled {
		t.Error("legacy AGENTCTL_AUTO_APPROVE=1 should enable auto-approve")
	}
}

func TestAutoApproveEnabledPreferNewVar(t *testing.T) {
	t.Setenv("WARDEN_AUTO_APPROVE", "0")
	t.Setenv("AGENTCTL_AUTO_APPROVE", "1")
	cfg := Load()
	if cfg.AutoApproveEnabled {
		t.Error("WARDEN_AUTO_APPROVE should take precedence over AGENTCTL_AUTO_APPROVE")
	}
}

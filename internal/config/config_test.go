package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// tmpConfig writes body to a config.yaml in a fresh temp dir and returns its path.
func tmpConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	return path
}

func TestLoadAbsentFileReturnsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	c := Load(path)
	d := defaults()
	require.Equal(t, d.Addr, c.Addr)
	require.True(t, c.ApprovalsEnabled)
	require.False(t, c.AutoApprove.Enabled)
	require.False(t, c.AutoApprove.AllowSticky)
	require.Equal(t, "auto", c.DefaultPermissionMode)
	require.Equal(t, 5, c.SpawnGateMaxAgents)
	require.Equal(t, "claude-sonnet-4-6", c.ModelDefault)
	require.True(t, c.PipelineHint)
}

func TestLoadReadsFileValues(t *testing.T) {
	path := tmpConfig(t, `
addr: 127.0.0.1:9999
approvals: false
auto_approve: true
spawn_gate_max_agents: 8
model_default: opus
auto_restart_max: 7
auto_restart_reset: 10m
`)
	c := Load(path)
	require.Equal(t, "127.0.0.1:9999", c.Addr)
	require.False(t, c.ApprovalsEnabled)
	require.True(t, c.AutoApprove.Enabled)
	require.Equal(t, 8, c.SpawnGateMaxAgents)
	require.Equal(t, "opus", c.ModelDefault)
	require.Equal(t, 7, c.AutoRestartMax)
	require.Equal(t, "10m", c.AutoRestartReset)
	// A key absent from the file keeps its default.
	require.True(t, c.MetricsEnabled)
}

func TestLoad_RateLimitResumePrompt_Default(t *testing.T) {
	path := tmpConfig(t, "") // empty file → all defaults
	c := Load(path)
	require.Equal(t, "", c.RateLimitResumePrompt, "default must be empty (keypress-only)")
}

func TestLoad_RateLimitResumePrompt_Set(t *testing.T) {
	path := tmpConfig(t, "rate_limit_resume_prompt: continue\n")
	c := Load(path)
	require.Equal(t, "continue", c.RateLimitResumePrompt)
}

func TestLoad_WorktreeRetention_Defaults(t *testing.T) {
	c := Load(tmpConfig(t, "")) // empty file → all defaults
	require.True(t, c.WorktreeKeepDone, "worktree_keep_done defaults to true (today's keep behavior)")
	require.False(t, c.WorktreeAutoPrune, "worktree_auto_prune defaults to false (opt-in)")
}

func TestLoad_WorktreeRetention_Set(t *testing.T) {
	c := Load(tmpConfig(t, "worktree_keep_done: false\nworktree_auto_prune: true\n"))
	require.False(t, c.WorktreeKeepDone)
	require.True(t, c.WorktreeAutoPrune)
}

func TestLoadInvalidPermissionModeFallsBack(t *testing.T) {
	path := tmpConfig(t, "default_permission_mode: nonsense\n")
	require.Equal(t, "auto", Load(path).DefaultPermissionMode)
}

func TestLoadValidPermissionModes(t *testing.T) {
	for _, mode := range []string{"acceptEdits", "auto", "bypassPermissions", "default", "dontAsk", "plan"} {
		path := tmpConfig(t, "default_permission_mode: "+mode+"\n")
		require.Equal(t, mode, Load(path).DefaultPermissionMode)
	}
}

func TestLoadTokenThresholdsResetWhenInverted(t *testing.T) {
	path := tmpConfig(t, "token_warn: 500000\ntoken_critical: 400000\n")
	c := Load(path)
	require.Equal(t, 200000, c.TokenWarn)
	require.Equal(t, 400000, c.TokenCritical)
}

func TestLoadBadDurationFallsBack(t *testing.T) {
	path := tmpConfig(t, "auto_restart_reset: not-a-duration\nrate_limit_buffer: 0s\n")
	c := Load(path)
	require.Equal(t, "5m", c.AutoRestartReset)
	require.Equal(t, "1m", c.RateLimitBuffer)
}

func TestLoadEmptyRequiredStringsFallBack(t *testing.T) {
	path := tmpConfig(t, "addr:\nmodel_default:\n")
	c := Load(path)
	require.Equal(t, defaults().Addr, c.Addr)
	require.Equal(t, "claude-sonnet-4-6", c.ModelDefault)
}

func TestLoadGarbledFileFallsBackToDefaults(t *testing.T) {
	path := tmpConfig(t, "this: is: not: valid: yaml: [\n")
	require.Equal(t, defaults().Addr, Load(path).Addr)
}

func TestReconcileCreatesFullFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, Reconcile(path))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	text := string(data)

	// Header comment present.
	require.Contains(t, text, "warden configuration")
	// Every schema key is written, each with its hint comment.
	for _, s := range schema {
		require.Contains(t, text, s.Key+":", "missing key %q", s.Key)
	}
	require.Contains(t, text, "Default permission mode for new agents")

	// The generated file round-trips into a valid Config.
	c := Load(path)
	require.Equal(t, defaults().Addr, c.Addr)
	require.Equal(t, "auto", c.DefaultPermissionMode)
}

func TestReconcileAddsOnlyMissingKeys(t *testing.T) {
	// A minimal hand-written file with a custom value and an unknown key.
	path := tmpConfig(t, `# my own note
addr: 127.0.0.1:7777
some_unknown_key: keep-me
`)
	require.NoError(t, Reconcile(path))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	text := string(data)

	// Existing value, comment, and unknown key all preserved.
	require.Contains(t, text, "127.0.0.1:7777")
	require.Contains(t, text, "my own note")
	require.Contains(t, text, "some_unknown_key: keep-me")
	// A previously-missing key was appended with its hint.
	require.Contains(t, text, "default_permission_mode:")
	require.Contains(t, text, "Default permission mode for new agents")

	// The custom addr survives a reload (not clobbered by the default).
	require.Equal(t, "127.0.0.1:7777", Load(path).Addr)

	// Every schema key now exists exactly once.
	var m map[string]any
	require.NoError(t, yaml.Unmarshal(data, &m))
	for _, s := range schema {
		_, ok := m[s.Key]
		require.True(t, ok, "key %q not added", s.Key)
	}
	require.Equal(t, "keep-me", m["some_unknown_key"])
}

func TestLoadLegacyFlatAutoApprove(t *testing.T) {
	// A flat `auto_approve: true` must still load (via the Policy UnmarshalYAML
	// shim) without a parse-error fallback, with other keys intact.
	path := tmpConfig(t, `addr: 127.0.0.1:9999
auto_approve: true
metrics: false
`)
	c := Load(path)
	require.True(t, c.AutoApprove.Enabled)
	require.False(t, c.AutoApprove.AllowSticky)
	require.Equal(t, "127.0.0.1:9999", c.Addr) // not the parse-error default
	require.False(t, c.MetricsEnabled)
}

func TestLoadNestedAutoApproveRoundTrips(t *testing.T) {
	path := tmpConfig(t, `auto_approve:
  enabled: true
  allow_sticky: true
  rules:
    allow:
      - tool: Edit
        paths:
          - src/**
    deny:
      - pattern: git push
`)
	c := Load(path)
	require.True(t, c.AutoApprove.Enabled)
	require.True(t, c.AutoApprove.AllowSticky)
	require.Len(t, c.AutoApprove.Rules.Allow, 1)
	require.Equal(t, "Edit", c.AutoApprove.Rules.Allow[0].Tool)
	require.Equal(t, []string{"src/**"}, c.AutoApprove.Rules.Allow[0].Paths)
	require.Len(t, c.AutoApprove.Rules.Deny, 1)
	require.Equal(t, "git push", c.AutoApprove.Rules.Deny[0].Pattern)
}

func TestReconcileMigratesFlatAutoApprove(t *testing.T) {
	// A Stage-A file: flat auto_approve + flat auto_approve_allow_sticky, with a
	// surrounding key and a comment.
	path := tmpConfig(t, `# my own note
addr: 127.0.0.1:7777
auto_approve: true
auto_approve_allow_sticky: true
metrics: false
`)
	require.NoError(t, Reconcile(path))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	text := string(data)

	// The flat sticky key is gone; auto_approve is now a nested block.
	require.NotContains(t, text, "auto_approve_allow_sticky")
	require.Contains(t, text, "my own note")
	require.Contains(t, text, "127.0.0.1:7777")

	// Reloads with the folded-in values and an empty (non-nil) rules block.
	c := Load(path)
	require.True(t, c.AutoApprove.Enabled)
	require.True(t, c.AutoApprove.AllowSticky)
	require.Empty(t, c.AutoApprove.Rules.Allow)
	require.Empty(t, c.AutoApprove.Rules.Deny)
	require.Equal(t, "127.0.0.1:7777", c.Addr)
	require.False(t, c.MetricsEnabled)

	// A second Reconcile is a no-op for the (now nested) auto_approve key.
	before := text
	require.NoError(t, Reconcile(path))
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, string(after))
}

func TestReconcileGeneratesNestedAutoApprove(t *testing.T) {
	// A brand-new file gets the full nested block from defaults.
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, Reconcile(path))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	text := string(data)
	require.Contains(t, text, "auto_approve:")
	require.Contains(t, text, "enabled:")
	require.Contains(t, text, "allow_sticky:")
	require.Contains(t, text, "rules:")
	require.NotContains(t, text, "auto_approve_allow_sticky")

	c := Load(path)
	require.False(t, c.AutoApprove.Enabled)
	require.NotNil(t, c.AutoApprove.Rules.Allow)
	require.NotNil(t, c.AutoApprove.Rules.Deny)
}

func TestReconcileIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, Reconcile(path))
	first, err := os.ReadFile(path)
	require.NoError(t, err)

	require.NoError(t, Reconcile(path))
	second, err := os.ReadFile(path)
	require.NoError(t, err)

	require.Equal(t, string(first), string(second), "second reconcile must be a no-op")
}

func TestReconcileRegeneratesEmptyFile(t *testing.T) {
	path := tmpConfig(t, "")
	require.NoError(t, Reconcile(path))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(data), "addr:")
}

// TestSchemaMatchesStructTags is the drift guard: the set of yaml tags on
// Config must equal the set of keys in the schema table, both directions.
func TestSchemaMatchesStructTags(t *testing.T) {
	tags := map[string]bool{}
	tp := reflect.TypeOf(Config{})
	for i := 0; i < tp.NumField(); i++ {
		tag := strings.Split(tp.Field(i).Tag.Get("yaml"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		tags[tag] = true
	}

	keys := map[string]bool{}
	for _, s := range schema {
		require.False(t, keys[s.Key], "duplicate schema key %q", s.Key)
		keys[s.Key] = true
	}

	for k := range tags {
		require.True(t, keys[k], "yaml tag %q has no schema entry", k)
	}
	for k := range keys {
		require.True(t, tags[k], "schema key %q has no struct field", k)
	}
}

// TestDefaultsCoverEverySchemaKey ensures file generation can emit a value for
// every documented key.
func TestDefaultsCoverEverySchemaKey(t *testing.T) {
	vals, err := defaultValueNodes()
	require.NoError(t, err)
	for _, s := range schema {
		_, ok := vals[s.Key]
		require.True(t, ok, "defaults() has no value for %q", s.Key)
	}
}

func TestDurationAccessors(t *testing.T) {
	path := tmpConfig(t, "auto_restart_reset: 90s\nrate_limit_retry_interval: 45m\nrate_limit_buffer: 2m\n")
	c := Load(path)
	require.Equal(t, "90s", c.AutoRestartReset) // valid strings are preserved verbatim
	require.Equal(t, int64(90), int64(c.AutoRestartResetDuration().Seconds()))
	require.Equal(t, 45.0, c.RateLimitRetryIntervalDuration().Minutes())
	require.Equal(t, 2.0, c.RateLimitBufferDuration().Minutes())
}

func TestConfigImplementsProviderAccessors(t *testing.T) {
	c := defaults()
	require.Equal(t, "auto", c.GetDefaultPermissionMode())
	require.Equal(t, "claude-sonnet-4-6", c.GetModelDefault())
	require.True(t, c.GetPipelineHint())
}

func TestDefaultPath(t *testing.T) {
	p := DefaultPath()
	require.True(t, strings.HasSuffix(p, filepath.Join(".warden", "config.yaml")), "got %q", p)
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

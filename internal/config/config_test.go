package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/approval"
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
	require.Equal(t, 5, c.Worktree.SpawnGateMax)
	require.Equal(t, "claude-sonnet-4-6", c.ModelDefault)
	require.True(t, c.PipelineHint)
}

func TestLoadReadsFileValues(t *testing.T) {
	path := tmpConfig(t, `
addr: 127.0.0.1:9999
approvals: false
auto_approve: true
model_default: opus
auto_restart_max: 7
auto_restart_reset: 10m
worktree:
  spawn_gate_max_agents: 8
`)
	c := Load(path)
	require.Equal(t, "127.0.0.1:9999", c.Addr)
	require.False(t, c.ApprovalsEnabled)
	require.True(t, c.AutoApprove.Enabled)
	require.Equal(t, 8, c.Worktree.SpawnGateMax)
	require.Equal(t, "opus", c.ModelDefault)
	require.Equal(t, 7, c.AutoRestartMax)
	require.Equal(t, "10m", c.AutoRestartReset)
	// A key absent from the file keeps its default.
	require.True(t, c.MetricsEnabled)
}

func TestLoad_TrustedProxies(t *testing.T) {
	// Absent ⇒ nil (feature off).
	require.Nil(t, Load(tmpConfig(t, "addr: 127.0.0.1:8765\n")).TrustedProxies)

	c := Load(tmpConfig(t, `
trusted_proxies:
  - 127.0.0.1
  - 10.0.0.0/8
`))
	require.Equal(t, []string{"127.0.0.1", "10.0.0.0/8"}, c.TrustedProxies)
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

// ---------------------------------------------------------------------------
// Namespaced: worktree group
// ---------------------------------------------------------------------------

func TestLoad_WorktreeRetention_Defaults(t *testing.T) {
	c := Load(tmpConfig(t, "")) // empty file → all defaults
	require.True(t, c.Worktree.KeepDone, "worktree.keep_done defaults to true")
	require.False(t, c.Worktree.AutoPrune, "worktree.auto_prune defaults to false (opt-in)")
}

func TestLoad_WorktreeRetention_Namespaced(t *testing.T) {
	c := Load(tmpConfig(t, "worktree:\n  keep_done: false\n  auto_prune: true\n"))
	require.False(t, c.Worktree.KeepDone)
	require.True(t, c.Worktree.AutoPrune)
}

func TestLoad_WorktreeRetention_FlatDeprecated(t *testing.T) {
	// Old flat keys must still load via in-memory migration.
	c := Load(tmpConfig(t, "worktree_keep_done: false\nworktree_auto_prune: true\n"))
	require.False(t, c.Worktree.KeepDone)
	require.True(t, c.Worktree.AutoPrune)
}

func TestLoad_SpawnGate_Namespaced(t *testing.T) {
	c := Load(tmpConfig(t, "worktree:\n  spawn_gate: false\n  spawn_gate_max_agents: 3\n"))
	require.False(t, c.Worktree.SpawnGate)
	require.Equal(t, 3, c.Worktree.SpawnGateMax)
}

func TestLoad_SpawnGate_FlatDeprecated(t *testing.T) {
	c := Load(tmpConfig(t, "spawn_gate: false\nspawn_gate_max_agents: 3\n"))
	require.False(t, c.Worktree.SpawnGate)
	require.Equal(t, 3, c.Worktree.SpawnGateMax)
}

// ---------------------------------------------------------------------------
// Namespaced: notify group
// ---------------------------------------------------------------------------

func TestLoad_Webhook_Defaults(t *testing.T) {
	c := Load(tmpConfig(t, "")) // empty file → all defaults
	require.False(t, c.Notify.WebhookEnabled, "notify.webhook_enabled defaults to false (opt-in)")
	require.Equal(t, "", c.Notify.WebhookURL, "notify.webhook_url defaults to empty")
}

func TestLoad_Webhook_Namespaced(t *testing.T) {
	c := Load(tmpConfig(t, "notify:\n  webhook_enabled: true\n  webhook_url: https://hooks.slack.com/services/T/B/xyz\n"))
	require.True(t, c.Notify.WebhookEnabled)
	require.Equal(t, "https://hooks.slack.com/services/T/B/xyz", c.Notify.WebhookURL)
}

func TestLoad_Webhook_FlatDeprecated(t *testing.T) {
	c := Load(tmpConfig(t, "webhook_enabled: true\nwebhook_url: https://hooks.slack.com/services/T/B/xyz\n"))
	require.True(t, c.Notify.WebhookEnabled)
	require.Equal(t, "https://hooks.slack.com/services/T/B/xyz", c.Notify.WebhookURL)
}

func TestLoad_Notify_FlatDeprecated(t *testing.T) {
	c := Load(tmpConfig(t, "notify: true\n"))
	require.True(t, c.Notify.Enabled)
}

// ---------------------------------------------------------------------------
// Namespaced: tokens group
// ---------------------------------------------------------------------------

func TestLoad_Tokens_Defaults(t *testing.T) {
	c := Load(tmpConfig(t, ""))
	require.True(t, c.Tokens.Guard)
	require.Equal(t, 200000, c.Tokens.Warn)
	require.Equal(t, 400000, c.Tokens.Critical)
	require.True(t, c.Tokens.Savings)
	require.False(t, c.Tokens.SavingsSamples)
	require.False(t, c.Tokens.BudgetGate)
}

func TestLoad_Tokens_Namespaced(t *testing.T) {
	c := Load(tmpConfig(t, "tokens:\n  guard: false\n  warn: 100000\n  critical: 300000\n  savings: false\n"))
	require.False(t, c.Tokens.Guard)
	require.Equal(t, 100000, c.Tokens.Warn)
	require.Equal(t, 300000, c.Tokens.Critical)
	require.False(t, c.Tokens.Savings)
}

func TestLoad_Tokens_FlatDeprecated(t *testing.T) {
	c := Load(tmpConfig(t, "token_guard: false\ntoken_warn: 100000\ntoken_critical: 300000\nsavings: false\n"))
	require.False(t, c.Tokens.Guard)
	require.Equal(t, 100000, c.Tokens.Warn)
	require.Equal(t, 300000, c.Tokens.Critical)
	require.False(t, c.Tokens.Savings)
}

func TestLoad_Budget_Namespaced(t *testing.T) {
	c := Load(tmpConfig(t, "tokens:\n  budget_gate: true\n  budget_daily_usd: 25\n  budget_weekly_usd: 100\n"))
	require.True(t, c.Tokens.BudgetGate)
	require.Equal(t, 25.0, c.Tokens.BudgetDailyUSD)
	require.Equal(t, 100.0, c.Tokens.BudgetWeeklyUSD)
}

func TestLoad_Budget_FlatDeprecated(t *testing.T) {
	c := Load(tmpConfig(t, "budget_gate: true\nbudget_daily_usd: 25\nbudget_weekly_usd: 100\n"))
	require.True(t, c.Tokens.BudgetGate)
	require.Equal(t, 25.0, c.Tokens.BudgetDailyUSD)
	require.Equal(t, 100.0, c.Tokens.BudgetWeeklyUSD)
}

func TestLoadTokenThresholdsResetWhenInverted(t *testing.T) {
	path := tmpConfig(t, "tokens:\n  warn: 500000\n  critical: 400000\n")
	c := Load(path)
	require.Equal(t, 200000, c.Tokens.Warn)
	require.Equal(t, 400000, c.Tokens.Critical)
}

func TestLoadTokenThresholdsResetWhenInverted_FlatDeprecated(t *testing.T) {
	path := tmpConfig(t, "token_warn: 500000\ntoken_critical: 400000\n")
	c := Load(path)
	require.Equal(t, 200000, c.Tokens.Warn)
	require.Equal(t, 400000, c.Tokens.Critical)
}

// ---------------------------------------------------------------------------
// Namespaced: rails group
// ---------------------------------------------------------------------------

func TestLoad_Rails_Defaults(t *testing.T) {
	c := Load(tmpConfig(t, ""))
	require.True(t, c.Rails.GitConventions)
	require.True(t, c.Rails.GitRedirect)
	require.True(t, c.Rails.CheckRedirect)
	require.True(t, c.Rails.RootGuard)
	require.True(t, c.Rails.IsolationGuard)
}

func TestLoad_Rails_Namespaced(t *testing.T) {
	c := Load(tmpConfig(t, "rails:\n  git_redirect: false\n  root_guard: false\n"))
	require.False(t, c.Rails.GitRedirect)
	require.False(t, c.Rails.RootGuard)
	// Other rails defaults still apply.
	require.True(t, c.Rails.GitConventions)
	require.True(t, c.Rails.IsolationGuard)
}

func TestLoad_Rails_FlatDeprecated(t *testing.T) {
	c := Load(tmpConfig(t, "git_redirect: false\nroot_guard: false\nisolation_guard: false\n"))
	require.False(t, c.Rails.GitRedirect)
	require.False(t, c.Rails.RootGuard)
	require.False(t, c.Rails.IsolationGuard)
	require.True(t, c.Rails.GitConventions) // default
}

// ---------------------------------------------------------------------------
// Namespaced: local_llm group
// ---------------------------------------------------------------------------

func TestLoad_LocalLLM_Defaults(t *testing.T) {
	c := Load(tmpConfig(t, ""))
	require.False(t, c.LocalLLM.Enabled)
	require.Equal(t, "http://localhost:11434", c.LocalLLM.URL)
	require.Equal(t, "qwen2.5-coder:7b", c.LocalLLM.Model)
	require.False(t, c.LocalLLM.Repl)
}

func TestLoad_LocalLLM_Namespaced(t *testing.T) {
	c := Load(tmpConfig(t, "local_llm:\n  enabled: true\n  url: http://myserver:11434\n  model: llama3\n  repl: true\n"))
	require.True(t, c.LocalLLM.Enabled)
	require.Equal(t, "http://myserver:11434", c.LocalLLM.URL)
	require.Equal(t, "llama3", c.LocalLLM.Model)
	require.True(t, c.LocalLLM.Repl)
}

func TestLoad_LocalLLM_FlatDeprecated(t *testing.T) {
	c := Load(tmpConfig(t, "local_llm: true\nlocal_llm_url: http://myserver:11434\nlocal_llm_model: llama3\n"))
	require.True(t, c.LocalLLM.Enabled)
	require.Equal(t, "http://myserver:11434", c.LocalLLM.URL)
	require.Equal(t, "llama3", c.LocalLLM.Model)
}

func TestLoad_Repl_FlatDeprecated(t *testing.T) {
	c := Load(tmpConfig(t, "repl: true\n"))
	require.True(t, c.LocalLLM.Repl)
	require.True(t, c.GetRepl())
}

func TestLoad_Orchestrator_LegacyAlias(t *testing.T) {
	// The pre-rename `orchestrator` key migrates to local_llm.repl.
	c := Load(tmpConfig(t, "orchestrator: true\n"))
	require.True(t, c.LocalLLM.Repl)
	require.True(t, c.GetRepl())
}

// ---------------------------------------------------------------------------
// Both flat and namespaced present: namespaced wins
// ---------------------------------------------------------------------------

func TestLoad_NamespacedWinsOverFlat(t *testing.T) {
	// When both forms are present, the namespaced value wins (it's processed after
	// migration folds the flat key into the block, and the pre-existing namespaced
	// sub-key takes precedence over the migrated flat alias).
	c := Load(tmpConfig(t, "tokens:\n  guard: false\ntoken_guard: true\n"))
	// The block was present; migration sees token_guard flat key, but tokens.guard
	// is already set in the block → flat value is NOT folded in.
	require.False(t, c.Tokens.Guard)
}

// ---------------------------------------------------------------------------
// Permission mode
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// Log settings
// ---------------------------------------------------------------------------

func TestLoadLogDefaults(t *testing.T) {
	c := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	require.Equal(t, "info", c.LogLevel)
	require.Equal(t, "text", c.LogFormat)
}

func TestLoadValidLogSettings(t *testing.T) {
	path := tmpConfig(t, "log_level: debug\nlog_format: json\n")
	c := Load(path)
	require.Equal(t, "debug", c.LogLevel)
	require.Equal(t, "json", c.LogFormat)
}

func TestLoadInvalidLogSettingsFallBack(t *testing.T) {
	path := tmpConfig(t, "log_level: chatty\nlog_format: xml\n")
	c := Load(path)
	require.Equal(t, "info", c.LogLevel)
	require.Equal(t, "text", c.LogFormat)
}

func TestLoadLogLevelNormalizesCase(t *testing.T) {
	path := tmpConfig(t, "log_level: DEBUG\nlog_format: JSON\n")
	c := Load(path)
	require.Equal(t, "debug", c.LogLevel)
	require.Equal(t, "json", c.LogFormat)
}

// ---------------------------------------------------------------------------
// Duration / string fallbacks
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// Reconcile: full generation and idempotence
// ---------------------------------------------------------------------------

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
	// Namespaced blocks are present with sub-keys.
	require.Contains(t, text, "rails:")
	require.Contains(t, text, "tokens:")
	require.Contains(t, text, "notify:")
	require.Contains(t, text, "worktree:")
	require.Contains(t, text, "local_llm:")

	// The generated file round-trips into a valid Config.
	c := Load(path)
	require.Equal(t, defaults().Addr, c.Addr)
	require.Equal(t, "auto", c.DefaultPermissionMode)
	require.True(t, c.Rails.GitConventions)
	require.True(t, c.Tokens.Guard)
	require.False(t, c.Notify.Enabled)
	require.True(t, c.Worktree.SpawnGate)
	require.False(t, c.LocalLLM.Enabled)
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

// ---------------------------------------------------------------------------
// Reconcile: flat→namespaced migration (written to disk)
// ---------------------------------------------------------------------------

func TestReconcileMigratesFlatTokenKeys(t *testing.T) {
	path := tmpConfig(t, `# my note
addr: 127.0.0.1:7777
token_guard: false
token_warn: 100000
token_critical: 300000
savings: false
budget_gate: true
budget_daily_usd: 25
`)
	require.NoError(t, Reconcile(path))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	text := string(data)

	// Flat ROOT-LEVEL keys removed (appear as "\nkey:" at column 0).
	// Sub-keys inside the tokens: block are indented and still present.
	require.NotContains(t, text, "\ntoken_guard:")
	require.NotContains(t, text, "\ntoken_warn:")
	require.NotContains(t, text, "\ntoken_critical:")
	require.NotContains(t, text, "\nbudget_gate:")
	require.Contains(t, text, "tokens:")

	// Values round-trip.
	c := Load(path)
	require.False(t, c.Tokens.Guard)
	require.Equal(t, 100000, c.Tokens.Warn)
	require.Equal(t, 300000, c.Tokens.Critical)
	require.False(t, c.Tokens.Savings)
	require.True(t, c.Tokens.BudgetGate)
	require.Equal(t, 25.0, c.Tokens.BudgetDailyUSD)

	// Surrounding content preserved.
	require.Contains(t, text, "my note")
	require.Contains(t, text, "127.0.0.1:7777")

	// Second reconcile is a no-op.
	before := text
	require.NoError(t, Reconcile(path))
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, string(after))
}

func TestReconcileMigratesFlatNotifyKeys(t *testing.T) {
	path := tmpConfig(t, "notify: true\nwebhook_enabled: true\nwebhook_url: https://hooks.slack.com/T/B/xyz\n")
	require.NoError(t, Reconcile(path))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	text := string(data)

	// Flat scalar `notify: true` is gone; only the block `notify:` remains.
	require.NotContains(t, text, "notify: true")       // scalar flat key gone
	require.NotContains(t, text, "\nwebhook_enabled:") // root-level flat key gone (sub-key is indented)
	require.Contains(t, text, "notify:")               // block present

	c := Load(path)
	require.True(t, c.Notify.Enabled)
	require.True(t, c.Notify.WebhookEnabled)
	require.Equal(t, "https://hooks.slack.com/T/B/xyz", c.Notify.WebhookURL)
}

func TestReconcileMigratesFlatRailsKeys(t *testing.T) {
	path := tmpConfig(t, "git_redirect: false\nroot_guard: false\nisolation_guard: false\n")
	require.NoError(t, Reconcile(path))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	text := string(data)

	// Root-level flat keys gone; sub-keys are inside the rails: block (indented).
	require.NotContains(t, text, "\ngit_redirect:")
	require.NotContains(t, text, "\nroot_guard:")
	require.NotContains(t, text, "\nisolation_guard:")
	require.Contains(t, text, "rails:")

	c := Load(path)
	require.False(t, c.Rails.GitRedirect)
	require.False(t, c.Rails.RootGuard)
	require.False(t, c.Rails.IsolationGuard)
}

func TestReconcileMigratesFlatWorktreeKeys(t *testing.T) {
	path := tmpConfig(t, "spawn_gate: false\nspawn_gate_max_agents: 3\nworktree_keep_done: false\nworktree_auto_prune: true\n")
	require.NoError(t, Reconcile(path))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	text := string(data)

	// Root-level flat keys gone; sub-keys are indented inside the worktree: block.
	require.NotContains(t, text, "\nspawn_gate:")
	require.NotContains(t, text, "\nworktree_keep_done:")
	require.NotContains(t, text, "\nworktree_auto_prune:")
	require.Contains(t, text, "worktree:")

	c := Load(path)
	require.False(t, c.Worktree.SpawnGate)
	require.Equal(t, 3, c.Worktree.SpawnGateMax)
	require.False(t, c.Worktree.KeepDone)
	require.True(t, c.Worktree.AutoPrune)
}

func TestReconcileMigratesFlatLocalLLMKeys(t *testing.T) {
	path := tmpConfig(t, "local_llm: true\nlocal_llm_url: http://myserver:11434\nlocal_llm_model: llama3\nrepl: true\n")
	require.NoError(t, Reconcile(path))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	text := string(data)

	require.NotContains(t, text, "\nrepl: true")
	require.Contains(t, text, "local_llm:")

	c := Load(path)
	require.True(t, c.LocalLLM.Enabled)
	require.Equal(t, "http://myserver:11434", c.LocalLLM.URL)
	require.Equal(t, "llama3", c.LocalLLM.Model)
	require.True(t, c.LocalLLM.Repl)
}

func TestReconcileMigratesOrchestratorLegacyKey(t *testing.T) {
	path := tmpConfig(t, "orchestrator: true\n")
	require.NoError(t, Reconcile(path))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	text := string(data)

	require.NotContains(t, text, "orchestrator:")
	require.Contains(t, text, "local_llm:")

	c := Load(path)
	require.True(t, c.LocalLLM.Repl)
	require.True(t, c.GetRepl())
}

// ---------------------------------------------------------------------------
// auto_approve migration
// ---------------------------------------------------------------------------

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

func TestWriteAutoApproveRoundTrips(t *testing.T) {
	// Start from a file with an unrelated custom key + comment we must preserve.
	path := tmpConfig(t, `# my own note
addr: 127.0.0.1:7777
metrics: false
`)
	pol := approval.Policy{
		Enabled:     true,
		AllowSticky: true,
		Rules: approval.Rules{
			Allow: []approval.Rule{{Tool: "Read"}, {Regex: `^Bash\(git (status|diff)\)$`}},
			Deny:  []approval.Rule{{Tool: "Bash", Pattern: "rm"}},
		},
		Agents: map[string]approval.Policy{
			"reviewer": {Enabled: true, Rules: approval.Rules{Allow: []approval.Rule{{Tool: "Grep"}}}},
		},
	}
	require.NoError(t, WriteAutoApprove(path, pol))

	// The unrelated key + comment survive.
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(data), "my own note")
	require.Contains(t, string(data), "127.0.0.1:7777")

	// And the policy reloads identically.
	c := Load(path)
	require.True(t, c.AutoApprove.Enabled)
	require.True(t, c.AutoApprove.AllowSticky)
	require.Len(t, c.AutoApprove.Rules.Allow, 2)
	require.Equal(t, "Read", c.AutoApprove.Rules.Allow[0].Tool)
	require.Equal(t, `^Bash\(git (status|diff)\)$`, c.AutoApprove.Rules.Allow[1].Regex)
	require.Len(t, c.AutoApprove.Rules.Deny, 1)
	require.Equal(t, "rm", c.AutoApprove.Rules.Deny[0].Pattern)
	ov, ok := c.AutoApprove.Agents["reviewer"]
	require.True(t, ok)
	require.True(t, ov.Enabled)
	require.Equal(t, "Grep", ov.Rules.Allow[0].Tool)
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

// ---------------------------------------------------------------------------
// Drift guard and defaults coverage
// ---------------------------------------------------------------------------

// TestSchemaMatchesStructTags is the drift guard: the set of yaml tags on
// Config must equal the set of keys in the schema table, both directions.
func TestSchemaMatchesStructTags(t *testing.T) {
	// Sub-struct types (RailsConfig, TokensConfig, etc.) are embedded via fields
	// on Config (rails, tokens, notify, worktree, local_llm). Their internal
	// sub-keys are NOT top-level Config yaml tags — only the block key is.
	// The drift guard only needs to check top-level Config yaml tags vs schema.
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
	require.True(t, c.GetIsolationGuard())
	require.True(t, c.GetGitConventions())
	require.True(t, c.GetRootGuard())
	require.True(t, c.GetSavings())
	require.False(t, c.GetLocalLLM())
	require.False(t, c.GetRepl())
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

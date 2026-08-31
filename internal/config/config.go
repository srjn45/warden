// Package config holds warden's user-facing configuration. Configuration lives
// in a single YAML file (default ~/.warden/config.yaml). Load reads it,
// Reconcile creates/migrates it, and DefaultPath resolves its location. The
// typed Config carries yaml tags for consumers; the parallel schema table (key
// + hint) drives file generation and migration. A drift-guard test asserts the
// two never diverge.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/logging"
	"github.com/srjn45/warden/internal/plugin"
	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// Namespaced sub-structs
// ---------------------------------------------------------------------------

// RailsConfig groups the guard/boundary-hook settings that constrain what
// agents can do: git conventions, redirect hooks, and isolation guards.
type RailsConfig struct {
	GitConventions bool `yaml:"git_conventions"`
	GitRedirect    bool `yaml:"git_redirect"`
	CheckRedirect  bool `yaml:"check_redirect"`
	RootGuard      bool `yaml:"root_guard"`
	IsolationGuard bool `yaml:"isolation_guard"`
}

// TokensConfig groups token-guard, budget-gate, and cost/savings settings.
type TokensConfig struct {
	Guard               bool    `yaml:"guard"`
	WarnAlert           bool    `yaml:"warn_alert"`
	AutoCompact         bool    `yaml:"auto_compact"`
	ForceCompact        bool    `yaml:"force_compact"`
	CompactResumePrompt string  `yaml:"compact_resume_prompt"`
	Warn                int     `yaml:"warn"`
	Critical            int     `yaml:"critical"`
	BudgetGate          bool    `yaml:"budget_gate"`
	BudgetDailyUSD      float64 `yaml:"budget_daily_usd"`
	BudgetWeeklyUSD     float64 `yaml:"budget_weekly_usd"`
	Savings             bool    `yaml:"savings"`
	SavingsSamples      bool    `yaml:"savings_samples"`
}

// NotifyConfig groups desktop-notification and webhook settings.
type NotifyConfig struct {
	Enabled        bool   `yaml:"enabled"`
	WebhookEnabled bool   `yaml:"webhook_enabled"`
	WebhookURL     string `yaml:"webhook_url"`
}

// WorktreeConfig groups worktree-retention and spawn-gate settings.
type WorktreeConfig struct {
	SpawnGate    bool `yaml:"spawn_gate"`
	SpawnGateMax int  `yaml:"spawn_gate_max_agents"`
	KeepDone     bool `yaml:"keep_done"`
	AutoPrune    bool `yaml:"auto_prune"`
}

// LocalLLMConfig groups local-model, REPL, and LLM-offload settings.
type LocalLLMConfig struct {
	Enabled    bool   `yaml:"enabled"`
	URL        string `yaml:"url"`
	Model      string `yaml:"model"`
	Timeout    string `yaml:"timeout"`
	Escalate   bool   `yaml:"escalate"`
	Tier       string `yaml:"tier"`
	Classifier string `yaml:"classifier"`
	Repl       bool   `yaml:"repl"`
}

// PipelineConfig groups pipeline-execution settings.
type PipelineConfig struct {
	KeepDone bool `yaml:"keep_done"`
	Hint     bool `yaml:"hint"`
}

// AutoRestartConfig groups the auto-restart supervisor settings.
type AutoRestartConfig struct {
	Max   int    `yaml:"max"`
	Reset string `yaml:"reset"`
}

// CollabConfig groups the file-conflict collaboration settings.
type CollabConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Interval string `yaml:"interval"`
	Hint     bool   `yaml:"hint"`
}

// MemoryConfig groups the project-memory (.warden/memory.md) settings.
type MemoryConfig struct {
	Inject bool `yaml:"inject"`
	Curate bool `yaml:"curate"`
	Ground bool `yaml:"ground"`
}

// BranchTrackConfig groups the branch/CI tracker settings.
type BranchTrackConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Interval string `yaml:"interval"`
}

// RateLimitConfig groups the rate-limit auto-resume scheduler settings.
type RateLimitConfig struct {
	RetryInterval      string `yaml:"retry_interval"`
	SpendRetryInterval string `yaml:"spend_retry_interval"`
	Buffer             string `yaml:"buffer"`
	AutoResume         bool   `yaml:"auto_resume"`
	ResumePrompt       string `yaml:"resume_prompt"`
}

// BackendsConfig groups the agent-backend registry / internal-thinking router
// settings (docs/specs/2026-08-06-backend-registry.md §10).
type BackendsConfig struct {
	// LimitRetry is how long a free CLI backend is skipped by the internal-thinking
	// router after it returns a rate-limit / spend signal (§7). A Go duration
	// string; defaults to 15m.
	LimitRetry string `yaml:"limit_retry"`
}

// HTTPConfig groups the daemon HTTP write-budget settings.
type HTTPConfig struct {
	TimeoutFast string `yaml:"timeout_fast"`
	TimeoutSlow string `yaml:"timeout_slow"`
}

// LogConfig groups the structured-logging settings.
type LogConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// PluginsConfig groups the plugin-system settings.
type PluginsConfig struct {
	Enabled  bool          `yaml:"enabled"`
	Registry []plugin.Spec `yaml:"registry"`
}

// AutopilotConfig is the `autopilot` block: the master switch and the per-plan
// brain/merge/guardian template (docs/specs/autopilot.md §10, master-plan
// schema). Enabling it OR-bundles auto_approve / rate_limit.auto_resume /
// auto_restart on for autopilot-owned agents (see AutopilotBundle). In the S1
// inert core only Enabled + Plans + Merge.{TargetBranch,Gate} are consumed; the
// rest is carried forward for the brain (S3), land (S4), and guardian (S5).
type AutopilotConfig struct {
	Enabled  bool                    `yaml:"enabled"`
	Plans    []AutopilotPlan         `yaml:"plans"`
	Brain    AutopilotBrainConfig    `yaml:"brain"`
	Merge    AutopilotMergeConfig    `yaml:"merge"`
	Guardian AutopilotGuardianConfig `yaml:"guardian"`
}

// AutopilotPlan is one configured plan file (one brain per plan; at most one
// active plan per repo).
type AutopilotPlan struct {
	File string `yaml:"file"`
}

// AutopilotBrainConfig is the default template applied to each plan's brain: the
// cost-tiered backend ladder plus persona/parallelism knobs (autopilot.md §7).
type AutopilotBrainConfig struct {
	Backends           AutopilotBackends `yaml:"backends"`
	AllowPayPerUse     bool              `yaml:"allow_pay_per_use"`
	Role               string            `yaml:"role"`
	Headless           bool              `yaml:"headless"`
	MaxParallelWorkers int               `yaml:"max_parallel_workers"`
}

// AutopilotBackends is the cost-tier ladder: order within a list is preference.
// pay_per_use is only ever used when AllowPayPerUse is true.
type AutopilotBackends struct {
	Free         []string `yaml:"free"`
	Subscription []string `yaml:"subscription"`
	PayPerUse    []string `yaml:"pay_per_use"`
}

// AutopilotMergeConfig governs how the brain lands worker branches into the
// integration branch (autopilot.md §6).
type AutopilotMergeConfig struct {
	TargetBranch string `yaml:"target_branch"`
	Strategy     string `yaml:"strategy"`
	Gate         string `yaml:"gate"` // auto | ci | local
	DeleteBranch bool   `yaml:"delete_branch"`
}

// AutopilotGuardianConfig configures the heartbeat guardian's heal ladder
// (autopilot.md §2.3). Duration-valued fields are Go duration strings.
type AutopilotGuardianConfig struct {
	Interval             string `yaml:"interval"`
	HeartbeatTimeout     string `yaml:"heartbeat_timeout"`
	BackoffMin           string `yaml:"backoff_min"`
	BackoffMax           string `yaml:"backoff_max"`
	RotateAtContext      string `yaml:"rotate_at_context"`
	NotifyEachEscalation bool   `yaml:"notify_each_escalation"`
}

// ---------------------------------------------------------------------------
// Top-level Config
// ---------------------------------------------------------------------------

// Config is the typed view of warden's configuration. Every field carries a
// yaml tag matching a key in the on-disk config file. Duration-valued settings
// are stored as Go duration strings (e.g. "5m"); use the typed accessor methods
// (AutoRestartResetDuration, etc.) to read them.
//
// Related settings are grouped into namespaced YAML blocks (rails, tokens,
// notify, worktree, local_llm, pipeline, auto_restart, collab, memory,
// branch_track, rate_limit, http, log, plugins). Old flat keys (e.g.
// token_guard, collab_enabled) are transparently migrated at load time and are
// kept as deprecated aliases — they still work but emit a deprecation warning
// once per daemon start.
type Config struct {
	Addr              string `yaml:"addr"`
	DataDir           string `yaml:"data_dir"`
	ClaudeProjectsDir string `yaml:"claude_projects_dir"`
	// WorkspacePath is the directory the "Open remote project" flow (TUI/daemon
	// `POST /fs/clone`) clones a GitHub/GitLab URL into, as <workspace_path>/
	// <repo-name>. Values: absolute path.
	WorkspacePath         string          `yaml:"workspace_path"`
	ApprovalsEnabled      bool            `yaml:"approvals"`
	AutoApprove           approval.Policy `yaml:"auto_approve"`
	DefaultPermissionMode string          `yaml:"default_permission_mode"`
	MetricsEnabled        bool            `yaml:"metrics"`
	// AllowNonLoopback is DEPRECATED and inert (audit #7): it no longer bypasses
	// authentication. A bearer token is mandatory for any non-loopback bind. The
	// field is kept so existing configs still parse; setting it true only logs a
	// deprecation warning at daemon startup.
	AllowNonLoopback bool `yaml:"allow_nonloopback"`

	// TrustedProxies lists IPs/CIDRs of reverse proxies or tunnels that front the
	// daemon (e.g. a Cloudflare Tunnel forwarding over loopback). It affects the
	// AUDIT ACTOR ONLY: when the immediate peer is one of these, the audit log
	// resolves the real client from X-Forwarded-For instead of recording the
	// proxy's address. It is deliberately NOT used for the auth-failure throttle
	// (which keeps the spoof-resistant RemoteAddr key). Empty ⇒ actor is always
	// the peer address. Accepts bare IPs and CIDRs (IPv4/IPv6).
	TrustedProxies []string `yaml:"trusted_proxies"`

	ModelDefault     string `yaml:"model_default"`
	Snapshots        bool   `yaml:"snapshots"`
	Tutorial         bool   `yaml:"tutorial"`
	Insights         bool   `yaml:"insights"`
	ApiDocs          bool   `yaml:"api_docs"`
	SchedulerEnabled bool   `yaml:"scheduler_enabled"`

	// Namespaced configuration groups. Each replaces a set of flat keys that are
	// now deprecated aliases (see migrateFlatToNamespaced). Old flat keys are
	// still transparently loaded by translating them at parse time.
	//
	// HTTP holds the daemon's write budgets: TimeoutFast bounds ordinary
	// data/action routes; TimeoutSlow bounds the lifecycle routes that do real,
	// possibly-minutes-long work (spawn's worktree checkout, commit/push hooks,
	// running checks). These are backstops against a wedged handler, not pacing
	// devices — keep them generous, especially in large monorepos where git
	// operations are slow.
	Rails       RailsConfig       `yaml:"rails"`
	Tokens      TokensConfig      `yaml:"tokens"`
	Notify      NotifyConfig      `yaml:"notify"`
	Worktree    WorktreeConfig    `yaml:"worktree"`
	LocalLLM    LocalLLMConfig    `yaml:"local_llm"`
	Pipeline    PipelineConfig    `yaml:"pipeline"`
	AutoRestart AutoRestartConfig `yaml:"auto_restart"`
	Collab      CollabConfig      `yaml:"collab"`
	Memory      MemoryConfig      `yaml:"memory"`
	BranchTrack BranchTrackConfig `yaml:"branch_track"`
	RateLimit   RateLimitConfig   `yaml:"rate_limit"`
	HTTP        HTTPConfig        `yaml:"http"`
	Log         LogConfig         `yaml:"log"`
	Plugins     PluginsConfig     `yaml:"plugins"`
	Autopilot   AutopilotConfig   `yaml:"autopilot"`
	Backends    BackendsConfig    `yaml:"backends"`
}

// setting describes one config key for file generation/migration: its YAML key
// and the head-comment documenting its allowed values. The ordered schema slice
// is the source of truth for the file's layout; defaults() supplies the values.
type setting struct {
	Key  string
	Hint string
}

// schema is the ordered list of every config key with its documentation hint.
// Order here is the order keys are written to a freshly generated file. A
// reflection-based drift-guard test asserts this key set equals the set of
// yaml tags on Config.
var schema = []setting{
	{"addr", "Daemon listen address. Values: host:port (a non-loopback bind always requires WARDEN_TOKEN for bearer-token auth)"},
	{"data_dir", "Directory for warden state (sessions, inbox, pipelines, metrics). Values: absolute path"},
	{"claude_projects_dir", "Claude Code transcript root. Values: absolute path"},
	{"workspace_path", "Directory the \"Open remote project\" flow clones a GitHub/GitLab URL into, as <workspace_path>/<repo-name>. Values: absolute path"},
	{"approvals", "Enable the approvals inbox (parse + answer permission prompts). Values: true | false"},
	{"auto_approve", "Auto-approve policy. With NO rules configured this is the simple on/off toggle (enabled answers every recognized, non-destructive prompt). With rules, the daemon answers a recognized prompt only when it matches an allow rule, matches no deny rule, and is not on the built-in destructive deny-list (which always wins). Sub-keys: enabled (master switch), allow_sticky (press \"don't ask again\" options), rules.allow / rules.deny (lists of {tool, pattern, regex, paths} — tool/pattern are case-insensitive, regex is a Go regexp), max_repeats (circuit breaker: how many times the IDENTICAL prompt may be consecutively approved for one agent before auto-approve halts and escalates to a human; 0 = default 10, negative = off), agents (per-agent overrides keyed by agent name or id, each its own {enabled, allow_sticky, rules, max_repeats} block that replaces the default for that agent)."},
	{"default_permission_mode", "Default permission mode for new agents.\nValues: auto | default | acceptEdits | bypassPermissions | dontAsk | plan"},
	{"metrics", "Record per-agent metrics to disk. Values: true | false"},
	{"allow_nonloopback", "DEPRECATED and inert: this no longer bypasses authentication. A bearer token (WARDEN_TOKEN) is now mandatory for any non-loopback bind. Setting it true only logs a deprecation warning. Values: true | false"},
	{"trusted_proxies", "Reverse proxies / tunnels that front the daemon (e.g. a Cloudflare Tunnel forwarding over loopback). Audit-actor-only: when the immediate peer is one of these, the audit log resolves the real client from X-Forwarded-For instead of recording the proxy address. NOT used for the auth-failure throttle. Values: list of IPs and/or CIDRs (IPv4/IPv6); empty disables it"},
	{"model_default", "Default model for new agents. Values: a claude model id or alias (sonnet, opus, haiku, fable)"},
	{"snapshots", "Enable the snapshot/checkpoint system (wd snapshot create/list/restore): capture an agent's worktree state (non-destructive git stash + transcript) and restore it later. Values: true | false"},
	{"tutorial", "Print a one-line first-run hint pointing at `wd tutorial` until the walkthrough is completed (it writes a tutorial-complete marker in data_dir). The hint is non-blocking and only shown on an interactive TTY; this gate disables it. Values: true | false"},
	{"insights", "Enable the AI-powered insights engine (wd insights + MCP insights): mine agent history for duration outliers, co-edited files, error rates, busy periods, and sequential-but-disjoint sessions that could run in parallel. Deterministic by default; narrated by the local model when local_llm is on. Values: true | false"},
	{"api_docs", "Serve the OpenAPI spec + interactive Swagger UI at /api/docs (public, like the static UI shell; the spec describes the API shape but holds no secrets). Values: true | false"},
	{"scheduler_enabled", "Enable the native scheduler (#15): recurring (--cron) and single-shot (--at) triggers that fire an agent spawn or a pipeline on a daemon-owned timer (wd schedule create/list/delete). OFF by default — the daemon must be running for schedules to fire, so this is deliberately opt-in. Values: true | false"},

	// Namespaced groups — each replaces a set of deprecated flat keys.
	{"rails", "Guard and boundary-hook settings (previously flat keys: git_conventions, git_redirect, check_redirect, root_guard, isolation_guard). Sub-keys: git_conventions, git_redirect, check_redirect, root_guard, isolation_guard. Flat keys still load as deprecated aliases."},
	{"tokens", "Token-guard, budget-gate, and cost/savings settings (previously flat keys: token_guard, token_warn_alert, token_auto_compact, token_force_compact, token_warn, token_critical, token_compact_resume_prompt, budget_gate, budget_daily_usd, budget_weekly_usd, savings, savings_samples). Sub-keys match without the token_ prefix. Flat keys still load as deprecated aliases."},
	{"notify", "Notification settings (previously flat keys: notify, webhook_enabled, webhook_url). Sub-keys: enabled (was notify), webhook_enabled, webhook_url. Flat keys still load as deprecated aliases."},
	{"worktree", "Worktree-retention and spawn-gate settings (previously flat keys: spawn_gate, spawn_gate_max_agents, worktree_keep_done, worktree_auto_prune). Sub-keys: spawn_gate, spawn_gate_max_agents, keep_done, auto_prune. Flat keys still load as deprecated aliases."},
	{"local_llm", "Local-model, REPL, and LLM-offload settings (previously flat keys: local_llm, local_llm_url, local_llm_model, local_llm_timeout, local_llm_escalate, local_llm_tier, local_llm_classifier, repl). Sub-keys: enabled (was local_llm), url, model, timeout, escalate, tier, classifier, repl. Flat keys still load as deprecated aliases."},
	{"pipeline", "Pipeline-execution settings (previously flat keys: pipeline_keep_done, pipeline_hint). Sub-keys: keep_done (keep a pipeline job's agent alive after it completes), hint (append the pipeline-decomposition hint to standalone agents). Flat keys still load as deprecated aliases. Values: true | false"},
	{"auto_restart", "Auto-restart supervisor for errored opted-in agents (previously flat keys: auto_restart_max, auto_restart_reset). Sub-keys: max (integer >= 0, max restart attempts), reset (Go duration, e.g. 5m — sustained-health window that resets the counter). Flat keys still load as deprecated aliases."},
	{"collab", "File-conflict collaboration settings (previously flat keys: collab_enabled, collab_interval, collab_hint). Sub-keys: enabled (warn agents editing the same file), interval (Go duration, e.g. 10s — scan interval), hint (append the conflict-check hint to spawned agents). Flat keys still load as deprecated aliases."},
	{"memory", "Project-memory (.warden/memory.md) settings (previously flat keys: memory_inject, memory_curate, memory_ground). Sub-keys: inject (project the repo's curated durable facts into every spawned agent via its system-prompt seam; off or an empty/absent file is byte-identical to no injection), curate (auto-propose UNVERIFIED entries from completion digests into the WORKING TREE only, gated by the committed diff — default OFF, opt-in), ground (answer project questions locally in `wd repl` on the local model, read-only, default ON — it REMOVES cloud round-trips). Flat keys still load as deprecated aliases. Values: true | false"},
	{"branch_track", "Branch/CI tracker settings (previously flat keys: branch_track_enabled, branch_track_interval). Sub-keys: enabled (monitor each agent's branch for CI failures and drift from main, delivering informational inbox/desktop alerts), interval (Go duration, e.g. 2m — scan interval). Flat keys still load as deprecated aliases."},
	{"rate_limit", "Rate-limit auto-resume scheduler settings (previously flat keys: rate_limit_retry_interval, rate_limit_spend_retry_interval, rate_limit_buffer, rate_limit_auto_resume, rate_limit_resume_prompt). Sub-keys: retry_interval (Go duration, e.g. 30m — fallback wait before retrying a session/weekly limit whose reset time could not be parsed), spend_retry_interval (Go duration, e.g. 6h — longer fallback for a monthly spend cap, which carries no reset time), buffer (Go duration, e.g. 1m — extra wait on top of a parsed reset time), auto_resume (true | false — auto-pick the wait-for-reset menu choice and resume agents after any limit clears), resume_prompt (text to type when a limit clears so the agent picks its work back up; default \"continue\", set to empty for a bare keypress with no injected user turn). Flat keys still load as deprecated aliases."},
	{"http", "Daemon HTTP write budgets (previously flat keys: http_timeout_fast, http_timeout_slow). Backstops against a wedged handler, not pacing devices — keep them generous, especially in large monorepos where git operations are slow. Sub-keys: timeout_fast (Go duration, e.g. 30s — ordinary data/action routes: list, status, send, …), timeout_slow (Go duration, e.g. 10m — slow lifecycle routes: spawn's worktree checkout, commit/push and their hooks, checks, snapshots, pipeline ops). Flat keys still load as deprecated aliases."},
	{"log", "Structured-logging settings (previously flat keys: log_level, log_format). Sub-keys: level (debug | info | warn | error — minimum severity the daemon logs), format (text (human-readable) | json (structured)). Flat keys still load as deprecated aliases."},
	{"plugins", "Plugin system (#47) settings (previously flat keys: plugins, plugin_registry). OFF by default — plugins execute external code, so this is deliberately opt-in. A broken, slow, or missing plugin fails open (logged and skipped); it never blocks or crashes an agent. Sub-keys: enabled (was plugins; load the executables in registry, register their custom task types, and invoke their subscribed lifecycle hooks over JSON-over-stdio), registry (was plugin_registry; a list of entries, each with name, path (the plugin executable), events (subscribed lifecycle hooks: any of pre-spawn, post-spawn, pre-commit, post-commit, pre-check, post-check, pre-teardown), and task_types (custom agent task types, each {name, worktree})). Flat keys still load as deprecated aliases."},
	{"backends", "Agent-backend registry / internal-thinking router settings (docs/specs/2026-08-06-backend-registry.md §10). Warden's own internal thinking (task classification, activity summaries, agent naming, digest narration, memory curation) is routed STRICTLY through free/local backends — it never makes a paid call. Sub-keys: limit_retry (Go duration, e.g. 15m — how long a free CLI backend is skipped by the router after it returns a rate-limit / spend signal, before it is retried)."},
	{"autopilot", "Autopilot defaults for named, durably registered runs. Create plans with `warden autopilot init --name <name>` or register existing plans with `warden autopilot register <file>`. Sub-keys: enabled (legacy per-repo switch), plans (DEPRECATED compatibility list; migrated into plans/ and the run store on boot), brain (role, headless, max_parallel_workers; backend tiers live in the backend registry), merge (target_branch, strategy, gate, delete_branch), guardian (interval, heartbeat_timeout, backoff_min, backoff_max, rotate_at_context, notify_each_escalation)."},
}

// fileHeader is the comment written at the very top of a generated config file.
const fileHeader = "warden configuration — edit values below; run `warden config` to see what's live."

// defaultCompactResumePrompt is sent to a force-compacted agent once the
// compaction lands, so it resumes the work the interrupt discarded. Kept generic
// because warden can't know the agent's specific task.
const defaultCompactResumePrompt = "Your context was just compacted to free up space. Continue the task you were working on before the compaction."

// defaultRateLimitResumePrompt is typed into a rate-limited agent's pane once its
// limit window clears, so the resume actually delivers a user turn instead of a
// bare keypress the agent may ignore. Kept to a single generic word — warden
// can't know the agent's specific task, and "continue" reads naturally as the
// nudge to pick the work back up. Set rate_limit.resume_prompt to "" to opt back
// into the bare-keypress un-pause (no injected user turn).
const defaultRateLimitResumePrompt = "continue"

// defaults returns a fully-populated Config holding every setting's default.
// It is the single source of truth for default values (file generation reads
// the values from here; Load starts from here and overlays the file).
func defaults() Config {
	return Config{
		Addr:              "127.0.0.1:8765",
		DataDir:           defaultDataDir(),
		ClaudeProjectsDir: defaultClaudeProjectsDir(),
		WorkspacePath:     defaultWorkspaceDir(),
		ApprovalsEnabled:  true,
		AutoApprove: approval.Policy{
			Enabled:     false,
			AllowSticky: false,
			Rules:       approval.Rules{Allow: []approval.Rule{}, Deny: []approval.Rule{}},
		},
		DefaultPermissionMode: "auto",
		MetricsEnabled:        true,
		AllowNonLoopback:      false,
		ModelDefault:          "claude-sonnet-4-6", // current "sonnet" alias; keep in sync with lifecycle.DefaultModel
		Snapshots:             true,
		Tutorial:              true,
		Insights:              true,
		ApiDocs:               true,
		SchedulerEnabled:      false,
		Rails: RailsConfig{
			GitConventions: true,
			GitRedirect:    true,
			CheckRedirect:  true,
			RootGuard:      true,
			IsolationGuard: true,
		},
		Tokens: TokensConfig{
			Guard:               true,
			WarnAlert:           true,
			AutoCompact:         true,
			ForceCompact:        false, // destructive (interrupts a busy turn) — opt-in only
			CompactResumePrompt: defaultCompactResumePrompt,
			Warn:                200000,
			Critical:            400000,
			BudgetGate:          false,
			BudgetDailyUSD:      0,
			BudgetWeeklyUSD:     0,
			Savings:             true,
			SavingsSamples:      false,
		},
		Notify: NotifyConfig{
			Enabled:        false,
			WebhookEnabled: false,
			WebhookURL:     "",
		},
		Worktree: WorktreeConfig{
			SpawnGate:    true,
			SpawnGateMax: 5,
			KeepDone:     true,
			AutoPrune:    false,
		},
		LocalLLM: LocalLLMConfig{
			Enabled:    false,
			URL:        "http://localhost:11434",
			Model:      "qwen2.5-coder:7b",
			Timeout:    "20s",
			Escalate:   true,
			Tier:       "auto",
			Classifier: "heuristic",
			Repl:       false,
		},
		Pipeline: PipelineConfig{
			KeepDone: false,
			Hint:     true,
		},
		AutoRestart: AutoRestartConfig{
			Max:   3,
			Reset: "5m",
		},
		Collab: CollabConfig{
			Enabled:  true,
			Interval: "10s",
			Hint:     true,
		},
		Memory: MemoryConfig{
			Inject: true,
			Curate: false, // opt-in: the risky half (proposals gated by the committed diff)
			Ground: true,  // $0 local-only lever: it only REMOVES cloud round-trips
		},
		BranchTrack: BranchTrackConfig{
			Enabled:  false,
			Interval: "2m",
		},
		RateLimit: RateLimitConfig{
			RetryInterval:      "30m",
			SpendRetryInterval: "6h",
			Buffer:             "1m",
			AutoResume:         true,
			ResumePrompt:       defaultRateLimitResumePrompt,
		},
		HTTP: HTTPConfig{
			TimeoutFast: "30s",
			TimeoutSlow: "10m",
		},
		Log: LogConfig{
			Level:  logging.DefaultLevel,
			Format: logging.DefaultFormat,
		},
		Plugins: PluginsConfig{
			Enabled:  false,
			Registry: []plugin.Spec{},
		},
		Autopilot: AutopilotConfig{
			Enabled: false,
			Plans:   []AutopilotPlan{},
			Brain: AutopilotBrainConfig{
				Backends: AutopilotBackends{
					Free:         []string{"antigravity"},
					Subscription: []string{"claude"},
					PayPerUse:    []string{},
				},
				AllowPayPerUse:     false,
				Role:               "autopilot",
				Headless:           true,
				MaxParallelWorkers: 3,
			},
			Merge: AutopilotMergeConfig{
				TargetBranch: "autopilot/integration",
				Strategy:     "squash",
				Gate:         "auto",
				DeleteBranch: true,
			},
			Guardian: AutopilotGuardianConfig{
				Interval:             "60s",
				HeartbeatTimeout:     "10m",
				BackoffMin:           "30s",
				BackoffMax:           "6h",
				RotateAtContext:      "critical",
				NotifyEachEscalation: true,
			},
		},
		Backends: BackendsConfig{
			LimitRetry: "15m",
		},
	}
}

func defaultClaudeProjectsDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".claude/projects"
	}
	return filepath.Join(home, ".claude", "projects")
}

func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".warden"
	}
	return filepath.Join(home, ".warden")
}

// defaultWorkspaceDir returns ~/.warden/workspace, the default clone
// destination root for the "Open remote project" flow.
func defaultWorkspaceDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".warden", "workspace")
	}
	return filepath.Join(home, ".warden", "workspace")
}

// DefaultPath returns the canonical config file location (~/.warden/config.yaml).
// It is bootstrap state: resolved before, and independent of, the data_dir
// setting the file itself contains. Falls back gracefully when home is unknown.
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".warden", "config.yaml")
	}
	return filepath.Join(home, ".warden", "config.yaml")
}

// Load reads config from path, applying defaults for any missing keys and
// validating the result. A missing or unreadable file yields an all-defaults
// Config. Load never writes.
//
// Deprecated flat keys (e.g. token_guard, local_llm_url, notify) are
// transparently migrated to their namespaced equivalents in memory — the file
// is not changed. A deprecation warning is logged once per deprecated key found.
func Load(path string) Config {
	c, err := LoadStrict(path)
	if err != nil {
		// Preserve the historic best-effort contract: an absent file is the normal
		// case (all defaults, no log); a parse/decode error logs and degrades to
		// defaults so a malformed file never stops the daemon from starting.
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("config: load error, using defaults", "path", path, "err", err)
		}
		return defaults()
	}
	return c
}

// LoadStrict reads, migrates, and validates the config exactly like Load but
// returns an error instead of silently falling back to defaults when the file is
// missing, unparseable, or undecodable. Hot-reload (Server.ApplyConfig via the
// config watcher) uses this so a bad mid-run edit KEEPS the last-good config
// rather than resetting every subsystem to its default — mirroring the plan
// mid-run-edit philosophy in docs/specs/autopilot.md §3 (a bad edit must never
// crash or silently wipe live state). validate() only normalizes (it never
// fails), so the returned error is always an I/O, YAML-parse, or decode error.
func LoadStrict(path string) (Config, error) {
	c := defaults()
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}
	// Parse to node tree so we can run the in-memory flat→namespaced migration
	// before struct decoding. This preserves backward compat for old flat keys.
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return Config{}, fmt.Errorf("parse config %q: %w", path, err)
	}
	if len(doc.Content) > 0 && doc.Content[0].Kind == yaml.MappingNode {
		migrateFlatToNamespaced(doc.Content[0])
		migrateAutoApprove(doc.Content[0])
		warnDeprecatedAutopilotBackends(doc.Content[0])
	}
	// Decode the (possibly migrated) node tree onto c — absent keys keep their
	// default value since c was pre-populated by defaults().
	if err := doc.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("decode config %q: %w", path, err)
	}
	validate(&c)
	return c, nil
}

// validate normalizes a loaded Config against the same rules the env-based
// loader enforced: required-string fallbacks, the permission-mode whitelist,
// the token warn/critical ordering, and well-formed duration strings.
func validate(c *Config) {
	d := defaults()
	if strings.TrimSpace(c.Addr) == "" {
		c.Addr = d.Addr
	}
	if strings.TrimSpace(c.DataDir) == "" {
		c.DataDir = d.DataDir
	}
	if strings.TrimSpace(c.ClaudeProjectsDir) == "" {
		c.ClaudeProjectsDir = d.ClaudeProjectsDir
	}
	if strings.TrimSpace(c.WorkspacePath) == "" {
		c.WorkspacePath = d.WorkspacePath
	}
	if strings.TrimSpace(c.ModelDefault) == "" {
		c.ModelDefault = d.ModelDefault
	}
	if strings.TrimSpace(c.Tokens.CompactResumePrompt) == "" {
		c.Tokens.CompactResumePrompt = d.Tokens.CompactResumePrompt
	}
	c.DefaultPermissionMode = validPermissionMode(c.DefaultPermissionMode)
	if c.Tokens.Critical <= c.Tokens.Warn { // inverted/degenerate → defaults
		c.Tokens.Warn, c.Tokens.Critical = d.Tokens.Warn, d.Tokens.Critical
	}
	if c.AutoRestart.Max < 0 {
		c.AutoRestart.Max = d.AutoRestart.Max
	}
	c.Log.Level = validLogLevel(c.Log.Level, d.Log.Level)
	c.Log.Format = validLogFormat(c.Log.Format, d.Log.Format)
	c.AutoRestart.Reset = validDuration(c.AutoRestart.Reset, d.AutoRestart.Reset)
	c.Collab.Interval = validDuration(c.Collab.Interval, d.Collab.Interval)
	c.BranchTrack.Interval = validDuration(c.BranchTrack.Interval, d.BranchTrack.Interval)
	c.RateLimit.RetryInterval = validDuration(c.RateLimit.RetryInterval, d.RateLimit.RetryInterval)
	c.RateLimit.SpendRetryInterval = validDuration(c.RateLimit.SpendRetryInterval, d.RateLimit.SpendRetryInterval)
	c.RateLimit.Buffer = validDuration(c.RateLimit.Buffer, d.RateLimit.Buffer)
	c.HTTP.TimeoutFast = validDuration(c.HTTP.TimeoutFast, d.HTTP.TimeoutFast)
	c.HTTP.TimeoutSlow = validDuration(c.HTTP.TimeoutSlow, d.HTTP.TimeoutSlow)
	c.Autopilot.Merge.Gate = validAutopilotGate(c.Autopilot.Merge.Gate, d.Autopilot.Merge.Gate)
	if strings.TrimSpace(c.Autopilot.Merge.TargetBranch) == "" {
		c.Autopilot.Merge.TargetBranch = d.Autopilot.Merge.TargetBranch
	}
	c.Autopilot.Guardian.Interval = validDuration(c.Autopilot.Guardian.Interval, d.Autopilot.Guardian.Interval)
	c.Autopilot.Guardian.HeartbeatTimeout = validDuration(c.Autopilot.Guardian.HeartbeatTimeout, d.Autopilot.Guardian.HeartbeatTimeout)
	c.Autopilot.Guardian.BackoffMin = validDuration(c.Autopilot.Guardian.BackoffMin, d.Autopilot.Guardian.BackoffMin)
	c.Autopilot.Guardian.BackoffMax = validDuration(c.Autopilot.Guardian.BackoffMax, d.Autopilot.Guardian.BackoffMax)
	if strings.TrimSpace(c.LocalLLM.URL) == "" {
		c.LocalLLM.URL = d.LocalLLM.URL
	}
	if strings.TrimSpace(c.LocalLLM.Model) == "" {
		c.LocalLLM.Model = d.LocalLLM.Model
	}
	c.LocalLLM.Timeout = validDuration(c.LocalLLM.Timeout, d.LocalLLM.Timeout)
	c.Backends.LimitRetry = validDuration(c.Backends.LimitRetry, d.Backends.LimitRetry)
}

func validPermissionMode(v string) string {
	switch v {
	case "acceptEdits", "auto", "bypassPermissions", "default", "dontAsk", "plan":
		return v
	}
	if v != "" {
		slog.Warn("config: invalid default_permission_mode, using auto", "value", v)
	}
	return "auto"
}

func validLogLevel(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	if logging.ValidLevel(v) {
		return strings.ToLower(strings.TrimSpace(v))
	}
	slog.Warn("config: invalid log_level, using default", "value", v, "default", def)
	return def
}

func validLogFormat(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	if logging.ValidFormat(v) {
		return strings.ToLower(strings.TrimSpace(v))
	}
	slog.Warn("config: invalid log_format, using default", "value", v, "default", def)
	return def
}

// validAutopilotGate normalizes the merge gate mode to auto|ci|local, falling
// back to def on any other (or empty) value.
func validAutopilotGate(v, def string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "auto", "ci", "local":
		return strings.ToLower(strings.TrimSpace(v))
	case "":
		return def
	}
	slog.Warn("config: invalid autopilot.merge.gate, using default", "value", v, "default", def)
	return def
}

func validDuration(v, def string) string {
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return v
	}
	if strings.TrimSpace(v) != "" {
		slog.Warn("config: invalid duration, using default", "value", v, "default", def)
	}
	return def
}

// Reconcile is the only writer. When the file is absent it generates a full,
// commented file from the schema + defaults. When present it parses the node
// tree, migrates any deprecated flat keys to their namespaced blocks, and
// appends only the keys not already there (with their hint comments), preserving
// existing values, comments, and unknown keys untouched.
func Reconcile(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		out, err := renderFull()
		if err != nil {
			return err
		}
		return writeFile(path, out)
	}
	if err != nil {
		return err
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("config: parse %s: %w", path, err)
	}
	// Empty or non-mapping document → regenerate from scratch.
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		out, err := renderFull()
		if err != nil {
			return err
		}
		return writeFile(path, out)
	}
	mapping := doc.Content[0]

	// Migrate deprecated flat keys to namespaced blocks. Must run before the
	// auto_approve migration and the add-missing loop, which use the same node.
	changed := migrateFlatToNamespaced(mapping)
	// Migrate a legacy flat auto_approve (scalar bool, plus the Stage-A
	// auto_approve_allow_sticky key) into the nested policy block.
	changed = migrateAutoApprove(mapping) || changed

	present := map[string]bool{}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		present[mapping.Content[i].Value] = true
	}
	defVals, err := defaultValueNodes()
	if err != nil {
		return err
	}
	for _, s := range schema {
		if present[s.Key] {
			continue
		}
		val, ok := defVals[s.Key]
		if !ok {
			continue
		}
		key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s.Key, HeadComment: comment(s.Hint)}
		mapping.Content = append(mapping.Content, key, val)
		changed = true
	}
	if !changed {
		return nil
	}
	out, err := marshalNode(&doc)
	if err != nil {
		return err
	}
	return writeFile(path, out)
}

// ---------------------------------------------------------------------------
// Flat-to-namespaced migration
// ---------------------------------------------------------------------------

// keyAlias maps one deprecated flat key to its sub-key in the new block.
type keyAlias struct {
	flat string // old flat key at the root level
	sub  string // sub-key name inside the new block
}

// keyGroup defines one namespaced block and the flat aliases it absorbs.
type keyGroup struct {
	block string
	keys  []keyAlias
}

// flatKeyGroups is the authoritative mapping from deprecated flat keys to the
// five namespaced configuration blocks. Order within each group matches the
// order sub-keys appear in the generated file.
var flatKeyGroups = []keyGroup{
	{"rails", []keyAlias{
		{"git_conventions", "git_conventions"},
		{"git_redirect", "git_redirect"},
		{"check_redirect", "check_redirect"},
		{"root_guard", "root_guard"},
		{"isolation_guard", "isolation_guard"},
	}},
	{"tokens", []keyAlias{
		{"token_guard", "guard"},
		{"token_warn_alert", "warn_alert"},
		{"token_auto_compact", "auto_compact"},
		{"token_force_compact", "force_compact"},
		{"token_compact_resume_prompt", "compact_resume_prompt"},
		{"token_warn", "warn"},
		{"token_critical", "critical"},
		{"budget_gate", "budget_gate"},
		{"budget_daily_usd", "budget_daily_usd"},
		{"budget_weekly_usd", "budget_weekly_usd"},
		{"savings", "savings"},
		{"savings_samples", "savings_samples"},
	}},
	{"notify", []keyAlias{
		{"notify", "enabled"},
		{"webhook_enabled", "webhook_enabled"},
		{"webhook_url", "webhook_url"},
	}},
	{"worktree", []keyAlias{
		{"spawn_gate", "spawn_gate"},
		{"spawn_gate_max_agents", "spawn_gate_max_agents"},
		{"worktree_keep_done", "keep_done"},
		{"worktree_auto_prune", "auto_prune"},
	}},
	{"local_llm", []keyAlias{
		{"local_llm", "enabled"},
		{"local_llm_url", "url"},
		{"local_llm_model", "model"},
		{"local_llm_timeout", "timeout"},
		{"local_llm_escalate", "escalate"},
		{"local_llm_tier", "tier"},
		{"local_llm_classifier", "classifier"},
		{"repl", "repl"},
		{"orchestrator", "repl"}, // pre-rename legacy alias
	}},
	{"pipeline", []keyAlias{
		{"pipeline_keep_done", "keep_done"},
		{"pipeline_hint", "hint"},
	}},
	{"auto_restart", []keyAlias{
		{"auto_restart_max", "max"},
		{"auto_restart_reset", "reset"},
	}},
	{"collab", []keyAlias{
		{"collab_enabled", "enabled"},
		{"collab_interval", "interval"},
		{"collab_hint", "hint"},
	}},
	{"memory", []keyAlias{
		{"memory_inject", "inject"},
		{"memory_curate", "curate"},
		{"memory_ground", "ground"},
	}},
	{"branch_track", []keyAlias{
		{"branch_track_enabled", "enabled"},
		{"branch_track_interval", "interval"},
	}},
	{"rate_limit", []keyAlias{
		{"rate_limit_retry_interval", "retry_interval"},
		{"rate_limit_spend_retry_interval", "spend_retry_interval"},
		{"rate_limit_buffer", "buffer"},
		{"rate_limit_auto_resume", "auto_resume"},
		{"rate_limit_resume_prompt", "resume_prompt"},
	}},
	{"http", []keyAlias{
		{"http_timeout_fast", "timeout_fast"},
		{"http_timeout_slow", "timeout_slow"},
	}},
	{"log", []keyAlias{
		{"log_level", "level"},
		{"log_format", "format"},
	}},
	{"plugins", []keyAlias{
		{"plugins", "enabled"},
		{"plugin_registry", "registry"},
	}},
}

// migrateFlatToNamespaced rewrites deprecated flat keys into their namespaced
// blocks in place on the YAML mapping node. It handles all five groups. Returns
// true when mapping was modified. Called at Load time (in-memory, not written)
// and at Reconcile time (written back to disk for a permanent upgrade).
func migrateFlatToNamespaced(mapping *yaml.Node) bool {
	changed := false
	for _, g := range flatKeyGroups {
		if migrateGroup(mapping, g.block, g.keys) {
			changed = true
		}
	}
	return changed
}

// migrateGroup moves any matching flat keys from mapping into the named block.
// It logs a deprecation warning for each flat key found, then folds the values
// into the block (creating the block if absent), and removes the flat keys.
// Returns true when mapping was modified.
func migrateGroup(mapping *yaml.Node, blockKey string, aliases []keyAlias) bool {
	type flatFound struct {
		alias keyAlias
		val   *yaml.Node
	}

	// First pass: collect flat keys that are present and need migration.
	var found []flatFound
	for _, alias := range aliases {
		val := findValue(mapping, alias.flat)
		if val == nil {
			continue
		}
		// If flat key name == block key name and value is already a mapping, the
		// file is already using the namespaced form — skip migration for this alias.
		if alias.flat == blockKey && val.Kind == yaml.MappingNode {
			continue
		}
		found = append(found, flatFound{alias, val})
	}
	if len(found) == 0 {
		return false
	}

	// Emit deprecation warnings.
	for _, f := range found {
		slog.Warn("config: deprecated flat key — please update to namespaced form",
			"key", f.alias.flat, "use_instead", blockKey+"."+f.alias.sub)
	}

	// Ensure the block mapping node exists in the root mapping.
	blockVal := findValue(mapping, blockKey)
	if blockVal == nil || blockVal.Kind != yaml.MappingNode {
		// blockVal might be a scalar if flat key == block key (e.g. notify: false).
		// We'll remove it below along with the other flat keys and add a fresh block.
		freshBlock := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		blockKeyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: blockKey}
		mapping.Content = append(mapping.Content, blockKeyNode, freshBlock)
		blockVal = freshBlock
	}

	// Fold each flat key's value into the block (if the sub-key is not already
	// present — the explicit namespaced value takes precedence over the flat alias).
	for _, f := range found {
		if findValue(blockVal, f.alias.sub) == nil {
			blockVal.Content = append(blockVal.Content, strNode(f.alias.sub), f.val)
		}
		// Remove the flat key from the root. Note: when flat key == block key,
		// removeKey removes the FIRST occurrence. Because we appended the new block
		// after the original flat entry, the first occurrence is the flat scalar,
		// which is what we want to remove.
		removeKey(mapping, f.alias.flat)
	}
	return true
}

// ---------------------------------------------------------------------------
// autopilot backend-ladder deprecation (backend registry §8)
// ---------------------------------------------------------------------------

// warnDeprecatedAutopilotBackends emits a deprecation warning when the file still
// carries the autopilot.brain.backends ladder or autopilot.brain.allow_pay_per_use
// gate. Both are superseded by the backend registry store (docs/specs/
// 2026-08-06-backend-registry.md §8), which is the source of truth for backend
// tiers and the paid-autopilot gate; the daemon imports these keys into the store
// once (backendstore.MigrateAutopilotLadder) and then reads the store live, so the
// config keys no longer drive selection. Unlike the flat→namespaced migration this
// does not rewrite the node (there is no in-config destination) — it only warns, so
// the one-time import can still read the values on the first post-upgrade boot.
// Callers may then delete the keys to silence the warning.
func warnDeprecatedAutopilotBackends(mapping *yaml.Node) {
	autopilotNode := nestedMapping(mapping, "autopilot")
	if autopilotNode != nil {
		if v := findValue(autopilotNode, "plans"); v != nil && v.Kind == yaml.SequenceNode && len(v.Content) > 0 {
			slog.Warn("config: deprecated key autopilot.plans — plans are migrated into each repository's plans/ directory and registered in the daemon store; run `warden autopilot register <plan>` for new plans, then remove this key",
				"key", "autopilot.plans")
		}
	}
	brain := nestedMapping(mapping, "autopilot", "brain")
	if brain == nil {
		return
	}
	if v := findValue(brain, "backends"); v != nil {
		slog.Warn("config: deprecated key autopilot.brain.backends — backend tiers now live in the registry store (edit via the backends API / TUI); this key is imported once then ignored",
			"key", "autopilot.brain.backends")
	}
	if v := findValue(brain, "allow_pay_per_use"); v != nil {
		slog.Warn("config: deprecated key autopilot.brain.allow_pay_per_use — the paid-autopilot gate now lives in the registry store; this key is imported once then ignored",
			"key", "autopilot.brain.allow_pay_per_use")
	}
}

// RemoveDeprecatedAutopilotBrainKeys removes the legacy backend ladder and paid
// gate after the daemon has successfully imported them into the registry store.
// Calling it before that import would lose compatibility data, so the daemon is
// intentionally the only production caller. The rewrite is idempotent.
func RemoveDeprecatedAutopilotBrainKeys(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return err
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil
	}
	brain := nestedMapping(doc.Content[0], "autopilot", "brain")
	if brain == nil {
		return nil
	}
	changed := findValue(brain, "backends") != nil || findValue(brain, "allow_pay_per_use") != nil
	if !changed {
		return nil
	}
	removeKey(brain, "backends")
	removeKey(brain, "allow_pay_per_use")
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), info.Mode().Perm())
}

// nestedMapping walks path from mapping, following each key to its mapping-node
// value, and returns the deepest mapping node (nil if any hop is missing or is not
// a mapping).
func nestedMapping(mapping *yaml.Node, path ...string) *yaml.Node {
	cur := mapping
	for _, key := range path {
		if cur == nil || cur.Kind != yaml.MappingNode {
			return nil
		}
		cur = findValue(cur, key)
	}
	if cur == nil || cur.Kind != yaml.MappingNode {
		return nil
	}
	return cur
}

// ---------------------------------------------------------------------------
// auto_approve migration (unchanged from original)
// ---------------------------------------------------------------------------

// WriteAutoApprove persists policy as the `auto_approve` block of the config
// file at path, preserving every other key, value, and comment. It is the
// durable half of the runtime PUT /auto-approve/policy endpoint: the daemon
// swaps the live poller policy and calls this so rule changes survive a restart.
// It generates the file from defaults first if it is missing, then replaces just
// the auto_approve value node (keeping that key's head-comment).
func WriteAutoApprove(path string, policy approval.Policy) error {
	if err := Reconcile(path); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("config: parse %s: %w", path, err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("config: unexpected shape in %s", path)
	}
	mapping := doc.Content[0]
	val, err := policyValueNodeFrom(policy)
	if err != nil {
		return err
	}
	if existing := findValue(mapping, "auto_approve"); existing != nil {
		// Swap only the value node so the key node keeps its position + head-comment.
		*existing = *val
	} else {
		key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "auto_approve", HeadComment: comment(autoApproveHint())}
		mapping.Content = append(mapping.Content, key, val)
	}
	out, err := marshalNode(&doc)
	if err != nil {
		return err
	}
	return writeFile(path, out)
}

// policyValueNodeFrom marshals an approval.Policy into a YAML value node (the
// mapping under `auto_approve`). Default marshaling matches the policy's yaml
// tags (enabled / allow_sticky / rules / agents).
func policyValueNodeFrom(policy approval.Policy) (*yaml.Node, error) {
	b, err := yaml.Marshal(policy)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("config: unexpected policy node shape")
	}
	return doc.Content[0], nil
}

// autoApproveHint returns the documentation hint for the auto_approve key from
// the schema (used only when the key is absent, e.g. a hand-trimmed file).
func autoApproveHint() string {
	for _, s := range schema {
		if s.Key == "auto_approve" {
			return s.Hint
		}
	}
	return ""
}

// migrateAutoApprove upgrades a legacy flat auto_approve key into the nested
// policy block in place. It handles two cases: (a) auto_approve is a scalar bool
// (the original on/off toggle), possibly alongside a flat
// auto_approve_allow_sticky key from Stage A; and (b) auto_approve is already a
// mapping but a stray auto_approve_allow_sticky key lingers. In both it folds the
// sticky flag into the block and drops the stray key, preserving the existing
// auto_approve key node and its head-comment (only the value node is swapped).
// Returns true when it modified mapping.Content.
func migrateAutoApprove(mapping *yaml.Node) bool {
	aaVal := findValue(mapping, "auto_approve")
	if aaVal == nil {
		return false
	}
	stickyVal := findValue(mapping, "auto_approve_allow_sticky")
	switch aaVal.Kind {
	case yaml.ScalarNode:
		enabled := scalarBool(aaVal)
		sticky := stickyVal != nil && scalarBool(stickyVal)
		// Swap only the value node in place; the key node (with its
		// head-comment) keeps its position in mapping.Content.
		*aaVal = *policyValueNode(enabled, sticky)
		removeKey(mapping, "auto_approve_allow_sticky")
		return true
	case yaml.MappingNode:
		if stickyVal == nil {
			return false // already migrated, nothing stray to fold in
		}
		if findValue(aaVal, "allow_sticky") == nil {
			aaVal.Content = append(aaVal.Content,
				strNode("allow_sticky"), boolNode(scalarBool(stickyVal)))
		}
		removeKey(mapping, "auto_approve_allow_sticky")
		return true
	default:
		return false
	}
}

// policyValueNode builds the nested auto_approve value: {enabled, allow_sticky,
// rules: {allow: [], deny: []}} with empty (but non-null) sequence nodes.
func policyValueNode(enabled, sticky bool) *yaml.Node {
	rules := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
		strNode("allow"), seqNode(),
		strNode("deny"), seqNode(),
	}}
	return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
		strNode("enabled"), boolNode(enabled),
		strNode("allow_sticky"), boolNode(sticky),
		strNode("rules"), rules,
	}}
}

// ---------------------------------------------------------------------------
// YAML node helpers
// ---------------------------------------------------------------------------

// findValue returns the value node for key in a mapping node, or nil if absent.
func findValue(mapping *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

// removeKey drops the first key/value pair matching key from a mapping node.
func removeKey(mapping *yaml.Node, key string) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
			return
		}
	}
}

// scalarBool decodes a scalar node as a bool (false on any decode error).
func scalarBool(n *yaml.Node) bool {
	var b bool
	_ = n.Decode(&b)
	return b
}

func strNode(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

func boolNode(b bool) *yaml.Node {
	v := "false"
	if b {
		v = "true"
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: v}
}

func seqNode() *yaml.Node {
	return &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
}

// ---------------------------------------------------------------------------
// File generation helpers
// ---------------------------------------------------------------------------

// renderFull builds a complete, commented config document from defaults().
func renderFull() ([]byte, error) {
	mapping, err := defaultsMapping()
	if err != nil {
		return nil, err
	}
	hints := map[string]string{}
	for _, s := range schema {
		hints[s.Key] = s.Hint
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		key := mapping.Content[i]
		if h, ok := hints[key.Value]; ok {
			key.HeadComment = comment(h)
		}
	}
	mapping.HeadComment = comment(fileHeader)
	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{mapping}}
	return marshalNode(doc)
}

// defaultsMapping marshals defaults() into a YAML mapping node (keys in struct
// order, values as properly-typed scalars). It is the value source for both
// full generation and add-missing reconcile.
func defaultsMapping() (*yaml.Node, error) {
	b, err := yaml.Marshal(defaults())
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("config: unexpected defaults node shape")
	}
	return doc.Content[0], nil
}

// defaultValueNodes returns a key→value-node map of every default, used to
// append missing keys during reconcile.
func defaultValueNodes() (map[string]*yaml.Node, error) {
	mapping, err := defaultsMapping()
	if err != nil {
		return nil, err
	}
	out := map[string]*yaml.Node{}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		out[mapping.Content[i].Value] = mapping.Content[i+1]
	}
	return out, nil
}

// comment turns hint text into a YAML head-comment, prefixing every line with
// "# " (yaml.v3 emits the stored string verbatim).
func comment(text string) string {
	return "# " + strings.ReplaceAll(text, "\n", "\n# ")
}

func marshalNode(n *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		_ = enc.Close()
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeFile(path string, data []byte) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, data, 0o644)
}

// legacyEnvNames are the config var basenames warden read from the environment
// before the move to a config file. They are now ignored; WarnIfLegacyEnv warns
// once at startup if any are still set.
var legacyEnvNames = []string{
	"ADDR", "DATA_DIR", "NOTIFY", "APPROVALS", "AUTO_APPROVE",
	"DEFAULT_PERMISSION_MODE", "SPAWN_GATE", "SPAWN_GATE_MAX_AGENTS",
	"METRICS", "ALLOW_NONLOOPBACK", "TOKEN_GUARD", "TOKEN_WARN_ALERT",
	"TOKEN_AUTO_COMPACT", "TOKEN_WARN", "TOKEN_CRITICAL",
	"PIPELINE_KEEP_DONE", "MODEL_DEFAULT", "NO_PIPELINE_HINT",
	"AUTO_RESTART_MAX", "AUTO_RESTART_RESET", "RATE_LIMIT_RETRY_INTERVAL",
	"RATE_LIMIT_BUFFER", "RATE_LIMIT_AUTO_RESUME",
}

// WarnIfLegacyEnv logs a warning for each WARDEN_*/AGENTCTL_* (or bare
// CLAUDE_PROJECTS_DIR) config env var that is still set. Configuration now comes
// only from the file at path; these vars are ignored. Per-agent IPC vars
// (WARDEN_SESSION_ID, WARDEN_PIPELINE_ID, WARDEN_JOB_ID) are not config and are
// unaffected.
func WarnIfLegacyEnv(path string) {
	var found []string
	for _, name := range legacyEnvNames {
		for _, prefix := range []string{"WARDEN_", "AGENTCTL_"} {
			if _, ok := os.LookupEnv(prefix + name); ok {
				found = append(found, prefix+name)
			}
		}
	}
	if _, ok := os.LookupEnv("CLAUDE_PROJECTS_DIR"); ok {
		found = append(found, "CLAUDE_PROJECTS_DIR")
	}
	for _, k := range found {
		slog.Warn("config: legacy env var is set but ignored — warden now reads configuration from a file (run `warden config`)", "var", k, "path", path)
	}
}

// IsLoopbackHost reports whether addr (host:port, or a bare host) binds only the
// loopback interface. An empty host (e.g. ":8765") binds all interfaces and is
// NOT loopback. Unresolvable hostnames are treated as non-loopback (fail safe).
// No DNS lookups.
func IsLoopbackHost(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr // no port present
	}
	host = strings.TrimSpace(host)
	switch host {
	case "":
		return false
	case "localhost":
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// ---------------------------------------------------------------------------
// Public accessor methods
// ---------------------------------------------------------------------------

// GetDefaultPermissionMode returns the configured default permission mode for agents.
func (c Config) GetDefaultPermissionMode() string { return c.DefaultPermissionMode }

// GetModelDefault returns the configured default model id/alias for new agents.
func (c Config) GetModelDefault() string { return c.ModelDefault }

// GetPipelineHint reports whether the pipeline-decomposition hint is appended
// to standalone agents.
func (c Config) GetPipelineHint() bool { return c.Pipeline.Hint }

// GetIsolationGuard reports whether the PreToolUse isolation-guard hook is
// installed into spawned agents (blocks edits that escape the agent's worktree).
func (c Config) GetIsolationGuard() bool { return c.Rails.IsolationGuard }

// GetCollabHint reports whether the conflict-check hint is appended to spawned
// agents so they coordinate on files other agents are editing.
func (c Config) GetCollabHint() bool { return c.Collab.Hint }

// GetMemoryInject reports whether the repo's curated .warden/memory.md is
// projected into spawned agents via the system-prompt seam (#53 PR-1). Default
// on; off (or an empty/absent memory file) makes a spawn byte-identical to no
// projection.
func (c Config) GetMemoryInject() bool { return c.Memory.Inject }

// GetMemoryCurate reports whether auto-curation of .warden/memory.md from
// completion digests is enabled (#53 PR-2). Default OFF (opt-in): this is the
// risky half — it proposes UNVERIFIED entries into the working tree only, gated by
// the committed diff, and never commits or pushes.
func (c Config) GetMemoryCurate() bool { return c.Memory.Curate }

// GetMemoryGround reports whether `wd repl` answers project questions locally from
// .warden/memory.md (#53 PR-3). Default on: it is the token-REMOVING lever (serves
// "where does X live?" from the local model, no cloud call), read-only, and degrades
// to the raw matching entries when no local model is configured.
func (c Config) GetMemoryGround() bool { return c.Memory.Ground }

// GetGitConventions reports whether the git-conventions hint (steer agents to
// wd commit/push/sync over raw git Bash) is appended to spawned agents.
func (c Config) GetGitConventions() bool { return c.Rails.GitConventions }

// GetGitRedirect reports whether the PreToolUse git-redirect hook is installed
// into spawned agents (denies raw git commit/push/pull/rebase in Bash and points
// the agent at the warden tools instead).
func (c Config) GetGitRedirect() bool { return c.Rails.GitRedirect }

// GetCheckRedirect reports whether the PreToolUse check-redirect hook is installed
// into spawned agents (denies a raw test/lint/build command the project's
// .warden/check.yml registers and points the agent at wd check instead). With no
// project config nothing is redirected, so this is effectively opt-in per repo.
func (c Config) GetCheckRedirect() bool { return c.Rails.CheckRedirect }

// GetRootGuard reports whether the PreToolUse root-guard hook is installed into
// spawned agents. It blocks any edit that targets the main repo working tree —
// the daemon-free backstop that catches even no-worktree (free-form / --in-repo)
// agents the isolation guard intentionally exempts.
func (c Config) GetRootGuard() bool { return c.Rails.RootGuard }

// GetSavings reports whether the token-savings ledger is enabled (the default).
// When off, lifecycle features record no savings and GET /savings returns 403.
func (c Config) GetSavings() bool { return c.Tokens.Savings }

// GetSavingsSamples reports whether the savings ledger retains opt-in provenance
// samples (truncated raw/kept output) for the wd savings --audit view. Off by
// default — the samples may hold sensitive substrings of real output.
func (c Config) GetSavingsSamples() bool { return c.Tokens.SavingsSamples }

// GetSnapshots reports whether the snapshot/checkpoint system is enabled (the
// daemon gates the wd snapshot create/list/restore endpoints on it).
func (c Config) GetSnapshots() bool { return c.Snapshots }

// GetTutorial reports whether the first-run tutorial hint is enabled (the CLI
// gates the one-line `wd tutorial` nudge on it).
func (c Config) GetTutorial() bool { return c.Tutorial }

// GetInsights reports whether the AI-powered insights engine is enabled (the
// `wd insights` CLI and the MCP insights tool gate on it).
func (c Config) GetInsights() bool { return c.Insights }

// GetApiDocs reports whether the public OpenAPI docs surface (/api/docs + the
// raw openapi.yaml) is served.
func (c Config) GetApiDocs() bool { return c.ApiDocs }

// GetSchedulerEnabled reports whether the native cron/at scheduler (#15) is
// enabled (the daemon gates the schedule endpoints and the reconcile loop on it).
func (c Config) GetSchedulerEnabled() bool { return c.SchedulerEnabled }

// GetLocalLLM reports whether warden routes its fuzzy-but-cheap tasks (task
// classification) to a local model instead of headless Claude.
func (c Config) GetLocalLLM() bool { return c.LocalLLM.Enabled }

// BackendsLimitRetryDuration returns how long the internal-thinking router skips
// a free CLI backend after a rate-limit / spend signal before retrying it
// (docs/specs/2026-08-06-backend-registry.md §7/§10). Defaults to 15m.
func (c Config) BackendsLimitRetryDuration() time.Duration {
	return durOr(c.Backends.LimitRetry, 15*time.Minute)
}

// LocalLLMTimeoutDuration returns the hard per-call timeout for the local model
// before warden falls back to Claude.
func (c Config) LocalLLMTimeoutDuration() time.Duration {
	return durOr(c.LocalLLM.Timeout, 20*time.Second)
}

// HTTPTimeoutFastDuration returns the daemon write budget for ordinary
// data/action routes (http.timeout_fast).
func (c Config) HTTPTimeoutFastDuration() time.Duration {
	return durOr(c.HTTP.TimeoutFast, 30*time.Second)
}

// HTTPTimeoutSlowDuration returns the daemon write budget for slow lifecycle
// routes — spawn, commit/push, checks, snapshots (http.timeout_slow).
func (c Config) HTTPTimeoutSlowDuration() time.Duration {
	return durOr(c.HTTP.TimeoutSlow, 10*time.Minute)
}

// GetLocalLLMEscalate reports whether the REPL may escalate an over-tier
// planning step to headless Claude (vs. degrading honestly).
func (c Config) GetLocalLLMEscalate() bool { return c.LocalLLM.Escalate }

// GetLocalLLMTier returns the explicit REPL planning-tier override
// ("auto"|"t0"|"t1"|"t2"); "auto" derives the tier from the model name.
func (c Config) GetLocalLLMTier() string { return c.LocalLLM.Tier }

// GetLocalLLMClassifier returns how the REPL buckets a request's needed
// planning tier: "heuristic" (cheap surface signals, no model call — the
// default) or "model" (a one-shot local-model classification that falls back to
// the heuristic on any error). Empty normalises to "heuristic".
func (c Config) GetLocalLLMClassifier() string {
	if strings.TrimSpace(c.LocalLLM.Classifier) == "" {
		return "heuristic"
	}
	return c.LocalLLM.Classifier
}

// GetRepl reports whether the cockpit master pane starts in REPL mode instead of
// a plain shell. The deprecated `repl:` and `orchestrator:` flat keys are
// transparently migrated to local_llm.repl on load.
func (c Config) GetRepl() bool { return c.LocalLLM.Repl }

// GetPluginsEnabled reports whether the plugin system (#47) is enabled (the
// daemon loads plugins.registry, registers custom task types, and wires the
// lifecycle-hook dispatcher only when this is true).
func (c Config) GetPluginsEnabled() bool { return c.Plugins.Enabled }

// GetPlugins returns the registered plugin specs (config key plugins.registry).
// They are only loaded when GetPluginsEnabled is true.
func (c Config) GetPlugins() []plugin.Spec { return c.Plugins.Registry }

// AutoRestartResetDuration returns the sustained-health window that resets the
// auto-restart counter.
func (c Config) AutoRestartResetDuration() time.Duration {
	return durOr(c.AutoRestart.Reset, 5*time.Minute)
}

// RateLimitRetryIntervalDuration returns the fallback wait before retrying a
// rate-limited agent.
func (c Config) RateLimitRetryIntervalDuration() time.Duration {
	return durOr(c.RateLimit.RetryInterval, 30*time.Minute)
}

// RateLimitSpendRetryIntervalDuration returns the (longer) fallback wait before
// retrying an agent parked on a monthly spend cap, which carries no reset time.
func (c Config) RateLimitSpendRetryIntervalDuration() time.Duration {
	return durOr(c.RateLimit.SpendRetryInterval, 6*time.Hour)
}

// CollabIntervalDuration returns the file-conflict scan interval.
func (c Config) CollabIntervalDuration() time.Duration {
	return durOr(c.Collab.Interval, 10*time.Second)
}

// BranchTrackIntervalDuration returns the branch-tracker scan interval.
func (c Config) BranchTrackIntervalDuration() time.Duration {
	return durOr(c.BranchTrack.Interval, 2*time.Minute)
}

// RateLimitBufferDuration returns the buffer added to a parsed rate-limit reset.
func (c Config) RateLimitBufferDuration() time.Duration {
	return durOr(c.RateLimit.Buffer, time.Minute)
}

func durOr(s string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	return def
}

// ---------------------------------------------------------------------------
// Autopilot accessors (docs/specs/autopilot.md)
// ---------------------------------------------------------------------------

// GetAutopilotEnabled reports the persisted master-switch value. The live toggle
// is driven through the daemon Controller (POST /autopilot); this is the on-disk
// default the daemon starts from.
func (c Config) GetAutopilotEnabled() bool { return c.Autopilot.Enabled }

// AutopilotPlanFiles returns the configured plan-file paths, skipping empties.
func (c Config) AutopilotPlanFiles() []string {
	out := make([]string, 0, len(c.Autopilot.Plans))
	for _, p := range c.Autopilot.Plans {
		if strings.TrimSpace(p.File) != "" {
			out = append(out, p.File)
		}
	}
	return out
}

// AutopilotIntegrationBranch returns the configured merge target (the only branch
// autopilot merges into).
func (c Config) AutopilotIntegrationBranch() string { return c.Autopilot.Merge.TargetBranch }

// AutopilotGate returns the configured gate mode (auto|ci|local).
func (c Config) AutopilotGate() string { return c.Autopilot.Merge.Gate }

// AutopilotMergeStrategy returns the configured land merge strategy
// (squash|merge|rebase). The autopilot package defaults an empty value to squash.
func (c Config) AutopilotMergeStrategy() string { return c.Autopilot.Merge.Strategy }

// AutopilotDeleteBranch reports whether `land` deletes the worker branch after
// merging it into the integration branch (autopilot.md §6).
func (c Config) AutopilotDeleteBranch() bool { return c.Autopilot.Merge.DeleteBranch }

// AutopilotBrainBackends returns the cost-tier backend ladder parsed from the
// deprecated autopilot.brain.backends config key (autopilot.md §7).
//
// Deprecated: the backend registry store is the source of truth for backend tiers
// (docs/specs/2026-08-06-backend-registry.md §8). The daemon derives the live
// ladder from the store (backendstore.Store.AutopilotLadder); this accessor exists
// only to feed the one-time store migration and emits a deprecation warning at load
// (see warnDeprecatedAutopilotBackends). Edit tiers in the store, not here.
func (c Config) AutopilotBrainBackends() AutopilotBackends { return c.Autopilot.Brain.Backends }

// AutopilotAllowPayPerUse reports the deprecated autopilot.brain.allow_pay_per_use
// gate (autopilot.md §7).
//
// Deprecated: superseded by the store's Settings.AllowPaidAutopilot (docs/specs/
// 2026-08-06-backend-registry.md §8). Retained only to seed the one-time migration;
// the live gate is read from the store.
func (c Config) AutopilotAllowPayPerUse() bool { return c.Autopilot.Brain.AllowPayPerUse }

// AutopilotGuardianInterval is the guardian tick cadence (autopilot.md §2.3).
func (c Config) AutopilotGuardianInterval() time.Duration {
	return durOr(c.Autopilot.Guardian.Interval, 60*time.Second)
}

// AutopilotGuardianHeartbeatTimeout is the brain-idle threshold that declares a
// wedge (autopilot.md §2.3). Generous by default (frictionless safeguards).
func (c Config) AutopilotGuardianHeartbeatTimeout() time.Duration {
	return durOr(c.Autopilot.Guardian.HeartbeatTimeout, 10*time.Minute)
}

// AutopilotGuardianBackoffMin is the first capped-exponential backoff step.
func (c Config) AutopilotGuardianBackoffMin() time.Duration {
	return durOr(c.Autopilot.Guardian.BackoffMin, 30*time.Second)
}

// AutopilotGuardianBackoffMax is the backoff ceiling the guardian never waits past
// (though it always keeps retrying — never parks).
func (c Config) AutopilotGuardianBackoffMax() time.Duration {
	return durOr(c.Autopilot.Guardian.BackoffMax, 6*time.Hour)
}

// AutopilotGuardianRotateAtContext is the brain context-window level at/above
// which a planned rotation fires (warning | critical).
func (c Config) AutopilotGuardianRotateAtContext() string {
	return c.Autopilot.Guardian.RotateAtContext
}

// AutopilotGuardianNotifyEach reports whether the guardian notifies the owner on
// every heal step, not just the stall/gate states.
func (c Config) AutopilotGuardianNotifyEach() bool {
	return c.Autopilot.Guardian.NotifyEachEscalation
}

// AutopilotBundle is the OR-bundle autopilot applies to its own agents when
// enabled (autopilot.md §10): auto_approve, rate_limit.auto_resume, and
// auto_restart are forced on for autopilot-owned agents unless already on. It is
// a PURE view of what enabling would flip — the S1 core computes it (and tests
// it) but nothing consumes it yet; the brain/guardian stages read it when they
// spawn autopilot-owned agents.
type AutopilotBundle struct {
	AutoApprove bool
	AutoResume  bool
	AutoRestart bool
}

// AutopilotBundle returns the effective supervision flags for autopilot-owned
// agents: each is the OR of its base config value and autopilot being enabled.
// When autopilot is off it returns the base values unchanged.
func (c Config) AutopilotBundle() AutopilotBundle {
	on := c.Autopilot.Enabled
	// auto_restart has no single master flag (it is per-agent opt-in with a cap);
	// enabling autopilot means autopilot-owned agents get it, which we model as
	// "on when autopilot is on" — the base has no global bool to OR against.
	return AutopilotBundle{
		AutoApprove: c.AutoApprove.Enabled || on,
		AutoResume:  c.RateLimit.AutoResume || on,
		AutoRestart: on,
	}
}

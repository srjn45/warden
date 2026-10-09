package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/config"
	"github.com/srjn45/warden/internal/knownprompts"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/preset"
	"github.com/srjn45/warden/internal/prompttemplate"
	"github.com/srjn45/warden/internal/role"
	"github.com/srjn45/warden/internal/store"
)

// applyAutoApproveAgent applies fn to the default policy (agent == "") or to the
// named per-agent override, creating the override (and the Agents map) on first
// use. Mirrors the CLI's applyToAgent.
func applyAutoApproveAgent(pol *approval.Policy, agent string, fn func(*approval.Policy)) {
	if agent == "" {
		fn(pol)
		return
	}
	if pol.Agents == nil {
		pol.Agents = map[string]approval.Policy{}
	}
	ov := pol.Agents[agent]
	fn(&ov)
	pol.Agents[agent] = ov
}

// parseSinceArg mirrors the CLI's parseSince (internal/cli/history.go): a window
// (24h, 7d, 2w), a Go duration, or a date (2006-01-02 / RFC3339). Empty = zero.
func parseSinceArg(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	now := time.Now()
	if n, ok := strings.CutSuffix(s, "d"); ok {
		if days, err := strconv.Atoi(n); err == nil {
			return now.Add(-time.Duration(days) * 24 * time.Hour), nil
		}
	}
	if n, ok := strings.CutSuffix(s, "w"); ok {
		if weeks, err := strconv.Atoi(n); err == nil {
			return now.Add(-time.Duration(weeks) * 7 * 24 * time.Hour), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(-d), nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("invalid since %q: want a window (24h, 7d, 2w) or a date (2006-01-02 / RFC3339)", s)
}

// --- argument structs for the parity tools ---

type forgetKnownArgs struct {
	ID  string `json:"id,omitempty" jsonschema:"the known-prompt id from list_known_prompts"`
	All bool   `json:"all,omitempty" jsonschema:"forget every learned prompt shape instead of one"`
}
type digestArgs struct {
	Ticket string `json:"ticket" jsonschema:"the agent's ticket / session id to summarize"`
}
type metricsArgs struct {
	History bool   `json:"history,omitempty" jsonschema:"return the historical samples instead of the live snapshot"`
	Since   string `json:"since,omitempty" jsonschema:"with history=true, lower-bound window (24h, 7d, 2w) or date"`
	Limit   int    `json:"limit,omitempty" jsonschema:"with history=true, cap the number of samples returned (0 = daemon default)"`
}
type savingsArgs struct {
	Since   string `json:"since,omitempty" jsonschema:"only count agents finished since this window (24h, 7d, 2w) or date; empty = all time"`
	Bucket  bool   `json:"bucket,omitempty" jsonschema:"include the per-day trend buckets"`
	Samples bool   `json:"samples,omitempty" jsonschema:"include opt-in provenance samples (per-agent token detail)"`
}
type searchArgs struct {
	Query  string `json:"query" jsonschema:"whitespace-separated terms (AND) matched across subject, prompt, type, name, pane, id, ticket, branch"`
	Closed bool   `json:"closed,omitempty" jsonschema:"also search the archived (closed) store"`
}
type historyArgs struct {
	Since string `json:"since,omitempty" jsonschema:"lower-bound window (24h, 7d, 2w) or date; empty = no bound"`
	Type  string `json:"type,omitempty" jsonschema:"filter by task type (development, analysis, …)"`
	Limit int    `json:"limit,omitempty" jsonschema:"cap the number of records (<=0 = no cap)"`
}
type auditLogArgs struct {
	Action string `json:"action,omitempty" jsonschema:"filter by action (spawn, terminate, delete, approve, pipeline_start, pipeline_cancel)"`
	Target string `json:"target,omitempty" jsonschema:"filter by target substring (agent or pipeline id)"`
	Since  string `json:"since,omitempty" jsonschema:"only records since this window (24h, 7d, 2w) or date"`
	Until  string `json:"until,omitempty" jsonschema:"only records up to this window or date"`
	Limit  int    `json:"limit,omitempty" jsonschema:"keep only the most recent N records (0 = all; default 50)"`
}
type listWorktreesArgs struct {
	Repo string `json:"repo,omitempty" jsonschema:"absolute path to the repo whose worktrees to list; defaults to the current directory"`
}
type pruneArgs struct {
	Repo            string `json:"repo,omitempty" jsonschema:"absolute path to the repo to prune; defaults to the current directory"`
	DryRun          bool   `json:"dry_run,omitempty" jsonschema:"report what would be removed without removing anything"`
	Force           bool   `json:"force,omitempty" jsonschema:"prune even dirty/unpushed worktrees"`
	IncludeArchived bool   `json:"include_archived,omitempty" jsonschema:"also reconcile worktrees of archived (closed) agents"`
}
type recoverArgs struct {
	Apply bool `json:"apply,omitempty" jsonschema:"false (default) only reports candidates; true re-inserts each one into the active store under its original id"`
}
type usageRecoverArgs struct {
	DryRun           bool   `json:"dry_run,omitempty" jsonschema:"when true, fetch fresh snapshots and calculate impact without starting recovery"`
	AiCli            string `json:"ai_cli,omitempty" jsonschema:"optional AI CLI / provider filter (e.g. claude); empty = all"`
	Project          string `json:"project,omitempty" jsonschema:"optional project path filter; empty = all projects"`
	MaxParallelSwaps int    `json:"max_parallel_swaps,omitempty" jsonschema:"temporary bounded concurrency override for this invocation; 0 = daemon default"`
}
type setAutoApproveArgs struct {
	Ticket  string `json:"ticket" jsonschema:"the agent's ticket / session id"`
	Enabled bool   `json:"enabled" jsonschema:"true to auto-answer this agent's recognized approval prompts, false to stop"`
}
type autoApprovePolicyArgs struct {
	Action  string   `json:"action" jsonschema:"what to do: show | allow | deny | clear | enable | disable"`
	Agent   string   `json:"agent,omitempty" jsonschema:"scope to a per-agent override (agent name or id); empty = the global default policy"`
	Tool    string   `json:"tool,omitempty" jsonschema:"allow/deny: exact tool name to match (e.g. Read, Bash)"`
	Pattern string   `json:"pattern,omitempty" jsonschema:"allow/deny: case-insensitive glob/substring over the action + question"`
	Regex   string   `json:"regex,omitempty" jsonschema:"allow/deny: Go regular expression over the action + question"`
	Paths   []string `json:"paths,omitempty" jsonschema:"allow/deny: path globs against the action target"`
}
type setPermissionModeArgs struct {
	Ticket string `json:"ticket" jsonschema:"the agent's ticket / session id"`
	Mode   string `json:"mode" jsonschema:"permission mode: acceptEdits|auto|bypassPermissions|default|dontAsk|plan"`
}
type setRoleArgs struct {
	Ticket string `json:"ticket" jsonschema:"the agent's ticket / session id"`
	Role   string `json:"role" jsonschema:"built-in role name: general|orchestrator|implementer|auto-merger|reviewer (general/empty clears the persona)"`
}
type setBackendTierArgs struct {
	ID   string `json:"id" jsonschema:"the backend id (e.g. claude, codex, aider)"`
	Tier string `json:"tier" jsonschema:"billing tier: free|subscription|pay_per_use|unclassified (the reserved local tier is system-set)"`
}
type setDefaultBackendArgs struct {
	ID string `json:"id" jsonschema:"the backend id to make the default; must be installed and enabled, and not the reserved local/terminal row"`
}
type setThinkingModeArgs struct {
	Mode string `json:"mode" jsonschema:"internal-thinking routing mode: local_only (keep on the $0 local model) | free_plus_local (prefer free cloud, fall back to local)"`
}
type setForceCompactArgs struct {
	Ticket string `json:"ticket" jsonschema:"the agent's ticket / session id"`
	State  string `json:"state" jsonschema:"force-compact override: on (always) | off (never) | inherit (follow the global token_force_compact)"`
}
type landArgs struct {
	AgentOrBranch string `json:"agent_or_branch" jsonschema:"the autopilot worker agent (id or name) or the branch to land into the integration branch"`
}
type brainConsultArgs struct {
	Intent       string   `json:"intent" jsonschema:"one-line summary, e.g. unblock stuck worker"`
	Situation    string   `json:"situation,omitempty" jsonschema:"free-form description of the current state"`
	Goal         string   `json:"goal,omitempty" jsonschema:"desired outcome"`
	AlreadyTried []string `json:"already_tried,omitempty" jsonschema:"actions already attempted"`
	Evidence     string   `json:"evidence,omitempty" jsonschema:"log excerpts, error messages, agent output snippets"`
	Allowed      []string `json:"allowed,omitempty" jsonschema:"optional subset of wait|nudge_agent|retry_job|mark_failed|skip_job|escalate|noop; default nudge_agent,wait,escalate,noop"`
	TaskID       string   `json:"task_id,omitempty" jsonschema:"optional task id for the audit trail"`
}
type exportArgs struct {
	All bool `json:"all,omitempty" jsonschema:"also include archived (closed) agent records"`
}
type importArgs struct {
	Data  string `json:"data" jsonschema:"a warden export envelope (the JSON produced by export_sessions)"`
	Merge bool   `json:"merge,omitempty" jsonschema:"overwrite colliding records instead of skipping them (default: skip by id)"`
}
type rotateAgentArgs struct {
	Ticket       string `json:"ticket" jsonschema:"the agent to retire; its successor inherits the same worktree (cwd) and permission mode"`
	ResumePrompt string `json:"resume_prompt" jsonschema:"the successor's initial task prompt"`
	ResumeFile   string `json:"resume_file,omitempty" jsonschema:"optional path to handoff notes the successor reads first (and deletes)"`
}
type handoffAgentArgs struct {
	To         string `json:"to,omitempty" jsonschema:"deliver into an existing agent's inbox instead of spawning a new delegate (mutually exclusive with retire)"`
	Repo       string `json:"repo,omitempty" jsonschema:"new-delegate mode: repo for the delegate; defaults to the current directory"`
	Type       string `json:"type,omitempty" jsonschema:"new-delegate mode: task type for the delegate"`
	Name       string `json:"name,omitempty" jsonschema:"new-delegate mode: optional human-readable name"`
	Branch     string `json:"branch,omitempty" jsonschema:"new-delegate mode: optional branch"`
	Prompt     string `json:"prompt" jsonschema:"the task being delegated (in retire mode, the successor's resume prompt)"`
	Context    string `json:"context,omitempty" jsonschema:"handoff context (goal, decisions, pointers) inlined into the delegate's prompt or the inbox message"`
	Force      bool   `json:"force,omitempty" jsonschema:"new-delegate mode: spawn past the memory-pressure gate"`
	Retire     bool   `json:"retire,omitempty" jsonschema:"retire mode: retire the ticket agent and hand its work to a fresh successor in the SAME worktree (same behavior as rotate_agent; mutually exclusive with to)"`
	Ticket     string `json:"ticket,omitempty" jsonschema:"retire mode: the agent to retire; its successor inherits the same worktree (cwd) and permission mode"`
	ResumeFile string `json:"resume_file,omitempty" jsonschema:"retire mode: optional path to handoff notes the successor reads first (and deletes)"`
}
type forkAgentArgs struct {
	Source         string `json:"source" jsonschema:"id of the agent whose recorded session to FORK; its backend session id must already be pinned (let it run a turn first)"`
	Prompt         string `json:"prompt,omitempty" jsonschema:"optional divergent first prompt for the fork; omit to just continue the source's conversation"`
	Type           string `json:"type,omitempty" jsonschema:"worktree-backed task type for the fork (default development)"`
	Name           string `json:"name,omitempty" jsonschema:"optional human-readable name for the fork"`
	Model          string `json:"model,omitempty" jsonschema:"optional model override (default: the source/backend default)"`
	PermissionMode string `json:"permission_mode,omitempty" jsonschema:"permission mode for the fork: acceptEdits|auto|bypassPermissions|default|dontAsk|plan (default: from config)"`
	Force          bool   `json:"force,omitempty" jsonschema:"fork even when the memory-pressure gate warns (default false)"`
}
type retryPipelineArgs struct {
	Pipeline string `json:"pipeline" jsonschema:"the pipeline id"`
	Job      string `json:"job" jsonschema:"the failed job id to retry"`
}
type editPipelineJobArgs struct {
	Pipeline string `json:"pipeline" jsonschema:"the pipeline id"`
	Job      string `json:"job" jsonschema:"the job id to edit"`
	Prompt   string `json:"prompt,omitempty" jsonschema:"new prompt for a pending job (omit to leave unchanged)"`
	Handoff  string `json:"handoff,omitempty" jsonschema:"new handoff/output for the job (omit to leave unchanged)"`
}
type emitPipelineArgs struct {
	Pipeline string `json:"pipeline" jsonschema:"the pipeline id"`
	Job      string `json:"job" jsonschema:"the job id whose handoff output to set"`
	Text     string `json:"text" jsonschema:"the output text to emit downstream"`
}
type validatePipelineArgs struct {
	Spec string `json:"spec" jsonschema:"the pipeline YAML spec to validate (same schema as create_pipeline); does not contact the daemon"`
}
type createScheduleArgs struct {
	Name   string `json:"name" jsonschema:"unique schedule name"`
	Cron   string `json:"cron,omitempty" jsonschema:"5-field cron spec (or @daily etc.) for a recurring run; mutually exclusive with at"`
	At     string `json:"at,omitempty" jsonschema:"single-shot time (RFC3339 or 2006-01-02T15:04; a time without a zone is the daemon host's local time); must be in the future; mutually exclusive with cron and now"`
	Now    bool   `json:"now,omitempty" jsonschema:"fire once as soon as possible (a single-shot due immediately); mutually exclusive with cron and at"`
	Repo   string `json:"repo,omitempty" jsonschema:"repo for an agent-spawn schedule (with the default worker role the agent runs in an isolated worktree off it)"`
	Cwd    string `json:"cwd,omitempty" jsonschema:"existing absolute directory a free-form agent launches in; required when there is no repo"`
	Role   string `json:"role,omitempty" jsonschema:"agent role (see list_roles); empty = worker when a repo is given, otherwise general"`
	Prompt string `json:"prompt,omitempty" jsonschema:"prompt for an agent-spawn schedule"`
	Agent  string `json:"agent,omitempty" jsonschema:"optional agent name for an agent-spawn schedule"`
	Branch string `json:"branch,omitempty" jsonschema:"optional branch for an agent-spawn schedule"`

	Model          string   `json:"model,omitempty" jsonschema:"model ID for ai_cli (requires ai_cli); empty lets the model-tier resolver pick"`
	AiCli          string   `json:"ai_cli,omitempty" jsonschema:"AI CLI the agent runs (claude, aider, opencode, codex, …); empty = the daemon default"`
	PermissionMode string   `json:"permission_mode,omitempty" jsonschema:"permission mode: acceptEdits|auto|bypassPermissions|default|dontAsk|plan; empty = the configured default"`
	AutoRestart    bool     `json:"auto_restart,omitempty" jsonschema:"auto-resume the agent if it crashes (capped)"`
	Tags           []string `json:"tags,omitempty" jsonschema:"labels stamped on every agent the schedule spawns"`
	Tier           string   `json:"tier,omitempty" jsonschema:"model tier for the quota-balanced resolver: tier-1|tier-2|tier-3"`
	ProjectID      string   `json:"project_id,omitempty" jsonschema:"project the agent joins; empty = the project owning its launch directory"`

	Spec string `json:"spec,omitempty" jsonschema:"a pipeline YAML spec to fire a whole pipeline on the schedule instead of a single agent; cannot be combined with any agent argument (prompt, repo, cwd, role, agent, branch, model, ai_cli, …)"`
}
type scheduleIDArgs struct {
	ID string `json:"id" jsonschema:"the schedule id"`
}

// updateScheduleArgs mirrors client.ScheduleUpdateRequest: only-present
// semantics — an omitted field is unchanged, an empty string clears an optional one.
type updateScheduleArgs struct {
	ID     string  `json:"id" jsonschema:"the schedule id"`
	Cron   *string `json:"cron,omitempty" jsonschema:"new recurring cadence (5-field cron or @daily etc.); mutually exclusive with at"`
	At     *string `json:"at,omitempty" jsonschema:"new single-shot time (RFC3339, or local time when no zone is given); mutually exclusive with cron"`
	Repo   *string `json:"repo,omitempty" jsonschema:"new repo (isolated worktree); empty string clears"`
	Cwd    *string `json:"cwd,omitempty" jsonschema:"new launch directory; empty string clears"`
	Role   *string `json:"role,omitempty" jsonschema:"new agent role; empty string clears"`
	Prompt *string `json:"prompt,omitempty" jsonschema:"new agent prompt; empty string clears"`
	Agent  *string `json:"agent,omitempty" jsonschema:"new agent name; empty string clears"`
	Branch *string `json:"branch,omitempty" jsonschema:"new branch; empty string clears"`
	Model  *string `json:"model,omitempty" jsonschema:"new model; empty string clears"`
	AiCli  *string `json:"ai_cli,omitempty" jsonschema:"new AI CLI backend; empty string clears"`
	Spec   *string `json:"spec,omitempty" jsonschema:"new pipeline YAML spec (pipeline schedules)"`
}

type listModelsArgs struct {
	Tier string `json:"tier,omitempty" jsonschema:"optional filter by model tier: tier-1 | tier-2 | tier-3"`
}
type setModelTierArgs struct {
	Backend string `json:"backend" jsonschema:"the backend id (e.g. claude, antigravity, codex)"`
	Model   string `json:"model" jsonschema:"the model id (e.g. sonnet)"`
	Tier    string `json:"tier" jsonschema:"the model tier: tier-1 | tier-2 | tier-3"`
}
type setRoleTierArgs struct {
	Role string `json:"role" jsonschema:"the agent role name (e.g. general, orchestrator, planner, worker)"`
	Tier string `json:"tier" jsonschema:"the default model tier: tier-1 | tier-2 | tier-3"`
}
type switchAgentArgs struct {
	Ticket  string `json:"ticket" jsonschema:"the agent's ticket / session id to switch"`
	AiCli   string `json:"ai_cli,omitempty" jsonschema:"explicit successor AI CLI id (claude, antigravity, codex, …). Canonical; preferred over deprecated backend"`
	Backend string `json:"backend,omitempty" jsonschema:"deprecated alias for ai_cli; accepted for one release. When both are set, ai_cli wins"`
	Model   string `json:"model,omitempty" jsonschema:"explicit successor model id"`
	Tier    string `json:"tier,omitempty" jsonschema:"resolve successor via quota-balanced router at this tier (tier-1 | tier-2 | tier-3)"`
	Role    string `json:"role,omitempty" jsonschema:"role to resolve tier from when tier is not given"`
	Reason  string `json:"reason,omitempty" jsonschema:"reason for switch: manual | context_fill | quota"`
	Prompt  string `json:"prompt,omitempty" jsonschema:"optional extra instruction appended to successor's continuation prompt"`
}
type setHandoverSettingsArgs struct {
	Enabled               *bool   `json:"enabled,omitempty" jsonschema:"enable or disable automated mid-session hot-swap on context fill"`
	ThresholdPercent      *int    `json:"threshold_percent,omitempty" jsonschema:"deprecated; ignored for provider quota switching"`
	RollingQuotaThreshold *int    `json:"rolling_quota_threshold,omitempty" jsonschema:"deprecated; ignored for provider quota switching"`
	ContextFillThreshold  *int    `json:"context_fill_threshold,omitempty" jsonschema:"context fill threshold percent (default 90)"`
	CooldownPeriod        *string `json:"cooldown_period,omitempty" jsonschema:"minimum cooldown between swaps, e.g. 15m, 1h"`
	CooldownMinutes       *int    `json:"cooldown_minutes,omitempty" jsonschema:"minimum cooldown between swaps in minutes"`
}

// registerExtraTools registers the parity tools that bring MCP coverage in line
// with the CLI: read/insight verbs, lifecycle controls, the rest of the pipeline
// and schedule verbs, and delegation. Each is a thin wrapper over an existing
// client method or a local helper the CLI already uses.
func (s *Server) registerExtraTools() {
	// --- read / insight ---

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "digest",
		Description: "Summarize one agent's recent activity into a compact digest: what it's working on, key transcript moments, git state, and whether it needs attention. Use to catch up on an agent without attaching. Returns the structured Digest.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a digestArgs) (*mcpsdk.CallToolResult, any, error) {
		d, err := s.cl.Digest(ctx, a.Ticket)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(d)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "get_metrics",
		Description: "Fleet resource metrics. Default: the live snapshot (CPU/memory/load, agent counts). With history=true: time-series samples (narrow with since/limit). Read-only.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a metricsArgs) (*mcpsdk.CallToolResult, any, error) {
		if a.History {
			samples, err := s.cl.GetMetricsHistory(ctx, a.Since, a.Limit)
			if err != nil {
				return textResult("error: " + err.Error()), nil, nil
			}
			return jsonResultAny(samples)
		}
		m, err := s.cl.GetMetrics(ctx)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(m)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "savings",
		Description: "The token-savings ledger: how much context/token spend warden's bounded-agent + pipeline model saved versus running the same work in one long-lived session. Optionally bucket by day and include opt-in provenance samples. Returns the savings Summary.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a savingsArgs) (*mcpsdk.CallToolResult, any, error) {
		since, err := parseSinceArg(a.Since)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		bucket := ""
		if a.Bucket {
			bucket = "day" // savings.GranularityDay; MCP keeps the simple day roll-up
		}
		sum, err := s.cl.Savings(ctx, since, bucket, a.Samples)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(sum)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "spend",
		Description: "Cost governance: the REAL billed Claude spend warden measured from agents' transcripts, priced per model into dollars and rolled up per-agent, per-repo, and per-day, plus the daily/weekly totals the budget gate enforces. The cost counterpart to the `savings` tool. Read-only; returns the spend Report.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ struct{}) (*mcpsdk.CallToolResult, any, error) {
		rep, err := s.cl.Spend(ctx)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(rep)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "search",
		Description: "Full-text search across agents (subject, prompt, type, name, pane, id, ticket, branch). AND of whitespace-separated terms. With closed=true also searches archived agents. Returns the matching sessions.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a searchArgs) (*mcpsdk.CallToolResult, any, error) {
		sessions, err := s.cl.Search(ctx, client.SearchParams{Query: a.Query, Closed: a.Closed})
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(sessions)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "history",
		Description: "Browse archived (closed) agents newest-first, narrowed by since/type/limit. Use to recall a finished agent's record. Returns the archived sessions.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a historyArgs) (*mcpsdk.CallToolResult, any, error) {
		since, err := parseSinceArg(a.Since)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		sessions, err := s.cl.History(ctx, client.HistoryParams{Since: since, Type: a.Type, Limit: a.Limit})
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(sessions)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "audit_log",
		Description: "Read the append-only action audit trail (~/.warden/audit.jsonl) — who did what, when, to which object — read directly from disk so it works even while the daemon is down. Filter by action/target/since/until; limit caps to the most recent N (default 50). Returns the audit events oldest-first.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a auditLogArgs) (*mcpsdk.CallToolResult, any, error) {
		since, err := parseSinceArg(a.Since)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		until, err := parseSinceArg(a.Until)
		if err != nil {
			return textResult("error: until: " + err.Error()), nil, nil
		}
		limit := a.Limit
		if limit == 0 {
			limit = 50
		}
		cfg := config.Load("")
		path := filepath.Join(cfg.DataDir, "audit.jsonl")
		events, err := audit.Read(path, audit.Filter{Action: a.Action, Target: a.Target, Since: since, Until: until, Limit: limit})
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(events)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "list_worktrees",
		Description: "List the git worktrees warden tracks under a repo's .worktrees, each with its branch and whether warden still has a record for it. Read-only — use prune_worktrees to reconcile orphans.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a listWorktreesArgs) (*mcpsdk.CallToolResult, any, error) {
		repo := a.Repo
		if repo == "" {
			repo = mcpDir("")
		}
		wts, err := s.cl.ListWorktrees(ctx, repo)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(wts)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "list_plugins",
		Description: "List registered warden plugins (#47) — their custom task types and subscribed lifecycle hook events — plus whether the plugin system is enabled. Read-only; reads the local config.",
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest, _ listArgs) (*mcpsdk.CallToolResult, any, error) {
		cfg := config.Load("")
		return jsonResultAny(map[string]any{"enabled": cfg.GetPluginsEnabled(), "plugins": cfg.GetPlugins()})
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "get_pressure",
		Description: "The memory-pressure gate's current verdict and headroom — the same signal the spawn gate consults before launching a new agent. Read-only.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ listArgs) (*mcpsdk.CallToolResult, any, error) {
		p, err := s.cl.Pressure(ctx)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(p)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "store_health",
		Description: "Agent-store integrity verdict (GET /api/v1/store/health): healthy/degraded, per-record failures, whether automated repair is available (repair_available is false until ScrivaDB ships Verify/Repair) and the safe next step. Read-only; running agents are unaffected by a degraded store. Repair is offline-only and is deliberately not an MCP action. Mirrors `warden doctor` and `warden repair agents`.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ listArgs) (*mcpsdk.CallToolResult, any, error) {
		h, err := s.cl.StoreHealth(ctx)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(h)
	})

	// --- lifecycle / control ---

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "set_auto_approve",
		Description: "Toggle auto-approval for one agent: when on, warden auto-answers that agent's recognized approval prompts with the default option. Use to let a trusted agent run unattended. Mirrors `warden auto-approve <id> on|off`.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a setAutoApproveArgs) (*mcpsdk.CallToolResult, any, error) {
		if err := s.cl.SetAutoApprove(ctx, a.Ticket, a.Enabled); err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		state := "off"
		if a.Enabled {
			state = "on"
		}
		return textResult("auto-approve " + state + " for " + a.Ticket), nil, nil
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "autopilot_status",
		Description: "Read one entry per autopilot run (run id, plan id, plan file, repo, state, gate, manager, workers, task rollup, backoff). Read-only. Mirrors `warden autopilot status`. Prefer list_plans / get_plan for plan lifecycle.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ listArgs) (*mcpsdk.CallToolResult, any, error) {
		st, err := s.cl.GetAutopilot(ctx)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(st)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "land",
		Description: "Land (merge) one autopilot worker branch into the integration branch — the brain's ONLY merge path. Runs every precondition (owning run active, branch autopilot-owned, a PR based on the integration branch, the resolved gate GREEN for the PR head, and the PR mergeable), merges with the configured strategy, deletes the worker branch if configured, and records the landing. Idempotent: re-issuing after a merge returns already_landed with no second merge. On a precondition failure returns the typed kind (gate_pending|gate_red|ci_missing|not_mergeable|not_found|not_owned|run_disabled|wrong_base) for you to reason over — never a human prompt. Autopilot-only. Mirrors `warden autopilot land`.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a landArgs) (*mcpsdk.CallToolResult, any, error) {
		res, err := s.cl.Land(ctx, a.AgentOrBranch)
		if err != nil {
			var le *client.AutopilotLandError
			if errors.As(err, &le) {
				return jsonResultAny(map[string]any{"landed": false, "kind": le.Kind, "detail": le.Detail})
			}
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(res)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "autopilot_complete",
		Description: "Declare YOUR autopilot run complete — call this ONLY after you (the brain) have verified the plan's done_when criteria are all satisfied. The daemon writes an in-place `status: complete` marker into your plan file (so the run is never executed again by mistake), tears you (the brain) down gracefully — in-flight workers keep running — and retains the run ledger. Idempotent: a second call is a no-op. The run is inferred from your own brain identity, so no arguments are needed. Autopilot brain-only.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ listArgs) (*mcpsdk.CallToolResult, any, error) {
		st, err := s.cl.CompleteAutopilot(ctx)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(st)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "brain_consult",
		Description: "Ask the shared short-lived brain resolver for a closed action recommendation (nudge_agent|wait|escalate|noop by default) — use this INSTEAD of spawn_agent with role=brain for unblock-worker / ad-hoc design decisions. The daemon spawns a role=brain agent, injects a structured situation package, waits for a single JSON reply, tears it down, and returns {action, reason, brain_id}. Shared audit + teardown with the pipeline stuck-recovery path. You (the manager) execute the returned action; the daemon does not mutate workers. Autopilot-manager only. Mirrors POST /api/v1/autopilot/brain-consult.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a brainConsultArgs) (*mcpsdk.CallToolResult, any, error) {
		res, err := s.cl.ConsultBrain(ctx, client.BrainConsultRequest{
			Intent:       a.Intent,
			Situation:    a.Situation,
			Goal:         a.Goal,
			AlreadyTried: a.AlreadyTried,
			Evidence:     a.Evidence,
			Allowed:      a.Allowed,
			TaskID:       a.TaskID,
		})
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(res)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "set_auto_approve_policy",
		Description: "Manage the auto-approve RULE policy (distinct from per-agent on/off via set_auto_approve). action=show returns the live policy; allow/deny appends a rule (by tool/pattern/regex/paths); clear drops rules; enable/disable flips the master switch. Use agent=<name|id> to scope to a per-agent override. With no rules an enabled policy approves every recognized, non-destructive prompt. Mirrors `warden auto-approve rules|allow|deny|clear|enable|disable`.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a autoApprovePolicyArgs) (*mcpsdk.CallToolResult, any, error) {
		pol, err := s.cl.GetAutoApprovePolicy(ctx)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		action := strings.ToLower(strings.TrimSpace(a.Action))
		if action == "" {
			action = "show"
		}
		if action == "show" {
			b, _ := json.MarshalIndent(pol, "", "  ")
			return textResult(string(b)), nil, nil
		}
		switch action {
		case "allow", "deny":
			rule := approval.Rule{Tool: a.Tool, Pattern: a.Pattern, Regex: a.Regex, Paths: a.Paths}
			if a.Tool == "" && a.Pattern == "" && a.Regex == "" && len(a.Paths) == 0 {
				return textResult("error: refusing an empty " + action + " rule (matches everything); set at least one of tool/pattern/regex/paths"), nil, nil
			}
			applyAutoApproveAgent(&pol, a.Agent, func(p *approval.Policy) {
				if action == "allow" {
					p.Rules.Allow = append(p.Rules.Allow, rule)
				} else {
					p.Rules.Deny = append(p.Rules.Deny, rule)
				}
			})
		case "clear":
			if a.Agent != "" {
				delete(pol.Agents, a.Agent)
			} else {
				pol.Rules = approval.Rules{}
			}
		case "enable", "disable":
			applyAutoApproveAgent(&pol, a.Agent, func(p *approval.Policy) { p.Enabled = action == "enable" })
		default:
			return textResult("error: unknown action " + a.Action + " (want show|allow|deny|clear|enable|disable)"), nil, nil
		}
		saved, err := s.cl.PutAutoApprovePolicy(ctx, pol)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		b, _ := json.MarshalIndent(saved, "", "  ")
		return textResult(string(b)), nil, nil
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "set_force_compact",
		Description: "Set one agent's force-compact override. When on, warden interrupts that agent (Escape) if it goes context-critical while still working, runs /compact once it is idle, then sends the resume prompt — destructive: the in-flight turn is discarded. state: on | off | inherit (follow the global token_force_compact). Mirrors `warden force-compact <id> on|off|inherit`.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a setForceCompactArgs) (*mcpsdk.CallToolResult, any, error) {
		state := a.State
		switch state {
		case "on", "off", "inherit":
		case "default", "clear":
			state = "inherit"
		default:
			return textResult("error: state must be on, off, or inherit"), nil, nil
		}
		if err := s.cl.SetForceCompact(ctx, a.Ticket, state); err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return textResult("force-compact " + state + " for " + a.Ticket), nil, nil
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "set_permission_mode",
		Description: "Change a running agent's Claude Code permission mode (acceptEdits|auto|bypassPermissions|default|dontAsk|plan). Mirrors `warden set-permission-mode <id> <mode>`.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a setPermissionModeArgs) (*mcpsdk.CallToolResult, any, error) {
		if err := s.cl.SetPermissionMode(ctx, a.Ticket, a.Mode); err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return textResult("permission mode for " + a.Ticket + " set to " + a.Mode), nil, nil
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "set_role",
		Description: "Switch a running agent's built-in role (general|orchestrator|implementer|auto-merger|reviewer). Persists the role and relaunches the agent so the new persona re-injects (its in-flight turn is discarded); general/empty clears the persona. Mirrors `warden set-role <id> <role>`.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a setRoleArgs) (*mcpsdk.CallToolResult, any, error) {
		r, ok := role.Get(strings.TrimSpace(a.Role))
		if !ok {
			return textResult("error: unknown role " + strconv.Quote(a.Role) + " (valid: " + strings.Join(role.Names(), ", ") + ")"), nil, nil
		}
		if err := s.cl.SetRole(ctx, a.Ticket, r.Name); err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return textResult("role for " + a.Ticket + " set to " + r.Name), nil, nil
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "list_roles",
		Description: "List warden's built-in agent roles (name + description) for a role picker — the same fixed catalog `spawn_agent`'s role param and `set_role` accept. Read-only; local.",
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest, _ listArgs) (*mcpsdk.CallToolResult, any, error) {
		roles := make([]map[string]string, 0)
		for _, r := range role.All() {
			roles = append(roles, map[string]string{"name": r.Name, "description": r.Description})
		}
		return jsonResultAny(map[string]any{"roles": roles})
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "list_known_prompts",
		Description: "List the known-prompts store: prompt shapes the Fast-Brain has learned (id, backend, templated question, option labels, affirmative option, sticky flags, hits, last_seen_at). Shapes only — never concrete commands or paths. Read-only; pair with forget_known_prompt to drop one that was learned wrong.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ listArgs) (*mcpsdk.CallToolResult, any, error) {
		entries, err := s.cl.KnownPrompts(ctx)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		if entries == nil {
			entries = []knownprompts.Entry{}
		}
		return jsonResultAny(map[string]any{"prompts": entries})
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "forget_known_prompt",
		Description: "Forget one learned prompt shape by id (see list_known_prompts), or every shape with all=true. The prompt is simply re-learned the next time it appears. Audit-logged.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a forgetKnownArgs) (*mcpsdk.CallToolResult, any, error) {
		id := strings.TrimSpace(a.ID)
		switch {
		case a.All && id != "":
			return textResult("error: pass either id or all=true, not both"), nil, nil
		case a.All:
			n, err := s.cl.ForgetAllKnownPrompts(ctx)
			if err != nil {
				return textResult("error: " + err.Error()), nil, nil
			}
			return textResult(fmt.Sprintf("forgot %d known prompt(s)", n)), nil, nil
		case id == "":
			return textResult("error: id is required (or all=true)"), nil, nil
		}
		if err := s.cl.ForgetKnownPrompt(ctx, id); err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return textResult("forgot " + id), nil, nil
	})

	// --- backend registry (docs/specs/2026-08-06-backend-registry.md) ---

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "list_backends",
		Description: "List the persisted agent-backend registry (one row per detected backend: id, installed, binary_path, tier, default, enabled, is_local, limited_until) plus store settings (internal_thinking_mode, allow_paid_autopilot). This is warden's source of truth for which backends exist and how they're tiered. Read-only; run rescan_backends first to refresh detection.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ listArgs) (*mcpsdk.CallToolResult, any, error) {
		state, err := s.cl.ListBackends(ctx)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(state)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "rescan_backends",
		Description: "Re-detect installed backends: sweep PATH for every registered backend and reconcile the detection fields (installed/binary_path/detected_at) into the registry, preserving each backend's tier/default/enabled. Returns the refreshed registry and settings.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ listArgs) (*mcpsdk.CallToolResult, any, error) {
		state, err := s.cl.RescanBackends(ctx)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(state)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "set_backend_tier",
		Description: "Assign a backend's billing tier: free | subscription | pay_per_use | unclassified. The reserved local tier is system-set and cannot be assigned, and the local row is not re-tierable. Returns the updated backend.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a setBackendTierArgs) (*mcpsdk.CallToolResult, any, error) {
		b, err := s.cl.SetBackendTier(ctx, a.ID, a.Tier)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(b)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "set_default_backend",
		Description: "Make a backend the single default (an empty spawn backend resolves to it). The target must be installed and enabled, and cannot be the reserved local/terminal row. Returns the updated registry and settings.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a setDefaultBackendArgs) (*mcpsdk.CallToolResult, any, error) {
		state, err := s.cl.SetDefaultBackend(ctx, a.ID)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(state)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "set_thinking_mode",
		Description: "Set how warden routes its own internal (non-user-facing) thinking: local_only keeps it on the $0 local model; free_plus_local prefers free cloud backends and falls back to the never-limited local model. Returns the updated settings.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a setThinkingModeArgs) (*mcpsdk.CallToolResult, any, error) {
		settings, err := s.cl.SetThinkingMode(ctx, a.Mode)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(settings)
	})

	// --- model routing & hot-swap (Stage 4) ---

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "list_models",
		Description: "List models in the catalog and their assigned tiers (tier-1, tier-2, tier-3). Optional tier filter.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a listModelsArgs) (*mcpsdk.CallToolResult, any, error) {
		models, err := s.cl.ListModels(ctx, a.Tier)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(models)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "set_model_tier",
		Description: "Assign a model's tier classification (tier-1: architecture/complex planning, tier-2: standard implementation, tier-3: fast/low-cost/CI triage).",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a setModelTierArgs) (*mcpsdk.CallToolResult, any, error) {
		m, err := s.cl.SetModelTier(ctx, a.Backend, a.Model, a.Tier)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(m)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "list_role_tiers",
		Description: "List agent roles and their default model tiers.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ listArgs) (*mcpsdk.CallToolResult, any, error) {
		mappings, err := s.cl.ListRoleTiers(ctx)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(mappings)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "set_role_tier",
		Description: "Set the default model tier for an agent role (tier-1 | tier-2 | tier-3).",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a setRoleTierArgs) (*mcpsdk.CallToolResult, any, error) {
		m, err := s.cl.SetRoleTier(ctx, a.Role, a.Tier)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(m)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "switch_agent",
		Description: "Hot-swap an agent session to a different AI CLI, model, or tier mid-task: retire active CLI and launch successor AI CLI in SAME worktree with extracted context handoff. Prefer ai_cli; deprecated backend alias is accepted (ai_cli wins if both are set).",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a switchAgentArgs) (*mcpsdk.CallToolResult, any, error) {
		aiCli := strings.TrimSpace(a.AiCli)
		if aiCli == "" {
			aiCli = strings.TrimSpace(a.Backend)
		}
		res, err := s.cl.SwitchSession(ctx, a.Ticket, client.SwitchSessionParams{
			AiCli:   aiCli,
			Backend: aiCli,
			Model:   a.Model,
			Tier:    a.Tier,
			Role:    a.Role,
			Reason:  a.Reason,
			Prompt:  a.Prompt,
		})
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(res)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "get_handover_settings",
		Description: "Get configuration for mid-session context handover and quota headroom triggers.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ listArgs) (*mcpsdk.CallToolResult, any, error) {
		settings, err := s.cl.GetHandoverSettings(ctx)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(settings)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "set_handover_settings",
		Description: "Update configuration for mid-session context handover and quota headroom triggers.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a setHandoverSettingsArgs) (*mcpsdk.CallToolResult, any, error) {
		current, err := s.cl.GetHandoverSettings(ctx)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		if a.Enabled != nil {
			current.Enabled = *a.Enabled
		}
		if a.ThresholdPercent != nil {
			current.ThresholdPercent = *a.ThresholdPercent
		}
		if a.RollingQuotaThreshold != nil {
			current.RollingQuotaThreshold = *a.RollingQuotaThreshold
		}
		if a.ContextFillThreshold != nil {
			current.ContextFillThreshold = *a.ContextFillThreshold
		}
		if a.CooldownPeriod != nil && strings.TrimSpace(*a.CooldownPeriod) != "" {
			d, err := time.ParseDuration(strings.TrimSpace(*a.CooldownPeriod))
			if err != nil {
				return textResult("error: invalid cooldown_period: " + err.Error()), nil, nil
			}
			current.CooldownPeriod = d
		} else if a.CooldownMinutes != nil {
			current.CooldownPeriod = time.Duration(*a.CooldownMinutes) * time.Minute
		}
		updated, err := s.cl.SetHandoverSettings(ctx, current)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(updated)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "prune_worktrees",
		Description: "Reconcile a repo's .worktrees against warden's records: remove orphaned worktrees whose agents are gone. dry_run reports without removing; dirty/unpushed worktrees are skipped unless force. Returns the per-worktree results.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a pruneArgs) (*mcpsdk.CallToolResult, any, error) {
		repo := a.Repo
		if repo == "" {
			repo = mcpDir("")
		}
		res, err := s.cl.Prune(ctx, client.PruneParams{Repo: repo, DryRun: a.DryRun, Force: a.Force, IncludeArchived: a.IncludeArchived})
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(res)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "recover_agents",
		Description: "Revive archived agent records whose tmux session is confirmed still alive — the safety net for the tombstone reaper, which should only ever archive a genuinely dead session but could previously be fooled by a stale orphaned status racing a daemon restart. apply=false (default) only reports candidates; apply=true re-inserts each one into the active store under its original id, reconnecting any children automatically (parent_id is untouched by archiving). Mirrors `warden recover`.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a recoverArgs) (*mcpsdk.CallToolResult, any, error) {
		res, err := s.cl.Recover(ctx, a.Apply)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(res)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name: "usage_recover",
		Description: "Operator-triggered one-shot usage reconciliation (mirrors `warden usage recover`). " +
			"Always fetches fresh supported provider usage snapshots, calculates bucket-to-agent impact, and — unless dry_run — " +
			"invokes the same backend recovery coordinator flow as the background usage poller. " +
			"Returns structured snapshots, impact (exhausted/affected/skipped/stale), and started/waiting (or would_*) outcomes with candidate decisions. " +
			"Skipped agents include unbound_legacy (backend/model-only records — never mass-swapped on a guessed account/bucket). " +
			"Optional ai_cli and project filters limit which agents may be affected; cached/stale data is never treated as forced exhaustion. " +
			"When rate_limit.recovery.usage_reconciliation.enabled is false, this remains the explicit operator path that may fetch usage.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a usageRecoverArgs) (*mcpsdk.CallToolResult, any, error) {
		if a.MaxParallelSwaps < 0 {
			return textResult("error: max_parallel_swaps must be >= 1 when set"), nil, nil
		}
		res, err := s.cl.UsageRecover(ctx, client.UsageRecoverParams{
			DryRun:           a.DryRun,
			AiCli:            a.AiCli,
			Project:          a.Project,
			MaxParallelSwaps: a.MaxParallelSwaps,
		})
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(res)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "export_sessions",
		Description: "Serialize agent session metadata to a JSON envelope for backup/migration (metadata only — worktrees/branches/tmux are NOT serialized). With all=true also includes archived agents. Pair with import_sessions.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a exportArgs) (*mcpsdk.CallToolResult, any, error) {
		sessions, err := s.cl.List(ctx)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		if a.All {
			closed, err := s.cl.History(ctx, client.HistoryParams{})
			if err != nil {
				return textResult("error: " + err.Error()), nil, nil
			}
			sessions = append(sessions, closed...)
		}
		if sessions == nil {
			sessions = []*store.Session{}
		}
		env := store.Export{Version: store.ExportVersion, ExportedAt: time.Now().UTC(), Sessions: sessions}
		return jsonResultAny(env)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "import_sessions",
		Description: "Insert agent session metadata from an export_sessions envelope. Idempotent by id (existing ids are skipped) unless merge=true overwrites them. Metadata only — worktrees/tmux are not recreated. Returns the ImportResult {inserted, skipped, …}.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a importArgs) (*mcpsdk.CallToolResult, any, error) {
		var env store.Export
		if err := json.Unmarshal([]byte(a.Data), &env); err != nil {
			return textResult("error: invalid export envelope: " + err.Error()), nil, nil
		}
		res, err := s.cl.Import(ctx, &env, a.Merge)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(res)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "rotate_agent",
		Description: "Retire an agent and hand its work to a fresh successor in the SAME worktree (cwd) and permission mode — useful when an agent's context is bloated/near-compaction. Spawns the successor first, then reaps the old agent (fail-safe: if the spawn fails the old agent is left running). With resume_file, the successor reads the handoff notes there first. Alias for `handoff_agent {retire: true}`; mirrors `warden rotate` / `warden handoff --retire`.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a rotateAgentArgs) (*mcpsdk.CallToolResult, any, error) {
		return s.rotateAgent(ctx, a.Ticket, a.ResumePrompt, a.ResumeFile)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "handoff_agent",
		Description: "Hand off work to another agent. Default: spawn a fresh delegate (in its own worktree) seeded with the task + inlined context; source keeps running. With to=<id>: deliver the handoff into an existing agent's inbox instead (wakes it if idle); source keeps running. With retire=true: retire the ticket agent and hand its work to a fresh successor in the SAME worktree (self-succession; subsumes rotate_agent). retire and to are mutually exclusive. Mirrors `warden handoff`.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a handoffAgentArgs) (*mcpsdk.CallToolResult, any, error) {
		if a.Retire && a.To != "" {
			return textResult("error: retire and to are mutually exclusive: retire reaps the ticket agent into a same-worktree successor, while to delegates to an existing agent and keeps it running"), nil, nil
		}
		// Retire mode routes through the same path as rotate_agent.
		if a.Retire {
			return s.rotateAgent(ctx, a.Ticket, a.Prompt, a.ResumeFile)
		}
		if strings.TrimSpace(a.Prompt) == "" {
			return textResult("error: prompt is required"), nil, nil
		}
		if a.To != "" {
			if _, err := s.cl.Get(ctx, a.To); err != nil {
				return textResult("error: handoff target " + a.To + ": " + err.Error()), nil, nil
			}
			// Mirrors composeHandoffMessage in internal/cli/handoff.go.
			body := fmt.Sprintf("🤝 Handoff from %s — a task is being delegated to you. Read the context, then take it on.\n\n"+
				"--- HANDOFF CONTEXT ---\n%s\n--- END HANDOFF CONTEXT ---\n\nThe ask:\n\n%s", ctxWriter(), a.Context, a.Prompt)
			_, woke, err := s.cl.MsgSend(ctx, a.To, ctxWriter(), body)
			if err != nil {
				return textResult("error: deliver handoff to " + a.To + ": " + err.Error()), nil, nil
			}
			return jsonResultAny(map[string]any{"delivered_to": a.To, "woke": woke})
		}
		repo := a.Repo
		if repo == "" {
			repo = mcpDir("")
		}
		// Mirrors composeDelegatePrompt in internal/cli/handoff.go.
		prompt := fmt.Sprintf("You are a fresh agent receiving a task delegated from another agent that continues its own work elsewhere. "+
			"The handoff context below has the goal, decisions already made, and pointers you need — read it first, then carry out the task.\n\n"+
			"--- HANDOFF CONTEXT ---\n%s\n--- END HANDOFF CONTEXT ---\n\nYour task:\n\n%s", a.Context, a.Prompt)
		delegate, err := s.cl.Spawn(ctx, client.SpawnParams{Type: a.Type, Repo: repo, Name: a.Name, Branch: a.Branch, Prompt: prompt, Force: a.Force})
		if err != nil {
			return textResult("error: spawn delegate: " + err.Error()), nil, nil
		}
		return jsonResultAny(map[string]any{"delegate": delegate.ID, "workdir": delegate.Workdir})
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "fork_agent",
		Description: "Fork an existing agent's recorded session into a NEW managed agent: branches the source's conversation/reasoning into a divergent session (codex `codex fork`) and continues it as its own agent — a fresh sibling worktree off the source's branch, seeded with the source's uncommitted tracked changes (dirty-tree carry; untracked/.gitignore'd artifacts are not carried), with its own tmux session warden manages and tears down. The source agent keeps running, untouched (fork branches sideways — unlike snapshot's rewind or rotate/handoff which drop the conversation). Only backends with a native session fork are forkable (codex); forking one without (claude) reports a clean cannot-fork. The source's backend session id must already be pinned — if it has not run a turn yet, retry after it has. The fork inherits the source's repo+backend. Thin wrapper over spawn_agent with fork_from set (no new endpoint). Mirrors `warden fork`.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a forkAgentArgs) (*mcpsdk.CallToolResult, any, error) {
		if strings.TrimSpace(a.Source) == "" {
			return textResult("error: source agent id is required"), nil, nil
		}
		typ := a.Type
		if typ == "" {
			typ = "development" // a fork needs a worktree-backed type (§7)
		}
		sess, err := s.cl.Spawn(ctx, client.SpawnParams{
			Type: typ, ForkFrom: a.Source, Prompt: a.Prompt, Name: a.Name, Model: a.Model,
			PermissionMode: a.PermissionMode, Force: a.Force, ParentID: sessionID(),
		})
		if err != nil {
			var cre *client.ErrConfirmationRequired
			if errors.As(err, &cre) {
				return textResult("memory-pressure gate: " + cre.Verdict.Reason +
					"\nRe-call fork_agent with force=true to fork anyway."), nil, nil
			}
			return textResult("error: " + err.Error()), nil, nil
		}
		res, err := jsonResult(sess)
		return res, nil, err
	})

	// --- pipeline verbs (beyond create/list/show/start/cancel) ---

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "pause_pipeline",
		Description: "Pause a running pipeline: no new jobs are spawned, in-flight jobs finish. Resume later with resume_pipeline. Mirrors `warden pipeline pause`.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a pipelineIDArgs) (*mcpsdk.CallToolResult, any, error) {
		if err := s.cl.PipelinePause(ctx, a.Pipeline); err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return textResult("paused pipeline " + a.Pipeline), nil, nil
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "resume_pipeline",
		Description: "Resume a paused pipeline: the scheduler starts spawning ready jobs again. Mirrors `warden pipeline resume`.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a pipelineIDArgs) (*mcpsdk.CallToolResult, any, error) {
		if err := s.cl.PipelineResume(ctx, a.Pipeline); err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return textResult("resumed pipeline " + a.Pipeline), nil, nil
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "retry_pipeline_job",
		Description: "Re-run a failed pipeline job (and unblock its dependents) without recreating the whole pipeline. Mirrors `warden pipeline job retry`.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a retryPipelineArgs) (*mcpsdk.CallToolResult, any, error) {
		if err := s.cl.PipelineRetry(ctx, a.Pipeline, a.Job); err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return textResult("retrying job " + a.Job + " in pipeline " + a.Pipeline), nil, nil
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "edit_pipeline_job",
		Description: "Edit a pending pipeline job's prompt and/or handoff output before it runs. Omit a field to leave it unchanged. Mirrors `warden pipeline job edit`.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a editPipelineJobArgs) (*mcpsdk.CallToolResult, any, error) {
		var prompt, handoff *string
		if a.Prompt != "" {
			prompt = &a.Prompt
		}
		if a.Handoff != "" {
			handoff = &a.Handoff
		}
		if err := s.cl.PipelineEditJob(ctx, a.Pipeline, a.Job, prompt, handoff); err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return textResult("edited job " + a.Job + " in pipeline " + a.Pipeline), nil, nil
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "emit_pipeline_output",
		Description: "Manually set a pipeline job's handoff output (the text passed downstream to dependents). Use to seed or correct a job's emitted result. Mirrors `warden pipeline emit`.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a emitPipelineArgs) (*mcpsdk.CallToolResult, any, error) {
		if err := s.cl.PipelineEmit(ctx, a.Pipeline, a.Job, a.Text); err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return textResult("emitted output for job " + a.Job + " in pipeline " + a.Pipeline), nil, nil
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "delete_pipeline",
		Description: "Delete a pipeline record (and its job bookkeeping). Use after a pipeline is finished/cancelled to clean up. Branches and worktrees are kept. Mirrors `warden pipeline delete`; cancel_pipeline stops a running one without deleting it — cancel terminates live job agents and cannot be undone. The calling agent is responsible for confirming before calling.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a pipelineIDArgs) (*mcpsdk.CallToolResult, any, error) {
		if err := s.cl.PipelineDelete(ctx, a.Pipeline); err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return textResult("deleted pipeline " + a.Pipeline), nil, nil
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "validate_pipeline",
		Description: "Validate a pipeline YAML spec locally without creating it — checks required fields, job ids, dependency references, worktree/run_if values, and DAG cycles. Does not contact the daemon. Returns {valid, id, jobs} or the validation error.",
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest, a validatePipelineArgs) (*mcpsdk.CallToolResult, any, error) {
		p, err := pipeline.ParseSpec([]byte(a.Spec))
		if err != nil {
			return textResult("invalid pipeline: " + err.Error()), nil, nil
		}
		return jsonResultAny(map[string]any{"valid": true, "id": p.ID, "jobs": len(p.Jobs)})
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "list_pipeline_templates",
		Description: "List the built-in pipeline templates and their placeholders (e.g. analyze→implement→review). Use one as a starting point for create_pipeline. Read-only; local.",
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest, _ listArgs) (*mcpsdk.CallToolResult, any, error) {
		return jsonResultAny(pipeline.ListTemplates())
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "library_list",
		Description: "Browse all three reusable launch-config libraries in one call: saved spawn presets (named `warden start` defaults), saved prompt templates (variabled prompt bodies), and the built-in pipeline templates. Returns {presets, prompt_templates, templates}. Reuses the same sources as the preset store, the prompt-template store, and list_pipeline_templates. Read-only; local. Mirrors `warden library list`.",
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest, _ listArgs) (*mcpsdk.CallToolResult, any, error) {
		st, err := preset.Load(preset.DefaultPath())
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		presets := make([]map[string]any, 0, len(st.Names()))
		for _, n := range st.Names() {
			p, _ := st.Get(n)
			presets = append(presets, map[string]any{"name": n, "preset": p})
		}
		pt, err := prompttemplate.Load(prompttemplate.DefaultPath())
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		promptTemplates := make([]map[string]any, 0, len(pt.Names()))
		for _, n := range pt.Names() {
			t, _ := pt.Get(n)
			promptTemplates = append(promptTemplates, map[string]any{"name": n, "prompt": t.Prompt, "vars": t.Vars})
		}
		return jsonResultAny(map[string]any{
			"presets":          presets,
			"prompt_templates": promptTemplates,
			"templates":        pipeline.ListTemplates(),
		})
	})

	// --- schedule write verbs (list_schedules already exists, read-only) ---

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "create_schedule",
		Description: "Create a daemon-side schedule that fires an agent spawn or a whole pipeline on its own timer. Use cron for recurring (5-field or @daily etc., evaluated in the daemon host's local time), at for a single-shot in the future (RFC3339, or local time when no zone is given; a past time is rejected), or now to fire once immediately. Provide prompt plus repo (isolated worktree) or cwd (launch directory), and optionally role, model/ai_cli, permission_mode, auto_restart, tags, tier, for an agent, or spec for a pipeline (a spec cannot be combined with agent arguments). An agent schedule starts the agent like `warden start` would. Mirrors `warden schedule create`.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a createScheduleArgs) (*mcpsdk.CallToolResult, any, error) {
		sch, err := s.cl.ScheduleCreate(ctx, client.ScheduleCreateRequest{
			Name: a.Name, Cron: a.Cron, At: a.At, Now: a.Now, Repo: a.Repo, Cwd: a.Cwd, Role: a.Role,
			Prompt: a.Prompt, Agent: a.Agent, Branch: a.Branch, Spec: a.Spec,
			Model: a.Model, AiCli: a.AiCli, PermissionMode: a.PermissionMode, AutoRestart: a.AutoRestart,
			Tags: a.Tags, Tier: a.Tier, ProjectID: a.ProjectID,
		})
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(sch)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "delete_schedule",
		Description: "Delete a schedule by id so it stops firing. Only the schedule record is removed: agents and pipelines it already started are not affected. Immediate and without confirmation; use disable_schedule to pause instead. Mirrors `warden schedule delete`.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a scheduleIDArgs) (*mcpsdk.CallToolResult, any, error) {
		if err := s.cl.ScheduleDelete(ctx, a.ID); err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return textResult("deleted schedule " + a.ID), nil, nil
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "get_schedule",
		Description: "Get one schedule by id, including its cadence, fire payload, enabled state, next/last run, and durable last-run outcome. Mirrors `warden schedule show`. (run_schedule test-fires and update_schedule edits a schedule.)",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a scheduleIDArgs) (*mcpsdk.CallToolResult, any, error) {
		sch, err := s.cl.ScheduleGet(ctx, a.ID)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(sch)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "run_schedule",
		Description: "Fire a schedule immediately as a test run, without changing its next run or enabled state. Returns the schedule and the run id. Mirrors `warden schedule run`.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a scheduleIDArgs) (*mcpsdk.CallToolResult, any, error) {
		sch, runID, err := s.cl.ScheduleRun(ctx, a.ID)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(map[string]any{"schedule": sch, "run_id": runID})
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "update_schedule",
		Description: "Edit an existing schedule. Only the fields you pass change; omitted fields are left as they are and an empty string clears an optional field. Provide at most one of cron or at. Mirrors `warden schedule edit`.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a updateScheduleArgs) (*mcpsdk.CallToolResult, any, error) {
		if a.Cron != nil && a.At != nil {
			return textResult("error: provide exactly one of cron or at, not both"), nil, nil
		}
		req := client.ScheduleUpdateRequest{
			Cron: a.Cron, At: a.At, Repo: a.Repo, Cwd: a.Cwd, Role: a.Role, Prompt: a.Prompt,
			Agent: a.Agent, Branch: a.Branch, Model: a.Model, AiCli: a.AiCli, Spec: a.Spec,
		}
		if req == (client.ScheduleUpdateRequest{}) {
			return textResult("error: nothing to change: pass at least one field to edit"), nil, nil
		}
		sch, err := s.cl.ScheduleUpdate(ctx, a.ID, req)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(sch)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "enable_schedule",
		Description: "Enable a schedule so it fires again, re-arming next_run from now. Idempotent. Mirrors `warden schedule enable`.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a scheduleIDArgs) (*mcpsdk.CallToolResult, any, error) {
		sch, err := s.cl.ScheduleEnable(ctx, a.ID)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(sch)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "disable_schedule",
		Description: "Disable a schedule so it stops firing (record and last-run history preserved). Idempotent; enable_schedule turns it back on. Mirrors `warden schedule disable`.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a scheduleIDArgs) (*mcpsdk.CallToolResult, any, error) {
		sch, err := s.cl.ScheduleDisable(ctx, a.ID)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(sch)
	})
}

// rotateAgent is the shared self-succession path behind both rotate_agent and
// handoff_agent{retire:true}: retire the ticket agent and hand its work to a
// fresh successor in the SAME worktree (cwd) and permission mode. Spawn-before-
// reap is fail-safe — if the spawn fails the old agent is left running. Mirrors
// the CLI's runRotate / composeSuccessorPrompt in internal/cli/rotate.go.
func (s *Server) rotateAgent(ctx context.Context, ticket, resumePrompt, resumeFile string) (*mcpsdk.CallToolResult, any, error) {
	if strings.TrimSpace(resumePrompt) == "" {
		return textResult("error: resume prompt is required"), nil, nil
	}
	old, err := s.cl.Get(ctx, ticket)
	if err != nil {
		return textResult("error: look up " + ticket + ": " + err.Error()), nil, nil
	}
	prompt := resumePrompt
	if resumeFile != "" {
		// Mirrors composeSuccessorPrompt in internal/cli/rotate.go.
		prompt = fmt.Sprintf("You are resuming work handed off from a previous agent that is being retired. "+
			"First read the handoff notes at %s for full context, decisions already made, and next steps. "+
			"Once you have read and internalized them, delete that handoff file. Then continue the work:\n\n%s", resumeFile, resumePrompt)
	}
	successor, err := s.cl.Spawn(ctx, client.SpawnParams{Prompt: prompt, Cwd: old.Workdir, PermissionMode: old.PermissionMode})
	if err != nil {
		return textResult("error: spawn successor (old agent left running): " + err.Error()), nil, nil
	}
	if err := s.cl.Terminate(ctx, ticket); err != nil {
		return jsonResultAny(map[string]any{"successor": successor.ID, "workdir": successor.Workdir, "retired": ticket, "warning": "successor spawned but reaping old agent failed: " + err.Error()})
	}
	return jsonResultAny(map[string]any{"successor": successor.ID, "workdir": successor.Workdir, "retired": ticket})
}

// jsonResultAny is jsonResult adapted to the (result, any, error) tool return
// signature, so handlers can `return jsonResultAny(v)` in one line.
func jsonResultAny(v any) (*mcpsdk.CallToolResult, any, error) {
	r, err := jsonResult(v)
	return r, nil, err
}

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
type setForceCompactArgs struct {
	Ticket string `json:"ticket" jsonschema:"the agent's ticket / session id"`
	State  string `json:"state" jsonschema:"force-compact override: on (always) | off (never) | inherit (follow the global token_force_compact)"`
}
type setAutopilotArgs struct {
	Enabled bool   `json:"enabled" jsonschema:"true enables autopilot (runs the enable-time preflight), false is the kill switch"`
	Repo    string `json:"repo,omitempty" jsonschema:"repo root to toggle (optional; defaults to the daemon's working directory) — the switch is per-repo"`
}
type landArgs struct {
	AgentOrBranch string `json:"agent_or_branch" jsonschema:"the autopilot worker agent (id or name) or the branch to land into the integration branch"`
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
	At     string `json:"at,omitempty" jsonschema:"single-shot time (RFC3339 or 2006-01-02T15:04, local); mutually exclusive with cron"`
	Type   string `json:"type,omitempty" jsonschema:"agent task type (for an agent-spawn schedule)"`
	Repo   string `json:"repo,omitempty" jsonschema:"repo for an agent-spawn schedule"`
	Prompt string `json:"prompt,omitempty" jsonschema:"prompt for an agent-spawn schedule"`
	Agent  string `json:"agent,omitempty" jsonschema:"optional agent name for an agent-spawn schedule"`
	Branch string `json:"branch,omitempty" jsonschema:"optional branch for an agent-spawn schedule"`
	Spec   string `json:"spec,omitempty" jsonschema:"a pipeline YAML spec to fire a whole pipeline on the schedule instead of a single agent"`
}
type scheduleIDArgs struct {
	ID string `json:"id" jsonschema:"the schedule id to delete"`
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
		Name:        "set_autopilot",
		Description: "Flip the per-repo autopilot switch. `repo` scopes the toggle to one repository (optional; defaults to the daemon's working directory). enabled=true runs the enable-time preflight (plan file valid, gh authenticated, integration branch present, at most one active run per repo) for that repo and returns the resulting status, or the FULL list of preflight failures to fix. enabled=false is the kill switch for that repo (stops spawning/landing; in-flight workers keep running). Mirrors `warden autopilot on|off`.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a setAutopilotArgs) (*mcpsdk.CallToolResult, any, error) {
		st, err := s.cl.SetAutopilot(ctx, a.Enabled, a.Repo)
		if err != nil {
			var pfe *client.AutopilotPreflightError
			if errors.As(err, &pfe) {
				return jsonResultAny(map[string]any{"enabled": false, "preflight_failed": true, "failures": pfe.Failures})
			}
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(st)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "autopilot_status",
		Description: "Read the autopilot status: the master switch plus one entry per active run (run id, plan file, repo, state, resolved gate, brain, workers, task rollup, backoff). Read-only. Mirrors `warden autopilot status`.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ listArgs) (*mcpsdk.CallToolResult, any, error) {
		st, err := s.cl.GetAutopilot(ctx)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(st)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "land",
		Description: "Land (merge) one autopilot worker branch into the integration branch — the brain's ONLY merge path. Runs every precondition (owning run active, branch autopilot-owned, a PR based on the integration branch, the resolved gate GREEN for the PR head, and the PR mergeable), merges with the configured strategy, deletes the worker branch if configured, and records the landing. Idempotent: re-issuing after a merge returns already_landed with no second merge. On a precondition failure returns the typed kind (gate_pending|gate_red|ci_missing|not_mergeable|not_owned|run_disabled|wrong_base) for you to reason over — never a human prompt. Autopilot-only. Mirrors `warden land`.",
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
		Description: "Re-run a failed pipeline job (and unblock its dependents) without recreating the whole pipeline. Mirrors `warden pipeline retry`.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a retryPipelineArgs) (*mcpsdk.CallToolResult, any, error) {
		if err := s.cl.PipelineRetry(ctx, a.Pipeline, a.Job); err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return textResult("retrying job " + a.Job + " in pipeline " + a.Pipeline), nil, nil
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "edit_pipeline_job",
		Description: "Edit a pending pipeline job's prompt and/or handoff output before it runs. Omit a field to leave it unchanged. Mirrors `warden pipeline edit-job`.",
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
		Description: "Delete a pipeline record (and its job bookkeeping). Use after a pipeline is finished/cancelled to clean up. Mirrors `warden pipeline delete`; cancel_pipeline stops a running one without deleting it.",
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
		Description: "Create a daemon-side schedule that fires an agent spawn or a whole pipeline on its own timer. Use cron for recurring (5-field or @daily etc.) or at for single-shot (RFC3339/local). Provide type+repo+prompt for an agent, or spec for a pipeline. Mirrors `warden schedule create`.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a createScheduleArgs) (*mcpsdk.CallToolResult, any, error) {
		sch, err := s.cl.ScheduleCreate(ctx, client.ScheduleCreateRequest{
			Name: a.Name, Cron: a.Cron, At: a.At, Type: a.Type, Repo: a.Repo,
			Prompt: a.Prompt, Agent: a.Agent, Branch: a.Branch, Spec: a.Spec,
		})
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(sch)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "delete_schedule",
		Description: "Delete a schedule by id so it stops firing. Mirrors `warden schedule delete`.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a scheduleIDArgs) (*mcpsdk.CallToolResult, any, error) {
		if err := s.cl.ScheduleDelete(ctx, a.ID); err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return textResult("deleted schedule " + a.ID), nil, nil
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

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/config"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/store"
)

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
type setAutoApproveArgs struct {
	Ticket  string `json:"ticket" jsonschema:"the agent's ticket / session id"`
	Enabled bool   `json:"enabled" jsonschema:"true to auto-answer this agent's recognized approval prompts, false to stop"`
}
type setPermissionModeArgs struct {
	Ticket string `json:"ticket" jsonschema:"the agent's ticket / session id"`
	Mode   string `json:"mode" jsonschema:"permission mode: acceptEdits|auto|bypassPermissions|default|dontAsk|plan"`
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
	To      string `json:"to,omitempty" jsonschema:"deliver into an existing agent's inbox instead of spawning a new delegate"`
	Repo    string `json:"repo,omitempty" jsonschema:"new-delegate mode: repo for the delegate; defaults to the current directory"`
	Type    string `json:"type,omitempty" jsonschema:"new-delegate mode: task type for the delegate"`
	Name    string `json:"name,omitempty" jsonschema:"new-delegate mode: optional human-readable name"`
	Branch  string `json:"branch,omitempty" jsonschema:"new-delegate mode: optional branch"`
	Prompt  string `json:"prompt" jsonschema:"the task being delegated"`
	Context string `json:"context,omitempty" jsonschema:"handoff context (goal, decisions, pointers) inlined into the delegate's prompt or the inbox message"`
	Force   bool   `json:"force,omitempty" jsonschema:"new-delegate mode: spawn past the memory-pressure gate"`
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
		sum, err := s.cl.Savings(ctx, since, a.Bucket, a.Samples)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(sum)
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
		Name:        "set_permission_mode",
		Description: "Change a running agent's Claude Code permission mode (acceptEdits|auto|bypassPermissions|default|dontAsk|plan). Mirrors `warden set-permission-mode <id> <mode>`.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a setPermissionModeArgs) (*mcpsdk.CallToolResult, any, error) {
		if err := s.cl.SetPermissionMode(ctx, a.Ticket, a.Mode); err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return textResult("permission mode for " + a.Ticket + " set to " + a.Mode), nil, nil
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
		Description: "Retire an agent and hand its work to a fresh successor in the SAME worktree (cwd) and permission mode — useful when an agent's context is bloated/near-compaction. Spawns the successor first, then reaps the old agent (fail-safe: if the spawn fails the old agent is left running). With resume_file, the successor reads the handoff notes there first. Mirrors `warden rotate`.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a rotateAgentArgs) (*mcpsdk.CallToolResult, any, error) {
		if strings.TrimSpace(a.ResumePrompt) == "" {
			return textResult("error: resume_prompt is required"), nil, nil
		}
		old, err := s.cl.Get(ctx, a.Ticket)
		if err != nil {
			return textResult("error: look up " + a.Ticket + ": " + err.Error()), nil, nil
		}
		prompt := a.ResumePrompt
		if a.ResumeFile != "" {
			// Mirrors composeSuccessorPrompt in internal/cli/rotate.go.
			prompt = fmt.Sprintf("You are resuming work handed off from a previous agent that is being retired. "+
				"First read the handoff notes at %s for full context, decisions already made, and next steps. "+
				"Once you have read and internalized them, delete that handoff file. Then continue the work:\n\n%s", a.ResumeFile, a.ResumePrompt)
		}
		successor, err := s.cl.Spawn(ctx, client.SpawnParams{Prompt: prompt, Cwd: old.Workdir, PermissionMode: old.PermissionMode})
		if err != nil {
			return textResult("error: spawn successor (old agent left running): " + err.Error()), nil, nil
		}
		if err := s.cl.Terminate(ctx, a.Ticket); err != nil {
			return jsonResultAny(map[string]any{"successor": successor.ID, "workdir": successor.Workdir, "retired": a.Ticket, "warning": "successor spawned but reaping old agent failed: " + err.Error()})
		}
		return jsonResultAny(map[string]any{"successor": successor.ID, "workdir": successor.Workdir, "retired": a.Ticket})
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "handoff_agent",
		Description: "Delegate a sub-task to a DIFFERENT agent (the source keeps running). Default: spawn a fresh delegate (in its own worktree) seeded with the task + inlined context. With to=<id>: deliver the handoff into an existing agent's inbox instead (wakes it if idle). Mirrors `warden handoff`.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a handoffAgentArgs) (*mcpsdk.CallToolResult, any, error) {
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

// jsonResultAny is jsonResult adapted to the (result, any, error) tool return
// signature, so handlers can `return jsonResultAny(v)` in one line.
func jsonResultAny(v any) (*mcpsdk.CallToolResult, any, error) {
	r, err := jsonResult(v)
	return r, nil, err
}

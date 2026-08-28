package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/role"
)

const approvalsDisabledMsg = "approvals disabled (enable with approvals: true in the config file)"

// Server wraps an MCP server bound to a daemon client.
type Server struct {
	mcp *mcpsdk.Server
	cl  *client.Client
}

// Tool argument structs (the SDK derives JSON schema from these).
type listArgs struct{}
type ticketArgs struct {
	Ticket string `json:"ticket" jsonschema:"the ticket / session id, e.g. PROJ-350"`
}

// `role` is REQUIRED (validated in the handler below — there is no implicit
// fallback to "general"; see role.Names() for the valid set). Every other
// field is optional in the schema: the daemon validates that EITHER a prompt
// OR (type + repo) is provided.
type spawnArgs struct {
	Type           string   `json:"type,omitempty" jsonschema:"task type: development|analysis|spike|pr-review|code|docs|website|debug-ci|tests|other"`
	Ticket         string   `json:"ticket,omitempty" jsonschema:"optional Jira ticket; becomes the session id when present"`
	Repo           string   `json:"repo,omitempty" jsonschema:"absolute path to the repo (managed-worktree mode)"`
	Branch         string   `json:"branch,omitempty" jsonschema:"optional; new branch (development) or checkout target (pr-review)"`
	PR             string   `json:"pr,omitempty" jsonschema:"optional PR number/url for pr-review"`
	Worktree       bool     `json:"worktree,omitempty" jsonschema:"create a scratch worktree for analysis/spike"`
	InRepo         bool     `json:"in_repo,omitempty" jsonschema:"write-agent opt-out: run in the shared repo instead of an isolated worktree (ignored for pr-review). Default false — write-agents isolate."`
	Prompt         string   `json:"prompt,omitempty" jsonschema:"what the agent should do — prompt-mode: auto-typed, no repo needed"`
	Dir            string   `json:"dir,omitempty" jsonschema:"directory to launch the agent from; defaults to the orchestrator's current working directory"`
	PermissionMode string   `json:"permission_mode,omitempty" jsonschema:"permission mode: acceptEdits|auto|bypassPermissions|default|dontAsk|plan; defaults to config or 'auto'"`
	Force          bool     `json:"force,omitempty" jsonschema:"spawn even when the memory-pressure gate warns (default false)"`
	Name           string   `json:"name,omitempty" jsonschema:"optional human-readable name for the agent (max 50 chars, alphanumeric/dash/underscore only)"`
	Model          string   `json:"model,omitempty" jsonschema:"claude model: opus, sonnet, haiku, fable, or full model ID; defaults to the model_default config setting (sonnet). Only needed alongside backend, or to override tier/role-based resolution — see role"`
	Backend        string   `json:"backend,omitempty" jsonschema:"agent backend to drive: claude (default), aider, opencode, codex, crush, goose, cursor, or antigravity. Backends differ in capabilities — aider & opencode are bring-your-own-model (set model, e.g. ollama_chat/qwen2.5-coder:3b or ollama/qwen2.5-coder:3b), with tokens-only spend. aider has no resume and runs an autonomous task that exits when done; opencode has a structured (Tier A) transcript and DOES resume the worktree's last session"`
	Kind           string   `json:"kind,omitempty" jsonschema:"session kind: empty/agent (default) spawns an AI agent with the chosen backend; terminal opens a plain interactive shell ($SHELL) in dir — NOT an AI agent (backend/model/role/prompt are ignored), excluded from spend/state/approvals"`
	Tags           []string `json:"tags,omitempty" jsonschema:"optional free-form labels for grouping/filtering (e.g. [\"backend\",\"urgent\"]); searchable and filterable via warden ls --tag"`
	Role           string   `json:"role" jsonschema:"REQUIRED — built-in agent role: general | orchestrator | implementer | auto-merger | reviewer | worker. Injects the role's persona as a system-prompt addendum and applies its default flags (type/model/permission_mode/auto_approve/tags) to any field left unset. See list_roles. On its own it is enough to spawn — backend+model are resolved from tier/task/role by the quota-balanced resolver when not pinned explicitly"`
	Tier           string   `json:"tier,omitempty" jsonschema:"optional model tier for the quota-balanced resolver that picks the backend+model: tier-1|tier-2|tier-3. Empty derives the tier from task, then role. An explicit backend/model still wins over the resolver"`
	Task           string   `json:"task,omitempty" jsonschema:"optional task name (task registry) used to derive the model tier when tier is empty"`
}
type adoptArgs struct {
	Dir         string `json:"dir,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
	TmuxSession string `json:"tmux_session,omitempty"`
}
type sendArgs struct {
	Ticket string `json:"ticket" jsonschema:"the agent's ticket / session id"`
	Text   string `json:"text" jsonschema:"the message to type into the agent's claude session"`
}
type outputArgs struct {
	Ticket string `json:"ticket" jsonschema:"the agent's ticket / session id"`
	Lines  int    `json:"lines" jsonschema:"how many recent pane lines to return (default 200)"`
}
type forceArgs struct {
	Ticket string `json:"ticket" jsonschema:"the agent's ticket / session id"`
	Force  bool   `json:"force,omitempty" jsonschema:"override the alive/uncommitted/unpushed guards"`
}
type deleteToolArgs struct {
	Ticket string `json:"ticket" jsonschema:"the agent's ticket / session id"`
	Hard   bool   `json:"hard,omitempty" jsonschema:"permanently purge instead of archiving"`
}

// stopArgs backs the umbrella stop_agent tool. The default (all keep_* false)
// is a full teardown: terminate + clear record + remove worktree. The keep_*
// flags are subtractive, mirroring the `wd stop` CLI flags.
type stopArgs struct {
	Ticket              string `json:"ticket" jsonschema:"the agent's ticket / session id"`
	KeepRecord          bool   `json:"keep_record,omitempty" jsonschema:"do not clear the stored record"`
	KeepWorktree        bool   `json:"keep_worktree,omitempty" jsonschema:"do not remove the git worktree (keep_worktree alone == the old done)"`
	Hard                bool   `json:"hard,omitempty" jsonschema:"purge the record instead of archiving"`
	PR                  bool   `json:"pr,omitempty" jsonschema:"open a GitHub PR for the agent's branch (pushes first) before tearing down"`
	Base                string `json:"base,omitempty" jsonschema:"base branch for the PR (default main); only meaningful with pr=true"`
	Force               bool   `json:"force,omitempty" jsonschema:"override the alive/uncommitted/unpushed worktree guards"`
	DeleteAdoptedBranch bool   `json:"delete_adopted_branch,omitempty" jsonschema:"also delete the branch even if warden did not create it (adopted branches are kept by default)"`
}

type gitCommitArgs struct {
	Message string `json:"message,omitempty" jsonschema:"the commit message — best to pass it, you wrote the change so you know the intent; if omitted warden generates one from the diff"`
	Dir     string `json:"dir,omitempty" jsonschema:"worktree to commit; defaults to the current directory"`
}
type gitPushArgs struct {
	Dir   string `json:"dir,omitempty" jsonschema:"worktree to push; defaults to the current directory"`
	Force bool   `json:"force,omitempty" jsonschema:"push with --force-with-lease after a rebase or amend — overwrites your own remote branch but aborts if a teammate pushed to it since your last fetch"`
}
type gitSyncArgs struct {
	Base string `json:"base,omitempty" jsonschema:"base branch to rebase onto; defaults to main"`
	Dir  string `json:"dir,omitempty" jsonschema:"worktree to sync; defaults to the current directory"`
}
type checkArgs struct {
	Name string `json:"name,omitempty" jsonschema:"the configured check to run (e.g. test, lint, build); omit to run them all"`
	Dir  string `json:"dir,omitempty" jsonschema:"worktree to check; defaults to the current directory"`
}

type ctxSetArgs struct {
	Key   string `json:"key" jsonschema:"the context key, e.g. global.findings or pipeline.<id>.<job>.output"`
	Value string `json:"value" jsonschema:"the value to store"`
}
type ctxCASArgs struct {
	Key      string `json:"key" jsonschema:"the context key, e.g. global.findings or pipeline.<id>.<job>.output"`
	Expected string `json:"expected,omitempty" jsonschema:"only write if the current value equals this (empty = the key must be absent)"`
	Value    string `json:"value" jsonschema:"the new value to store"`
}
type ctxAppendArgs struct {
	Key   string `json:"key" jsonschema:"the context key to append to"`
	Value string `json:"value" jsonschema:"the value to append"`
	Sep   string `json:"sep,omitempty" jsonschema:"separator inserted before the value when the key already exists (default newline)"`
}
type ctxGetArgs struct {
	Key string `json:"key" jsonschema:"the context key to read"`
}
type ctxListArgs struct {
	Prefix string `json:"prefix,omitempty" jsonschema:"optional key prefix filter (empty = all keys)"`
}

type sendMessageArgs struct {
	To   string `json:"to" jsonschema:"recipient agent's session id"`
	Body string `json:"body" jsonschema:"the message text"`
}
type readInboxArgs struct {
	Agent  string `json:"agent,omitempty" jsonschema:"whose inbox to read; defaults to this agent ($WARDEN_SESSION_ID)"`
	Unread bool   `json:"unread,omitempty" jsonschema:"only return unread messages"`
}
type waitForMessageArgs struct {
	Agent      string `json:"agent,omitempty" jsonschema:"whose inbox to wait on; defaults to this agent ($WARDEN_SESSION_ID)"`
	From       string `json:"from,omitempty" jsonschema:"only wait for a message from this sender"`
	TimeoutSec int    `json:"timeout_sec,omitempty" jsonschema:"seconds to wait before giving up (default 300; daemon caps at 600)"`
}

type approveArgs struct {
	Ticket string `json:"ticket" jsonschema:"the agent's ticket / session id with the pending prompt"`
	Option int    `json:"option" jsonschema:"the 1-based option number to answer (as shown by list_approvals)"`
}

type whoIsEditingArgs struct {
	File string `json:"file" jsonschema:"repo-relative file path (as reported by git diff)"`
}

type createPipelineArgs struct {
	Spec string `json:"spec" jsonschema:"the pipeline definition as a YAML spec — top-level name, repo, and a jobs list (each job: id, prompt, optional depends_on, worktree none|fresh|from:<job>, type, run_if, supervised). Same schema as a 'warden pipeline create -f' file."`
}
type pipelineIDArgs struct {
	Pipeline string `json:"pipeline" jsonschema:"the pipeline id (equals its name)"`
}

type insightsArgs struct {
	Limit int `json:"limit,omitempty" jsonschema:"cap the number of archived sessions mined (0 = daemon default)"`
}

type snapshotCreateArgs struct {
	Message string `json:"message,omitempty" jsonschema:"optional label for the snapshot"`
	Dir     string `json:"dir,omitempty" jsonschema:"worktree to snapshot; defaults to the current directory (pinned to this agent's own worktree)"`
}
type snapshotListArgs struct {
	Session string `json:"session,omitempty" jsonschema:"filter to one agent's snapshots; defaults to this agent ($WARDEN_SESSION_ID); empty with all=true lists every session"`
	All     bool   `json:"all,omitempty" jsonschema:"list snapshots across all sessions instead of just this agent's"`
}
type snapshotRestoreArgs struct {
	ID    string `json:"id" jsonschema:"the snapshot id to restore (as shown by snapshot_list)"`
	Force bool   `json:"force,omitempty" jsonschema:"restore even when the worktree has uncommitted changes (default false)"`
}

// findApproval locates the view for id in the live queue and validates that the
// option is answerable, mirroring the CLI/web guards. The daemon still re-verifies
// the fingerprint on POST (TOCTOU 409 guard); this just surfaces friendly errors.
func findApproval(views []approval.View, id string, option int) (approval.View, error) {
	for _, v := range views {
		if v.ID != id {
			continue
		}
		if !v.Recognized {
			return approval.View{}, fmt.Errorf("prompt for %s is not a recognized menu — attach instead", id)
		}
		if option < 1 || option > len(v.Options) {
			return approval.View{}, fmt.Errorf("option %d out of range (1-%d)", option, len(v.Options))
		}
		return v, nil
	}
	return approval.View{}, fmt.Errorf("no pending approval for %s (run list_approvals)", id)
}

// sessionID reads this agent's id from the tmux session env, preferring the
// canonical WARDEN_SESSION_ID and falling back to the legacy AGENTCTL_SESSION_ID.
func sessionID() string {
	if id := os.Getenv("WARDEN_SESSION_ID"); id != "" {
		return id
	}
	return os.Getenv("AGENTCTL_SESSION_ID")
}

// ctxWriter attributes shared-context writes to this agent when running inside
// one (WARDEN_SESSION_ID), else a generic "agent".
func ctxWriter() string {
	if id := sessionID(); id != "" {
		return id
	}
	return "agent"
}

// mcpDir resolves the working dir for a git tool: the explicit arg (made
// absolute) when given, else the process cwd. The agent calls these from its
// worktree, so cwd is the right default.
func mcpDir(arg string) string {
	if arg != "" {
		if abs, err := filepath.Abs(arg); err == nil {
			return abs
		}
		return arg
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return ""
}

func textResult(s string) *mcpsdk.CallToolResult {
	return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: s}}}
}

func jsonResult(v any) (*mcpsdk.CallToolResult, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return textResult(string(b)), nil
}

func NewServer(daemonBase string) *Server {
	s := &Server{
		mcp: mcpsdk.NewServer(&mcpsdk.Implementation{Name: "warden", Version: "0.1.0"}, nil),
		cl:  client.New(daemonBase),
	}

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "list_agents",
		Description: "List all active Claude Code agents and their current status.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ listArgs) (*mcpsdk.CallToolResult, any, error) {
		sessions, err := s.cl.List(ctx)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		res, err := jsonResult(sessions)
		return res, nil, err
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "get_agent",
		Description: "Get full detail (status, events, worktree) for one agent.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a ticketArgs) (*mcpsdk.CallToolResult, any, error) {
		sess, err := s.cl.Get(ctx, a.Ticket)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		res, err := jsonResult(sess)
		return res, nil, err
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "spawn_agent",
		Description: "Spawn an agent. `role` is REQUIRED — no implicit fallback (see list_roles for the valid set); it alone is enough to resolve a backend+model, or pin one explicitly with `tier`, or `backend`+`model`. Provide `prompt` for a quick auto-typed agent (no repo needed). OR provide `type`+`repo` for a managed worktree. Every write-agent (development/pr-review/code/docs/website/debug-ci/tests) is isolated in its own worktree by default so parallel agents never collide; pass `in_repo=true` to deliberately share the repo (ignored for pr-review). analysis/spike take an optional worktree via `worktree=true`. Launches the configured default model (sonnet) and permission mode (auto) unless `model`/`permission_mode` override them; risky tools prompt → answerable in the approvals inbox. If the memory-pressure gate blocks the spawn, re-call with force=true to bypass the warning.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a spawnArgs) (*mcpsdk.CallToolResult, any, error) {
		roleName := strings.TrimSpace(a.Role)
		if roleName == "" {
			return textResult("error: role is required (valid: " + strings.Join(role.Names(), ", ") + ")"), nil, nil
		}
		if _, ok := role.Get(roleName); !ok {
			return textResult("error: unknown role " + fmt.Sprintf("%q", roleName) + " (valid: " + strings.Join(role.Names(), ", ") + ")"), nil, nil
		}
		cwd := a.Dir
		if cwd == "" {
			if wd, err := os.Getwd(); err == nil {
				cwd = wd
			}
		} else if abs, err := filepath.Abs(cwd); err == nil {
			cwd = abs
		}
		sess, err := s.cl.Spawn(ctx, client.SpawnParams{
			Type: a.Type, Ticket: a.Ticket, Repo: a.Repo,
			Branch: a.Branch, PR: a.PR, Worktree: a.Worktree, InRepo: a.InRepo,
			Prompt: a.Prompt, Cwd: cwd, PermissionMode: a.PermissionMode, Force: a.Force,
			Name: a.Name, Model: a.Model, Backend: a.Backend, Kind: a.Kind, Tags: a.Tags,
			Role: roleName, Tier: a.Tier, Task: a.Task, ParentID: sessionID(),
		})
		if err != nil {
			var cre *client.ErrConfirmationRequired
			if errors.As(err, &cre) {
				return textResult("memory-pressure gate: " + cre.Verdict.Reason +
					"\nRe-call spawn_agent with force=true to spawn anyway."), nil, nil
			}
			return textResult("error: " + err.Error()), nil, nil
		}
		res, err := jsonResult(sess)
		return res, nil, err
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "adopt_agent",
		Description: "Register an existing Claude Code session into warden: resume the newest conversation for a directory under tmux, or (when tmux_session is given) register a running tmux session live without relaunch.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a adoptArgs) (*mcpsdk.CallToolResult, any, error) {
		cwd := a.Dir
		if cwd == "" {
			if wd, err := os.Getwd(); err == nil {
				cwd = wd
			}
		} else if abs, err := filepath.Abs(cwd); err == nil {
			cwd = abs
		}
		res, err := s.cl.Adopt(ctx, client.AdoptParams{
			Cwd: cwd, SessionID: a.SessionID, TmuxSession: a.TmuxSession,
		})
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		msg := "adopted as " + res.Session.ID
		if res.Warning != "" {
			msg += " (warning: " + res.Warning + ")"
		}
		return textResult(msg), res.Session, nil
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "send_to_agent",
		Description: "Type a message into a specific agent's claude session and press Enter.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a sendArgs) (*mcpsdk.CallToolResult, any, error) {
		if err := s.cl.Input(ctx, a.Ticket, a.Text); err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return textResult("sent to " + a.Ticket), nil, nil
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "get_agent_output",
		Description: "Return the recent terminal output of a specific agent's claude session.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a outputArgs) (*mcpsdk.CallToolResult, any, error) {
		out, err := s.cl.Output(ctx, a.Ticket, a.Lines)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return textResult(out), nil, nil
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "commit",
		Description: "Stage and commit every change in the worktree on its branch — one call in place of git status/add/commit/rev-parse. warden refuses protected branches (main/master), runs pre-commit hooks and returns ONLY a failure, and links the commit to this agent. Pass `message` when you can (you made the change, so you know the intent); omit it and warden writes one from the diff (local model, else a deterministic conventional-commit floor). Returns {committed, sha, branch, files} or a hook failure to fix.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a gitCommitArgs) (*mcpsdk.CallToolResult, any, error) {
		res, err := s.cl.GitCommit(ctx, sessionID(), mcpDir(a.Dir), a.Message)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		r, err := jsonResult(res)
		return r, nil, err
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "push",
		Description: "Push the current worktree branch to origin (sets upstream). warden refuses to push protected branches (main/master) directly — push your agent branch and open a PR. Pass force=true to push with --force-with-lease after a rebase or amend (safe force: overwrites your own remote branch, aborts if a teammate pushed since your last fetch). Returns {branch, remote, pushed, forced}.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a gitPushArgs) (*mcpsdk.CallToolResult, any, error) {
		res, err := s.cl.GitPush(ctx, sessionID(), mcpDir(a.Dir), a.Force)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		r, err := jsonResult(res)
		return r, nil, err
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "sync",
		Description: "Fetch origin and rebase the current branch onto origin/<base> (default main). Refuses a dirty tree (commit first). On conflict warden leaves the rebase in progress and returns ONLY the conflicting files — resolve those, then `git rebase --continue`. Returns {branch, base, updated, conflicts}.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a gitSyncArgs) (*mcpsdk.CallToolResult, any, error) {
		res, err := s.cl.GitSync(ctx, sessionID(), mcpDir(a.Dir), a.Base)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		r, err := jsonResult(res)
		return r, nil, err
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "check",
		Description: "Run the project's configured checks (from .warden/check.yml) and get back a compact pass/fail summary with output for ONLY the failing checks — in place of running tests/lint/build in Bash and reading hundreds of lines. Pass `name` for one check (e.g. test, lint, build) or omit to run them all. Use this instead of raw `go test` / `npm test` / `make verify`. Returns {passed, checks:[{name,cmd,passed,exit_code,output}]}.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a checkArgs) (*mcpsdk.CallToolResult, any, error) {
		res, err := s.cl.Check(ctx, sessionID(), mcpDir(a.Dir), a.Name)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		r, err := jsonResult(res)
		return r, nil, err
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "ctx_set",
		Description: "Write a value to the shared context — a key/value store all agents share. Use to publish a result other agents will read (e.g. global.findings).",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a ctxSetArgs) (*mcpsdk.CallToolResult, any, error) {
		if _, err := s.cl.CtxSet(ctx, a.Key, a.Value, ctxWriter()); err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return textResult("set " + a.Key), nil, nil
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "ctx_cas",
		Description: "Atomically write a shared-context key only if its current value equals `expected` (empty expected = key must be absent). Use this instead of ctx_set for read-modify-write coordination (e.g. claiming a task); on a conflict, re-read with ctx_get and retry.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a ctxCASArgs) (*mcpsdk.CallToolResult, any, error) {
		_, err := s.cl.CtxCAS(ctx, a.Key, a.Expected, a.Value, ctxWriter())
		if errors.Is(err, client.ErrCASConflict) {
			return textResult("conflict: " + a.Key + " changed (current value != expected); ctx_get and retry"), nil, nil
		}
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return textResult("set " + a.Key), nil, nil
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "ctx_append",
		Description: "Atomically append to a shared-context key's value (creating it if absent). Race-free way for multiple agents to accumulate into one key (e.g. global.findings).",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a ctxAppendArgs) (*mcpsdk.CallToolResult, any, error) {
		sep := a.Sep
		if sep == "" {
			sep = "\n"
		}
		if _, err := s.cl.CtxAppend(ctx, a.Key, a.Value, sep, ctxWriter()); err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return textResult("appended to " + a.Key), nil, nil
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "ctx_get",
		Description: "Read a value from the shared context by key.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a ctxGetArgs) (*mcpsdk.CallToolResult, any, error) {
		e, err := s.cl.CtxGet(ctx, a.Key)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return textResult(e.Value), nil, nil
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "ctx_list",
		Description: "List shared-context keys, optionally filtered by prefix.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a ctxListArgs) (*mcpsdk.CallToolResult, any, error) {
		entries, err := s.cl.CtxList(ctx, a.Prefix)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		res, err := jsonResult(entries)
		return res, nil, err
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "send_message",
		Description: "Send a directed message to another agent's inbox (wakes it only if it's idle/waiting). Use for peer consultation or handoff signals.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a sendMessageArgs) (*mcpsdk.CallToolResult, any, error) {
		m, woke, err := s.cl.MsgSend(ctx, a.To, ctxWriter(), a.Body)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		msg := "sent to " + a.To + " (id " + m.ID + ")"
		if woke {
			msg += " — woke recipient"
		}
		return textResult(msg), nil, nil
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "read_inbox",
		Description: "Read directed messages addressed to this agent (marks them read).",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a readInboxArgs) (*mcpsdk.CallToolResult, any, error) {
		who := a.Agent
		if who == "" {
			who = sessionID()
		}
		if who == "" {
			return textResult("error: no agent id — set WARDEN_SESSION_ID or pass `agent`"), nil, nil
		}
		msgs, err := s.cl.MsgInbox(ctx, who, a.Unread)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		res, err := jsonResult(msgs)
		return res, nil, err
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "wait_for_message",
		Description: "Block until a directed message arrives in this agent's inbox (or timeout), then return it and mark it read. One call, no busy-polling across turns — prefer this over repeatedly calling read_inbox when awaiting a reply.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a waitForMessageArgs) (*mcpsdk.CallToolResult, any, error) {
		who := a.Agent
		if who == "" {
			who = sessionID()
		}
		if who == "" {
			return textResult("error: no agent id — set WARDEN_SESSION_ID or pass `agent`"), nil, nil
		}
		timeout := a.TimeoutSec
		if timeout <= 0 {
			timeout = 300
		}
		m, err := s.cl.MsgWait(ctx, who, a.From, timeout)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		if m == nil {
			return textResult("(timed out — no message)"), nil, nil
		}
		res, err := jsonResult(m)
		return res, nil, err
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "get_collaboration_status",
		Description: "List files currently being edited by more than one agent (inter-agent file conflicts). Use to check whether another agent is touching the same code before you dig in.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ listArgs) (*mcpsdk.CallToolResult, any, error) {
		conflicts, err := s.cl.CollabConflicts(ctx)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		res, err := jsonResult(conflicts)
		return res, nil, err
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "get_branch_status",
		Description: "Per-agent CI + branch-vs-main status: each tracked agent's latest GitHub CI run and how its branch sits against origin/main (behind/ahead/merged). Read-only. Empty if the branch tracker is disabled.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ listArgs) (*mcpsdk.CallToolResult, any, error) {
		statuses, err := s.cl.BranchStatuses(ctx)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		res, err := jsonResult(statuses)
		return res, nil, err
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "who_is_editing_file",
		Description: "Show which agents are currently editing a specific file. Returns the agents sharing that file, or a note that no other agent is.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a whoIsEditingArgs) (*mcpsdk.CallToolResult, any, error) {
		conflicts, err := s.cl.CollabConflicts(ctx)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		for _, c := range conflicts {
			if c.File == a.File {
				res, err := jsonResult(c.Agents)
				return res, nil, err
			}
		}
		return textResult("no other agent is editing " + a.File), nil, nil
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "list_approvals",
		Description: "List pending tool-permission prompts waiting for an answer (supervised agents). Recognized prompts include their numbered options + a stable fingerprint; answer one with the approve tool. Returns the disabled message when approvals are off.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ listArgs) (*mcpsdk.CallToolResult, any, error) {
		enabled, views, err := s.cl.Approvals(ctx)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		if !enabled {
			return textResult(approvalsDisabledMsg), nil, nil
		}
		res, err := jsonResult(views)
		return res, nil, err
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "approve",
		Description: "Answer a pending tool-permission prompt by 1-based option number. Re-fetches the live queue, validates the option, and passes the prompt's fingerprint so the daemon can re-verify the menu hasn't changed (TOCTOU guard). Returns the disabled message when approvals are off.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a approveArgs) (*mcpsdk.CallToolResult, any, error) {
		enabled, views, err := s.cl.Approvals(ctx)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		if !enabled {
			return textResult(approvalsDisabledMsg), nil, nil
		}
		v, err := findApproval(views, a.Ticket, a.Option)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		if err := s.cl.Approve(ctx, a.Ticket, a.Option, v.Fingerprint); err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return textResult(fmt.Sprintf("approved %s → %d. %s", a.Ticket, a.Option, v.Options[a.Option-1])), nil, nil
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "terminate_agent",
		Description: "Stop an agent: kill its tmux+claude session. Keeps the record and worktree (reversible via restore_agent). The default 'stop this agent' action.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a ticketArgs) (*mcpsdk.CallToolResult, any, error) {
		if err := s.cl.Terminate(ctx, a.Ticket); err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return textResult("terminated " + a.Ticket), nil, nil
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "delete_agent",
		Description: "Clear an agent's stored record (archives by default; hard=true purges). Does not touch tmux or the worktree.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a deleteToolArgs) (*mcpsdk.CallToolResult, any, error) {
		if err := s.cl.Delete(ctx, a.Ticket, a.Hard); err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return textResult("deleted " + a.Ticket), nil, nil
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "remove_worktree",
		Description: "Remove an agent's git worktree + branch. DESTRUCTIVE and always requires explicit user confirmation first. Refuses if the agent is still running (terminate first) or has uncommitted/unpushed work, unless force=true.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a forceArgs) (*mcpsdk.CallToolResult, any, error) {
		if err := s.cl.RemoveWorktree(ctx, a.Ticket, a.Force, false); err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return textResult("removed worktree for " + a.Ticket), nil, nil
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "stop_agent",
		Description: "Tear down an agent — the single umbrella verb. Default (all keep_* false) is a FULL teardown: terminate the session, clear (archive) the record, AND remove the git worktree + branch. Subtractive flags keep parts: keep_record, keep_worktree (keep_worktree alone == the old 'done'). hard=true purges the record; pr=true opens a GitHub PR first while the agent is intact (safe order: PR → terminate → clear record → remove worktree). DESTRUCTIVE when it removes the worktree — only do so after explicit user confirmation; force=true overrides the alive/uncommitted/unpushed guards.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a stopArgs) (*mcpsdk.CallToolResult, any, error) {
		// Safe order: PR (while the agent is intact) → terminate → clear record → remove worktree.
		prefix := ""
		if a.PR {
			res, err := s.cl.CreatePR(ctx, a.Ticket, a.Base)
			if err != nil {
				return textResult("error: create PR: " + err.Error() + " (agent left running — fix the issue and retry)"), nil, nil
			}
			verb := "opened PR"
			if !res.Created {
				verb = "PR already exists"
			}
			prefix = verb + ": " + res.URL + "; "
		}
		if err := s.cl.Terminate(ctx, a.Ticket); err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		if !a.KeepRecord {
			if err := s.cl.Delete(ctx, a.Ticket, a.Hard); err != nil {
				return textResult("error: " + err.Error()), nil, nil
			}
		}
		if !a.KeepWorktree {
			if err := s.cl.RemoveWorktree(ctx, a.Ticket, a.Force, a.DeleteAdoptedBranch); err != nil {
				return textResult("error: " + err.Error()), nil, nil
			}
		}
		return textResult(prefix + "stopped " + a.Ticket), nil, nil
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "restore_agent",
		Description: "Recreate and resume a lost/orphaned agent's tmux + claude session (claude --resume). Use only when the agent's tmux session is gone (status orphaned).",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a ticketArgs) (*mcpsdk.CallToolResult, any, error) {
		if err := s.cl.Restore(ctx, a.Ticket); err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return textResult("restoring " + a.Ticket), nil, nil
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "create_pipeline",
		Description: "Create a DAG pipeline of agent jobs from a YAML spec (the daemon parses, validates, and stores it). Use this to drive a multi-stage / dependent agent workflow (e.g. analyze→implement→review) instead of spawning and wiring agents by hand. The pipeline starts in `pending` — call start_pipeline to spawn its entry jobs. Returns the created pipeline {id, status, jobs}.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a createPipelineArgs) (*mcpsdk.CallToolResult, any, error) {
		p, err := s.cl.PipelineCreate(ctx, a.Spec)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		r, err := jsonResult(p)
		return r, nil, err
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "list_pipelines",
		Description: "List all pipelines with their status and job count.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ listArgs) (*mcpsdk.CallToolResult, any, error) {
		ps, err := s.cl.PipelineList(ctx)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		r, err := jsonResult(ps)
		return r, nil, err
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "list_schedules",
		Description: "List the daemon's schedules (recurring cron and single-shot at triggers that fire an agent or pipeline), with each one's next run, enabled state, and last error. Returns a 403 error when the scheduler is disabled (scheduler_enabled config).",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ listArgs) (*mcpsdk.CallToolResult, any, error) {
		list, err := s.cl.ScheduleList(ctx)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		r, err := jsonResult(list)
		return r, nil, err
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "show_pipeline",
		Description: "Show one pipeline's jobs and their status, including each job's branch and emitted handoff output — so a finished pipeline's results are readable here even after its agents are gone.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a pipelineIDArgs) (*mcpsdk.CallToolResult, any, error) {
		p, err := s.cl.PipelineGet(ctx, a.Pipeline)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		r, err := jsonResult(p)
		return r, nil, err
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "start_pipeline",
		Description: "Start a pending pipeline: spawns its jobs that have no dependencies; dependents spawn automatically as their upstreams emit. Returns an error if the pipeline was already started.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a pipelineIDArgs) (*mcpsdk.CallToolResult, any, error) {
		if err := s.cl.PipelineStart(ctx, a.Pipeline); err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return textResult("started " + a.Pipeline), nil, nil
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "cancel_pipeline",
		Description: "Cancel a pipeline: terminates any live job sessions and marks remaining jobs skipped. A finished pipeline cannot be canceled (delete it via the CLI instead).",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a pipelineIDArgs) (*mcpsdk.CallToolResult, any, error) {
		if err := s.cl.PipelineCancel(ctx, a.Pipeline); err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return textResult("canceled " + a.Pipeline), nil, nil
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "insights",
		Description: "Mine warden's own history (#48) into a compact, deterministic report: typical/outlier session durations by type, frequently co-edited files, error rates, busiest hours, live-agent anomalies, and — the headline — pairs of finished sessions that ran sequentially but touched disjoint files in the same repo, so they could have run in parallel as a 2-job pipeline. No model required; returns the structured Report {durations, co_edits, error_rates, busiest_periods, parallelizable, anomalies}.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a insightsArgs) (*mcpsdk.CallToolResult, any, error) {
		rep, err := s.cl.Insights(ctx, client.InsightsParams{HistoryLimit: a.Limit})
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		r, err := jsonResult(rep)
		return r, nil, err
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "snapshot_create",
		Description: "Checkpoint this agent's worktree + session transcript at a known-good point (#46). Captures the worktree non-destructively (git stash create — the working tree is untouched) plus the recorded HEAD/branch/dirty-file list, and saves the transcript. Returns the snapshot {id, branch, head_sha, stash_sha, dirty_files, transcript_path}. Roll back later with snapshot_restore.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a snapshotCreateArgs) (*mcpsdk.CallToolResult, any, error) {
		snap, err := s.cl.SnapshotCreate(ctx, sessionID(), mcpDir(a.Dir), a.Message)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		r, err := jsonResult(snap)
		return r, nil, err
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "snapshot_list",
		Description: "List this agent's snapshots newest-first (or all sessions with all=true). Returns the saved checkpoints {id, created_at, branch, head_sha, dirty_files, message} to pick one to restore.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a snapshotListArgs) (*mcpsdk.CallToolResult, any, error) {
		session := a.Session
		if session == "" {
			session = sessionID()
		}
		if a.All {
			session = ""
		}
		snaps, err := s.cl.SnapshotList(ctx, session)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		r, err := jsonResult(snaps)
		return r, nil, err
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "snapshot_restore",
		Description: "Re-apply a snapshot onto its recorded worktree (#46). Refuses a dirty tree unless force=true and never restores onto main/master. Reversible-safe — re-applies the stash without resetting HEAD or dropping the snapshot. Returns {applied, head_match, conflicts, transcript_path}; resolve any conflicts as with a rebase.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a snapshotRestoreArgs) (*mcpsdk.CallToolResult, any, error) {
		res, err := s.cl.SnapshotRestore(ctx, a.ID, a.Force)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		r, err := jsonResult(res)
		return r, nil, err
	})

	// Parity tools (read/insight, lifecycle controls, the rest of the pipeline
	// and schedule verbs, delegation) live in tools_extra.go to keep this
	// constructor readable.
	s.registerExtraTools()

	return s
}

// Run serves the MCP server over the given transport until ctx is cancelled.
func (s *Server) Run(ctx context.Context, t mcpsdk.Transport) error {
	return s.mcp.Run(ctx, t)
}

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/client"
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

// All fields are optional in the schema: the daemon validates that EITHER a
// prompt OR (type + repo) is provided, so no single field is required here.
type spawnArgs struct {
	Type           string `json:"type,omitempty" jsonschema:"task type: development|analysis|spike|pr-review|code|docs|website|debug-ci|tests|other"`
	Ticket         string `json:"ticket,omitempty" jsonschema:"optional Jira ticket; becomes the session id when present"`
	Repo           string `json:"repo,omitempty" jsonschema:"absolute path to the repo (managed-worktree mode)"`
	Branch         string `json:"branch,omitempty" jsonschema:"optional; new branch (development) or checkout target (pr-review)"`
	PR             string `json:"pr,omitempty" jsonschema:"optional PR number/url for pr-review"`
	Worktree       bool   `json:"worktree,omitempty" jsonschema:"create a scratch worktree for analysis/spike"`
	Prompt         string `json:"prompt,omitempty" jsonschema:"what the agent should do — prompt-mode: auto-typed, no repo needed"`
	Dir            string `json:"dir,omitempty" jsonschema:"directory to launch the agent from; defaults to the orchestrator's current working directory"`
	PermissionMode string `json:"permission_mode,omitempty" jsonschema:"permission mode: acceptEdits|auto|bypassPermissions|default|dontAsk|plan; defaults to config or 'auto'"`
	Force          bool   `json:"force,omitempty" jsonschema:"spawn even when the memory-pressure gate warns (default false)"`
	Name           string `json:"name,omitempty" jsonschema:"optional human-readable name for the agent (max 50 chars, alphanumeric/dash/underscore only)"`
	Model          string `json:"model,omitempty" jsonschema:"claude model: opus, sonnet, haiku, fable, or full model ID; defaults to sonnet-4.5 or the model_default config setting"`
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

type ctxSetArgs struct {
	Key   string `json:"key" jsonschema:"the context key, e.g. global.findings or pipeline.<id>.<job>.output"`
	Value string `json:"value" jsonschema:"the value to store"`
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

type approveArgs struct {
	Ticket string `json:"ticket" jsonschema:"the agent's ticket / session id with the pending prompt"`
	Option int    `json:"option" jsonschema:"the 1-based option number to answer (as shown by list_approvals)"`
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
		Description: "Spawn an agent. Provide `prompt` for a quick auto-typed agent (no repo needed). OR provide `type`+`repo` for a managed worktree (development/pr-review get a worktree; code/docs/website/debug-ci/tests run in the repo; analysis/spike take an optional worktree). Launches claude-sonnet-4-5 (1M context) with --permission-mode acceptEdits (risky tools prompt → answerable in the approvals inbox). If the memory-pressure gate blocks the spawn, re-call with force=true to bypass the warning.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a spawnArgs) (*mcpsdk.CallToolResult, any, error) {
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
			Branch: a.Branch, PR: a.PR, Worktree: a.Worktree,
			Prompt: a.Prompt, Cwd: cwd, PermissionMode: a.PermissionMode, Force: a.Force,
			Name: a.Name, Model: a.Model,
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
		Name:        "ctx_set",
		Description: "Write a value to the shared context — a key/value store all agents share. Use to publish a result other agents will read (e.g. global.findings).",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a ctxSetArgs) (*mcpsdk.CallToolResult, any, error) {
		if _, err := s.cl.CtxSet(ctx, a.Key, a.Value, ctxWriter()); err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return textResult("set " + a.Key), nil, nil
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
		if err := s.cl.RemoveWorktree(ctx, a.Ticket, a.Force); err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return textResult("removed worktree for " + a.Ticket), nil, nil
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

	return s
}

// Run serves the MCP server over the given transport until ctx is cancelled.
func (s *Server) Run(ctx context.Context, t mcpsdk.Transport) error {
	return s.mcp.Run(ctx, t)
}

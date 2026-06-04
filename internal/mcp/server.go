package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/srajanpathak/agentctl/internal/client"
)

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
	Type     string `json:"type,omitempty" jsonschema:"task type: development|analysis|spike|pr-review|buildkite-debug|test-run|env-test|other"`
	Ticket   string `json:"ticket,omitempty" jsonschema:"optional Jira ticket; becomes the session id when present"`
	Repo     string `json:"repo,omitempty" jsonschema:"absolute path to the repo (managed-worktree mode)"`
	Branch   string `json:"branch,omitempty" jsonschema:"optional; new branch (development) or checkout target (pr-review)"`
	PR       string `json:"pr,omitempty" jsonschema:"optional PR number/url for pr-review"`
	Worktree   bool   `json:"worktree,omitempty" jsonschema:"create a scratch worktree for analysis/spike"`
	Prompt     string `json:"prompt,omitempty" jsonschema:"what the agent should do — prompt-mode: auto-typed, no repo needed"`
	Dir        string `json:"dir,omitempty" jsonschema:"directory to launch the agent from; defaults to the orchestrator's current working directory"`
	Supervised bool   `json:"supervised,omitempty" jsonschema:"supervised mode: launch with --permission-mode acceptEdits so risky tools prompt (answerable in the approvals inbox) instead of bypassing all permissions"`
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

// ctxWriter attributes shared-context writes to this agent when running inside
// one (AGENTCTL_SESSION_ID), else a generic "agent".
func ctxWriter() string {
	if id := os.Getenv("AGENTCTL_SESSION_ID"); id != "" {
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
		mcp: mcpsdk.NewServer(&mcpsdk.Implementation{Name: "agentctl", Version: "0.1.0"}, nil),
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
		Description: "Spawn an agent. Provide `prompt` for a quick auto-typed agent (no repo needed). OR provide `type`+`repo` for a managed worktree (development/pr-review get a worktree; buildkite-debug/test-run/env-test run in the repo; analysis/spike take an optional worktree). Launches claude --dangerously-skip-permissions by default; set supervised=true for --permission-mode acceptEdits (risky tools prompt → answerable in the approvals inbox).",
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
			Prompt: a.Prompt, Cwd: cwd, Supervised: a.Supervised,
		})
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		res, err := jsonResult(sess)
		return res, nil, err
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "adopt_agent",
		Description: "Register an existing Claude Code session into agentctl: resume the newest conversation for a directory under tmux, or (when tmux_session is given) register a running tmux session live without relaunch.",
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

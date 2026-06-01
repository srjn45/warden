package mcp

import (
	"context"
	"encoding/json"

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
type spawnArgs struct {
	Type     string `json:"type" jsonschema:"task type: development|analysis|spike|pr-review|buildkite-debug|test-run|env-test|other"`
	Ticket   string `json:"ticket" jsonschema:"optional Jira ticket; becomes the session id when present"`
	Repo     string `json:"repo" jsonschema:"absolute path to the repo"`
	Branch   string `json:"branch" jsonschema:"optional; new branch (development) or checkout target (pr-review)"`
	PR       string `json:"pr" jsonschema:"optional PR number/url for pr-review"`
	Worktree bool   `json:"worktree" jsonschema:"create a scratch worktree for analysis/spike"`
}
type sendArgs struct {
	Ticket string `json:"ticket" jsonschema:"the agent's ticket / session id"`
	Text   string `json:"text" jsonschema:"the message to type into the agent's claude session"`
}
type outputArgs struct {
	Ticket string `json:"ticket" jsonschema:"the agent's ticket / session id"`
	Lines  int    `json:"lines" jsonschema:"how many recent pane lines to return (default 200)"`
}
type cleanupArgs struct {
	Ticket string `json:"ticket" jsonschema:"the agent's ticket / session id"`
	Force  bool   `json:"force" jsonschema:"override the uncommitted/unpushed guard"`
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
		Description: "Spawn a new agent of a given task type. development/pr-review get a worktree; buildkite-debug/test-run/env-test run in the repo; analysis/spike take an optional worktree. Launches claude --dangerously-skip-permissions.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a spawnArgs) (*mcpsdk.CallToolResult, any, error) {
		sess, err := s.cl.Spawn(ctx, client.SpawnParams{
			Type: a.Type, Ticket: a.Ticket, Repo: a.Repo,
			Branch: a.Branch, PR: a.PR, Worktree: a.Worktree,
		})
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		res, err := jsonResult(sess)
		return res, nil, err
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
		Name:        "cleanup_agent",
		Description: "Tear down an agent: kill tmux, prune worktree/branch (guarded), archive the record.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a cleanupArgs) (*mcpsdk.CallToolResult, any, error) {
		if err := s.cl.Cleanup(ctx, a.Ticket, a.Force, false); err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return textResult("cleaned up " + a.Ticket), nil, nil
	})

	return s
}

// Run serves the MCP server over the given transport until ctx is cancelled.
func (s *Server) Run(ctx context.Context, t mcpsdk.Transport) error {
	return s.mcp.Run(ctx, t)
}

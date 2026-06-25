package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/llm"
)

// ToolCall is the model's request to invoke a registry tool. It is an alias for
// llm.ToolCall so the loop can pass the model's reply straight into Dispatch.
type ToolCall = llm.ToolCall

// Tool is one warden verb the model may call. Mutating decides the gate: a
// read-only tool auto-executes; a mutating tool is rendered and confirmed first.
// There is deliberately no edit/write/bash/shell tool — that absence is the
// structural enforcement of "conducts, never implements" (see the registry test).
type Tool struct {
	Schema   llm.ToolSchema
	Mutating bool
	invoke   func(ctx context.Context, d Daemon, args map[string]any) (string, error)
}

// Registry is the complete, static list of what the orchestrator can do.
type Registry struct{ tools []Tool }

// Tools returns every registered tool.
func (r *Registry) Tools() []Tool { return r.tools }

// ToolSchemas returns the schemas to hand the model on a Chat call.
func (r *Registry) ToolSchemas() []llm.ToolSchema {
	out := make([]llm.ToolSchema, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t.Schema)
	}
	return out
}

// Lookup finds a tool by name.
func (r *Registry) Lookup(name string) (Tool, bool) {
	for _, t := range r.tools {
		if t.Schema.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// Dispatch validates a call against the registry and runs it. The returned
// string is fed back to the model as the tool result on the next turn; an
// unknown tool errors so the loop can recover.
func (r *Registry) Dispatch(ctx context.Context, d Daemon, c ToolCall) (string, error) {
	tl, ok := r.Lookup(c.Name)
	if !ok {
		return "", fmt.Errorf("unknown tool %q", c.Name)
	}
	return tl.invoke(ctx, d, c.Args)
}

// --- arg coercion: a small model may send a number where a string is wanted,
// or omit a key entirely; pull args defensively and never panic on a bad map. ---

func argStr(args map[string]any, key string) string {
	switch v := args[key].(type) {
	case string:
		return v
	case float64:
		// JSON numbers decode to float64; render without a trailing ".0".
		return strings.TrimSuffix(fmt.Sprintf("%v", v), ".0")
	case bool:
		return fmt.Sprintf("%v", v)
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", v)
	}
}

func argBool(args map[string]any, key string) bool {
	switch v := args[key].(type) {
	case bool:
		return v
	case string:
		return v == "true" || v == "1" || v == "yes"
	default:
		return false
	}
}

func argInt(args map[string]any, key string) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		var n int
		_, _ = fmt.Sscanf(v, "%d", &n)
		return n
	default:
		return 0
	}
}

// requireStr returns an error string (not a panic) for a missing required arg so
// the loop can feed it back and let the model retry.
func requireStr(args map[string]any, key string) (string, error) {
	s := strings.TrimSpace(argStr(args, key))
	if s == "" {
		return "", fmt.Errorf("missing required argument %q", key)
	}
	return s, nil
}

// jsonish renders a value as compact JSON for the model to read as text.
func jsonish(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// objSchema builds an "object" JSON-Schema with the given properties/required.
func objSchema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}
func boolProp(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}
func intProp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

// NewRegistry builds the static tool table — one Tool per warden verb the
// orchestrator exposes, split read-only (auto-execute) vs. mutating (gated).
func NewRegistry() *Registry {
	return &Registry{tools: []Tool{
		// ---- reads (auto-execute) ----
		{
			Schema: llm.ToolSchema{Name: "list_agents",
				Description: "List all agents and their current status.",
				Parameters:  objSchema(map[string]any{})},
			invoke: func(ctx context.Context, d Daemon, _ map[string]any) (string, error) {
				s, err := d.List(ctx)
				if err != nil {
					return "", err
				}
				return jsonish(s), nil
			},
		},
		{
			Schema: llm.ToolSchema{Name: "get_agent",
				Description: "Get full detail (status, events, worktree) for one agent by id.",
				Parameters:  objSchema(map[string]any{"ticket": strProp("agent id")}, "ticket")},
			invoke: func(ctx context.Context, d Daemon, a map[string]any) (string, error) {
				id, err := requireStr(a, "ticket")
				if err != nil {
					return "", err
				}
				s, err := d.Get(ctx, id)
				if err != nil {
					return "", err
				}
				return jsonish(s), nil
			},
		},
		{
			Schema: llm.ToolSchema{Name: "get_agent_output",
				Description: "Return the recent terminal output of an agent's session.",
				Parameters: objSchema(map[string]any{
					"ticket": strProp("agent id"), "lines": intProp("how many trailing lines (default 200)")}, "ticket")},
			invoke: func(ctx context.Context, d Daemon, a map[string]any) (string, error) {
				id, err := requireStr(a, "ticket")
				if err != nil {
					return "", err
				}
				lines := argInt(a, "lines")
				if lines <= 0 {
					lines = 200
				}
				return d.Output(ctx, id, lines)
			},
		},
		{
			Schema: llm.ToolSchema{Name: "get_collaboration_status",
				Description: "List files currently edited by more than one agent (inter-agent conflicts).",
				Parameters:  objSchema(map[string]any{})},
			invoke: func(ctx context.Context, d Daemon, _ map[string]any) (string, error) {
				c, err := d.CollabConflicts(ctx)
				if err != nil {
					return "", err
				}
				return jsonish(c), nil
			},
		},
		{
			Schema: llm.ToolSchema{Name: "read_inbox",
				Description: "Read directed messages addressed to an agent (marks them read).",
				Parameters: objSchema(map[string]any{
					"agent": strProp("agent id whose inbox to read"), "unread": boolProp("only unread")}, "agent")},
			invoke: func(ctx context.Context, d Daemon, a map[string]any) (string, error) {
				who, err := requireStr(a, "agent")
				if err != nil {
					return "", err
				}
				msgs, err := d.MsgInbox(ctx, who, argBool(a, "unread"))
				if err != nil {
					return "", err
				}
				return jsonish(msgs), nil
			},
		},
		{
			Schema: llm.ToolSchema{Name: "list_approvals",
				Description: "List pending tool-permission prompts waiting for an answer.",
				Parameters:  objSchema(map[string]any{})},
			invoke: func(ctx context.Context, d Daemon, _ map[string]any) (string, error) {
				enabled, views, err := d.Approvals(ctx)
				if err != nil {
					return "", err
				}
				if !enabled {
					return "approvals are disabled", nil
				}
				return jsonish(views), nil
			},
		},
		{
			Schema: llm.ToolSchema{Name: "ctx_get",
				Description: "Read a value from the shared context by key.",
				Parameters:  objSchema(map[string]any{"key": strProp("context key")}, "key")},
			invoke: func(ctx context.Context, d Daemon, a map[string]any) (string, error) {
				key, err := requireStr(a, "key")
				if err != nil {
					return "", err
				}
				e, err := d.CtxGet(ctx, key)
				if err != nil {
					return "", err
				}
				return e.Value, nil
			},
		},
		{
			Schema: llm.ToolSchema{Name: "ctx_list",
				Description: "List shared-context keys, optionally filtered by prefix.",
				Parameters:  objSchema(map[string]any{"prefix": strProp("optional key prefix")})},
			invoke: func(ctx context.Context, d Daemon, a map[string]any) (string, error) {
				entries, err := d.CtxList(ctx, argStr(a, "prefix"))
				if err != nil {
					return "", err
				}
				return jsonish(entries), nil
			},
		},
		{
			Schema: llm.ToolSchema{Name: "pipeline_list",
				Description: "List all pipelines and their status.",
				Parameters:  objSchema(map[string]any{})},
			invoke: func(ctx context.Context, d Daemon, _ map[string]any) (string, error) {
				p, err := d.PipelineList(ctx)
				if err != nil {
					return "", err
				}
				return jsonish(p), nil
			},
		},
		{
			Schema: llm.ToolSchema{Name: "pipeline_get",
				Description: "Get full detail for one pipeline by id.",
				Parameters:  objSchema(map[string]any{"id": strProp("pipeline id")}, "id")},
			invoke: func(ctx context.Context, d Daemon, a map[string]any) (string, error) {
				id, err := requireStr(a, "id")
				if err != nil {
					return "", err
				}
				p, err := d.PipelineGet(ctx, id)
				if err != nil {
					return "", err
				}
				return jsonish(p), nil
			},
		},

		// ---- mutations (confirm gate) ----
		{
			Schema: llm.ToolSchema{Name: "spawn_agent",
				Description: "Spawn an agent. Provide `prompt` for a quick auto-typed agent, or `type`+`repo` for a managed worktree. Use this — never edit code yourself — to do any code work.",
				Parameters: objSchema(map[string]any{
					"type":            strProp("agent type (development/pr-review/code/docs/analysis/...)"),
					"prompt":          strProp("the task for the agent"),
					"repo":            strProp("repo path for a managed worktree"),
					"branch":          strProp("branch"),
					"worktree":        boolProp("isolate in its own worktree"),
					"in_repo":         boolProp("deliberately share the repo"),
					"name":            strProp("human label"),
					"model":           strProp("model override"),
					"permission_mode": strProp("permission mode"),
				})},
			Mutating: true,
			invoke: func(ctx context.Context, d Daemon, a map[string]any) (string, error) {
				s, err := d.Spawn(ctx, client.SpawnParams{
					Type: argStr(a, "type"), Prompt: argStr(a, "prompt"), Repo: argStr(a, "repo"),
					Branch: argStr(a, "branch"), Worktree: argBool(a, "worktree"), InRepo: argBool(a, "in_repo"),
					Name: argStr(a, "name"), Model: argStr(a, "model"), PermissionMode: argStr(a, "permission_mode"),
				})
				if err != nil {
					return "", err
				}
				return "spawned " + s.ID, nil
			},
		},
		{
			Schema: llm.ToolSchema{Name: "send_to_agent",
				Description: "Type a message into a specific agent's session and press Enter.",
				Parameters:  objSchema(map[string]any{"ticket": strProp("agent id"), "text": strProp("message")}, "ticket", "text")},
			Mutating: true,
			invoke: func(ctx context.Context, d Daemon, a map[string]any) (string, error) {
				id, err := requireStr(a, "ticket")
				if err != nil {
					return "", err
				}
				if err := d.Input(ctx, id, argStr(a, "text")); err != nil {
					return "", err
				}
				return "sent to " + id, nil
			},
		},
		{
			Schema: llm.ToolSchema{Name: "terminate_agent",
				Description: "Stop an agent: kill its session. Keeps the record and worktree (reversible via restore_agent).",
				Parameters:  objSchema(map[string]any{"ticket": strProp("agent id")}, "ticket")},
			Mutating: true,
			invoke: func(ctx context.Context, d Daemon, a map[string]any) (string, error) {
				id, err := requireStr(a, "ticket")
				if err != nil {
					return "", err
				}
				if err := d.Terminate(ctx, id); err != nil {
					return "", err
				}
				return "terminated " + id, nil
			},
		},
		{
			Schema: llm.ToolSchema{Name: "delete_agent",
				Description: "Clear an agent's stored record (archives by default; hard=true purges).",
				Parameters:  objSchema(map[string]any{"ticket": strProp("agent id"), "hard": boolProp("purge instead of archive")}, "ticket")},
			Mutating: true,
			invoke: func(ctx context.Context, d Daemon, a map[string]any) (string, error) {
				id, err := requireStr(a, "ticket")
				if err != nil {
					return "", err
				}
				if err := d.Delete(ctx, id, argBool(a, "hard")); err != nil {
					return "", err
				}
				return "deleted " + id, nil
			},
		},
		{
			Schema: llm.ToolSchema{Name: "restore_agent",
				Description: "Restore a terminated/archived agent.",
				Parameters:  objSchema(map[string]any{"ticket": strProp("agent id")}, "ticket")},
			Mutating: true,
			invoke: func(ctx context.Context, d Daemon, a map[string]any) (string, error) {
				id, err := requireStr(a, "ticket")
				if err != nil {
					return "", err
				}
				if err := d.Restore(ctx, id); err != nil {
					return "", err
				}
				return "restored " + id, nil
			},
		},
		{
			Schema: llm.ToolSchema{Name: "approve",
				Description: "Answer a pending tool-permission prompt by 1-based option number.",
				Parameters:  objSchema(map[string]any{"ticket": strProp("agent id"), "option": intProp("1-based option")}, "ticket", "option")},
			Mutating: true,
			invoke: func(ctx context.Context, d Daemon, a map[string]any) (string, error) {
				id, err := requireStr(a, "ticket")
				if err != nil {
					return "", err
				}
				option := argInt(a, "option")
				enabled, views, err := d.Approvals(ctx)
				if err != nil {
					return "", err
				}
				if !enabled {
					return "approvals are disabled", nil
				}
				fp := ""
				for _, v := range views {
					if v.ID == id {
						if option < 1 || option > len(v.Options) {
							return "", fmt.Errorf("option %d out of range (1..%d)", option, len(v.Options))
						}
						fp = v.Fingerprint
						break
					}
				}
				if fp == "" {
					return "", fmt.Errorf("no pending approval for %q", id)
				}
				if err := d.Approve(ctx, id, option, fp); err != nil {
					return "", err
				}
				return fmt.Sprintf("approved %s → %d", id, option), nil
			},
		},
		{
			Schema: llm.ToolSchema{Name: "commit",
				Description: "Stage and commit every change in an agent's worktree on its branch. Omit `message` to have warden write one from the diff.",
				Parameters:  objSchema(map[string]any{"agent": strProp("agent id whose worktree to commit"), "dir": strProp("worktree dir"), "message": strProp("commit message (optional)")}, "agent")},
			Mutating: true,
			invoke: func(ctx context.Context, d Daemon, a map[string]any) (string, error) {
				agent, err := requireStr(a, "agent")
				if err != nil {
					return "", err
				}
				res, err := d.GitCommit(ctx, agent, argStr(a, "dir"), argStr(a, "message"))
				if err != nil {
					return "", err
				}
				return jsonish(res), nil
			},
		},
		{
			Schema: llm.ToolSchema{Name: "push",
				Description: "Push an agent's worktree branch to origin (sets upstream).",
				Parameters:  objSchema(map[string]any{"agent": strProp("agent id"), "dir": strProp("worktree dir")}, "agent")},
			Mutating: true,
			invoke: func(ctx context.Context, d Daemon, a map[string]any) (string, error) {
				agent, err := requireStr(a, "agent")
				if err != nil {
					return "", err
				}
				res, err := d.GitPush(ctx, agent, argStr(a, "dir"))
				if err != nil {
					return "", err
				}
				return jsonish(res), nil
			},
		},
		{
			Schema: llm.ToolSchema{Name: "sync",
				Description: "Fetch origin and rebase an agent's branch onto origin/<base> (default main).",
				Parameters:  objSchema(map[string]any{"agent": strProp("agent id"), "dir": strProp("worktree dir"), "base": strProp("base branch (default main)")}, "agent")},
			Mutating: true,
			invoke: func(ctx context.Context, d Daemon, a map[string]any) (string, error) {
				agent, err := requireStr(a, "agent")
				if err != nil {
					return "", err
				}
				res, err := d.GitSync(ctx, agent, argStr(a, "dir"), argStr(a, "base"))
				if err != nil {
					return "", err
				}
				return jsonish(res), nil
			},
		},
		{
			Schema: llm.ToolSchema{Name: "check",
				Description: "Run an agent's configured checks (.warden/check.yml). Pass `name` for one check or omit to run all.",
				Parameters:  objSchema(map[string]any{"agent": strProp("agent id"), "dir": strProp("worktree dir"), "name": strProp("single check name (optional)")}, "agent")},
			Mutating: true,
			invoke: func(ctx context.Context, d Daemon, a map[string]any) (string, error) {
				agent, err := requireStr(a, "agent")
				if err != nil {
					return "", err
				}
				res, err := d.Check(ctx, agent, argStr(a, "dir"), argStr(a, "name"))
				if err != nil {
					return "", err
				}
				return jsonish(res), nil
			},
		},
		{
			Schema: llm.ToolSchema{Name: "ctx_set",
				Description: "Write a value to the shared context (a key/value store all agents share).",
				Parameters:  objSchema(map[string]any{"key": strProp("context key"), "value": strProp("value")}, "key", "value")},
			Mutating: true,
			invoke: func(ctx context.Context, d Daemon, a map[string]any) (string, error) {
				key, err := requireStr(a, "key")
				if err != nil {
					return "", err
				}
				if _, err := d.CtxSet(ctx, key, argStr(a, "value"), "orchestrator"); err != nil {
					return "", err
				}
				return "set " + key, nil
			},
		},
		{
			Schema: llm.ToolSchema{Name: "send_message",
				Description: "Send a directed message to another agent's inbox (wakes it if idle).",
				Parameters:  objSchema(map[string]any{"to": strProp("recipient agent id"), "body": strProp("message body")}, "to", "body")},
			Mutating: true,
			invoke: func(ctx context.Context, d Daemon, a map[string]any) (string, error) {
				to, err := requireStr(a, "to")
				if err != nil {
					return "", err
				}
				m, woke, err := d.MsgSend(ctx, to, "orchestrator", argStr(a, "body"))
				if err != nil {
					return "", err
				}
				msg := "sent to " + to + " (id " + m.ID + ")"
				if woke {
					msg += " — woke recipient"
				}
				return msg, nil
			},
		},
		{
			Schema: llm.ToolSchema{Name: "pipeline_create",
				Description: "Create a pipeline from a YAML spec.",
				Parameters:  objSchema(map[string]any{"spec": strProp("pipeline spec YAML")}, "spec")},
			Mutating: true,
			invoke: func(ctx context.Context, d Daemon, a map[string]any) (string, error) {
				spec, err := requireStr(a, "spec")
				if err != nil {
					return "", err
				}
				p, err := d.PipelineCreate(ctx, spec)
				if err != nil {
					return "", err
				}
				return "created pipeline " + p.ID, nil
			},
		},
		{
			Schema: llm.ToolSchema{Name: "pipeline_cancel",
				Description: "Cancel a running pipeline by id.",
				Parameters:  objSchema(map[string]any{"id": strProp("pipeline id")}, "id")},
			Mutating: true,
			invoke: func(ctx context.Context, d Daemon, a map[string]any) (string, error) {
				id, err := requireStr(a, "id")
				if err != nil {
					return "", err
				}
				if err := d.PipelineCancel(ctx, id); err != nil {
					return "", err
				}
				return "cancelled pipeline " + id, nil
			},
		},
	}}
}

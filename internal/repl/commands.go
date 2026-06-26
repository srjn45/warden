package repl

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// command is one deterministic `/` verb. Unlike a bare line, it never consults
// the model: build turns the operator's words straight into warden tool calls,
// which then ride the same path the model's calls do — reads auto-execute,
// mutations pass through the confirm gate. This is the reliable half of the
// hybrid REPL: it keeps working even when the local model is misbehaving.
type command struct {
	name         string
	aliases      []string
	usage        string
	summary      string
	takesAgentID bool // hint the Tab completer to offer live agent ids
	build        func(args []string) ([]ToolCall, error)
	// tool is the underlying registry verb. It lets the argument form open even
	// when build can't run (e.g. `/spawn+` or `/spawn` with the required prompt
	// missing), where there is no built call to read the tool name from.
	tool string
	// formAuto opens the interactive argument form (instead of just printing the
	// usage line) when a required argument is missing. requiredFields are the
	// fields that auto-opened form collects.
	formAuto       bool
	requiredFields []string
}

// one builds a single-call result, the common case.
func one(name string, args map[string]any) ([]ToolCall, error) {
	return []ToolCall{{Name: name, Args: args}}, nil
}

// needID pulls the first arg as an agent/pipeline id, erroring with the usage
// line when it is missing.
func needID(args []string, usage string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("usage: " + usage)
	}
	return args[0], nil
}

// commandList is the static, ordered table of deterministic verbs. Order is the
// order /help prints them in: reads first, then mutations.
var commandList = []command{
	// ---- reads ----
	{name: "/agents", aliases: []string{"/ls", "/a"}, usage: "/agents", summary: "list all agents and their status",
		build: func([]string) ([]ToolCall, error) { return one("list_agents", map[string]any{}) }},
	{name: "/agent", usage: "/agent <id>", summary: "full detail for one agent", takesAgentID: true,
		build: func(a []string) ([]ToolCall, error) {
			id, err := needID(a, "/agent <id>")
			if err != nil {
				return nil, err
			}
			return one("get_agent", map[string]any{"ticket": id})
		}},
	{name: "/output", aliases: []string{"/out", "/log"}, usage: "/output <id> [lines]", summary: "recent terminal output of an agent", takesAgentID: true,
		build: func(a []string) ([]ToolCall, error) {
			id, err := needID(a, "/output <id> [lines]")
			if err != nil {
				return nil, err
			}
			args := map[string]any{"ticket": id}
			if len(a) > 1 {
				if n, err := strconv.Atoi(a[1]); err == nil {
					args["lines"] = n
				}
			}
			return one("get_agent_output", args)
		}},
	{name: "/collab", usage: "/collab", summary: "files edited by more than one agent",
		build: func([]string) ([]ToolCall, error) { return one("get_collaboration_status", map[string]any{}) }},
	{name: "/inbox", usage: "/inbox <id> [--unread]", summary: "read an agent's directed messages", takesAgentID: true,
		build: func(a []string) ([]ToolCall, error) {
			id, err := needID(a, "/inbox <id> [--unread]")
			if err != nil {
				return nil, err
			}
			return one("read_inbox", map[string]any{"agent": id, "unread": hasFlag(a, "--unread")})
		}},
	{name: "/approvals", usage: "/approvals", summary: "pending tool-permission prompts",
		build: func([]string) ([]ToolCall, error) { return one("list_approvals", map[string]any{}) }},
	{name: "/ctx", usage: "/ctx [prefix]", summary: "list shared-context keys",
		build: func(a []string) ([]ToolCall, error) {
			args := map[string]any{}
			if len(a) > 0 {
				args["prefix"] = a[0]
			}
			return one("ctx_list", args)
		}},
	{name: "/ctx-get", usage: "/ctx-get <key>", summary: "read one shared-context value",
		build: func(a []string) ([]ToolCall, error) {
			id, err := needID(a, "/ctx-get <key>")
			if err != nil {
				return nil, err
			}
			return one("ctx_get", map[string]any{"key": id})
		}},
	{name: "/pipelines", aliases: []string{"/pl"}, usage: "/pipelines", summary: "list all pipelines",
		build: func([]string) ([]ToolCall, error) { return one("pipeline_list", map[string]any{}) }},
	{name: "/pipeline", usage: "/pipeline <id>", summary: "full detail for one pipeline",
		build: func(a []string) ([]ToolCall, error) {
			id, err := needID(a, "/pipeline <id>")
			if err != nil {
				return nil, err
			}
			return one("pipeline_get", map[string]any{"id": id})
		}},

	// ---- mutations (confirm gate) ----
	{name: "/spawn", usage: "/spawn <prompt...>", summary: "spawn an agent to do a task",
		tool: "spawn_agent", formAuto: true, requiredFields: []string{"prompt"},
		build: func(a []string) ([]ToolCall, error) {
			if len(a) == 0 {
				return nil, errors.New("usage: /spawn <prompt...>")
			}
			return one("spawn_agent", map[string]any{"prompt": strings.Join(a, " ")})
		}},
	{name: "/tell", aliases: []string{"/send"}, usage: "/tell <id> <text...>", summary: "type a message into an agent's session", takesAgentID: true,
		build: func(a []string) ([]ToolCall, error) {
			if len(a) < 2 {
				return nil, errors.New("usage: /tell <id> <text...>")
			}
			return one("send_to_agent", map[string]any{"ticket": a[0], "text": strings.Join(a[1:], " ")})
		}},
	{name: "/stop", aliases: []string{"/terminate", "/kill"}, usage: "/stop <id>", summary: "stop an agent (reversible)", takesAgentID: true,
		build: func(a []string) ([]ToolCall, error) {
			id, err := needID(a, "/stop <id>")
			if err != nil {
				return nil, err
			}
			return one("terminate_agent", map[string]any{"ticket": id})
		}},
	{name: "/restore", usage: "/restore <id>", summary: "restore a stopped/archived agent", takesAgentID: true,
		build: func(a []string) ([]ToolCall, error) {
			id, err := needID(a, "/restore <id>")
			if err != nil {
				return nil, err
			}
			return one("restore_agent", map[string]any{"ticket": id})
		}},
	{name: "/rm", aliases: []string{"/delete"}, usage: "/rm <id> [--hard]", summary: "clear an agent's record", takesAgentID: true,
		build: func(a []string) ([]ToolCall, error) {
			id, err := needID(a, "/rm <id> [--hard]")
			if err != nil {
				return nil, err
			}
			return one("delete_agent", map[string]any{"ticket": id, "hard": hasFlag(a, "--hard")})
		}},
	{name: "/commit", usage: "/commit <id> [message...]", summary: "commit an agent's worktree", takesAgentID: true,
		build: func(a []string) ([]ToolCall, error) {
			id, err := needID(a, "/commit <id> [message...]")
			if err != nil {
				return nil, err
			}
			args := map[string]any{"agent": id}
			if len(a) > 1 {
				args["message"] = strings.Join(a[1:], " ")
			}
			return one("commit", args)
		}},
	{name: "/push", usage: "/push <id>", summary: "push an agent's branch", takesAgentID: true,
		build: func(a []string) ([]ToolCall, error) {
			id, err := needID(a, "/push <id>")
			if err != nil {
				return nil, err
			}
			return one("push", map[string]any{"agent": id})
		}},
	{name: "/sync", usage: "/sync <id> [base]", summary: "rebase an agent's branch on its base", takesAgentID: true,
		build: func(a []string) ([]ToolCall, error) {
			id, err := needID(a, "/sync <id> [base]")
			if err != nil {
				return nil, err
			}
			args := map[string]any{"agent": id}
			if len(a) > 1 {
				args["base"] = a[1]
			}
			return one("sync", args)
		}},
	{name: "/check", usage: "/check <id> [name]", summary: "run an agent's project checks", takesAgentID: true,
		build: func(a []string) ([]ToolCall, error) {
			id, err := needID(a, "/check <id> [name]")
			if err != nil {
				return nil, err
			}
			args := map[string]any{"agent": id}
			if len(a) > 1 {
				args["name"] = a[1]
			}
			return one("check", args)
		}},
	{name: "/ctx-set", usage: "/ctx-set <key> <value...>", summary: "write a shared-context value",
		build: func(a []string) ([]ToolCall, error) {
			if len(a) < 2 {
				return nil, errors.New("usage: /ctx-set <key> <value...>")
			}
			return one("ctx_set", map[string]any{"key": a[0], "value": strings.Join(a[1:], " ")})
		}},
	{name: "/msg", usage: "/msg <id> <body...>", summary: "send a directed message to an agent", takesAgentID: true,
		build: func(a []string) ([]ToolCall, error) {
			if len(a) < 2 {
				return nil, errors.New("usage: /msg <id> <body...>")
			}
			return one("send_message", map[string]any{"to": a[0], "body": strings.Join(a[1:], " ")})
		}},
	{name: "/approve", usage: "/approve <id> <option>", summary: "answer a pending approval by option number", takesAgentID: true,
		build: func(a []string) ([]ToolCall, error) {
			if len(a) < 2 {
				return nil, errors.New("usage: /approve <id> <option>")
			}
			n, err := strconv.Atoi(a[1])
			if err != nil {
				return nil, errors.New("option must be a number")
			}
			return one("approve", map[string]any{"ticket": a[0], "option": n})
		}},
	{name: "/cancel", usage: "/cancel <pipeline-id>", summary: "cancel a pipeline",
		build: func(a []string) ([]ToolCall, error) {
			id, err := needID(a, "/cancel <pipeline-id>")
			if err != nil {
				return nil, err
			}
			return one("pipeline_cancel", map[string]any{"id": id})
		}},
}

// commandIndex maps every name and alias to its command for O(1) lookup.
var commandIndex = func() map[string]command {
	m := make(map[string]command, len(commandList)*2)
	for _, c := range commandList {
		m[c.name] = c
		for _, a := range c.aliases {
			m[a] = c
		}
	}
	return m
}()

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// RunCommand executes a deterministic `/` line and reports the result. The bool
// is false only when line is not a slash command at all (so the caller routes it
// to the model instead). An unknown `/verb` is still handled here — with a hint
// — so a typo never silently falls through to the model.
func (s *Session) RunCommand(ctx context.Context, line string) (string, bool) {
	fields := strings.Fields(line)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return "", false
	}
	name := fields[0]
	if name == "/help" || name == "/?" || name == "/commands" {
		return commandHelp(), true
	}
	// A trailing `+` on the verb (e.g. `/spawn+`) opts into the full argument
	// form: every fillable field is offered, not just what was typed.
	full := false
	if len(name) > 1 && strings.HasSuffix(name, "+") {
		full, name = true, strings.TrimSuffix(name, "+")
	}
	// Allow `--hard`-style flags to be dropped before the positional args by
	// leaving them in place; builders read the flags they care about.
	cmd, ok := commandIndex[name]
	if !ok {
		return fmt.Sprintf("unknown command %s — type /help for the list", name), true
	}
	args := fields[1:]
	query := strings.Join(args, " ") // operator's words, for the form's LLM pre-fill
	calls, err := cmd.build(args)
	switch {
	case full:
		return s.runForm(ctx, cmd, calls, err, query, formFull), true
	case err != nil && cmd.formAuto && s.canForm():
		return s.runForm(ctx, cmd, calls, err, query, formRequired), true
	case err != nil:
		return err.Error(), true
	default:
		return s.runPlan(ctx, calls), true
	}
}

// formMode selects how many fields a /-command form offers.
type formMode int

const (
	formRequired formMode = iota // only the command's required fields (auto-open)
	formFull                     // every fillable field (the `+` gesture)
)

// canForm reports whether the gate can drive an interactive form (a real *Gate
// over a terminal; the scripted gate in tests is not).
func (s *Session) canForm() bool {
	_, ok := s.gate.(*Gate)
	return ok
}

// runForm collects a /-command's arguments interactively and then runs the
// completed call through the normal plan path — reads execute, mutations still
// pass the confirm gate. seed comes from whatever build produced; if build
// couldn't run (a missing required arg, or a bare `/spawn+`), the command's
// declared tool name seeds an empty call so the form can still open.
func (s *Session) runForm(ctx context.Context, cmd command, calls []ToolCall, buildErr error, query string, mode formMode) string {
	g, ok := s.gate.(*Gate)
	if !ok { // non-interactive gate (tests / non-TTY): no form, plain behaviour
		if buildErr != nil {
			return buildErr.Error()
		}
		return s.runPlan(ctx, calls)
	}
	var seed ToolCall
	switch {
	case len(calls) > 0:
		seed = calls[0]
	case cmd.tool != "":
		seed = ToolCall{Name: cmd.tool}
	default:
		if buildErr != nil {
			return buildErr.Error()
		}
		return "nothing to do"
	}
	fields := g.formFields(seed.Name)
	if mode == formRequired {
		fields = g.formFields(seed.Name, cmd.requiredFields...)
	}
	done := g.Form(ctx, query, seed, fields)
	return s.runPlan(ctx, []ToolCall{done})
}

// commandHelp renders the `/help` listing: every command with its usage and a
// one-line summary, plus the always-available specials.
func commandHelp() string {
	var b strings.Builder
	b.WriteString("Commands (deterministic — no model):\n")
	for _, c := range commandList {
		fmt.Fprintf(&b, "  %-26s %s\n", c.usage, c.summary)
	}
	b.WriteString("\nAlso:\n")
	b.WriteString("  /help                      this list\n")
	b.WriteString("  !<cmd>                     run a shell command in your $SHELL\n")
	b.WriteString("  <anything else>            ask the orchestrator in natural language\n")
	b.WriteString("  Ctrl-D / exit              close interactive mode\n")
	return strings.TrimRight(b.String(), "\n")
}

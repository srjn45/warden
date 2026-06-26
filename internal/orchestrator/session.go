package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/srjn45/warden/internal/llm"
	"github.com/srjn45/warden/internal/store"
)

// maxTurns bounds the Chat turn loop so a confused model can't call tools
// forever; past it the operator gets an honest "couldn't complete".
const maxTurns = 6

const systemPrompt = `You are warden's orchestrator: a conductor for a fleet of Claude Code coding agents. You turn the operator's natural-language intent into warden tool calls. You never write or edit code yourself — to do ANY code work (review, refactor, fix, docs, analysis), you spawn a Claude agent and let IT do the work.

warden concepts:
- An "agent" is a Claude Code session working in its own git worktree. Spawn one with spawn_agent; watch it with list_agents / get_agent / get_agent_output; steer it with send_to_agent; stop it with terminate_agent.
- A "pipeline" is a multi-stage DAG of dependent agent jobs (pipeline_create / pipeline_list / pipeline_get / pipeline_cancel).
- Agents share a key/value blackboard (ctx_get / ctx_set / ctx_list) and can message each other (send_message / read_inbox).
- The git lifecycle for an agent's worktree runs through commit / push / sync; its project checks through check.

How to act:
- To satisfy a request like "review/refactor/fix/document X", call spawn_agent with a clear, self-contained "prompt" describing the task. That is almost always the right move — do it; don't just describe it.
- Every tool call is shown to the operator, who approves it before it runs. So propose the call; don't ask permission in prose.
- After a tool runs you get its result back. Report the outcome concisely (e.g. the new agent's id), or the error.

spawn_agent rules (follow EXACTLY — bad args are the #1 failure):
- For a normal request, pass ONLY "prompt" (and optionally a short "name"). warden fills in sensible defaults for everything else.
- NEVER invent a "repo" path — omit it unless the operator gave you a real path. "/path/to/..." is never valid.
- NEVER set "model" unless the operator explicitly named a model. There is no "gpt-4"; warden uses Claude models (sonnet, opus, haiku, fable).
- Omit "type", "branch", "worktree", "in_repo", "permission_mode" unless the operator asked for them. Do not guess values.
- Put any file paths or context the agent needs INSIDE the prompt text, not in other fields.

Be concise. Never emit a tool call as plain text or JSON in your reply — use the tool-call mechanism.`

// Session is one operator's running conversation with the orchestrator. It owns
// the message history and ties the Chatter, registry, confirm gate, and tier
// router together. Built against fakes in tests; against the real client + a
// local Ollama in `wd orch`.
type Session struct {
	chat llm.Chatter
	daem Daemon
	reg  *Registry
	gate confirmer
	tier *Router
	msgs []llm.Message
}

// NewSession wires a session. router may be nil (then every request plans
// locally); gate is the confirm seam (a *Gate in the REPL, a scripted fake in
// tests).
func NewSession(chat llm.Chatter, d Daemon, reg *Registry, gate confirmer, router *Router) *Session {
	// The supervision verbs (fleet_digest / clean_up / …) ride the same loop and
	// gate. Their condenser reuses the local model when it is also a Completer
	// (the *Ollama provider is both); otherwise the Monitor falls back to a
	// deterministic table.
	comp, _ := chat.(llm.Completer)
	reg.AddMonitoring(NewMonitorWithGate(d, NewCondenser(comp), gate))
	// Hand the gate the tool schemas so its [e]dit flow can prompt field-by-field
	// (a tool may expose a field the model omitted, e.g. branch).
	if g, ok := gate.(*Gate); ok {
		g.useRegistry(reg)
	}
	return &Session{chat: chat, daem: d, reg: reg, gate: gate, tier: router}
}

// Handle runs one operator line to completion (the model yields prose) or to the
// turn budget. Read-only calls auto-execute; mutating calls go through the gate;
// every tool result feeds back so the model sees what happened.
func (s *Session) Handle(ctx context.Context, line string) string {
	s.msgs = append(s.msgs, llm.Message{Role: llm.RoleUser, Content: line})

	// Capability-tier routing, before the expensive planning turn.
	if s.tier != nil {
		switch r := s.tier.Route(ctx, line); r.Mode {
		case Degrade:
			return r.OperatorMessage
		case Escalate:
			return s.runPlan(ctx, r.Calls)
		}
	}

	for turn := 0; turn < maxTurns; turn++ {
		reply, err := s.chat.Chat(ctx, s.withContext(ctx), s.reg.ToolSchemas())
		if err != nil {
			return s.surface(err)
		}
		s.msgs = append(s.msgs, assistantMsg(reply))
		if len(reply.ToolCalls) == 0 {
			return reply.Text // model yielded prose — done
		}
		s.runCalls(ctx, reply.ToolCalls)
	}
	return "couldn't complete that within the turn budget — try a smaller ask"
}

// runPlan executes an already-drafted plan (e.g. from a Claude escalation): the
// mutations still pass through the confirm gate. It reports each call's real
// result — an honest "spawn_agent: error: …" beats a canned "done" when a
// dispatch actually failed (the operator must see when nothing happened).
func (s *Session) runPlan(ctx context.Context, calls []ToolCall) string {
	if len(calls) == 0 {
		return "nothing to do"
	}
	start := len(s.msgs)
	s.runCalls(ctx, calls)
	var b strings.Builder
	for _, m := range s.msgs[start:] {
		if m.Role == llm.RoleTool {
			fmt.Fprintf(&b, "%s: %s\n", m.ToolName, m.Content)
		}
	}
	if b.Len() == 0 {
		return "done"
	}
	return strings.TrimRight(b.String(), "\n")
}

// runCalls partitions a batch: read-only calls dispatch immediately; the
// mutating ones are confirmed as one unit, then dispatched (or rejected). Each
// result (or error) is appended as a tool message the model sees next turn.
func (s *Session) runCalls(ctx context.Context, calls []ToolCall) {
	var mutating []ToolCall
	for _, c := range calls {
		tl, ok := s.reg.Lookup(c.Name)
		if !ok {
			s.recordTool(c.Name, fmt.Sprintf("unknown tool %q — choose one of the provided tools", c.Name))
			continue
		}
		if tl.Mutating {
			mutating = append(mutating, c)
			continue
		}
		s.dispatch(ctx, c)
	}
	if len(mutating) == 0 {
		return
	}
	switch d := s.gate.Confirm(mutating); d.Action {
	case Approve, Edit:
		for _, c := range d.Calls {
			s.dispatch(ctx, c)
		}
	default: // Reject
		for _, c := range mutating {
			s.recordTool(c.Name, "operator rejected this action")
		}
	}
}

func (s *Session) dispatch(ctx context.Context, c ToolCall) {
	res, err := s.reg.Dispatch(ctx, s.daem, c)
	if err != nil {
		s.recordTool(c.Name, "error: "+err.Error())
		return
	}
	s.recordTool(c.Name, res)
}

func (s *Session) recordTool(name, content string) {
	s.msgs = append(s.msgs, llm.Message{Role: llm.RoleTool, ToolName: name, Content: content})
}

// withContext prepends the system prompt + a compact fleet snapshot to the
// running history for the Chat call. The snapshot is one line per agent (not
// full records) so it stays cheap.
func (s *Session) withContext(ctx context.Context) []llm.Message {
	sys := systemPrompt
	if snap := s.fleetSnapshot(ctx); snap != "" {
		sys += "\n\nCurrent fleet:\n" + snap
	}
	out := make([]llm.Message, 0, len(s.msgs)+1)
	out = append(out, llm.Message{Role: llm.RoleSystem, Content: sys})
	return append(out, s.msgs...)
}

func (s *Session) fleetSnapshot(ctx context.Context) string {
	sessions, err := s.daem.List(ctx)
	if err != nil || len(sessions) == 0 {
		return ""
	}
	var b strings.Builder
	for _, sess := range sessions {
		fmt.Fprintf(&b, "- %s [%s] %s\n", sess.ID, sess.Status, oneLine(sess))
	}
	return b.String()
}

func oneLine(s *store.Session) string {
	if s.Subject != "" {
		return s.Subject
	}
	return string(s.Type)
}

func (s *Session) surface(err error) string {
	return "the local model is unavailable right now (" + err.Error() + ") — try again, or use the warden CLI directly"
}

func assistantMsg(r llm.Reply) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: r.Text, ToolCalls: r.ToolCalls}
}

// ObserveShell records a `!` command and its (tail-bounded) output into the
// history as plain context the model may later be asked about — WITHOUT invoking
// the model. This is the passivity invariant in one place: `!` output is
// observed and reported; the orchestrator initiates no action from it. The
// command only enters the model's view on the next *bare* line the operator
// types (e.g. "what went wrong?").
func (s *Session) ObserveShell(line string, res RunResult) {
	obs := fmt.Sprintf("[operator ran shell: %s] (exit %d)\n%s", line, res.ExitCode, res.Captured)
	s.msgs = append(s.msgs, llm.Message{Role: llm.RoleUser, Content: obs})
}

// RunREPL is the read-line → (shell | Handle) → print loop for `wd orch`. It
// shares one input scanner with the confirm gate (when the gate is a *Gate) so
// the gate's approve/edit/reject read doesn't race the REPL's line read on the
// same stdin. A `!`-prefixed line is a raw command run in the operator's own
// shell (sh, the persistent ShellRunner); its output streams verbatim and is
// only OBSERVED — never acted on, even on failure. A bare line goes to the
// model via Handle.
func RunREPL(ctx context.Context, s *Session, sh ShellRunner, r io.Reader, w io.Writer) error {
	st := newStyler(w)
	lr := newLineReader(s, r, w, historyFilePath())
	defer lr.Close()
	if g, ok := s.gate.(*Gate); ok {
		g.useReader(lr, st) // single source of truth for stdin
	}
	fmt.Fprintln(w, st.banner.Render("warden interactive — natural-language conductor over your fleet"))
	fmt.Fprintln(w, st.hint.Render("↑/↓ history · type / for commands · Tab complete · !cmd shell · Ctrl-D to exit"))
	for {
		line, err := lr.Prompt(st.Promptf())
		if errors.Is(err, errInterrupted) {
			continue // Ctrl-C abandons the line, keeps the session
		}
		if err != nil {
			return nil // EOF / Ctrl-D closes interactive mode
		}
		line = strings.TrimSpace(line)
		switch {
		case line == "":
		case line == "exit", line == "quit":
			return nil
		case strings.HasPrefix(line, "!"):
			runBang(ctx, s, sh, strings.TrimPrefix(line, "!"), w)
		case strings.HasPrefix(line, "/"):
			if out, handled := s.RunCommand(ctx, line); handled {
				fmt.Fprintln(w, out)
			}
		default:
			fmt.Fprintln(w, s.Handle(ctx, line))
		}
	}
}

// runBang executes a `!` command in the operator's shell and stays passive: the
// shell streams its output verbatim to w; we surface a non-zero exit and record
// the run as context. Crucially we never call the model from here — not even on
// failure. The operator drives any follow-up.
func runBang(ctx context.Context, s *Session, sh ShellRunner, cmd string, w io.Writer) {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return
	}
	if sh == nil {
		fmt.Fprintln(w, "(no shell available)")
		return
	}
	res, err := sh.Run(ctx, cmd)
	if err != nil {
		fmt.Fprintf(w, "(shell error: %v)\n", err)
		return
	}
	if res.ExitCode != 0 {
		fmt.Fprintf(w, "[exit %d]\n", res.ExitCode)
	}
	s.ObserveShell(cmd, res)
}

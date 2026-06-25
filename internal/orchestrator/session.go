package orchestrator

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/srjn45/warden/internal/llm"
	"github.com/srjn45/warden/internal/store"
)

// maxTurns bounds the Chat turn loop so a confused model can't call tools
// forever; past it the operator gets an honest "couldn't complete".
const maxTurns = 6

const systemPrompt = `You are warden's orchestrator. You conduct agents and pipelines and run the git/check lifecycle through the provided tools. You never write or edit code — to do any code work, spawn a Claude agent with spawn_agent. Propose tool calls; the operator confirms every mutation. Be concise.`

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
// mutations still pass through the confirm gate.
func (s *Session) runPlan(ctx context.Context, calls []ToolCall) string {
	if len(calls) == 0 {
		return "nothing to do"
	}
	s.runCalls(ctx, calls)
	return "done — see the results above"
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

// RunREPL is the read-line → Handle → print loop for `wd orch`. It shares one
// input scanner with the confirm gate (when the gate is a *Gate) so the gate's
// approve/edit/reject read doesn't race the REPL's line read on the same stdin.
func RunREPL(ctx context.Context, s *Session, r io.Reader, w io.Writer) error {
	sc := bufio.NewScanner(r)
	if g, ok := s.gate.(*Gate); ok {
		g.in = sc // single source of truth for stdin
	}
	fmt.Fprintln(w, "warden orchestrator — natural-language conductor. Type a request, or 'exit'.")
	fmt.Fprint(w, "warden› ")
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch line {
		case "":
		case "exit", "quit":
			return nil
		default:
			fmt.Fprintln(w, s.Handle(ctx, line))
		}
		fmt.Fprint(w, "warden› ")
	}
	return sc.Err()
}

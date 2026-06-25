package orchestrator

import (
	"context"
	"fmt"
	"strings"

	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/llm"
	"github.com/srjn45/warden/internal/store"
)

// maxDigestLines bounds the per-agent output the condenser sees and the
// deterministic fallback prints, so a runaway log never floods the operator.
const maxDigestLines = 20

// Condenser turns a compact fact list into a short operator digest. Backed by
// the local model; any failure falls back to a deterministic table at the call
// site (condenseOr), never an error to the operator.
type Condenser interface {
	Condense(ctx context.Context, facts string) (string, error)
}

// Monitor answers the high-frequency, low-stakes fleet questions ("what's
// running / stuck", "what's X doing", "anything waiting on me", "clean up") as
// fixed read recipes plus a condensation pass — a digest, not raw JSON. The only
// mutating verb, CleanUp, routes its proposal through the existing confirm gate.
type Monitor struct {
	d    Daemon
	c    Condenser
	gate confirmer
}

// NewMonitor builds a read-only monitor.
func NewMonitor(d Daemon, c Condenser) *Monitor { return &Monitor{d: d, c: c} }

// NewMonitorWithGate builds a monitor that can also propose teardown through the
// given confirm gate (the same gate the session loop uses).
func NewMonitorWithGate(d Daemon, c Condenser, gate confirmer) *Monitor {
	return &Monitor{d: d, c: c, gate: gate}
}

// FleetDigest condenses "what's running / what's stuck" — one compact line per
// agent through the condenser, with a deterministic table as the fallback.
func (m *Monitor) FleetDigest(ctx context.Context) (string, error) {
	sessions, err := m.d.List(ctx)
	if err != nil {
		return "", err
	}
	if len(sessions) == 0 {
		return "nothing running", nil
	}
	facts := summarizeFleet(sessions)
	return m.condenseOr(ctx, facts, facts), nil
}

// AgentDigest condenses what one agent is doing from its recent output tail. An
// unknown id returns a clean message, never an error.
func (m *Monitor) AgentDigest(ctx context.Context, id string) (string, error) {
	out, err := m.d.Output(ctx, id, 200)
	if err != nil {
		return "no such agent " + id + " (or no output yet)", nil
	}
	return m.condenseOr(ctx, out, tail(out, maxDigestLines)), nil
}

// PendingForMe summarizes what's awaiting the operator: pending approval prompts.
func (m *Monitor) PendingForMe(ctx context.Context) (string, error) {
	enabled, approvals, err := m.d.Approvals(ctx)
	if err != nil {
		return "", err
	}
	if !enabled || len(approvals) == 0 {
		return "nothing waiting on you", nil
	}
	facts := summarizePending(approvals)
	return m.condenseOr(ctx, facts, facts), nil
}

// CleanUp proposes terminating terminal (done/errored/orphaned) agents and
// routes the proposal through the confirm gate — never an automatic reap. Active
// agents are never in the proposed set.
func (m *Monitor) CleanUp(ctx context.Context) (string, error) {
	sessions, err := m.d.List(ctx)
	if err != nil {
		return "", err
	}
	terminal := filterTerminal(sessions)
	if len(terminal) == 0 {
		return "nothing to clean up — no terminal agents", nil
	}
	if m.gate == nil {
		return "", fmt.Errorf("clean up needs a confirm gate")
	}
	calls := toTerminateCalls(terminal)
	d := m.gate.Confirm(calls)
	if d.Action != Approve && d.Action != Edit {
		return "left them as-is", nil
	}
	return m.runTeardown(ctx, d.Calls)
}

func (m *Monitor) runTeardown(ctx context.Context, calls []ToolCall) (string, error) {
	var reaped []string
	for _, c := range calls {
		id := argStr(c.Args, "ticket")
		if id == "" {
			continue
		}
		if err := m.d.Terminate(ctx, id); err != nil {
			return "", fmt.Errorf("terminate %s: %w", id, err)
		}
		reaped = append(reaped, id)
	}
	return "cleaned up: " + strings.Join(reaped, ", "), nil
}

// condenseOr tries the model condensation; on any failure or empty result it
// returns the deterministic fallback. Never errors out to the operator.
func (m *Monitor) condenseOr(ctx context.Context, facts, fallback string) string {
	if m.c != nil {
		if out, err := m.c.Condense(ctx, facts); err == nil && strings.TrimSpace(out) != "" {
			return out
		}
	}
	return fallback
}

// summarizeFleet renders one compact line per agent: id, state, and (for blocked
// ones) what they're waiting on. Kept small — it's the condenser's input and the
// deterministic fallback.
func summarizeFleet(sessions []*store.Session) string {
	var running, stuck []string
	for _, s := range sessions {
		line := fmt.Sprintf("%s [%s] %s", s.ID, s.Status, oneLine(s))
		if isStuck(s.Status) {
			stuck = append(stuck, line)
		} else {
			running = append(running, line)
		}
	}
	var b strings.Builder
	if len(running) > 0 {
		b.WriteString("running:\n")
		for _, l := range running {
			b.WriteString("  - " + l + "\n")
		}
	}
	if len(stuck) > 0 {
		b.WriteString("blocked/terminal:\n")
		for _, l := range stuck {
			b.WriteString("  - " + l + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func summarizePending(approvals []approval.View) string {
	var b strings.Builder
	b.WriteString("pending approvals:\n")
	for _, v := range approvals {
		fmt.Fprintf(&b, "  - %s: %s\n", v.ID, v.Question)
	}
	return strings.TrimRight(b.String(), "\n")
}

// isStuck reports whether a status means the agent is blocked or terminal (i.e.
// not making progress) — what an operator wants surfaced.
func isStuck(s store.Status) bool {
	switch s {
	case store.StatusWaitingForInput, store.StatusErrored, store.StatusDone,
		store.StatusOrphaned, store.StatusRateLimited:
		return true
	}
	return false
}

// filterTerminal returns the agents safe to reap: done / errored / orphaned.
// Active or waiting agents are never included.
func filterTerminal(sessions []*store.Session) []*store.Session {
	var out []*store.Session
	for _, s := range sessions {
		switch s.Status {
		case store.StatusDone, store.StatusErrored, store.StatusOrphaned:
			out = append(out, s)
		}
	}
	return out
}

func toTerminateCalls(sessions []*store.Session) []ToolCall {
	calls := make([]ToolCall, 0, len(sessions))
	for _, s := range sessions {
		calls = append(calls, ToolCall{Name: "terminate_agent", Args: map[string]any{"ticket": s.ID}})
	}
	return calls
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// completerCondenser backs Condenser with an llm.Completer (the same local model
// the orchestrator uses). It bounds its input and returns the model's terse
// summary; the caller (condenseOr) handles any failure as a deterministic
// fallback, so this never has to.
type completerCondenser struct{ c llm.Completer }

// NewCondenser wraps a Completer as a Condenser. A nil Completer yields a nil
// Condenser, which Monitor treats as "always use the deterministic table".
func NewCondenser(c llm.Completer) Condenser {
	if c == nil {
		return nil
	}
	return completerCondenser{c: c}
}

// AddMonitoring appends the supervision verbs to the registry as model-callable
// tools, so the existing loop and gate handle them with no special-casing. The
// digests are read-only (auto-execute); clean_up is read-only to the loop but
// self-gates the expanded terminate proposal through the same confirm gate, so
// the operator confirms the concrete reap, not an opaque "clean_up".
func (r *Registry) AddMonitoring(m *Monitor) {
	r.tools = append(r.tools,
		Tool{
			Schema: llm.ToolSchema{Name: "fleet_digest",
				Description: "Condensed 'what's running / what's stuck' across the whole fleet — prefer this over list_agents when the operator asks how things are going.",
				Parameters:  objSchema(map[string]any{})},
			invoke: func(ctx context.Context, _ Daemon, _ map[string]any) (string, error) {
				return m.FleetDigest(ctx)
			},
		},
		Tool{
			Schema: llm.ToolSchema{Name: "agent_digest",
				Description: "Condensed summary of what one agent is currently doing.",
				Parameters:  objSchema(map[string]any{"ticket": strProp("agent id")}, "ticket")},
			invoke: func(ctx context.Context, _ Daemon, a map[string]any) (string, error) {
				id, err := requireStr(a, "ticket")
				if err != nil {
					return "", err
				}
				return m.AgentDigest(ctx, id)
			},
		},
		Tool{
			Schema: llm.ToolSchema{Name: "pending_for_me",
				Description: "Summary of what's awaiting the operator (pending approval prompts).",
				Parameters:  objSchema(map[string]any{})},
			invoke: func(ctx context.Context, _ Daemon, _ map[string]any) (string, error) {
				return m.PendingForMe(ctx)
			},
		},
		Tool{
			Schema: llm.ToolSchema{Name: "clean_up",
				Description: "Propose tearing down terminal (done/errored/orphaned) agents. The operator confirms the exact set before anything is reaped; active agents are never touched.",
				Parameters:  objSchema(map[string]any{})},
			invoke: func(ctx context.Context, _ Daemon, _ map[string]any) (string, error) {
				return m.CleanUp(ctx)
			},
		},
	)
}

func (cc completerCondenser) Condense(ctx context.Context, facts string) (string, error) {
	prompt := "Summarize this warden fleet status for an operator in 1-2 short sentences. " +
		"Name anything blocked or waiting. Be terse, no preamble.\n\n" + tail(facts, 60)
	return cc.c.Complete(ctx, prompt)
}

package ptyhost

import (
	"context"
	"sync"
	"testing"

	"github.com/srjn45/warden/internal/ptyevent"
)

type seenSet struct {
	mu sync.Mutex
	n  map[string]int
}

func (s *seenSet) add(name string, _ ptyevent.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.n == nil {
		s.n = map[string]int{}
	}
	s.n[name]++
}
func (s *seenSet) get(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n[name]
}

// G5 / F-KIND-02: AI-only observers never receive a plain shell's events,
// whatever the runtime mode, and a shell is recognised by creation path or
// durable record — never by name.
func TestObserveAgentsNeverSeesTerminals(t *testing.T) {
	for _, mode := range []Mode{ModeOff, ModeShadow, ModeOn} {
		t.Run(string(mode), func(t *testing.T) {
			m := newManager(t, tmuxRunner{}, t.TempDir(), mode)
			defer m.Close()
			var agents, all seenSet
			m.ObserveAgents(agents.add)
			m.Observe(all.add)

			m.MarkTerminal("shell-1")
			m.dispatch("shell-1", ptyevent.Event{})
			m.dispatch("agent-1", ptyevent.Event{})
			if agents.get("shell-1") != 0 {
				t.Fatal("AI observer received a shell event")
			}
			if agents.get("agent-1") != 1 {
				t.Fatal("AI observer must still receive agent events")
			}
			if all.get("shell-1") != 1 {
				t.Fatal("kind-neutral observer must see shells (lifecycle bookkeeping)")
			}
		})
	}
}

// After a daemon restart the in-memory marks are gone; the durable terminal
// record decides — including when an agent record shares the name.
func TestTerminalKindFromRecordSurvivesRestart(t *testing.T) {
	m := newManager(t, tmuxRunner{}, t.TempDir(), ModeShadow)
	defer m.Close()
	records := map[string]bool{"shell-1": true}
	m.SetTerminalResolver(func(name string) bool { return records[name] })
	var agents seenSet
	m.ObserveAgents(agents.add)

	m.dispatch("shell-1", ptyevent.Event{}) // an agent of the same name may exist; kind wins
	m.dispatch("agent-1", ptyevent.Event{})
	if agents.get("shell-1") != 0 || agents.get("agent-1") != 1 {
		t.Fatalf("shell=%d agent=%d", agents.get("shell-1"), agents.get("agent-1"))
	}
}

// An agent that takes over a name previously used by a terminal is an agent:
// creating it through the agent host clears the stale shell mark.
func TestAgentReusingTerminalNameIsAnAgent(t *testing.T) {
	run := newTmux(t)
	m := newManager(t, run, t.TempDir(), ModeOff)
	defer m.Close()
	ctx := context.Background()
	m.MarkTerminal("x")
	if !m.IsTerminal("x") {
		t.Fatal("mark lost")
	}
	if err := m.Host().NewSession(ctx, "x", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	defer m.Host().KillSession(ctx, "x")
	if m.IsTerminal("x") {
		t.Fatal("agent session inherited a stale terminal mark")
	}
}

// Terminals created through the terminal host are marked before any event.
func TestTerminalHostMarksKind(t *testing.T) {
	run := newTmux(t)
	m := newManager(t, run, t.TempDir(), ModeOff)
	defer m.Close()
	ctx := context.Background()
	if err := m.TerminalHost().NewSession(ctx, "t1", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	defer m.Host().KillSession(ctx, "t1")
	if !m.IsTerminal("t1") {
		t.Fatal("terminal host creation must mark the session as a shell")
	}
}

package ptyhost

import (
	"context"
	"errors"
	"testing"

	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/srjn45/warden/internal/ptyruntime"
)

func seq(o ptyevent.Origin, s string) ptyevent.Sequence {
	return ptyevent.Sequence{Origin: o, Reason: "test", Parts: []ptyevent.SeqPart{{Data: []byte(s)}}}
}

// Automatic input reaches only directly hosted AI agents. A shell (including any
// AI CLI a human launched inside it) refuses warden-origin input outright; a
// legacy tmux session is reported not-direct so the caller keeps its own path.
func TestAuthoredInputBoundaries(t *testing.T) {
	noLeaks(t)
	ctx := context.Background()
	m, _ := newNoTmuxManager(t, ModeOn)
	defer m.Close()
	m.SetAgentDirectReady(true)
	h := m.Host().(routed)
	dir := t.TempDir()

	if err := h.LaunchCommand(ctx, "agent-1", dir, []string{"/bin/sh", "-c", `printf 'ready\n'; cat`}); err != nil {
		t.Fatal(err)
	}
	if err := m.TerminalHost().NewSession(ctx, "term-1", dir); err != nil {
		t.Fatal(err)
	}
	waitScreen(t, m, "agent-1", "ready")

	if err := h.AuthoredInput(ctx, "agent-1", seq(ptyevent.OriginWarden, "hi\n")); err != nil {
		t.Fatalf("agent: %v", err)
	}
	waitScreen(t, m, "agent-1", "hi")
	if err := h.AuthoredInput(ctx, "agent-1", seq(ptyevent.OriginWarden, "hi\n")); err != nil {
		t.Fatalf("no once-key means no dedupe: %v", err)
	}
	once := seq(ptyevent.OriginWarden, "seed\n")
	once.OnceKey = "seed:agent-1"
	if err := h.AuthoredInput(ctx, "agent-1", once); err != nil {
		t.Fatal(err)
	}
	if err := h.AuthoredInput(ctx, "agent-1", once); !errors.Is(err, ptyruntime.ErrDuplicate) {
		t.Fatalf("replayed once-only: %v", err)
	}

	if err := h.AuthoredInput(ctx, "term-1", seq(ptyevent.OriginWarden, "rm -rf x\n")); !errors.Is(err, ErrNotManagedAI) {
		t.Fatalf("warden input to a shell: %v, want ErrNotManagedAI", err)
	}
	if err := m.TerminalHost().(routed).AuthoredInput(ctx, "agent-1", seq(ptyevent.OriginWarden, "x")); !errors.Is(err, ErrNotManagedAI) {
		t.Fatalf("terminal-scoped host must not drive an agent automatically: %v", err)
	}
	if err := h.AuthoredInput(ctx, "legacy-tmux", seq(ptyevent.OriginWarden, "x")); !errors.Is(err, ErrNotDirect) {
		t.Fatalf("legacy tmux session: %v, want ErrNotDirect", err)
	}
}

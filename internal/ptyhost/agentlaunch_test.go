package ptyhost

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/ptyruntime"
)

// noTmux fails every command: proves a launch needs no agent-hosting tmux.
type noTmux struct{ calls int }

func (n *noTmux) Run(context.Context, string, string, ...string) (string, error) {
	n.calls++
	return "", errors.New("tmux unavailable")
}

type uncertainAgentLauncher struct{ adopts, launches int }

func (f *uncertainAgentLauncher) Has(string) bool { return true }
func (f *uncertainAgentLauncher) Adopt(context.Context, string) (*ptyruntime.Adoption, error) {
	f.adopts++
	return nil, errors.New("holder authentication failed")
}
func (f *uncertainAgentLauncher) Launch(context.Context, string, []string, string, []string, uint16, uint16) (*ptyruntime.Adoption, error) {
	f.launches++
	return nil, errors.New("must not launch")
}
func (*uncertainAgentLauncher) Release(context.Context, string) {}

func waitScreen(t *testing.T, m *Manager, name, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if out, err := m.Host().CapturePane(context.Background(), name); err == nil && strings.Contains(out, want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	out, _ := m.Host().CapturePane(context.Background(), name)
	t.Fatalf("screen never showed %q; got %q", want, out)
}

func newNoTmuxManager(t *testing.T, mode Mode) (*Manager, *noTmux) {
	t.Helper()
	nt := &noTmux{}
	return New(Config{Mode: mode, Dir: t.TempDir(), Run: nt, Shell: fakeShell(t), ReconcileEvery: 50 * time.Millisecond,
		Options: ptyruntime.Options{IdleFlush: 20 * time.Millisecond, KillGrace: 500 * time.Millisecond, DrainGrace: 300 * time.Millisecond}}), nt
}

func TestAgentDirectLaunchGuardDefaultsDisabled(t *testing.T) {
	m := newManager(t, tmuxRunner{}, t.TempDir(), ModeOn)
	defer m.Close()
	h := m.Host().(routed)
	if m.AgentDirectReady() || h.PlanAgentDirect("a1") {
		t.Fatal("agent direct launch must default disabled")
	}
	if err := h.LaunchCommand(context.Background(), "a1", t.TempDir(), []string{"/bin/sh", "-c", "true"}); !errors.Is(err, ErrAgentDirectNotPlanned) {
		t.Fatalf("want ErrAgentDirectNotPlanned, got %v", err)
	}
}

func TestAgentDirectLaunchOnlyInOnMode(t *testing.T) {
	for _, mode := range []Mode{ModeOff, ModeShadow} {
		m := newManager(t, tmuxRunner{}, t.TempDir(), mode)
		m.SetAgentDirectReady(true)
		if m.Host().(routed).PlanAgentDirect("a1") {
			t.Fatalf("mode %s must keep the tmux path", mode)
		}
		m.Close()
	}
}

// A scripted AI CLI and a plain shell both run on direct PTYs with the tmux
// runner failing every call; ownership is inspected, not assumed.
func TestAgentAndShellLaunchDirectWithoutTmux(t *testing.T) {
	noLeaks(t)
	ctx := context.Background()
	m, nt := newNoTmuxManager(t, ModeOn)
	defer m.Close()
	m.SetAgentDirectReady(true)
	h := m.Host().(routed)
	dir := t.TempDir()

	if err := h.LaunchCommand(ctx, "agent-1", dir, []string{"/bin/sh", "-c", `printf 'FAKE-CLI ready %s %s\n' "$PWD" "$WARDEN_JOB_ID"; sleep 30`}, "WARDEN_JOB_ID=j7"); err != nil {
		t.Fatal(err)
	}
	if !h.HostedDirectly("agent-1") || !h.HasSession(ctx, "agent-1") {
		t.Fatal("agent must be hosted directly")
	}
	if m.IsTerminal("agent-1") {
		t.Fatal("agent must not be marked a shell")
	}
	if p := h.PinnedMode("agent-1"); p != string(ModeOn) {
		t.Fatalf("pin = %q", p)
	}
	waitScreen(t, m, "agent-1", "FAKE-CLI ready")
	waitScreen(t, m, "agent-1", "j7")
	if err := h.SendKeys(ctx, "agent-1", "-l", "hello"); err != nil {
		t.Fatal(err)
	}

	// Existing on-mode direct shells keep working alongside the agent.
	if err := m.TerminalHost().NewSession(ctx, "term-1", dir); err != nil {
		t.Fatal(err)
	}
	if !h.HostedDirectly("term-1") || !m.IsTerminal("term-1") {
		t.Fatal("shell must be a direct terminal")
	}

	// Ownership survives a config flip: the pin keeps the live agent direct.
	m.SetMode(ModeOff)
	if p := h.PinnedMode("agent-1"); p != string(ModeOn) || !h.HostedDirectly("agent-1") {
		t.Fatalf("pin lost on flip: %q", p)
	}
	// Duplicate launch of a live agent is refused, not doubled.
	if err := h.LaunchCommand(ctx, "agent-1", dir, []string{"/bin/sh", "-c", "true"}); err == nil {
		t.Fatal("duplicate launch must fail")
	}
	if nt.calls != 0 {
		t.Fatalf("tmux used %d times", nt.calls)
	}
	_ = h.KillSession(ctx, "agent-1")
	_ = h.KillSession(ctx, "term-1")
}

func TestAgentLaunchFailsClosedWhenExistingHolderCannotBeAdopted(t *testing.T) {
	m := newManager(t, tmuxRunner{}, t.TempDir(), ModeOn)
	defer m.Close()
	m.SetAgentDirectReady(true)
	fake := &uncertainAgentLauncher{}
	m.direct.Launcher = fake
	err := m.Host().(routed).LaunchCommand(context.Background(), "agent-uncertain", t.TempDir(), []string{"/bin/sh", "-c", "true"})
	if err == nil || !strings.Contains(err.Error(), "adopt existing agent holder") {
		t.Fatalf("LaunchCommand error = %v, want failed-closed adoption error", err)
	}
	if fake.adopts != 1 || fake.launches != 0 {
		t.Fatalf("adopt/launch = %d/%d, want 1/0", fake.adopts, fake.launches)
	}
}

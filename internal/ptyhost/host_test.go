package ptyhost

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/srjn45/warden/internal/ptyevent"
)

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"": ModeOff, "off": ModeOff, "SHADOW": ModeShadow, " on ": ModeOn, "bogus": ModeOff} {
		if got := ParseMode(in); got != want {
			t.Errorf("ParseMode(%q) = %q, want %q", in, got, want)
		}
	}
	if Valid("bogus") || !Valid("On") {
		t.Error("Valid misclassifies")
	}
}

// Off is the default and must be byte-for-byte the legacy tmux path: no
// journal, no mirror, nothing to look up.
func TestOffModeIsNoOp(t *testing.T) {
	noLeaks(t)
	run := newTmux(t)
	dir := filepath.Join(t.TempDir(), "pty")
	m := newManager(t, run, dir, ModeOff)
	defer m.Close()
	ctx := context.Background()
	for _, h := range []struct {
		name string
		host interface {
			NewSession(context.Context, string, string, ...string) error
		}
	}{{"agent-1", m.Host()}, {"term-1", m.TerminalHost()}} {
		if err := h.host.NewSession(ctx, h.name, t.TempDir()); err != nil {
			t.Fatal(err)
		}
		if _, err := run.Run(ctx, "", "tmux", "has-session", "-t", h.name); err != nil {
			t.Fatalf("%s: off mode must create a plain tmux session", h.name)
		}
		if _, ok := m.Lookup(h.name); ok {
			t.Fatalf("%s: off mode must not journal", h.name)
		}
		if !m.Host().HasSession(ctx, h.name) {
			t.Fatalf("%s: routed HasSession must see the tmux session", h.name)
		}
		if err := m.Host().KillSession(ctx, h.name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("off mode created %s (err=%v)", dir, err)
	}
}

// Shadow mirrors a tmux pane without changing its launch path: what the pane
// shows reaches the journal (parity with capture-pane), secrets never do, and
// teardown ends the epoch.
func TestShadowMirrorsTmuxPaneWithParity(t *testing.T) {
	noLeaks(t)
	run := newTmux(t)
	dir := filepath.Join(t.TempDir(), "pty")
	m := newManager(t, run, dir, ModeShadow)
	defer m.Close()
	ctx := context.Background()
	h := m.Host()
	if err := h.NewSession(ctx, "agent-1", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	tg, ok := m.Lookup("agent-1")
	if !ok || tg.Direct != nil || tg.Presentation != ptyevent.PresentationTmux {
		t.Fatalf("shadow lookup = %+v, %v; want an observe-only tmux stream", tg, ok)
	}
	secret := fakeSecret()
	// printf assembles the marker and the secret at run time, so the typed
	// command line itself carries neither literally.
	if err := h.SendKeys(ctx, "agent-1", "printf 'shadow-%s\\n' parity", "Enter"); err != nil {
		t.Fatal(err)
	}
	if err := h.SendKeys(ctx, "agent-1", "printf 'key=%s%s\\n' "+secret[:8]+" "+secret[8:], "Enter"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "mirrored output", func() bool {
		return strings.Contains(liveOutput(t, m, "agent-1"), "REDACTED")
	})
	pane, err := h.CapturePane(ctx, "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	jout := liveOutput(t, m, "agent-1")
	if !strings.Contains(pane, "shadow-parity") || !strings.Contains(jout, "shadow-parity") {
		t.Fatalf("parity: pane has marker=%v, journal has marker=%v", strings.Contains(pane, "shadow-parity"), strings.Contains(jout, "shadow-parity"))
	}
	if strings.Contains(jout, secret) {
		t.Fatal("unredacted secret reached the shadow journal")
	}
	// The pane itself is untouched (shadow writes nothing): tmux still shows it.
	if !strings.Contains(pane, secret) {
		t.Fatal("shadow must not alter what the pane shows")
	}

	if err := h.KillSession(ctx, "agent-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Lookup("agent-1"); ok {
		t.Fatal("killed pane is still mirrored")
	}
	evs := journal(t, shadowDir(dir, "agent-1"), "agent-1")
	validStream(t, evs)
	if lc := lastLifecycle(evs); lc == nil || lc.Phase != ptyevent.PhaseExited {
		t.Fatalf("last lifecycle = %+v, want exited", lc)
	}
	if _, err := os.Stat(filepath.Join(dir, "shadow", ".fifo", "agent-1")); !os.IsNotExist(err) {
		t.Fatal("FIFO left behind")
	}
}

// A daemon restart leaves tmux panes running; the next daemon re-adopts the
// mirror on a new epoch (recovered, reset, snapshot of the live screen).
func TestShadowReadoptsAcrossDaemonRestart(t *testing.T) {
	noLeaks(t)
	run := newTmux(t)
	dir := filepath.Join(t.TempDir(), "pty")
	ctx := context.Background()
	m1 := newManager(t, run, dir, ModeShadow)
	if err := m1.Host().NewSession(ctx, "agent-1", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	_ = m1.Host().SendKeys(ctx, "agent-1", "printf 'before-%s\\n' restart", "Enter")
	eventually(t, "pre-restart output", func() bool {
		return strings.Contains(liveOutput(t, m1, "agent-1"), "before-restart")
	})
	m1.Close() // daemon stops; the pane lives on in tmux

	m2 := newManager(t, run, dir, ModeShadow)
	defer m2.Close()
	if n := m2.RecoverShadow(ctx, []string{"agent-1", "never-mirrored"}); n != 1 {
		t.Fatalf("re-adopted %d panes, want 1", n)
	}
	_ = m2.Host().SendKeys(ctx, "agent-1", "printf 'after-%s\\n' restart", "Enter")
	eventually(t, "post-restart output", func() bool {
		return strings.Contains(liveOutput(t, m2, "agent-1"), "after-restart")
	})
	if err := m2.Host().KillSession(ctx, "agent-1"); err != nil {
		t.Fatal(err)
	}
	evs := journal(t, shadowDir(dir, "agent-1"), "agent-1")
	validStream(t, evs)
	var recovered, reset, snap bool
	for _, ev := range evs {
		switch {
		case ev.Kind == ptyevent.KindLifecycle && ev.Lifecycle.Phase == ptyevent.PhaseRecovered:
			recovered = ev.Epoch == 2
		case ev.Kind == ptyevent.KindMarker && ev.Marker.Type == ptyevent.MarkerReset:
			reset = true
		case ev.Kind == ptyevent.KindMarker && ev.Marker.Type == ptyevent.MarkerSnapshot && ev.Epoch == 2:
			snap = snap || strings.Contains(ev.Marker.Snapshot, "before-restart")
		}
	}
	if !recovered || !reset || !snap {
		t.Fatalf("re-adoption: recovered=%v reset=%v seeded snapshot=%v", recovered, reset, snap)
	}
}

// A pane that dies outside warden's view (agent exits, tmux killed) is reaped
// by the reconcile loop and its epoch ends.
func TestShadowReconcileEndsDeadPane(t *testing.T) {
	noLeaks(t)
	run := newTmux(t)
	dir := filepath.Join(t.TempDir(), "pty")
	m := newManager(t, run, dir, ModeShadow)
	defer m.Close()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); m.Run(ctx) }()
	defer func() { cancel(); wg.Wait() }()
	if err := m.Host().NewSession(ctx, "agent-1", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := run.Run(ctx, "", "tmux", "kill-session", "-t", "agent-1"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "reaped mirror", func() bool { _, ok := m.Lookup("agent-1"); return !ok })
	cancel()
	wg.Wait()
	m.Close()
	evs := journal(t, shadowDir(dir, "agent-1"), "agent-1")
	validStream(t, evs)
	if lc := lastLifecycle(evs); lc == nil || lc.Phase != ptyevent.PhaseExited || lc.Detail != "tmux session ended" {
		t.Fatalf("last lifecycle = %+v", lc)
	}
}

// Turning the mode off at runtime stops mirrors but leaves the panes alone.
func TestModeOffStopsMirrors(t *testing.T) {
	noLeaks(t)
	run := newTmux(t)
	dir := filepath.Join(t.TempDir(), "pty")
	m := newManager(t, run, dir, ModeShadow)
	defer m.Close()
	ctx := context.Background()
	if err := m.Host().NewSession(ctx, "agent-1", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	m.SetMode(ModeOff)
	if _, ok := m.Lookup("agent-1"); ok {
		t.Fatal("mirror survived mode off")
	}
	if !m.Host().HasSession(ctx, "agent-1") {
		t.Fatal("mode off must not touch the pane")
	}
	if err := m.Host().NewSession(ctx, "agent-2", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Lookup("agent-2"); ok {
		t.Fatal("new pane mirrored after mode off")
	}
}

// In mode on a plain-shell terminal runs on a direct warden-owned PTY —
// launch, key input, capture and teardown all through the routed host, with
// no tmux session behind it. Agent panes stay in tmux (mirrored).
func TestDirectTerminalLifecycle(t *testing.T) {
	noLeaks(t)
	run := newTmux(t)
	dir := filepath.Join(t.TempDir(), "pty")
	m := newManager(t, run, dir, ModeOn)
	defer m.Close()
	ctx := context.Background()
	var mu sync.Mutex
	var seen []ptyevent.Event
	m.Observe(func(name string, ev ptyevent.Event) {
		if name == "term-1" {
			mu.Lock()
			seen = append(seen, ev)
			mu.Unlock()
		}
	})
	if err := m.TerminalHost().NewSession(ctx, "term-1", t.TempDir(), fakeEnv+"=repl"); err != nil {
		t.Fatal(err)
	}
	h := m.Host()
	if !h.HasSession(ctx, "term-1") {
		t.Fatal("direct terminal not alive")
	}
	if _, err := run.Run(ctx, "", "tmux", "has-session", "-t", "term-1"); err == nil {
		t.Fatal("a direct terminal must not have a tmux session")
	}
	tg, ok := m.Lookup("term-1")
	if !ok || tg.Direct == nil || tg.Presentation != ptyevent.PresentationDirect {
		t.Fatalf("lookup = %+v, %v", tg, ok)
	}
	if err := h.SendKeys(ctx, "term-1", "echo direct", "Enter"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "direct output on screen", func() bool {
		pane, _ := h.CapturePane(ctx, "term-1")
		return strings.Contains(pane, "said:direct")
	})
	// An agent pane created by the same manager stays in tmux, mirrored.
	if err := h.NewSession(ctx, "agent-1", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if ag, ok := m.Lookup("agent-1"); !ok || ag.Direct != nil {
		t.Fatal("mode on must keep agents in tmux (mirrored)")
	}

	if err := h.KillSession(ctx, "term-1"); err != nil {
		t.Fatal(err)
	}
	if h.HasSession(ctx, "term-1") {
		t.Fatal("killed terminal still alive")
	}
	evs := journal(t, directDir(dir, "term-1"), "term-1")
	validStream(t, evs)
	if lc := lastLifecycle(evs); lc == nil || !lc.Phase.Terminal() {
		t.Fatalf("last lifecycle = %+v, want an exit", lc)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("observer saw no events")
	}
	for i := 1; i < len(seen); i++ {
		if seen[i].Seq <= seen[i-1].Seq {
			t.Fatalf("observer got seq %d after %d", seen[i].Seq, seen[i-1].Seq)
		}
	}
}

package ptyhost

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/srjn45/warden/internal/ptyruntime"
)

func (c *wsClient) frameOf(kind string) *ServerFrame {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.srv {
		if c.srv[i].Type == kind {
			f := c.srv[i]
			return &f
		}
	}
	return nil
}

func (c *wsClient) waitFrame(kind string) *ServerFrame {
	c.t.Helper()
	var f *ServerFrame
	eventually(c.t, kind+" frame", func() bool { f = c.frameOf(kind); return f != nil })
	return f
}

func (c *wsClient) inputFrame(gen, id uint64, data string) {
	c.t.Helper()
	c.send(ClientFrame{Type: "input", Gen: gen, ID: id, Data: []byte(data)})
}

func fenceRig(t *testing.T) (m *Manager, url string) {
	t.Helper()
	noLeaks(t)
	dir := filepath.Join(t.TempDir(), "pty")
	m = newManager(t, newTmux(t), dir, ModeOn)
	t.Cleanup(m.Close)
	startDirect(t, m, "term-f")
	return m, wsServer(t, func() *Manager { return m }, "term-f", false)
}

// F-FENCE-01: one controller per session; a second interactive client is a
// viewer until it takes over, which demotes the first and bumps the generation.
func TestF_FENCE_01_OneControllerTakeoverBumpsGeneration(t *testing.T) {
	m, url := fenceRig(t)
	a := dial(t, url+"?c=a")
	a.send(ClientFrame{Type: "hello", Mode: ModeInteractive, Cols: 100, Rows: 30})
	pa := a.waitPlans(1)[0]
	if !pa.Controller || pa.Generation == 0 {
		t.Fatalf("first interactive client must be the controller: %+v", pa)
	}
	a.waitEventOutput("ready")

	b := dial(t, url+"?c=b")
	b.send(ClientFrame{Type: "hello", Mode: ModeInteractive, Cols: 40, Rows: 10})
	pb := b.waitPlans(1)[0]
	if pb.Controller || pb.Holder != "sock-a" {
		t.Fatalf("second client must be a viewer held back by sock-a: %+v", pb)
	}
	b.waitEventOutput("ready")
	b.keys("echo from-viewer\r") // a viewer's keystrokes go nowhere
	a.keys("echo from-controller\r")
	a.waitRaw("said:from-controller")
	if strings.Contains(liveOutput(t, m, "term-f"), "from-viewer") {
		t.Fatal("a viewer's input reached the PTY")
	}

	b.send(ClientFrame{Type: "takeover"})
	cf := b.waitFrame("controller")
	if cf.Generation <= pa.Generation {
		t.Fatalf("takeover generation %d must exceed %d", cf.Generation, pa.Generation)
	}
	df := a.waitFrame("demoted")
	if df.Holder != "sock-b" {
		t.Fatalf("demoted frame holder = %q, want sock-b", df.Holder)
	}
	a.keys("echo old-controller\r")
	b.keys("echo new-controller\r")
	b.waitRaw("said:new-controller")
	if strings.Contains(liveOutput(t, m, "term-f"), "old-controller") {
		t.Fatal("the demoted controller's keystrokes reached the PTY")
	}
	// The takeover is journaled with both parties named.
	var journaled bool
	for _, ev := range liveEvents(t, m, "term-f") {
		journaled = journaled || (ev.Kind == ptyevent.KindLifecycle && strings.Contains(ev.Lifecycle.Detail, "takeover from sock-a"))
	}
	if !journaled {
		t.Fatal("takeover not journaled")
	}
}

// F-FENCE-02: a write under a stale generation (taken over, or from before the
// epoch changed) is rejected and never delivered.
func TestF_FENCE_02_StaleGenerationRejected(t *testing.T) {
	m, url := fenceRig(t)
	a := dial(t, url+"?c=a")
	a.send(ClientFrame{Type: "hello", Mode: ModeInteractive})
	pa := a.waitPlans(1)[0]
	a.inputFrame(pa.Generation, 1, "echo fresh\r")
	a.waitRaw("said:fresh")

	b := dial(t, url+"?c=b")
	b.send(ClientFrame{Type: "hello", Mode: ModeInteractive, Takeover: true})
	pb := b.waitPlans(1)[0]
	if !pb.Controller || pb.Generation <= pa.Generation {
		t.Fatalf("takeover hello: %+v (was %d)", pb, pa.Generation)
	}
	a.inputFrame(pa.Generation, 2, "echo stale\r")
	a.waitFrame("rejected")
	// Even a client lying about being current is checked against the lease.
	a.inputFrame(pb.Generation, 3, "echo forged\r")
	b.inputFrame(pb.Generation, 1, "echo current\r")
	b.waitRaw("said:current")
	out := liveOutput(t, m, "term-f")
	if strings.Contains(out, "said:stale") || strings.Contains(out, "said:forged") {
		t.Fatalf("stale-generation input was delivered:\n%s", out)
	}
	// Epoch change: a Session from a new epoch issues generations above every
	// generation of the old one, so a client fenced by the old epoch is stale.
	s := m.Direct("term-f")
	_, g := s.ControllerInfo()
	if g>>32 != s.Epoch() {
		t.Fatalf("generation %#x is not in epoch %d's space", g, s.Epoch())
	}
}

// F-FENCE-03: a client reconnecting after a drop (same client_id) learns what
// was applied and resending it again is a no-op.
func TestF_FENCE_03_ReconnectNeverDuplicatesInput(t *testing.T) {
	m, url := fenceRig(t)
	a := dial(t, url+"?c=a")
	a.send(ClientFrame{Type: "hello", Mode: ModeInteractive, ClientID: "laptop"})
	pa := a.waitPlans(1)[0]
	a.inputFrame(pa.Generation, 1, "echo once\r")
	a.waitRaw("said:once")
	a.close() // network drop

	a2 := dial(t, url+"?c=a2")
	a2.send(ClientFrame{Type: "hello", Mode: ModeInteractive, ClientID: "laptop"})
	p2 := a2.waitPlans(1)[0]
	if p2.LastInputID != 1 || !p2.Controller || p2.Generation <= pa.Generation {
		t.Fatalf("reconnect plan = %+v, want last_input_id 1 and a newer controller generation", p2)
	}
	// The client cannot know whether its last write landed, so it resends: id 1
	// is dropped, id 2 is new.
	a2.inputFrame(p2.Generation, 1, "echo once\r")
	a2.inputFrame(p2.Generation, 2, "echo twice\r")
	a2.waitRaw("said:twice")
	if n := strings.Count(liveOutput(t, m, "term-f"), "said:once"); n != 1 {
		t.Fatalf("input id 1 ran %d times, want exactly 1", n)
	}
}

// F-ATTACH-03: resize follows the controller (last controller wins), SIGWINCH
// reaches the foreground process, and the resize is journaled with its origin.
func TestF_ATTACH_03_ResizeArbitrationAndSIGWINCH(t *testing.T) {
	noLeaks(t)
	dir := filepath.Join(t.TempDir(), "pty")
	m := New(Config{Mode: ModeOn, Dir: dir, Run: newTmux(t),
		Shell:   []string{"/bin/sh", "-c", `trap 'echo WINCH-$(stty size)' WINCH; echo ready; while :; do sleep 0.02; done`},
		Options: ptyruntime.Options{IdleFlush: 20 * time.Millisecond, KillGrace: 500 * time.Millisecond}})
	t.Cleanup(m.Close)
	if err := m.TerminalHost().NewSession(context.Background(), "term-r", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	url := wsServer(t, func() *Manager { return m }, "term-r", false)

	a := dial(t, url+"?c=a")
	a.send(ClientFrame{Type: "hello", Mode: ModeInteractive, Cols: 100, Rows: 30})
	a.waitPlans(1)
	a.waitEventOutput("WINCH-30 100") // the hello's size is applied and signalled
	b := dial(t, url+"?c=b")
	b.send(ClientFrame{Type: "hello", Mode: ModeInteractive, Cols: 50, Rows: 10})
	if p := b.waitPlans(1)[0]; p.Controller {
		t.Fatal("viewer became controller")
	}
	b.send(ClientFrame{Type: "resize", Cols: 50, Rows: 10}) // ignored: not the controller
	a.send(ClientFrame{Type: "resize", Cols: 120, Rows: 40})
	a.waitEventOutput("WINCH-40 120")
	if strings.Contains(outputOf(liveEvents(t, m, "term-r")), "WINCH-10 50") {
		t.Fatal("a viewer resized the session")
	}
	b.send(ClientFrame{Type: "takeover"})
	b.waitFrame("controller")
	b.send(ClientFrame{Type: "resize", Cols: 60, Rows: 20})
	b.waitEventOutput("WINCH-20 60")
	var origins []string
	for _, ev := range liveEvents(t, m, "term-r") {
		if ev.Kind == ptyevent.KindResize {
			origins = append(origins, ev.Resize.ClientID)
		}
	}
	if len(origins) < 3 || origins[len(origins)-1] != "sock-b" {
		t.Fatalf("resizes journaled for %v, want …sock-b last", origins)
	}
}

// Durable vs local: a password typed at a no-echo prompt over the fenced input
// path reaches the shell but never the journal — the journaled input event
// records only that it was secret, its length and the client's input id.
func TestFencedSecretInputNeverReachesTheJournal(t *testing.T) {
	noLeaks(t)
	dir := filepath.Join(t.TempDir(), "pty")
	m := New(Config{Mode: ModeOn, Dir: dir, Run: newTmux(t),
		Shell:   []string{"/bin/sh", "-c", `echo ready; stty -echo; echo noecho-set; read pw; stty echo; echo got-${#pw}`},
		Options: ptyruntime.Options{IdleFlush: 20 * time.Millisecond, KillGrace: 500 * time.Millisecond}})
	t.Cleanup(m.Close)
	if err := m.TerminalHost().NewSession(context.Background(), "term-s", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	url := wsServer(t, func() *Manager { return m }, "term-s", false)
	c := dial(t, url+"?c=a")
	c.send(ClientFrame{Type: "hello", Mode: ModeInteractive})
	p := c.waitPlans(1)[0]
	c.waitEventOutput("ready")
	const pw = "hunter2-s3cret"
	c.waitEventOutput("noecho-set") // stty has finished: the line below is read with echo off
	c.inputFrame(p.Generation, 1, pw+"\r")
	c.waitRaw("got-14")
	var secretInput bool
	for _, ev := range liveEvents(t, m, "term-s") {
		blob, _ := json.Marshal(ev)
		if strings.Contains(string(blob), pw) {
			t.Fatalf("the password reached the journal: %s", blob)
		}
		if ev.Kind == ptyevent.KindInput && ev.Input.Secret {
			secretInput = ev.Input.SecretLen == len(pw)+1 && ev.Input.InputID == 1
		}
	}
	if !secretInput {
		t.Fatal("the secret input was not journaled as length-only with its input id")
	}
	// The journal on disk too.
	var disk strings.Builder
	for _, ev := range journal(t, dir, "term-s") {
		b, _ := json.Marshal(ev)
		disk.Write(b)
	}
	if strings.Contains(disk.String(), pw) {
		t.Fatal("the password reached the on-disk journal")
	}
}

// F-SURV-04 / F-AUTH-02 for agents: tmux is the compat holder of an agent's PTY.
// A daemon restart re-adopts the very pane process it mirrored, and refuses a
// pane whose process is not the recorded one (pid reuse, a replaced pane).
func TestF_SURV_04_AgentPaneAdoptionChecksIdentity(t *testing.T) {
	noLeaks(t)
	run := newTmux(t)
	dir := filepath.Join(t.TempDir(), "pty")
	ctx := context.Background()
	m1 := newManager(t, run, dir, ModeShadow)
	for _, n := range []string{"agent-ok", "agent-swapped"} {
		if err := m1.Host().NewSession(ctx, n, t.TempDir()); err != nil {
			t.Fatal(err)
		}
	}
	idFile := func(n string) string { return filepath.Join(shadowDir(dir, n), paneIdentityFile) }
	for _, n := range []string{"agent-ok", "agent-swapped"} {
		if _, err := os.Stat(idFile(n)); err != nil {
			t.Fatalf("pane identity not recorded for %s: %v", n, err)
		}
	}
	m1.Close() // daemon gone; panes live on in tmux

	// Make "agent-swapped" look like a recycled pid: the record names another start time.
	var rec paneIdentity
	b, _ := os.ReadFile(idFile("agent-swapped"))
	_ = json.Unmarshal(b, &rec)
	rec.Start++
	b, _ = json.Marshal(rec)
	if err := os.WriteFile(idFile("agent-swapped"), b, 0o600); err != nil {
		t.Fatal(err)
	}

	m2 := newManager(t, run, dir, ModeShadow)
	defer m2.Close()
	if n := m2.RecoverShadow(ctx, []string{"agent-ok", "agent-swapped"}); n != 1 {
		t.Fatalf("re-adopted %d panes, want only the one whose process matches", n)
	}
	if _, ok := m2.Lookup("agent-ok"); !ok {
		t.Fatal("the genuine pane was not adopted")
	}
	if _, ok := m2.Lookup("agent-swapped"); ok {
		t.Fatal("a pane with a different process identity was adopted")
	}
	evs := journal(t, shadowDir(dir, "agent-swapped"), "agent-swapped")
	lc := lastLifecycle(evs)
	if lc == nil || lc.Phase != ptyevent.PhaseCrashed || !strings.Contains(lc.Detail, "not adopting a different process") {
		t.Fatalf("refusal not journaled with its cause: %+v", lc)
	}
	// The pane itself was left alone (still alive in tmux).
	if !m2.tmux.HasSession(ctx, "agent-swapped") {
		t.Fatal("refusing adoption must not touch the pane")
	}
}

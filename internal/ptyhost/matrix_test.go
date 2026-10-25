package ptyhost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/srjn45/warden/internal/ptyruntime"
	"github.com/srjn45/warden/internal/terminalcontract"
)

// The 02b-05 t1 adversarial matrix (docs/rollout/2026-10-25-02b-05-t1-adversarial-matrix.md).
// Every test here runs with the agent-hosting tmux runner failing every call
// (noTmux): direct AI agents and shells must work with no tmux at all. Agents
// are the scripted fake CLI (fakeREPL) launched through the same
// routed.LaunchCommand seam lifecycle uses; no provider is contacted.

// matrixRig is a no-tmux on-mode manager with direct agents enabled through the
// internal readiness guard (a test seam; production keeps it off).
func matrixRig(t *testing.T) (*Manager, routed, *noTmux) {
	t.Helper()
	noLeaks(t)
	m, nt := newNoTmuxManager(t, ModeOn)
	t.Cleanup(func() {
		if nt.calls != 0 {
			t.Errorf("agent-hosting tmux was used %d times", nt.calls)
		}
	})
	t.Cleanup(m.Close)
	m.SetAgentDirectReady(true)
	return m, m.Host().(routed), nt
}

// launchFakeAgent starts the scripted fake AI CLI as a direct agent.
func launchFakeAgent(t *testing.T, h routed, name string) {
	t.Helper()
	if err := h.LaunchCommand(context.Background(), name, t.TempDir(), fakeShell(t), fakeEnv+"=repl"); err != nil {
		t.Fatal(err)
	}
}

func p99(ds []time.Duration) time.Duration {
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[(len(s)*99+99)/100-1]
}

func within(t *testing.T, what string, got, budget time.Duration) {
	t.Helper()
	t.Logf("budget %s: p99=%v (budget %v, slack x%d)", what, got, budget, raceSlack)
	if got > budget*raceSlack {
		t.Errorf("%s p99 %v exceeds budget %v (x%d)", what, got, budget, raceSlack)
	}
}

func journalHasOutput(m *Manager, name, want string) bool {
	tg, ok := m.Lookup(name)
	if !ok {
		return false
	}
	found := false
	_ = tg.Stream.Journal().Read(1, func(ev ptyevent.Event) error {
		if ev.Kind == ptyevent.KindOutput && strings.Contains(string(ev.Output.Data), want) {
			found = true
		}
		return nil
	})
	return found
}

// §6.9 latency budgets measured on a DIRECT AI AGENT (not only terminals):
// launch -> first journal byte, runtime identity resolve, attach -> first
// frame, keystroke echo, lease takeover and resize -> SIGWINCH-observed size.
func TestT1_DirectAgentLatencyBudgetsWithoutTmux(t *testing.T) {
	m, h, _ := matrixRig(t)
	b := terminalcontract.Default
	ctx := context.Background()

	// Agent launch -> first journaled byte. The fake CLI is a re-exec of this
	// test binary, so its startup is included: the measurement is an upper bound.
	var launch []time.Duration
	for i := range 10 {
		name := fmt.Sprintf("agent-l%d", i)
		t0 := time.Now()
		launchFakeAgent(t, h, name)
		eventually(t, "first byte "+name, func() bool { return journalHasOutput(m, name, "ready") })
		launch = append(launch, time.Since(t0))
		_ = h.KillSession(ctx, name)
	}
	within(t, "AgentLaunchToFirstByte(incl. CLI start)", p99(launch), b.AgentLaunchToFirstByteP99)

	launchFakeAgent(t, h, "agent-b")
	defer func() { _ = h.KillSession(ctx, "agent-b") }()
	eventually(t, "agent ready", func() bool { return journalHasOutput(m, "agent-b", "ready") })

	var resolve []time.Duration
	for range 200 {
		t0 := time.Now()
		info := m.Resolve(ctx, "agent-b")
		resolve = append(resolve, time.Since(t0))
		if info.Kind != "direct" || info.ProcessOwner == "tmux" {
			t.Fatalf("direct agent resolved as %+v", info)
		}
	}
	within(t, "RuntimeIdentityResolve", p99(resolve), b.RuntimeIdentityResolveP99)

	url := wsServer(t, func() *Manager { return m }, "agent-b", false)
	var attach []time.Duration
	for i := range 10 {
		t0 := time.Now()
		c := dial(t, fmt.Sprintf("%s?c=probe%d", url, i))
		c.send(ClientFrame{Type: "hello", Mode: ModeObserve})
		c.waitPlans(1)
		attach = append(attach, time.Since(t0))
		c.close()
	}
	within(t, "AttachToFirstByte", p99(attach), b.AttachToFirstByteP99)

	a := dial(t, url+"?c=a")
	a.send(ClientFrame{Type: "hello", Mode: ModeInteractive, Cols: 80, Rows: 24})
	if p := a.waitPlans(1)[0]; !p.Controller {
		t.Fatalf("first interactive client must control: %+v", p)
	}
	var echo []time.Duration
	for i := range 30 {
		want := fmt.Sprintf("said:k%d\r", i)
		t0 := time.Now()
		a.keys(fmt.Sprintf("echo k%d\r", i))
		a.waitRaw(want)
		echo = append(echo, time.Since(t0))
	}
	within(t, "KeystrokeEcho", p99(echo), b.KeystrokeEchoP99)

	var resize []time.Duration
	for i := range 10 {
		cols, rows := uint16(81+i), uint16(25+i)
		t0 := time.Now()
		a.send(ClientFrame{Type: "resize", Cols: cols, Rows: rows})
		a.keys("size\r")
		a.waitRaw(fmt.Sprintf("size:%dx%d", cols, rows))
		resize = append(resize, time.Since(t0))
	}
	within(t, "ResizeToSIGWINCH(incl. query echo)", p99(resize), b.ResizeToSIGWINCHP99)

	bc := dial(t, url+"?c=b")
	bc.send(ClientFrame{Type: "hello", Mode: ModeInteractive})
	bc.waitPlans(1)
	var takeover []time.Duration
	for i := range 10 {
		from, to := bc, a
		if i%2 == 1 {
			from, to = a, bc
		}
		_ = to
		t0 := time.Now()
		from.send(ClientFrame{Type: "takeover"})
		eventually(t, "controller frame", func() bool { return countKind(from, "controller") > i/2 })
		takeover = append(takeover, time.Since(t0))
	}
	within(t, "LeaseTakeover", p99(takeover), b.LeaseTakeoverP99)
}

func countKind(c *wsClient, kind string) int {
	_, _, _, kinds := c.snapshot()
	n := 0
	for _, k := range kinds {
		if k == kind {
			n++
		}
	}
	return n
}

// Viewer cap (ViewersPerSession): 16 slots per session, the 17th is refused
// with an actionable error, a released slot is reusable and release is
// idempotent; sessions are counted independently.
func TestT1_ViewerCapBoundedAndReleased(t *testing.T) {
	m, _, _ := matrixRig(t)
	if MaxViewersPerSession != terminalcontract.Default.ViewersPerSession {
		t.Fatalf("cap %d != budget %d", MaxViewersPerSession, terminalcontract.Default.ViewersPerSession)
	}
	var rel []func()
	for i := range MaxViewersPerSession {
		r, err := m.AdmitViewer("s")
		if err != nil {
			t.Fatalf("viewer %d refused: %v", i, err)
		}
		rel = append(rel, r)
	}
	_, err := m.AdmitViewer("s")
	if !errors.Is(err, ErrTooManyViewers) || !strings.Contains(err.Error(), "close another") {
		t.Fatalf("17th viewer: want actionable ErrTooManyViewers, got %v", err)
	}
	if _, err := m.AdmitViewer("other"); err != nil {
		t.Fatalf("other session must have its own slots: %v", err)
	}
	rel[0]()
	rel[0]() // idempotent: must not free a second slot
	if m.ViewerCount("s") != MaxViewersPerSession-1 {
		t.Fatalf("count after double release = %d", m.ViewerCount("s"))
	}
	r, err := m.AdmitViewer("s")
	if err != nil {
		t.Fatalf("freed slot not reusable: %v", err)
	}
	r()
	for _, r := range rel[1:] {
		r()
	}
	if m.ViewerCount("s") != 0 {
		t.Fatalf("slots leaked: %d", m.ViewerCount("s"))
	}
}

// Adversarial resize: an attach client asking for 65535x65535 (or any size past
// the screen-model bounds) is refused with a notice; the session keeps its size,
// the daemon does not allocate the grid, and the agent keeps working.
func TestT1_AdversarialResizeRefusedAndSessionSurvives(t *testing.T) {
	m, h, _ := matrixRig(t)
	launchFakeAgent(t, h, "agent-r")
	defer func() { _ = h.KillSession(context.Background(), "agent-r") }()
	url := wsServer(t, func() *Manager { return m }, "agent-r", false)
	c := dial(t, url+"?c=r")
	c.send(ClientFrame{Type: "hello", Mode: ModeInteractive, Cols: 100, Rows: 30})
	c.waitPlans(1)
	c.waitRaw("ready")

	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	for _, sz := range [][2]uint16{{65535, 65535}, {ptyruntime.MaxCols + 1, 10}, {600, 600}} {
		c.send(ClientFrame{Type: "resize", Cols: sz[0], Rows: sz[1]})
	}
	eventually(t, "notice frames", func() bool { return countKind(c, "notice") >= 3 })
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if grown := int64(after.HeapAlloc) - int64(before.HeapAlloc); grown > 64<<20 {
		t.Fatalf("oversized resize grew the heap by %d bytes", grown)
	}
	c.keys("size\r")
	c.waitRaw("size:100x30")
	// A legal large size still works (the bound is far above real terminals).
	c.send(ClientFrame{Type: "resize", Cols: 512, Rows: 200})
	c.keys("size\r")
	c.waitRaw("size:512x200")
}

// Concurrent views + input: one controller types while viewers attach and
// detach concurrently. Every viewer sees the same ordered stream (no gap, dup or
// reorder), viewer keystrokes go nowhere, and the journal stream is valid.
func TestT1_ConcurrentViewersAndInputKeepOneOrderedStream(t *testing.T) {
	m, h, _ := matrixRig(t)
	launchFakeAgent(t, h, "agent-c")
	defer func() { _ = h.KillSession(context.Background(), "agent-c") }()
	url := wsServer(t, func() *Manager { return m }, "agent-c", false)
	ctl := dial(t, url+"?c=ctl")
	ctl.send(ClientFrame{Type: "hello", Mode: ModeInteractive, Cols: 80, Rows: 24})
	ctl.waitPlans(1)
	ctl.waitRaw("ready")

	const viewers, lines = 6, 40
	var wg sync.WaitGroup
	vs := make([]*wsClient, viewers)
	for i := range viewers {
		vs[i] = dial(t, fmt.Sprintf("%s?c=v%d", url, i))
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range lines {
			ctl.keys(fmt.Sprintf("echo L%03d\r", i))
			time.Sleep(2 * time.Millisecond)
		}
	}()
	for i, v := range vs {
		mode := ModeObserve
		if i%2 == 0 {
			mode = ModeInteractive // interactive viewer without the lease
		}
		v.send(ClientFrame{Type: "hello", Mode: mode, Cursor: &ptyevent.Cursor{}})
		if mode == ModeInteractive {
			v.keys("echo VIEWER-INJECT\r")
		}
	}
	wg.Wait()
	ctl.waitRaw(fmt.Sprintf("said:L%03d", lines-1))
	for i, v := range vs {
		v.waitEventOutput(fmt.Sprintf("said:L%03d", lines-1))
		_, evs, _, _ := v.snapshot()
		out := outputOf(evs)
		last := -1
		for j := range lines {
			at := strings.Index(out, fmt.Sprintf("said:L%03d", j))
			if at < 0 || at < last {
				t.Fatalf("viewer %d: line %d missing or out of order", i, j)
			}
			if strings.Count(out, fmt.Sprintf("said:L%03d\r", j)) != 1 {
				t.Fatalf("viewer %d: line %d duplicated", i, j)
			}
			last = at
		}
	}
	if journalHasOutput(m, "agent-c", "VIEWER-INJECT") {
		t.Fatal("a viewer without the lease typed into the agent")
	}
	evs := liveEvents(t, m, "agent-c")
	if len(evs) == 0 {
		t.Fatal("no journal events")
	}
	validStream(t, evs)
}

// Terminal injection from untrusted program output: a secret smuggled inside an
// OSC title or wrapped in SGR is still redacted in the journal and in replay;
// OSC/DCS sequences never reach the screen-model snapshot; a planted
// approval-shaped prompt in a SHELL never produces prompt automation.
func TestT1_TerminalInjectionAndSecretsInControlSequences(t *testing.T) {
	m, h, _ := matrixRig(t)
	secret := fakeSecret()
	script := `printf '\033]0;title %s\007' "$1"; printf '\033[31m%s\033[0m\n' "$2"; printf '\033P+q544e\033\\'; printf '\033]52;c;aGVsbG8=\007'; printf 'Allow this command? (y/n)\n'; printf 'INJECT-DONE\n'; exec sleep 30`
	if err := h.LaunchCommand(context.Background(), "agent-i", t.TempDir(),
		[]string{"/bin/sh", "-c", script, "sh", secret, secret}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.KillSession(context.Background(), "agent-i") }()
	eventually(t, "injection output", func() bool { return journalHasOutput(m, "agent-i", "INJECT-DONE") })
	evs := liveEvents(t, m, "agent-i")
	for _, ev := range evs {
		if ev.Kind == ptyevent.KindOutput && strings.Contains(string(ev.Output.Data), secret) {
			t.Fatalf("secret in durable journal: %q", ev.Output.Data)
		}
	}
	if got := outputOf(evs); !strings.Contains(got, ptyevent.Placeholder(ptyevent.SecretAnthropicKey)) {
		t.Fatalf("secret was not redacted in place: %q", got)
	}
	scr := m.Direct("agent-i").Screen()
	if strings.Contains(scr, "\x1b]") || strings.Contains(scr, "\x1bP") || strings.Contains(scr, "aGVsbG8") {
		t.Fatalf("control-sequence payload leaked into the screen snapshot: %q", scr)
	}
	// Redacted replay (observe client) never carries the secret.
	url := wsServer(t, func() *Manager { return m }, "agent-i", true)
	c := dial(t, url+"?c=obs")
	c.send(ClientFrame{Type: "hello", Mode: ModeObserve, Cursor: &ptyevent.Cursor{}})
	c.waitEventOutput("INJECT-DONE")
	raw, revs, _, _ := c.snapshot()
	if raw != "" || strings.Contains(outputOf(revs), secret) {
		t.Fatal("observe replay carried raw bytes or the secret")
	}

	// The same approval-shaped screen on a SHELL is never shown to AI observers.
	var seen seenSet
	m.ObserveAgents(seen.add)
	if err := m.TerminalHost().NewSession(context.Background(), "term-i", t.TempDir(), fakeEnv+"=repl"); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.TerminalHost().KillSession(context.Background(), "term-i") }()
	_ = m.Direct("term-i")
	eventually(t, "shell ready", func() bool { return journalHasOutput(m, "term-i", "ready") })
	if n := seen.get("term-i"); n != 0 {
		t.Fatalf("shell events reached AI observers: %d", n)
	}
}

// Shell exit vs AI CLI exit, Ctrl-C and EOF on a direct agent with no tmux:
// exit status is journaled per session, the process group is reaped, and
// neither exit is reported as the other's.
func TestT1_ShellAndCLIExitsAreDistinctAndReaped(t *testing.T) {
	noLeaks(t)
	nt := &noTmux{}
	m := New(Config{Mode: ModeOn, Dir: t.TempDir(), Run: nt, Shell: []string{"/bin/sh"}, ReconcileEvery: 50 * time.Millisecond,
		Options: ptyruntime.Options{IdleFlush: 20 * time.Millisecond, KillGrace: 500 * time.Millisecond, DrainGrace: 300 * time.Millisecond}})
	defer m.Close()
	m.SetAgentDirectReady(true)
	h := m.Host().(routed)
	ctx := context.Background()
	if err := h.LaunchCommand(ctx, "agent-x", t.TempDir(), []string{"/bin/sh", "-c", "trap 'echo got-int' INT; echo cli-up; until read l; do :; done; exit 4"}); err != nil {
		t.Fatal(err)
	}
	if err := m.TerminalHost().NewSession(ctx, "term-x", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	ag, sh := m.Direct("agent-x"), m.Direct("term-x")
	agPID, shPID := ag.PID(), sh.PID()
	eventually(t, "cli up", func() bool { return strings.Contains(ag.Screen(), "cli-up") })
	if _, err := ag.Input(ptyruntime.Write{Origin: ptyevent.OriginHuman, Data: []byte{0x03}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "SIGINT delivered via the tty", func() bool { return strings.Contains(ag.Screen(), "got-int") })
	if _, err := ag.Input(ptyruntime.Write{Origin: ptyevent.OriginHuman, Data: []byte("\r")}); err != nil {
		t.Fatal(err)
	}
	if _, err := sh.Input(ptyruntime.Write{Origin: ptyevent.OriginHuman, Data: []byte("exit 9\r")}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		s    *ptyruntime.Session
		name string
		code int
	}{{ag, "agent-x", 4}, {sh, "term-x", 9}} {
		select {
		case <-c.s.Done():
		case <-time.After(waitFor):
			t.Fatalf("%s never exited", c.name)
		}
		lc := lastLifecycle(liveEvents(t, m, c.name))
		if lc == nil || lc.Phase != ptyevent.PhaseExited || lc.ExitCode == nil || *lc.ExitCode != c.code {
			t.Fatalf("%s exit = %+v, want exited %d", c.name, lc, c.code)
		}
	}
	for _, pid := range []int{agPID, shPID} {
		eventually(t, "process reaped", func() bool { return unix.Kill(pid, 0) != nil })
	}
	if m.IsTerminal("agent-x") || !m.IsTerminal("term-x") {
		t.Fatal("agent/shell kind crossed on exit")
	}
	if nt.calls != 0 {
		t.Fatalf("tmux used %d times", nt.calls)
	}
}

// Mixed agent+shell soak without tmux: per-session FD/goroutine budgets,
// and no descriptor, goroutine or child-process leak after teardown.
func TestT1_MixedDirectSoakNoLeaksWithoutTmux(t *testing.T) {
	m, h, _ := matrixRig(t)
	ctx := context.Background()
	b := terminalcontract.Default
	n := b.SoakSessions
	if testing.Short() {
		n = 10
	}
	runtime.GC()
	baseFD, baseG := countOpenFDs(), runtime.NumGoroutine()
	pids := map[string]int{}
	for i := range n {
		name := fmt.Sprintf("mix-%02d", i)
		if i%2 == 0 {
			launchFakeAgent(t, h, name)
		} else if err := m.TerminalHost().NewSession(ctx, name, t.TempDir(), fakeEnv+"=repl"); err != nil {
			t.Fatal(err)
		}
		pids[name] = m.Direct(name).PID()
	}
	for name := range pids {
		eventually(t, name+" ready", func() bool { return journalHasOutput(m, name, "ready") })
	}
	fdPer := float64(countOpenFDs()-baseFD) / float64(n)
	gPer := float64(runtime.NumGoroutine()-baseG) / float64(n)
	t.Logf("mixed soak n=%d: %.2f FDs/session, %.2f goroutines/session", n, fdPer, gPer)
	if fdPer > float64(b.PerSessionFDs) || gPer > float64(b.PerSessionGoroutines) {
		t.Fatalf("per-session FDs %.2f / goroutines %.2f exceed %d / %d", fdPer, gPer, b.PerSessionFDs, b.PerSessionGoroutines)
	}
	for name := range pids {
		if err := h.KillSession(ctx, name); err != nil {
			t.Fatal(err)
		}
	}
	for name, pid := range pids {
		eventually(t, name+" child reaped", func() bool { return unix.Kill(pid, 0) != nil })
	}
	eventually(t, "fds released", func() bool { return countOpenFDs() <= baseFD+2 })
	if _, err := os.Stat(filepath.Join(m.dir, "direct")); err != nil {
		t.Fatalf("journals must remain for audit after stop: %v", err)
	}
}

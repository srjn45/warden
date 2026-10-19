package ptyholder

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/srjn45/warden/internal/ptyruntime"
)

// startFakeDaemon runs the stand-in daemon, returns it and the shell pid it reported.
func startFakeDaemon(t *testing.T, root, journals, name string) (*exec.Cmd, int) {
	t.Helper()
	exe, _ := os.Executable()
	cmd := exec.Command(exe, "__daemon", root, journals, name)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	pid, sc := 0, bufio.NewScanner(out)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "PID "):
			pid, _ = strconv.Atoi(strings.TrimPrefix(line, "PID "))
		case line == "READY":
			return cmd, pid
		default:
			t.Fatalf("fake daemon: %s", line)
		}
	}
	t.Fatal("fake daemon exited before READY")
	return nil, 0
}

func alive(pid int) bool { return unix.Kill(pid, 0) == nil }

func phases(t *testing.T, s *ptyruntime.Session) []ptyevent.LifecyclePhase {
	t.Helper()
	var out []ptyevent.LifecyclePhase
	_ = s.Journal().Read(1, func(ev ptyevent.Event) error {
		if ev.Kind == ptyevent.KindLifecycle {
			out = append(out, ev.Lifecycle.Phase)
		}
		return nil
	})
	return out
}

// F-SURV-01: SIGKILL the daemon — the shell keeps its pid and keeps running, and
// the next daemon adopts the very same process with the scrollback intact.
func TestF_SURV_01_DaemonSIGKILLShellSurvivesAndIsAdopted(t *testing.T) {
	root, journals := tmpDirs(t)
	d, pid := startFakeDaemon(t, root, journals, "t1")

	if err := d.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = d.Wait()
	// The daemon is dead; the master it held is closed. The shell must still run.
	if !alive(pid) {
		t.Fatalf("shell %d died with the daemon (the holder must keep the PTY master)", pid)
	}

	h := newHost(root, journals)
	defer h.Close()
	if err := h.Adopt(context.Background(), "t1"); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	s := h.Session("t1")
	if s.PID() != pid {
		t.Fatalf("adopted pid %d, want the original %d (no relaunch)", s.PID(), pid)
	}
	ph := phases(t, s)
	for _, p := range ph {
		if p == ptyevent.PhaseCrashed || p == ptyevent.PhaseExited {
			t.Fatalf("journal shows %v; a surviving process must not look crashed: %v", p, ph)
		}
	}
	if ph[0] != ptyevent.PhaseSpawned || ph[len(ph)-1] != ptyevent.PhaseRecovered {
		t.Fatalf("lifecycle %v, want spawned … recovered", ph)
	}
	// Scrollback from before the crash is still in the journal.
	found := false
	_ = s.Journal().Read(1, func(ev ptyevent.Event) error {
		if ev.Kind == ptyevent.KindOutput && strings.Contains(string(ev.Output.Data), "survive-marker-1") {
			found = true
		}
		return nil
	})
	if !found {
		t.Fatal("pre-crash output missing from the journal")
	}
	// The same shell still answers (its own pid proves it is not a fresh one).
	if err := h.SendKeys(context.Background(), "t1", "-l", "echo pid-is-$$"); err != nil {
		t.Fatal(err)
	}
	_ = h.SendKeys(context.Background(), "t1", "Enter")
	eventually(t, "the adopted shell to answer", func() bool {
		return strings.Contains(s.Screen(), "pid-is-"+strconv.Itoa(pid))
	})
}

// F-SURV-02: a graceful daemon stop (Host.Close) detaches without killing or
// relaunching; adopting again yields the same process and exactly one of it.
func TestF_SURV_02_GracefulStopAndRestartKeepsTheProcess(t *testing.T) {
	root, journals := tmpDirs(t)
	h1 := newHost(root, journals)
	if err := h1.NewSession(context.Background(), "t2", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	pid := h1.Session("t2").PID()
	h1.Close() // the daemon stopping / being replaced by a new binary

	if !alive(pid) {
		t.Fatalf("graceful stop killed shell %d", pid)
	}
	h2 := newHost(root, journals)
	if err := h2.Adopt(context.Background(), "t2"); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if got := h2.Session("t2").PID(); got != pid {
		t.Fatalf("adopted %d, want %d", got, pid)
	}
	// Adopting twice is idempotent: still one process.
	if err := h2.Adopt(context.Background(), "t2"); err != nil {
		t.Fatalf("second adopt: %v", err)
	}
	h2.Close()
	h3 := newHost(root, journals)
	defer h3.Close()
	if err := h3.Adopt(context.Background(), "t2"); err != nil {
		t.Fatalf("third adopt: %v", err)
	}
	if got := h3.Session("t2").PID(); got != pid {
		t.Fatalf("after two restarts pid %d, want %d", got, pid)
	}
	if err := h3.KillSession(context.Background(), "t2"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "kill to end the process", func() bool { return !alive(pid) })
}

// F-SURV-03: holder dies (SIGKILL) — the adoption fails with a cause, the
// failure is journalable, and nothing relaunches a shell.
func TestF_SURV_03_HolderDeathIsActionableNeverRelaunched(t *testing.T) {
	root, journals := tmpDirs(t)
	h1 := newHost(root, journals)
	if err := h1.NewSession(context.Background(), "t3", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	pid := h1.Session("t3").PID()
	h1.Close()

	hh, err := Open(root, "t3")
	if err != nil {
		t.Fatal(err)
	}
	m, _ := hh.Meta()
	if err := syscall.Kill(m.HolderPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	eventually(t, "holder to die", func() bool { return !alive(m.HolderPID) })

	h2 := newHost(root, journals)
	defer h2.Close()
	err = h2.Adopt(context.Background(), "t3")
	if !errors.Is(err, ErrGone) {
		t.Fatalf("adopt after holder death: %v, want ErrGone", err)
	}
	if h2.Session("t3") != nil {
		t.Fatal("a session was started although nothing could be adopted")
	}
	if err := h2.MarkLost("t3", "PTY holder is gone; run `wd agent restore t3` for a fresh shell"); err != nil {
		t.Fatal(err)
	}
	h3 := newHost(root, journals)
	defer h3.Close()
	// The journal now ends with an actionable crashed event.
	j, err := ptyruntime.OpenJournal(journals+"/t3", "t3", ptyruntime.Retention{})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if !j.Ended() {
		t.Fatal("journal not ended after MarkLost")
	}
	var detail string
	_ = j.Read(1, func(ev ptyevent.Event) error {
		if ev.Kind == ptyevent.KindLifecycle && ev.Lifecycle.Phase == ptyevent.PhaseCrashed {
			detail = ev.Lifecycle.Detail
		}
		return nil
	})
	if !strings.Contains(detail, "restore") {
		t.Fatalf("crashed event lacks a next step: %q", detail)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL) // the orphaned shell must not outlive the test
}

// F-SURV-04 (agent tree): an agent-shaped process tree — a leader with a
// background descendant — survives the daemon SIGKILL as a whole.
func TestF_SURV_04_ProcessTreeSurvivesDaemonKill(t *testing.T) {
	root, journals := tmpDirs(t)
	d, pid := startFakeDaemon(t, root, journals, "a1")
	if err := d.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = d.Wait()

	h := newHost(root, journals)
	if err := h.Adopt(context.Background(), "a1"); err != nil {
		t.Fatal(err)
	}
	s := h.Session("a1")
	// `set +m` keeps the background job in the leader's process group; with job
	// control on (an interactive shell's default) a job gets its own group and is
	// reached by the shell's own SIGHUP forwarding, exactly as under tmux.
	_ = h.SendKeys(context.Background(), "a1", "-l", "set +m; sleep 300 & echo child=$!")
	_ = h.SendKeys(context.Background(), "a1", "Enter")
	var child int
	eventually(t, "background child to start", func() bool {
		for _, ln := range strings.Split(s.Screen(), "\n") {
			if rest, ok := strings.CutPrefix(strings.TrimSpace(ln), "child="); ok {
				child, _ = strconv.Atoi(rest)
			}
		}
		return child > 0
	})
	h.Close() // detach, as on a daemon update
	if !alive(pid) || !alive(child) {
		t.Fatalf("tree lost on detach: leader=%v child=%v", alive(pid), alive(child))
	}
	h2 := newHost(root, journals)
	defer h2.Close()
	if err := h2.Adopt(context.Background(), "a1"); err != nil {
		t.Fatal(err)
	}
	if err := h2.KillSession(context.Background(), "a1"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "whole tree to end on explicit kill", func() bool { return !alive(pid) && !alive(child) })
}

// Lease generation, input-id dedupe and human precedence hold ACROSS a daemon
// restart: the new epoch's generations are above every old one, a resend of an
// input the old daemon already applied is dropped, and a typing human still
// outranks operator/warden input on the adopted session.
func TestFencingAndPrecedenceSurviveAdoption(t *testing.T) {
	root, journals := tmpDirs(t)
	h1 := newHost(root, journals)
	if err := h1.NewSession(context.Background(), "t5", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	s1 := h1.Session("t5")
	c1, err := s1.Acquire("laptop", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []struct {
		id   uint64
		data string
	}{{1, "echo one\r"}, {2, "echo two\r"}} {
		if _, err := s1.Input(ptyruntime.Write{Origin: ptyevent.OriginHuman, ClientID: "laptop", Data: []byte(in.data),
			Fence: &ptyruntime.Fence{Generation: c1.Generation, InputID: in.id}}); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "both lines to run", func() bool { return strings.Contains(s1.Screen(), "two") })
	h1.Close() // daemon restart

	h2 := newHost(root, journals)
	defer func() { _ = h2.KillSession(context.Background(), "t5"); h2.Close() }()
	if err := h2.Adopt(context.Background(), "t5"); err != nil {
		t.Fatal(err)
	}
	s2 := h2.Session("t5")
	if s2.LastInputID("laptop") != 2 {
		t.Fatalf("applied input ids not restored from the journal: %d", s2.LastInputID("laptop"))
	}
	// The pre-restart lease is void: a client still holding it is stale.
	_, err = s2.Input(ptyruntime.Write{Origin: ptyevent.OriginHuman, ClientID: "laptop", Data: []byte("echo stale\r"),
		Fence: &ptyruntime.Fence{Generation: c1.Generation, InputID: 3}})
	if !errors.Is(err, ptyruntime.ErrStaleController) {
		t.Fatalf("write under the pre-restart generation: %v, want ErrStaleController", err)
	}
	c2, err := s2.Acquire("laptop", false)
	if err != nil {
		t.Fatal(err)
	}
	if c2.Generation <= c1.Generation {
		t.Fatalf("generation did not advance across the restart: %d -> %d", c1.Generation, c2.Generation)
	}
	// The client resends everything it never saw acknowledged: 2 was applied.
	_, err = s2.Input(ptyruntime.Write{Origin: ptyevent.OriginHuman, ClientID: "laptop", Data: []byte("echo two\r"),
		Fence: &ptyruntime.Fence{Generation: c2.Generation, InputID: 2}})
	if !errors.Is(err, ptyruntime.ErrDuplicateInput) {
		t.Fatalf("resent input 2: %v, want ErrDuplicateInput", err)
	}
	if _, err := s2.Input(ptyruntime.Write{Origin: ptyevent.OriginHuman, ClientID: "laptop", Data: []byte("echo three"),
		Fence: &ptyruntime.Fence{Generation: c2.Generation, InputID: 3}}); err != nil {
		t.Fatal(err)
	}
	// A human is mid-line ("echo three" has no Enter): operator and warden input
	// are deferred, not interleaved into the line.
	arb, err := s2.Input(ptyruntime.Write{Origin: ptyevent.OriginOperator, Data: []byte("rm -rf nothing\r")})
	if err != nil || arb.Verdict != ptyevent.VerdictDefer {
		t.Fatalf("operator input during a human line: %+v %v, want defer", arb, err)
	}
	arb, err = s2.Input(ptyruntime.Write{Origin: ptyevent.OriginWarden, DecisionID: "d1", Data: []byte("y\r")})
	if err != nil || arb.Verdict != ptyevent.VerdictDefer {
		t.Fatalf("warden input during a human line: %+v %v, want defer", arb, err)
	}
	if _, err := s2.Input(ptyruntime.Write{Origin: ptyevent.OriginHuman, ClientID: "laptop", Data: []byte("\r"),
		Fence: &ptyruntime.Fence{Generation: c2.Generation, InputID: 4}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the typed line to run exactly once", func() bool { return strings.Count(s2.Screen(), "three") >= 2 })
	// The adopted screen starts empty, so any "two" here would be a second run.
	if strings.Contains(s2.Screen(), "two") {
		t.Fatalf("the resent `echo two` ran again:\n%s", s2.Screen())
	}
}

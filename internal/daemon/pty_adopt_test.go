package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/srjn45/warden/internal/ptyholder"
	"github.com/srjn45/warden/internal/ptyhost"
	"github.com/srjn45/warden/internal/ptyruntime"
	"github.com/srjn45/warden/internal/store"
	"github.com/srjn45/warden/internal/terminalstore"
)

// adoptRig is a daemon "generation": a PTY manager over shared state dirs, with
// shells hosted by real holder processes (this test binary re-exec'd).
type adoptRig struct {
	t      *testing.T
	dir    string
	srv    *Server
	mgr    *ptyhost.Manager
	ts     *terminalstore.Store
	life   *fakeLife
	agents *fakeStore
}

func newAdoptRig(t *testing.T) *adoptRig {
	t.Helper()
	d, err := os.MkdirTemp("", "wda") // short: holder socket paths are length-limited
	require.NoError(t, err)
	t.Cleanup(func() { ptyholder.KillAll(filepath.Join(d, "pty", "holder")); os.RemoveAll(d) })
	ts, err := terminalstore.New(filepath.Join(d, "terminals-db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = ts.Close() })
	r := &adoptRig{t: t, dir: d, ts: ts, life: &fakeLife{}, agents: newFakeStore()}
	r.restart()
	return r
}

// restart is a daemon stop + start: the old manager detaches (graceful) and a
// new manager/Server comes up over the same data.
func (r *adoptRig) restart() {
	if r.mgr != nil {
		r.mgr.Close()
	}
	exe, _ := os.Executable()
	r.mgr = ptyhost.New(ptyhost.Config{Mode: ptyhost.ModeOn, Dir: filepath.Join(r.dir, "pty"), Shell: []string{"/bin/sh"},
		Launcher: &ptyholder.Launcher{Root: filepath.Join(r.dir, "pty", "holder"), Command: []string{exe, "__ptyholder"}, ExitedLinger: time.Minute},
		Options:  ptyruntime.Options{KillGrace: 200 * time.Millisecond}})
	r.mgr.SetTerminalResolver(func(name string) bool { _, err := r.ts.Get(context.Background(), name); return err == nil })
	r.srv = &Server{store: r.agents, life: r.life, terminals: r.ts, hub: newHub(), done: make(chan struct{}), pty: r.mgr}
}

func (r *adoptRig) spawnAgent(id string) int {
	return r.spawnAgentCommand(id, "exec sleep 1000")
}

func (r *adoptRig) spawnAgentCommand(id, command string) int {
	r.t.Helper()
	r.mgr.SetAgentDirectReady(true)
	h, ok := r.mgr.Host().(interface {
		LaunchCommand(context.Context, string, string, []string, ...string) error
	})
	require.True(r.t, ok)
	require.NoError(r.t, h.LaunchCommand(context.Background(), id, r.t.TempDir(), []string{"/bin/sh", "-c", command}))
	d := r.mgr.Direct(id)
	require.NotNil(r.t, d)
	holderPID, holderStart, proto, ok := r.mgr.HolderMeta(id)
	require.True(r.t, ok)
	require.NoError(r.t, r.agents.Insert(context.Background(), &agentstore.Agent{
		ID: id, TmuxSession: id, Workdir: r.t.TempDir(), Status: store.StatusWorking,
		Runtime: &store.SessionRuntime{Kind: "direct", PinnedMode: "on", HolderPID: holderPID, HolderStart: holderStart, Proto: proto},
	}))
	return d.PID()
}

func (r *adoptRig) launchUnrecordedAgent(id, command string) int {
	r.t.Helper()
	r.mgr.SetAgentDirectReady(true)
	h, ok := r.mgr.Host().(interface {
		LaunchCommand(context.Context, string, string, []string, ...string) error
	})
	require.True(r.t, ok)
	require.NoError(r.t, h.LaunchCommand(context.Background(), id, r.t.TempDir(), []string{"/bin/sh", "-c", command}))
	d := r.mgr.Direct(id)
	require.NotNil(r.t, d)
	return d.PID()
}

func (r *adoptRig) spawn(id string) (pid int) {
	r.t.Helper()
	wd := r.t.TempDir()
	require.NoError(r.t, r.mgr.TerminalHost().NewSession(context.Background(), id, wd))
	require.NoError(r.t, r.ts.Create(context.Background(), &terminalstore.Terminal{ID: id, TmuxSession: id, Workdir: wd,
		Status: terminalstore.StatusRunning, CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	return r.mgr.Direct(id).PID()
}

func procAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// F-SURV-01/02 at the daemon seam: after a restart, startup recovery adopts the
// live shell (same pid) and does NOT relaunch it.
func TestRecoverPTYAdoptsLiveHolderTerminal(t *testing.T) {
	r := newAdoptRig(t)
	ctx := context.Background()
	pid := r.spawn("term-keep")
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })

	r.restart() // daemon update / restart
	require.Nil(t, r.mgr.Direct("term-keep"), "nothing hosted before recovery")
	r.srv.recoverPTY(ctx)

	d := r.mgr.Direct("term-keep")
	require.NotNil(t, d, "the live shell must be adopted")
	require.Equal(t, pid, d.PID(), "same process, not a relaunch")
	require.Empty(t, r.life.restored, "recovery must not relaunch a live shell")
	rec, err := r.ts.Get(ctx, "term-keep")
	require.NoError(t, err)
	require.Equal(t, terminalstore.StatusRunning, rec.Status)
	require.True(t, r.mgr.HolderHosted("term-keep"))

	// And it is the same interactive shell: it answers, with its own pid.
	require.NoError(t, r.mgr.Host().SendKeys(ctx, "term-keep", "-l", "echo still-$$"))
	require.NoError(t, r.mgr.Host().SendKeys(ctx, "term-keep", "Enter"))
	require.Eventually(t, func() bool { return strings.Contains(d.Screen(), "still-"+itoa(pid)) }, 10*time.Second, 10*time.Millisecond)
}

// F-SURV-03 at the daemon seam: if the holder is gone the terminal is NOT
// silently relaunched; it is marked lost with an actionable journal entry and
// stays restorable by an explicit command.
func TestRecoverPTYHolderGoneIsReportedNotRelaunched(t *testing.T) {
	r := newAdoptRig(t)
	ctx := context.Background()
	pid := r.spawn("term-lost")
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	h, err := ptyholder.Open(filepath.Join(r.dir, "pty", "holder"), "term-lost")
	require.NoError(t, err)
	m, _ := h.Meta()

	r.restart()
	require.NoError(t, syscall.Kill(m.HolderPID, syscall.SIGKILL))
	require.Eventually(t, func() bool { return !procAlive(m.HolderPID) }, 10*time.Second, 5*time.Millisecond)
	r.srv.recoverPTY(ctx)

	require.Empty(t, r.life.restored, "a lost holder must never trigger a silent relaunch")
	require.Nil(t, r.mgr.Direct("term-lost"))
	rec, _ := r.ts.Get(ctx, "term-lost")
	require.Equal(t, terminalstore.StatusOrphaned, rec.Status, "restorable, and visibly not running")

	j, err := ptyruntime.OpenJournal(filepath.Join(r.dir, "pty", "direct", "term-lost"), "term-lost", ptyruntime.Retention{})
	require.NoError(t, err)
	defer j.Close()
	var detail string
	_ = j.Read(1, func(ev ptyevent.Event) error {
		if ev.Kind == ptyevent.KindLifecycle && ev.Lifecycle.Phase == ptyevent.PhaseCrashed {
			detail = ev.Lifecycle.Detail
		}
		return nil
	})
	require.Contains(t, detail, "warden agent restore term-lost", "the journal names the cause and the next step")
}

// A shell that ended while the daemon was away is recorded with its real status.
func TestRecoverPTYShellExitedWhileDaemonDown(t *testing.T) {
	r := newAdoptRig(t)
	ctx := context.Background()
	r.spawn("term-gone")
	require.NoError(t, r.mgr.Host().SendKeys(ctx, "term-gone", "-l", "exit 7"))
	require.NoError(t, r.mgr.Host().SendKeys(ctx, "term-gone", "Enter"))
	require.Eventually(t, func() bool {
		select {
		case <-r.mgr.Direct("term-gone").Done():
			return true
		default:
			return false
		}
	}, 10*time.Second, 5*time.Millisecond)
	// The exit was already journaled by this generation; make the store forget it
	// (as if the daemon died before recording) and bring up a new generation.
	require.NoError(t, r.ts.Update(ctx, "term-gone", func(u *terminalstore.Terminal) error {
		u.Status, u.ExitCode = terminalstore.StatusRunning, nil
		return nil
	}))
	r.mgr.Close()
	r.restart()
	r.srv.recoverPTY(ctx)
	require.Eventually(t, func() bool {
		rec, _ := r.ts.Get(ctx, "term-gone")
		return rec.Status == terminalstore.StatusExited
	}, 10*time.Second, 5*time.Millisecond)
	rec, _ := r.ts.Get(ctx, "term-gone")
	require.NotNil(t, rec.ExitCode)
	require.Equal(t, 7, *rec.ExitCode)
}

// Direct agents have provider-native conversations, so recovery must adopt the
// exact holder-owned child even when runtime.pty was rolled back globally. It
// must also retain its agent identity (not be misclassified as a shell).
func TestRecoverPTYAdoptsLiveHolderAgentAfterRollback(t *testing.T) {
	r := newAdoptRig(t)
	ctx := context.Background()
	pid := r.spawnAgent("agent-keep")
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })

	r.restart()
	r.mgr.SetMode(ptyhost.ModeOff) // applies only to new sessions
	r.srv.recoverPTY(ctx)

	d := r.mgr.Direct("agent-keep")
	require.NotNil(t, d)
	require.Equal(t, pid, d.PID(), "recovery must retain the existing child")
	require.False(t, r.mgr.IsTerminal("agent-keep"), "adopting an agent must not turn it into a terminal")
	rec, err := r.agents.Get(ctx, "agent-keep")
	require.NoError(t, err)
	require.Equal(t, store.StatusWorking, rec.Status)
}

// A crash after the holder creates the child but before agent-record persistence
// leaves an unknown holder. Retrying that launch must adopt the same child, not
// run the task/seed command a second time.
func TestAgentLaunchAdoptsUnrecordedHolderWithoutDuplicateTaskStart(t *testing.T) {
	r := newAdoptRig(t)
	ctx := context.Background()
	starts := filepath.Join(r.t.TempDir(), "starts")
	pid := r.launchUnrecordedAgent("agent-unrecorded", "printf first-start >>"+starts+"; printf conversation-marker; exec sleep 1000")
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })

	r.restart()
	r.mgr.SetAgentDirectReady(true)
	h, ok := r.mgr.Host().(interface {
		LaunchCommand(context.Context, string, string, []string, ...string) error
	})
	require.True(t, ok)
	require.NoError(t, h.LaunchCommand(ctx, "agent-unrecorded", r.t.TempDir(), []string{"/bin/sh", "-c", "printf replacement-start >>" + starts + "; exec sleep 1000"}))
	d := r.mgr.Direct("agent-unrecorded")
	require.NotNil(t, d)
	require.Equal(t, pid, d.PID(), "retry must adopt the original child")
	require.Eventually(t, func() bool { return strings.Contains(d.Screen(), "conversation-marker") }, 5*time.Second, 10*time.Millisecond)
	data, err := os.ReadFile(starts)
	require.NoError(t, err)
	require.Equal(t, "first-start", string(data), "task must start exactly once")
	require.Empty(t, r.life.restored, "adoption must not invoke lifecycle restore")
}

// Output written by a direct agent while the daemon is absent is retained in
// the kernel PTY buffer for a bounded burst and appears after holder adoption.
// This is deliberately not a claim of unbounded/lossless offline output: once
// the kernel buffer fills, the child receives normal PTY backpressure.
func TestRecoverPTYAgentRetainsBoundedOfflineOutput(t *testing.T) {
	r := newAdoptRig(t)
	ctx := context.Background()
	pid := r.spawnAgentCommand("agent-offline-output", "sleep 0.1; printf offline-agent-output; exec sleep 1000")
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })

	r.mgr.Close() // simulate daemon absence; the holder retains the PTY master
	time.Sleep(250 * time.Millisecond)
	r.restart()
	r.srv.recoverPTY(ctx)
	d := r.mgr.Direct("agent-offline-output")
	require.NotNil(t, d)
	require.Equal(t, pid, d.PID(), "adoption must retain the original child")
	require.Eventually(t, func() bool { return strings.Contains(d.Screen(), "offline-agent-output") }, 5*time.Second, 10*time.Millisecond)
}

// An agent that exits while the daemon is absent is adopted only to collect its
// real lifecycle result; recovery never starts another provider process.
func TestRecoverPTYAgentExitWhileDaemonDownIsNotRelaunched(t *testing.T) {
	r := newAdoptRig(t)
	ctx := context.Background()
	pid := r.spawnAgentCommand("agent-offline-exit", "sleep 0.1; exit 7")

	r.mgr.Close()
	time.Sleep(250 * time.Millisecond)
	r.restart()
	r.srv.recoverPTY(ctx)
	require.Eventually(t, func() bool {
		rec, err := r.agents.Get(ctx, "agent-offline-exit")
		return err == nil && rec.Status == store.StatusOrphaned
	}, 5*time.Second, 10*time.Millisecond)
	require.Empty(t, r.life.restored, "offline exit must not start a replacement")
	_ = pid // retained as an explicit same-child fixture identity
}

// A direct agent without a verifiable holder is unsafe to restart: preserve
// its conversation/worktree record as orphaned and leave launch to an explicit
// restore operation.
func TestRecoverPTYLostHolderAgentIsOrphanedNotRelaunched(t *testing.T) {
	r := newAdoptRig(t)
	ctx := context.Background()
	pid := r.spawnAgent("agent-lost")
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	h, err := ptyholder.Open(filepath.Join(r.dir, "pty", "holder"), "agent-lost")
	require.NoError(t, err)
	m, _ := h.Meta()

	r.restart()
	require.NoError(t, syscall.Kill(m.HolderPID, syscall.SIGKILL))
	require.Eventually(t, func() bool { return !procAlive(m.HolderPID) }, 10*time.Second, 5*time.Millisecond)
	r.srv.recoverPTY(ctx)

	require.Nil(t, r.mgr.Direct("agent-lost"))
	rec, err := r.agents.Get(ctx, "agent-lost")
	require.NoError(t, err)
	require.Equal(t, store.StatusOrphaned, rec.Status)
}

// PID reuse or an old state generation must not make a different holder look
// adoptable merely because it still has the same session name.
func TestRecoverPTYRejectsStaleAgentHolderIdentity(t *testing.T) {
	r := newAdoptRig(t)
	ctx := context.Background()
	pid := r.spawnAgent("agent-stale")
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	require.NoError(t, r.agents.Update(ctx, "agent-stale", func(a *agentstore.Agent) error {
		a.Runtime.HolderStart++
		return nil
	}))

	r.restart()
	r.srv.recoverPTY(ctx)
	require.Nil(t, r.mgr.Direct("agent-stale"), "a stale runtime identity must not attach")
	rec, err := r.agents.Get(ctx, "agent-stale")
	require.NoError(t, err)
	require.Equal(t, store.StatusOrphaned, rec.Status)
}

func itoa(n int) string {
	b := []byte{}
	if n == 0 {
		return "0"
	}
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

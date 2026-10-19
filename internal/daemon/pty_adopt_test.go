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

	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/srjn45/warden/internal/ptyholder"
	"github.com/srjn45/warden/internal/ptyhost"
	"github.com/srjn45/warden/internal/ptyruntime"
	"github.com/srjn45/warden/internal/terminalstore"
)

// adoptRig is a daemon "generation": a PTY manager over shared state dirs, with
// shells hosted by real holder processes (this test binary re-exec'd).
type adoptRig struct {
	t    *testing.T
	dir  string
	srv  *Server
	mgr  *ptyhost.Manager
	ts   *terminalstore.Store
	life *fakeLife
}

func newAdoptRig(t *testing.T) *adoptRig {
	t.Helper()
	d, err := os.MkdirTemp("", "wda") // short: holder socket paths are length-limited
	require.NoError(t, err)
	t.Cleanup(func() { ptyholder.KillAll(filepath.Join(d, "pty", "holder")); os.RemoveAll(d) })
	ts, err := terminalstore.New(filepath.Join(d, "terminals-db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = ts.Close() })
	r := &adoptRig{t: t, dir: d, ts: ts, life: &fakeLife{}}
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
	r.srv = &Server{store: newFakeStore(), life: r.life, terminals: r.ts, hub: newHub(), done: make(chan struct{}), pty: r.mgr}
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

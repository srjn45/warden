package ptyhost

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/srjn45/warden/internal/ptyruntime"
)

func newShManager(t *testing.T, run tmuxRunner, dir string, mode Mode) *Manager {
	t.Helper()
	return New(Config{Mode: mode, Dir: dir, Run: run, Shell: []string{"/bin/sh"}, ReconcileEvery: 50 * time.Millisecond,
		Options: ptyruntime.Options{IdleFlush: 20 * time.Millisecond, KillGrace: 500 * time.Millisecond, DrainGrace: 300 * time.Millisecond}})
}

// F-LIFE-05: the identity a shell sees — cwd, session id env — is the same
// whether the terminal is a tmux pane or a direct PTY. (TERM is deliberately
// excluded: tmux sets its own TERM; the direct host pins xterm-256color.)
func TestTerminalIdentityEqualTmuxAndDirect(t *testing.T) {
	probe := `echo "ID[$WARDEN_SESSION_ID|$AGENTCTL_SESSION_ID] CWD[$(basename "$(pwd -P)")]"`
	got := map[Mode]string{}
	cwd, _ := filepath.EvalSymlinks(t.TempDir())
	for _, mode := range []Mode{ModeOff, ModeOn} {
		noLeaks(t)
		run := newTmux(t)
		m := newShManager(t, run, filepath.Join(t.TempDir(), "pty"), mode)
		ctx := context.Background()
		if err := m.TerminalHost().NewSession(ctx, "term-id", cwd); err != nil {
			t.Fatal(err)
		}
		h := m.Host()
		// tmux starts its own login shell asynchronously; retry the probe until
		// the prompt accepts it (output is the only readiness signal tmux gives).
		want := "ID[term-id|term-id] CWD[" + filepath.Base(cwd) + "]"
		eventually(t, string(mode)+" probe output", func() bool {
			_ = h.SendKeys(ctx, "term-id", probe, "Enter")
			pane, _ := h.CapturePane(ctx, "term-id")
			for _, l := range strings.Split(pane, "\n") {
				if strings.TrimSpace(l) == want {
					got[mode] = strings.TrimSpace(l)
					return true
				}
			}
			return false
		})
		_ = h.KillSession(ctx, "term-id")
		m.Close()
	}
	if got[ModeOff] != got[ModeOn] || got[ModeOn] == "" {
		t.Fatalf("identity differs: tmux=%q direct=%q", got[ModeOff], got[ModeOn])
	}
}

// F-LIFE-04: `exit 3` in a direct shell ends the SESSION (no nested shell) and
// journals exit code 3 — the value the daemon stores as ExitCode.
func TestDirectShellExitCodeJournaled(t *testing.T) {
	noLeaks(t)
	run := newTmux(t)
	dir := filepath.Join(t.TempDir(), "pty")
	m := newShManager(t, run, dir, ModeOn)
	defer m.Close()
	ctx := context.Background()
	if err := m.TerminalHost().NewSession(ctx, "term-x", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := m.Host().SendKeys(ctx, "term-x", "exit 3", "Enter"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "exit lifecycle", func() bool {
		lc := lastLifecycle(journal(t, directDir(dir, "term-x"), "term-x"))
		return lc != nil && lc.Phase == ptyevent.PhaseExited
	})
	lc := lastLifecycle(journal(t, directDir(dir, "term-x"), "term-x"))
	if lc.ExitCode == nil || *lc.ExitCode != 3 {
		t.Fatalf("exit lifecycle = %+v, want exit code 3", lc)
	}
}

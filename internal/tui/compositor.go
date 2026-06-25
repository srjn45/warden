package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"github.com/srjn45/warden/internal/lifecycle"
)

// cockpitSession returns the tmux session name for the cockpit owned by the given pid.
func cockpitSession(pid int) string {
	return fmt.Sprintf("warden-tui-%d", pid)
}

type cockpitOpts struct {
	session   string // tmux session name, e.g. "warden-tui-1234"
	self      string // absolute path to the warden binary
	homeDir   string // cwd for the list pane process
	masterCwd string // cwd for the master shell pane (the launching shell's dir)
}

// shquote single-quotes s so tmux's `sh -c <command>` preserves spaces.
func shquote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// listPaneCmd is the shell command tmux runs for the top-left list pane. It is
// told the detail pane's id so it can drive (respawn) it when the user opens an
// agent with Enter.
func listPaneCmd(self, detailPane string) string {
	return self + " tui --pane=list --detail-pane=" + detailPane
}

// detailPlaceholderCmd keeps the right pane alive showing a hint until the user
// opens an agent into it. `exec sleep` so the process is cleanly replaceable by
// `respawn-pane`.
func detailPlaceholderCmd() string {
	return `sh -c 'printf "Select an agent and press Enter to open it here.\n"; exec sleep 2147483647'`
}

// runPaneCreate runs a pane-creating tmux command (-P -F '#{pane_id}') and
// returns the new pane id, so later commands target panes by stable id rather
// than by spatial index (which tmux renumbers on every split).
func runPaneCreate(ctx context.Context, run lifecycle.Runner, args ...string) (string, error) {
	out, err := run.Run(ctx, "", "tmux", args...)
	if err != nil {
		return "", fmt.Errorf("tmux %s: %w: %s", args[0], err, out)
	}
	return strings.TrimSpace(out), nil
}

// buildCockpit constructs the three-pane layout in a detached tmux session:
//
//	┌─ list (top-left) ─┐┌─ detail ─────┐
//	├─ master (shell) ──┤│ (full height)│
//	└───────────────────┘└──────────────┘
//
// Panes are created right-to-left so the list pane (created last) can be handed
// the detail pane's stable id (--detail-pane) and drive it via respawn-pane. The
// detail pane starts as a placeholder; the list pane opens an agent into it on
// Enter. The master pane runs a shell ($SHELL) for terminal access. The caller
// attaches afterwards.
func buildCockpit(ctx context.Context, run lifecycle.Runner, o cockpitOpts) error {
	// 1. Detail pane fills the window initially (placeholder); capture its id.
	detailID, err := runPaneCreate(ctx, run,
		"new-session", "-d", "-s", o.session, "-c", o.homeDir,
		"-P", "-F", "#{pane_id}", detailPlaceholderCmd())
	if err != nil {
		return err
	}
	// 2. Master shell to the LEFT of detail (-b), 40% width, in the launch dir.
	// Uses $SHELL (defaulting to /bin/sh) for a plain terminal pane.
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	masterID, err := runPaneCreate(ctx, run,
		"split-window", "-h", "-b", "-l", "40%", "-t", detailID, "-c", o.masterCwd,
		"-P", "-F", "#{pane_id}", shell)
	if err != nil {
		return err
	}
	// 3. List pane ABOVE master (-b), 50% of the left column; it gets detailID.
	// It runs in masterCwd (the launching shell's dir) so agents spawned from it
	// (`n`) launch in that dir — os.Getwd() in the pane is what spawnCmd sends.
	listID, err := runPaneCreate(ctx, run,
		"split-window", "-v", "-b", "-l", "50%", "-t", masterID, "-c", o.masterCwd,
		"-P", "-F", "#{pane_id}", listPaneCmd(o.self, detailID))
	if err != nil {
		return err
	}
	// 4. Keep the detail pane (showing [exited]) instead of collapsing the layout
	//    when an opened agent's attach exits.
	if out, err := run.Run(ctx, "", "tmux", "set-option", "-p", "-t", detailID, "remain-on-exit", "on"); err != nil {
		return fmt.Errorf("tmux set-option remain-on-exit: %w: %s", err, out)
	}
	// 5. Mouse + prefix-less Alt+Arrow pane navigation.
	if out, err := run.Run(ctx, "", "tmux", "set-option", "-t", o.session, "mouse", "on"); err != nil {
		return fmt.Errorf("tmux set-option mouse: %w: %s", err, out)
	}
	// Make a newline key work in the detail Claude pane: extended-keys passthrough
	// (Shift+Enter on capable terminals) plus an Alt+Enter fallback for terminals
	// like VTE/GNOME that can't report Shift+Enter. Best-effort: never block the
	// cockpit on it.
	lifecycle.EnsureExtendedKeys(ctx, run)
	for _, b := range [][2]string{{"M-Left", "-L"}, {"M-Right", "-R"}, {"M-Up", "-U"}, {"M-Down", "-D"}} {
		if out, err := run.Run(ctx, "", "tmux", "bind-key", "-n", b[0], "select-pane", b[1]); err != nil {
			return fmt.Errorf("tmux bind-key %s: %w: %s", b[0], err, out)
		}
	}
	// 6. Focus the list pane.
	if out, err := run.Run(ctx, "", "tmux", "select-pane", "-t", listID); err != nil {
		return fmt.Errorf("tmux select-pane: %w: %s", err, out)
	}
	// 7. Bind <prefix> Enter to "switch to the last session". The full-screen
	// attach (`a`) moves this client to the agent's session (switch-client; tmux
	// can't nest an attach), so the user needs a way back to this dashboard —
	// <prefix> Enter returns them. -l is per-client, so it stays correct even with
	// multiple cockpits. switchClientCmd flashes this hint; killCockpitCmd unbinds.
	if out, err := run.Run(ctx, "", "tmux", "bind-key", "Enter", "switch-client", "-l"); err != nil {
		return fmt.Errorf("tmux bind-key: %w: %s", err, out)
	}
	return nil
}

// pidAlive reports whether a process with pid exists (signal 0 probe).
func pidAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 probes existence: nil = alive-and-ours; EPERM = alive but owned
	// by another user (still alive). Only ESRCH (no such process) means dead.
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

// cleanStaleCockpits kills cockpit tmux sessions (warden-tui-<pid>) whose
// owning warden process is dead — orphans left behind when a user detached
// (Ctrl-b d) instead of quitting. Best-effort: a missing tmux server (no
// sessions yet) or any error is ignored. Live cockpits and the user's own
// sessions are never touched.
func cleanStaleCockpits(run lifecycle.Runner) {
	out, err := run.Run(context.Background(), "", "tmux", "list-sessions", "-F", "#{session_name}")
	if err != nil {
		return
	}
	const prefix = "warden-tui-"
	for _, name := range strings.Split(strings.TrimSpace(out), "\n") {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimPrefix(name, prefix))
		if err != nil {
			continue
		}
		if !pidAlive(pid) {
			_, _ = run.Run(context.Background(), "", "tmux", "kill-session", "-t", name)
		}
	}
}

// RunCockpit builds the tmux cockpit for this process and attaches to it,
// blocking until the user detaches/quits. masterCwd is the launching shell's
// directory (where the master shell pane runs).
func RunCockpit(a api, self, masterCwd string) error {
	_ = a // the panes hold their own clients; reserved for future inline checks
	// Reap cockpits orphaned by a prior detach (their owning pid is gone).
	cleanStaleCockpits(lifecycle.HintingExecRunner{Inner: lifecycle.ExecRunner{}})

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home dir: %w", err)
	}
	o := cockpitOpts{
		session:   cockpitSession(os.Getpid()),
		self:      self,
		homeDir:   home,
		masterCwd: masterCwd,
	}
	if err := buildCockpit(context.Background(), lifecycle.HintingExecRunner{Inner: lifecycle.ExecRunner{}}, o); err != nil {
		// Tear down a half-built session so we never leave an orphan.
		_, _ = lifecycle.HintingExecRunner{Inner: lifecycle.ExecRunner{}}.Run(context.Background(), "", "tmux", "kill-session", "-t", o.session)
		return err
	}

	// We deliberately do NOT kill the session on return. Quitting (`q`) tears the
	// cockpit down explicitly from inside (killCockpitCmd); a bare detach
	// (Ctrl-b d) leaves it alive so an accidental detach doesn't destroy the
	// dashboard. Any cockpit orphaned by a detach is reaped by cleanStaleCockpits
	// on the next launch (its owning pid is gone).
	attach := exec.Command("tmux", "attach", "-t", o.session)
	attach.Stdin, attach.Stdout, attach.Stderr = os.Stdin, os.Stdout, os.Stderr
	return attach.Run()
}

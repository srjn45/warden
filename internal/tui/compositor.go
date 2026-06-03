package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/srajanpathak/agentctl/internal/lifecycle"
)

// chooseClassic decides whether to use the legacy single-pane TUI instead of the
// tmux-composited cockpit. We require a real, non-nested tmux: composited mode
// builds a new session and attaches to it, which can't be done cleanly from
// inside an existing tmux client.
func chooseClassic(classicFlag, tmuxAvailable, insideTmux bool) bool {
	return classicFlag || !tmuxAvailable || insideTmux
}

// tmuxAvailable reports whether the tmux binary is on PATH.
func tmuxAvailable() bool {
	_, err := exec.LookPath("tmux")
	return err == nil
}

// ChooseClassic is the exported wrapper for chooseClassic (used by the CLI).
func ChooseClassic(classicFlag, tmuxAvailable, insideTmux bool) bool {
	return chooseClassic(classicFlag, tmuxAvailable, insideTmux)
}

// TmuxAvailable reports whether the tmux binary is on PATH (used by the CLI).
func TmuxAvailable() bool { return tmuxAvailable() }

// cockpitSession returns the tmux session name for the cockpit owned by the given pid.
func cockpitSession(pid int) string {
	return fmt.Sprintf("agentctl-tui-%d", pid)
}

// cockpitStateDir returns the per-pid cockpit state directory under base.
func cockpitStateDir(base string, pid int) string {
	return filepath.Join(base, fmt.Sprintf("tui-%d", pid))
}

type cockpitOpts struct {
	session   string // tmux session name, e.g. "agentctl-tui-1234"
	self      string // absolute path to the agentctl binary
	stateDir  string // per-pid selection state dir
	homeDir   string // cwd for the list/detail pane processes
	masterCwd string // cwd for the master claude pane (the launching shell's dir)
}

// shquote single-quotes s so tmux's `sh -c <command>` preserves spaces.
func shquote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// listPaneCmd is the shell command tmux runs for the top-left list pane.
func listPaneCmd(self, stateDir string) string {
	return self + " tui --pane=list --state-dir=" + stateDir
}

// detailPaneCmd is the shell command tmux runs for the full-height right detail pane.
func detailPaneCmd(self, stateDir string) string {
	return self + " tui --pane=detail --state-dir=" + stateDir
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
//	├─ master (claude) ─┤│ (full height)│
//	└───────────────────┘└──────────────┘
//
// The caller attaches afterwards. tmux is the compositor; each pane is its own
// process. NOTE: if homeDir/stateDir/masterCwd can contain spaces, the pane
// command strings must be shquote()'d; agentctl paths are space-free in practice,
// and quoting them would change the exact strings asserted in tests.
func buildCockpit(ctx context.Context, run lifecycle.Runner, o cockpitOpts) error {
	listID, err := runPaneCreate(ctx, run,
		"new-session", "-d", "-s", o.session, "-c", o.homeDir,
		"-P", "-F", "#{pane_id}", listPaneCmd(o.self, o.stateDir))
	if err != nil {
		return err
	}
	if _, err := runPaneCreate(ctx, run,
		"split-window", "-h", "-l", "60%", "-t", listID, "-c", o.homeDir,
		"-P", "-F", "#{pane_id}", detailPaneCmd(o.self, o.stateDir)); err != nil {
		return err
	}
	if _, err := runPaneCreate(ctx, run,
		"split-window", "-v", "-l", "50%", "-t", listID, "-c", o.masterCwd,
		"-P", "-F", "#{pane_id}", "claude"); err != nil {
		return err
	}
	if out, err := run.Run(ctx, "", "tmux", "set-option", "-t", o.session, "mouse", "on"); err != nil {
		return fmt.Errorf("tmux set-option mouse: %w: %s", err, out)
	}
	if out, err := run.Run(ctx, "", "tmux", "select-pane", "-t", listID); err != nil {
		return fmt.Errorf("tmux select-pane: %w: %s", err, out)
	}
	// Bind <prefix> Enter to "switch to the last session". Attaching to an agent
	// moves this client to the agent's session (switch-client; tmux can't nest an
	// attach), so the user needs a way back to this dashboard — <prefix> Enter
	// returns them to wherever they came from. -l is per-client, so it stays
	// correct even with multiple cockpits. switchClientCmd flashes this hint on
	// attach; killCockpitCmd unbinds it on quit.
	if out, err := run.Run(ctx, "", "tmux", "bind-key", "Enter", "switch-client", "-l"); err != nil {
		return fmt.Errorf("tmux bind-key: %w: %s", err, out)
	}
	return nil
}

// cockpitBaseDir is the parent of all per-pid cockpit state dirs.
func cockpitBaseDir() string {
	if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" {
		return filepath.Join(x, "agentctl")
	}
	return filepath.Join(os.TempDir(), "agentctl")
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

// cleanStaleStateDirs removes tui-<pid> dirs under base whose pid is no longer
// alive. Best-effort: errors are ignored (a leftover dir is harmless).
func cleanStaleStateDirs(base string) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "tui-") {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimPrefix(e.Name(), "tui-"))
		if err != nil {
			continue
		}
		if !pidAlive(pid) {
			_ = os.RemoveAll(filepath.Join(base, e.Name()))
		}
	}
}

// cleanStaleCockpits kills cockpit tmux sessions (agentctl-tui-<pid>) whose
// owning agentctl process is dead — orphans left behind when a user detached
// (Ctrl-b d) instead of quitting. Best-effort: a missing tmux server (no
// sessions yet) or any error is ignored. Live cockpits and the user's own
// sessions are never touched.
func cleanStaleCockpits(run lifecycle.Runner) {
	out, err := run.Run(context.Background(), "", "tmux", "list-sessions", "-F", "#{session_name}")
	if err != nil {
		return
	}
	const prefix = "agentctl-tui-"
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
// directory (where the master claude pane runs). It cleans up this run's state
// dir on exit and sweeps stale dirs from dead prior runs on entry.
func RunCockpit(a api, self, masterCwd string) error {
	_ = a // the panes hold their own clients; reserved for future inline checks
	pid := os.Getpid()
	base := cockpitBaseDir()
	cleanStaleStateDirs(base)
	cleanStaleCockpits(lifecycle.ExecRunner{})

	stateDir := cockpitStateDir(base, pid)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	defer os.RemoveAll(stateDir)

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home dir: %w", err)
	}
	o := cockpitOpts{
		session:   cockpitSession(pid),
		self:      self,
		stateDir:  stateDir,
		homeDir:   home,
		masterCwd: masterCwd,
	}
	if err := buildCockpit(context.Background(), lifecycle.ExecRunner{}, o); err != nil {
		// Tear down a half-built session so we never leave an orphan.
		_, _ = lifecycle.ExecRunner{}.Run(context.Background(), "", "tmux", "kill-session", "-t", o.session)
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

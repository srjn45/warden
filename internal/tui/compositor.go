package tui

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

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

func cockpitSession(pid int) string {
	return fmt.Sprintf("agentctl-tui-%d", pid)
}

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

func listPaneCmd(self, stateDir string) string {
	return self + " tui --pane=list --state-dir=" + stateDir
}

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
	return nil
}

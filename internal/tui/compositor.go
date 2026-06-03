package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
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

type cockpitOpts struct {
	session   string // tmux session name, e.g. "agentctl-tui-1234"
	self      string // absolute path to the agentctl binary
	homeDir   string // cwd for the list pane process
	masterCwd string // cwd for the master claude pane (the launching shell's dir)
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
//	├─ master (claude) ─┤│ (full height)│
//	└───────────────────┘└──────────────┘
//
// Panes are created right-to-left so the list pane (created last) can be handed
// the detail pane's stable id (--detail-pane) and drive it via respawn-pane. The
// detail pane starts as a placeholder; the list pane opens an agent into it on
// Enter. The caller attaches afterwards.
func buildCockpit(ctx context.Context, run lifecycle.Runner, o cockpitOpts) error {
	// 1. Detail pane fills the window initially (placeholder); capture its id.
	detailID, err := runPaneCreate(ctx, run,
		"new-session", "-d", "-s", o.session, "-c", o.homeDir,
		"-P", "-F", "#{pane_id}", detailPlaceholderCmd())
	if err != nil {
		return err
	}
	// 2. Master claude to the LEFT of detail (-b), 40% width, in the launch dir.
	masterID, err := runPaneCreate(ctx, run,
		"split-window", "-h", "-b", "-l", "40%", "-t", detailID, "-c", o.masterCwd,
		"-P", "-F", "#{pane_id}", "claude")
	if err != nil {
		return err
	}
	// 3. List pane ABOVE master (-b), 50% of the left column; it gets detailID.
	listID, err := runPaneCreate(ctx, run,
		"split-window", "-v", "-b", "-l", "50%", "-t", masterID, "-c", o.homeDir,
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
	for _, b := range [][2]string{{"M-Left", "-L"}, {"M-Right", "-R"}, {"M-Up", "-U"}, {"M-Down", "-D"}} {
		if out, err := run.Run(ctx, "", "tmux", "bind-key", "-n", b[0], "select-pane", b[1]); err != nil {
			return fmt.Errorf("tmux bind-key %s: %w: %s", b[0], err, out)
		}
	}
	// 6. Focus the list pane.
	if out, err := run.Run(ctx, "", "tmux", "select-pane", "-t", listID); err != nil {
		return fmt.Errorf("tmux select-pane: %w: %s", err, out)
	}
	return nil
}

// RunCockpit builds the tmux cockpit for this process and attaches to it,
// blocking until the user detaches/quits. masterCwd is the launching shell's
// directory (where the master claude pane runs).
func RunCockpit(a api, self, masterCwd string) error {
	_ = a // the panes hold their own clients; reserved for future inline checks
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
	if err := buildCockpit(context.Background(), lifecycle.ExecRunner{}, o); err != nil {
		// Tear down a half-built session so we never leave an orphan.
		_, _ = lifecycle.ExecRunner{}.Run(context.Background(), "", "tmux", "kill-session", "-t", o.session)
		return err
	}
	// Always tear the session down on return (covers detach, where `tmux attach`
	// returns 0 while the session keeps running). kill-session on a gone session
	// is a harmless ignored error.
	defer lifecycle.ExecRunner{}.Run(context.Background(), "", "tmux", "kill-session", "-t", o.session)

	attach := exec.Command("tmux", "attach", "-t", o.session)
	attach.Stdin, attach.Stdout, attach.Stderr = os.Stdin, os.Stdout, os.Stderr
	return attach.Run()
}

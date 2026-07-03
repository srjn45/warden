package tui

import (
	"context"
	"fmt"
	"os"

	"github.com/srjn45/warden/internal/lifecycle"
)

// InsideTmux reports whether warden was launched from inside an existing tmux
// session (the $TMUX env var is set by tmux for every process it spawns). The
// classic cockpit (RunCockpit) builds its own tmux session and `tmux attach`es
// to it — but tmux refuses to nest an attach, so launching it from inside tmux
// fails. When InsideTmux is true the CLI routes to the tmux-native cockpit
// instead, which lays the dashboard out as a native window in the *current*
// session (no nesting, native keybindings, no dueling prefixes).
func InsideTmux() bool { return os.Getenv("TMUX") != "" }

// nativeCockpitWindow names the cockpit window created in the user's session.
const nativeCockpitWindow = "warden-cockpit"

// listPaneNativeCmd is the command tmux runs for the native cockpit's list pane.
// It mirrors listPaneCmd but adds --kill-window so `q` tears down only this
// window rather than the user's whole tmux session (see killCockpitArgs).
func listPaneNativeCmd(self, detailPane string) string {
	return listPaneCmd(self, detailPane) + " --kill-window"
}

// nativeDetailPlaceholderCmd keeps the native cockpit's detail pane alive with a
// hint until the user opens an agent into it. Unlike the classic placeholder it
// carries no mouse/shift-drag tip: the native cockpit doesn't force `mouse on`
// (that is session-scoped and would clobber the user's own tmux preference), so
// it inherits whatever the user already runs.
func nativeDetailPlaceholderCmd() string {
	return `sh -c 'printf "Select an agent on the left and press Enter to open it here,\nor press a to zoom it full-screen (Ctrl-b L returns to the cockpit).\n\nPress q to close the cockpit window.\n"; exec sleep 2147483647'`
}

// tmuxNativeOpts configures the native cockpit window.
type tmuxNativeOpts struct {
	self string // absolute path to the warden binary (the panes re-exec it)
	cwd  string // dir the list/detail panes run in (agents spawned via `n` land here)
}

// buildTmuxNativeCockpit lays the cockpit out as a native tmux window inside the
// user's *current* session, so it works when warden is launched from inside an
// existing tmux session — where the classic cockpit's `tmux attach` refuses to
// nest. It mirrors buildCockpit's proven list+detail split, but as a window (not
// a new session) and without touching any session-/server-global options or key
// bindings, so it never disturbs the user's own tmux config:
//
//	┌─ list ─┐┌─ detail (agent, opened on Enter) ─┐
//	└────────┘└──────────────────────────────────┘
//
// The detail pane is created first (fills the window) so the list pane, split in
// afterwards, can be handed its stable id (--detail-pane) to drive via
// respawn-pane. tmux `new-window` selects the new window for the attached client,
// so the user lands on the cockpit; the caller then simply returns (the window
// lives on in the user's session). Deliberately omitted vs. the classic cockpit:
// the master shell/REPL pane (the user's own tmux already provides shells one
// keypress away) and the global M-Arrow/M-t/Enter bindings (the user keeps their
// own navigation). Those are noted as gaps, not regressions — the classic cockpit
// never ran inside tmux at all.
func buildTmuxNativeCockpit(ctx context.Context, run lifecycle.Runner, o tmuxNativeOpts) error {
	// 1. Detail pane fills the new window initially (placeholder); capture its id.
	detailID, err := runPaneCreate(ctx, run,
		"new-window", "-n", nativeCockpitWindow, "-c", o.cwd,
		"-P", "-F", "#{pane_id}", nativeDetailPlaceholderCmd())
	if err != nil {
		return err
	}
	// 2. List pane to the LEFT of detail (-b), 40% width, in the launch dir so
	// agents spawned from it (`n`) launch there. It gets detailID to drive.
	listID, err := runPaneCreate(ctx, run,
		"split-window", "-h", "-b", "-l", "40%", "-t", detailID, "-c", o.cwd,
		"-P", "-F", "#{pane_id}", listPaneNativeCmd(o.self, detailID))
	if err != nil {
		return err
	}
	// 3. Keep the detail pane (showing [exited]) instead of collapsing the layout
	//    when an opened agent's attach exits. Pane-scoped: touches only our pane.
	if out, err := run.Run(ctx, "", "tmux", "set-option", "-p", "-t", detailID, "remain-on-exit", "on"); err != nil {
		return fmt.Errorf("tmux set-option remain-on-exit: %w: %s", err, out)
	}
	// 4. Best-effort: make Shift+Enter reach the detail Claude pane as a newline
	//    (extended-keys passthrough + Alt+Enter fallback). Never blocks the cockpit.
	lifecycle.EnsureExtendedKeys(ctx, run)
	// 5. Focus the list pane.
	if out, err := run.Run(ctx, "", "tmux", "select-pane", "-t", listID); err != nil {
		return fmt.Errorf("tmux select-pane: %w: %s", err, out)
	}
	return nil
}

// RunTmuxNativeCockpit builds the native cockpit window in the user's current
// tmux session and returns — the window persists in that session (the user's own
// client is already attached to it), so unlike RunCockpit there is nothing to
// block on. self is the absolute path to the warden binary; cwd is the launching
// shell's directory (where the list/detail panes run). Callers must ensure
// InsideTmux() is true first (buildTmuxNativeCockpit's `new-window` targets the
// caller's session).
func RunTmuxNativeCockpit(a api, self, cwd string) error {
	_ = a // the panes hold their own clients; reserved for future inline checks
	return buildTmuxNativeCockpit(context.Background(), lifecycle.HintingExecRunner{Inner: lifecycle.ExecRunner{}}, tmuxNativeOpts{self: self, cwd: cwd})
}

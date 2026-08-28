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

	"github.com/srjn45/warden/internal/lifecycle"
)

// cockpitSession returns the tmux session name for the cockpit owned by the given pid.
func cockpitSession(pid int) string {
	return fmt.Sprintf("warden-tui-%d", pid)
}

type cockpitOpts struct {
	session   string // tmux session name, e.g. "warden-tui-1234"
	self      string // absolute path to the warden binary
	homeDir   string // cwd for the control pane process
	launchCwd string // cwd the terminal pane's default terminal opens in (the launching shell's dir)
}

// controlPaneCmd is the shell command tmux runs for the top-left control pane. It
// is told both viewport panes' ids so it can drive (respawn) them: the agent pane
// when the user opens an agent with Enter, and the terminal pane when it opens a
// terminal (or the default terminal at startup). An empty terminalPane (the
// tmux-native cockpit, which has no terminal pane) omits the flag entirely so the
// control pane degrades its terminal features to a status hint.
func controlPaneCmd(self, agentPane, terminalPane string) string {
	cmd := self + " tui --pane=control --agent-pane=" + agentPane
	if terminalPane != "" {
		cmd += " --terminal-pane=" + terminalPane
	}
	return cmd
}

// agentPlaceholderCmd keeps the right pane alive showing a hint until the user
// opens an agent into it. `exec sleep` so the process is cleanly replaceable by
// `respawn-pane`.
func agentPlaceholderCmd() string {
	return `sh -c 'printf "Select an agent and press Enter to open it here.\n\nTip: the mouse drives tmux here — hold Shift while dragging to select\ntext the normal way, then Ctrl+Shift+C to copy.\n"; exec sleep 2147483647'`
}

// terminalPlaceholderCmd keeps the bottom-left terminal pane alive until the
// control pane opens a terminal session into it (the default terminal at startup,
// or one picked with Enter/`t`). `exec sleep` so it is cleanly replaceable by
// `respawn-pane`, exactly like the agent pane's placeholder.
func terminalPlaceholderCmd() string {
	return `sh -c 'printf "Opening a terminal here…\n\nPress t in the control pane for a terminal in the focused agent'\''s folder.\n"; exec sleep 2147483647'`
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
//	┌─ control (top-left) ─┐┌─ agent ──────┐
//	├─ terminal (shell) ───┤│ (full height)│
//	└──────────────────────┘└──────────────┘
//
// Panes are created right-to-left so the control pane (created last) can be handed
// both viewport pane ids (--agent-pane, --terminal-pane) and drive them via
// respawn-pane. The agent and terminal panes start as placeholders; the control
// pane opens an agent into the agent pane on Enter and the default terminal into
// the terminal pane at startup. The caller attaches afterwards.
func buildCockpit(ctx context.Context, run lifecycle.Runner, o cockpitOpts) error {
	// 1. Agent pane fills the window initially (placeholder); capture its id.
	agentPaneID, err := runPaneCreate(ctx, run,
		"new-session", "-d", "-s", o.session, "-c", o.homeDir,
		"-P", "-F", "#{pane_id}", agentPlaceholderCmd())
	if err != nil {
		return err
	}
	// 2. Terminal pane to the LEFT of the agent pane (-b), 40% width, in the launch dir.
	// Starts as a placeholder; the control pane opens the default terminal session
	// into it at startup (and terminals picked with Enter/`t` thereafter).
	terminalPaneID, err := runPaneCreate(ctx, run,
		"split-window", "-h", "-b", "-l", "40%", "-t", agentPaneID, "-c", o.launchCwd,
		"-P", "-F", "#{pane_id}", terminalPlaceholderCmd())
	if err != nil {
		return err
	}
	// 3. Control pane ABOVE the terminal pane (-b), 50% of the left column; it gets
	// both viewport pane ids (agent + terminal). It runs in launchCwd (the launching
	// shell's dir) so agents and the default terminal spawned from it launch in that
	// dir — os.Getwd() in the pane is what spawnCmd sends.
	controlPaneID, err := runPaneCreate(ctx, run,
		"split-window", "-v", "-b", "-l", "50%", "-t", terminalPaneID, "-c", o.launchCwd,
		"-P", "-F", "#{pane_id}", controlPaneCmd(o.self, agentPaneID, terminalPaneID))
	if err != nil {
		return err
	}
	// Keep the terminal pane alive (showing [exited]) instead of collapsing the
	// layout when an opened terminal's attach exits — the control pane recreates a
	// default terminal so the pane is never permanently empty (§11).
	if out, err := run.Run(ctx, "", "tmux", "set-option", "-p", "-t", terminalPaneID, "remain-on-exit", "on"); err != nil {
		return fmt.Errorf("tmux set-option remain-on-exit (terminal): %w: %s", err, out)
	}
	// 4. Keep the agent pane (showing [exited]) instead of collapsing the layout
	//    when an opened agent's attach exits.
	if out, err := run.Run(ctx, "", "tmux", "set-option", "-p", "-t", agentPaneID, "remain-on-exit", "on"); err != nil {
		return fmt.Errorf("tmux set-option remain-on-exit: %w: %s", err, out)
	}
	// 5. Mouse + prefix-less Alt+Arrow pane navigation.
	if out, err := run.Run(ctx, "", "tmux", "set-option", "-t", o.session, "mouse", "on"); err != nil {
		return fmt.Errorf("tmux set-option mouse: %w: %s", err, out)
	}
	// With mouse on, a plain drag drives tmux (copy-mode / app), not native text
	// selection — easy to forget. Keep a permanent reminder on the status line so
	// the Shift-to-select trick stays discoverable. Scoped to this session so it
	// never touches the user's other tmux sessions.
	if out, err := run.Run(ctx, "", "tmux", "set-option", "-t", o.session, "status-right",
		"#[fg=yellow]shift+drag = select/copy#[default]  %H:%M "); err != nil {
		return fmt.Errorf("tmux set-option status-right: %w: %s", err, out)
	}
	// Make a newline key work in the agent pane: extended-keys passthrough
	// (Shift+Enter on capable terminals) plus an Alt+Enter fallback for terminals
	// like VTE/GNOME that can't report Shift+Enter. Best-effort: never block the
	// cockpit on it.
	lifecycle.EnsureExtendedKeys(ctx, run)
	for _, b := range [][2]string{{"M-Left", "-L"}, {"M-Right", "-R"}, {"M-Up", "-U"}, {"M-Down", "-D"}} {
		if out, err := run.Run(ctx, "", "tmux", "bind-key", "-n", b[0], "select-pane", b[1]); err != nil {
			return fmt.Errorf("tmux bind-key %s: %w: %s", b[0], err, out)
		}
	}
	// Global Alt rotation (§8): flip which entity each viewport shows, from any
	// pane, so you can rotate while typing in a shell or agent. Each key just
	// forwards itself to the control pane, which owns the rotation state
	// (openedTerminal/openedAgent) and respawns the target pane — M-t cycles the
	// terminal pane, M-a the agent pane through the Projects frame's live agents.
	// (M-p, the old pipeline-agent rotation, is retired — pipelines are reached
	// inside the Projects tree, not a rotation of their own.) M-t is exactly the key
	// freed by removing the old shell-toggle. The control pane is targeted explicitly
	// (-t) so rotation works even when another pane has focus. Alt (not Ctrl) avoids
	// clobbering C-a/C-p readline keys inside shells. The shifted variants (M-T/M-A =
	// Alt+Shift+t/a) rotate the same viewports in reverse; tmux treats them as keys
	// distinct from their lowercase forms.
	for _, key := range []string{"M-t", "M-a", "M-T", "M-A"} {
		if out, err := run.Run(ctx, "", "tmux", "bind-key", "-n", key, "send-keys", "-t", controlPaneID, key); err != nil {
			return fmt.Errorf("tmux bind-key %s: %w: %s", key, err, out)
		}
	}
	// Config-free prefix fallback for the SAME rotation: <prefix> t/a (and shifted
	// T/A). The root M-… bindings above only fire if the terminal emulator sends
	// Meta for Alt/Option combos — which macOS Terminal.app and iTerm2 do NOT by
	// default (Option+a emits "å", not ESC+a), so those users would otherwise have no
	// rotation key. <prefix> (Ctrl-b) is a plain control byte every terminal produces,
	// so this path needs no emulator setting. Each key forwards the matching M-<key>
	// to the control pane, reusing the identical rotation handling. These override the
	// tmux default for t (clock) inside this session and are unbound on teardown
	// (killCockpitArgs).
	for _, m := range [][2]string{{"t", "M-t"}, {"a", "M-a"}, {"T", "M-T"}, {"A", "M-A"}} {
		if out, err := run.Run(ctx, "", "tmux", "bind-key", m[0], "send-keys", "-t", controlPaneID, m[1]); err != nil {
			return fmt.Errorf("tmux bind-key %s: %w: %s", m[0], err, out)
		}
	}
	// 6. Focus the control pane.
	if out, err := run.Run(ctx, "", "tmux", "select-pane", "-t", controlPaneID); err != nil {
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

// WebCockpitSession is the stable tmux session name for the daemon-owned cockpit
// that the web "TUI" tab attaches to. Unlike the foreground TUI's per-PID
// sessions (warden-tui-<pid>), there is a single shared web cockpit: warden is a
// single-user, single-daemon tool, so one session gives the same live dashboard
// from any browser or device (continuity), and `window-size latest` on attach
// lets the most-recently-active client drive sizing.
const WebCockpitSession = "warden-web-cockpit"

// cockpitHealthy reports whether an existing web cockpit session still has the
// shape a fresh buildCockpit would produce: exactly three panes, with the
// top-left pane (pane_at_top=1, pane_at_left=1 — the control pane) running the
// warden binary (the `warden tui --pane=control` bloom app) rather than having been
// dropped to a bare shell. A session left degraded — a partial build (wrong pane
// count) from a daemon crash mid-buildCockpit, or a control pane that fell back to a
// shell — reports false so the caller tears it down and rebuilds a fresh one,
// making the cockpit self-healing. Any tmux error is treated as unhealthy.
//
// Only pane_current_command (the pane's live foreground process) is inspected —
// `sh -c "warden tui …"` exec's into warden, so a healthy control pane reports the
// warden binary's basename. self is the absolute path to the warden binary.
func cockpitHealthy(ctx context.Context, run lifecycle.Runner, session, self string) bool {
	out, err := run.Run(ctx, "", "tmux", "list-panes", "-t", session,
		"-F", "#{pane_at_top}#{pane_at_left} #{pane_current_command}")
	if err != nil {
		return false
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		return false
	}
	want := filepath.Base(self)
	for _, ln := range lines {
		// "11" is the only top-left pane: the agent pane is top-RIGHT (10) and the
		// terminal pane bottom-left (01), so this uniquely picks out the control pane.
		pos, cmd, ok := strings.Cut(ln, " ")
		if !ok || pos != "11" {
			continue
		}
		return strings.TrimSpace(cmd) == want
	}
	return false // no top-left pane at all → degraded layout
}

// EnsureWebCockpit makes sure the daemon-owned web cockpit session exists AND is
// healthy, building it (detached) on first call and reusing it thereafter, and
// returns its tmux session name. It is the headless counterpart to RunCockpit: it
// builds the same three-pane layout but never attaches — the browser attaches
// over the daemon's WebSocket PTY bridge instead. self is the absolute path to
// the warden binary (the panes re-exec it) and launchCwd the directory the
// control/terminal panes run in (agents and the default terminal launch there).
// Idempotent and safe to call on every attach.
//
// An existing session is validated (cockpitHealthy) before reuse: a wedged
// session — the web cockpit lives in the tmux server, not the daemon, so it
// survives daemon restarts/reinstalls — is killed and rebuilt transparently, so
// a client attaching always lands on a healthy cockpit. forceRebuild skips the
// reuse check entirely and always kills+rebuilds (the `warden tui
// --rebuild-web-cockpit` escape hatch), even when the session currently looks
// healthy.
func EnsureWebCockpit(ctx context.Context, run lifecycle.Runner, self, launchCwd string, forceRebuild bool) (string, error) {
	// has-session exits 0 only when the session already exists.
	if _, err := run.Run(ctx, "", "tmux", "has-session", "-t", WebCockpitSession); err == nil {
		// Reuse it only when this isn't a forced rebuild and it's actually healthy;
		// otherwise tear it down and rebuild a fresh one below.
		if !forceRebuild && cockpitHealthy(ctx, run, WebCockpitSession, self) {
			return WebCockpitSession, nil
		}
		_, _ = run.Run(ctx, "", "tmux", "kill-session", "-t", WebCockpitSession)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	o := cockpitOpts{
		session:   WebCockpitSession,
		self:      self,
		homeDir:   home,
		launchCwd: launchCwd,
	}
	if err := buildCockpit(ctx, run, o); err != nil {
		// Tear down a half-built session so we never leave an orphan.
		_, _ = run.Run(ctx, "", "tmux", "kill-session", "-t", o.session)
		return "", err
	}
	return WebCockpitSession, nil
}

// RunCockpit builds the tmux cockpit for this process and attaches to it,
// blocking until the user detaches/quits. launchCwd is the launching shell's
// directory (where the default terminal opens and agents spawned via `n` launch).
func RunCockpit(a api, self, launchCwd string) error {
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
		launchCwd: launchCwd,
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

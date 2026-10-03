// Package tmuxproc exposes the small shared tmux/process behavior used by both
// Agent and Terminal lifecycle without sharing a persisted model.
//
// Agent records live in the session/agent store; Terminal records live in
// terminalstore. Both drive the same detached tmux pane mechanics through Host.
package tmuxproc

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Runner executes an external command. It matches lifecycle.Runner so the
// daemon can share one ExecRunner between Lifecycle and Host without an import
// cycle (lifecycle → tmuxproc is fine; tmuxproc must not import lifecycle).
type Runner interface {
	Run(ctx context.Context, dir string, name string, args ...string) (string, error)
}

// Host is the shared pane-process seam: create, probe, capture, key, and kill a
// detached tmux session. It intentionally carries no Agent/Terminal identity —
// callers pass the tmux session name and cwd themselves.
type Host interface {
	// NewSession creates a detached tmux session named name in cwd and applies
	// the scroll/mouse options shared by agents and terminals.
	NewSession(ctx context.Context, name, cwd string, env ...string) error
	// KillSession terminates the tmux session. Missing sessions are not an error.
	KillSession(ctx context.Context, name string) error
	// HasSession reports whether a tmux session by that name currently exists.
	HasSession(ctx context.Context, name string) bool
	// CapturePane returns the visible pane text for the named session.
	CapturePane(ctx context.Context, name string) (string, error)
	// SendKeys types keys into the named session's pane (tmux send-keys).
	SendKeys(ctx context.Context, name string, keys ...string) error
}

// historyLimit is the scrollback depth panes inherit at creation.
const historyLimit = 50000

// Adapter implements Host on top of a Runner (typically lifecycle.ExecRunner).
type Adapter struct {
	Run Runner
}

// New returns a Host backed by run. run must be non-nil.
func New(run Runner) Host {
	return Adapter{Run: run}
}

// NewSession implements Host.
func (a Adapter) NewSession(ctx context.Context, name, cwd string, env ...string) error {
	a.ensureScrollback(ctx)
	EnsureExtendedKeys(ctx, a.Run)
	args := []string{"new-session", "-d", "-s", name,
		"-e", "WARDEN_SESSION_ID=" + name,
		"-e", "AGENTCTL_SESSION_ID=" + name}
	for _, kv := range env {
		args = append(args, "-e", kv)
	}
	args = append(args, "-c", cwd)
	if out, err := a.Run.Run(ctx, "", "tmux", args...); err != nil {
		return fmt.Errorf("tmux new-session: %w: %s", err, out)
	}
	_, _ = a.Run.Run(ctx, "", "tmux", "set-option", "-t", name, "mouse", "on")
	// detach-on-destroy on ensures that nested tmux clients attached to this
	// session inside cockpit panes exit cleanly when the session is killed.
	// If set to off, killing an agent causes the nested pane client to switch
	// to the cockpit session, nesting the cockpit inside itself and collapsing
	// the TUI into a 1-character-wide column (#478).
	_, _ = a.Run.Run(ctx, "", "tmux", "set-option", "-t", name, "detach-on-destroy", "on")
	return nil
}

// KillSession implements Host.
func (a Adapter) KillSession(ctx context.Context, name string) error {
	_, _ = a.Run.Run(ctx, "", "tmux", "kill-session", "-t", name)
	return nil
}

// HasSession implements Host.
func (a Adapter) HasSession(ctx context.Context, name string) bool {
	_, err := a.Run.Run(ctx, "", "tmux", "has-session", "-t", name)
	return err == nil
}

// CapturePane implements Host.
func (a Adapter) CapturePane(ctx context.Context, name string) (string, error) {
	return a.Run.Run(ctx, "", "tmux", "capture-pane", "-p", "-t", name)
}

// SendKeys implements Host.
func (a Adapter) SendKeys(ctx context.Context, name string, keys ...string) error {
	args := append([]string{"send-keys", "-t", name}, keys...)
	out, err := a.Run.Run(ctx, "", "tmux", args...)
	if err != nil {
		return fmt.Errorf("tmux send-keys: %w: %s", err, out)
	}
	return nil
}

func (a Adapter) ensureScrollback(ctx context.Context) {
	if out, err := a.Run.Run(ctx, "", "tmux", "show-options", "-g", "-v", "history-limit"); err == nil {
		if cur, perr := strconv.Atoi(strings.TrimSpace(out)); perr == nil && cur >= historyLimit {
			return
		}
	}
	_, _ = a.Run.Run(ctx, "", "tmux", "set-option", "-g", "history-limit", strconv.Itoa(historyLimit))
}

// EnsureExtendedKeys configures tmux so Shift/Alt+Enter insert a newline in
// agent panes. Best-effort — a keyboard-protocol quirk must never block a spawn.
// Exported so lifecycle.EnsureExtendedKeys can delegate without duplicating the
// bind/set sequence (cockpit launch also calls it).
func EnsureExtendedKeys(ctx context.Context, run Runner) {
	_, _ = run.Run(ctx, "", "tmux", "bind-key", "-n", "M-Enter", "send-keys", "C-j")
	_, _ = run.Run(ctx, "", "tmux", "set-option", "-s", "extended-keys", "on")
	if out, err := run.Run(ctx, "", "tmux", "show-options", "-s", "-v", "terminal-features"); err == nil && strings.Contains(out, "extkeys") {
		return
	}
	_, _ = run.Run(ctx, "", "tmux", "set-option", "-sa", "terminal-features", "*:extkeys")
}

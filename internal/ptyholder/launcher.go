package ptyholder

import (
	"context"
	"log/slog"
	"time"

	"github.com/srjn45/warden/internal/ptyruntime"
)

// Launcher is the ptyruntime.Launcher backed by holder processes.
type Launcher struct {
	// Root is the holder state directory (<data>/pty/holder).
	Root string
	// Command starts a holder (see LaunchSpec.Command); default: this binary's
	// hidden `ptyholder` subcommand.
	Command []string
	// ExitedLinger is passed to every holder (tests shorten it).
	ExitedLinger time.Duration
}

var _ ptyruntime.Launcher = (*Launcher)(nil)

// Launch implements ptyruntime.Launcher.
func (l *Launcher) Launch(ctx context.Context, name string, argv []string, cwd string, env []string, cols, rows uint16) (*ptyruntime.Adoption, error) {
	l.Release(ctx, name) // a stale holder under this name must not outlive its replacement
	h, err := Launch(ctx, LaunchSpec{Root: l.Root, ID: name, Argv: argv, Cwd: cwd, Env: env, Cols: cols, Rows: rows,
		Command: l.Command, ExitedLinger: l.ExitedLinger})
	if err != nil {
		return nil, err
	}
	ad, err := h.Adopt(ctx, false)
	if err != nil {
		_ = h.Release(ctx)
		Forget(l.Root, name)
		return nil, err
	}
	return ad, nil
}

// Adopt implements ptyruntime.Launcher.
func (l *Launcher) Adopt(ctx context.Context, name string) (*ptyruntime.Adoption, error) {
	h, err := Open(l.Root, name)
	if err != nil {
		return nil, err
	}
	return h.Adopt(ctx, true)
}

// Has implements ptyruntime.Launcher.
func (l *Launcher) Has(name string) bool { return ValidID(name) && Exists(l.Root, name) }

// Release implements ptyruntime.Launcher.
func (l *Launcher) Release(ctx context.Context, name string) {
	if !l.Has(name) {
		return
	}
	if h, err := Open(l.Root, name); err == nil {
		if err := h.Release(ctx); err != nil {
			slog.Debug("ptyholder: release", "session", name, "err", err)
		}
	}
	Forget(l.Root, name)
}

// VerifyHolder verifies that name's holder process is alive and matches the recorded identity.
func (l *Launcher) VerifyHolder(name string) (pid int, start uint64, proto int, ok bool) {
	if !l.Has(name) {
		return 0, 0, 0, false
	}
	h, err := Open(l.Root, name)
	if err != nil {
		return 0, 0, 0, false
	}
	m, ok := h.Meta()
	if !ok {
		return 0, 0, 0, false
	}
	st, err := StartTime(m.HolderPID)
	if err != nil || st != m.HolderStart {
		return 0, 0, 0, false
	}
	return m.HolderPID, m.HolderStart, m.Proto, true
}

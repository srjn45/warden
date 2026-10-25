package ptyholder

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/srjn45/warden/internal/ptyruntime"
)

// Launcher is the ptyruntime.Launcher backed by holder processes.
type Launcher struct {
	TestOwner *Owner
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
	if err := l.release(ctx, name); err != nil {
		cleanupFailures.Add(1)
		return nil, err // never replace a holder whose ownership/cleanup is unresolved
	}
	h, err := Launch(ctx, LaunchSpec{Root: l.Root, ID: name, Argv: argv, Cwd: cwd, Env: env, Cols: cols, Rows: rows,
		Command: l.Command, ExitedLinger: l.ExitedLinger, TestOwner: l.TestOwner})
	if err != nil {
		return nil, err
	}
	ad, err := h.Adopt(ctx, false)
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if cleanupErr := l.release(cleanupCtx, name); cleanupErr != nil {
			cleanupFailures.Add(1)
			slog.Warn("ptyholder: failed adoption cleanup; retaining ownership record", "session", name, "err", cleanupErr)
		}
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
	if err := l.release(ctx, name); err != nil {
		cleanupFailures.Add(1)
		slog.Warn("ptyholder: release failed; retaining ownership record", "session", name, "err", err)
	}
}

func (l *Launcher) release(ctx context.Context, name string) error {
	if !l.Has(name) {
		return nil
	}
	h, err := Open(l.Root, name)
	if errors.Is(err, ErrGone) {
		return nil // record forgotten concurrently
	}
	if err != nil {
		return err
	}
	if err := h.Release(ctx); err != nil {
		if !errors.Is(err, ErrGone) {
			return err
		}
		gone, gerr := recordedGone(ctx, h)
		if gerr != nil {
			return fmt.Errorf("%w; ownership unresolved: %w", err, gerr)
		}
		if !gone {
			return err
		}
	}
	// Released, or a dead holder whose recorded incarnations are verified gone:
	// its record must not block an explicit restore/replacement launch.
	Forget(l.Root, name)
	return nil
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

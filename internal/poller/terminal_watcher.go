package poller

import (
	"context"
	"log/slog"
	"time"

	"github.com/srjn45/warden/internal/store"
)

// TerminalDeps is the TerminalWatcher's view of the world — a subset of Deps.
// No new daemon capabilities are required; the narrower interface keeps the
// watcher independently testable without constructing a full Poller.
type TerminalDeps interface {
	List(ctx context.Context) ([]*store.Session, error)
	SessionAlive(ctx context.Context, tmuxName string) bool
	CapturePane(ctx context.Context, tmuxName string) (string, error)
	UpdateStatusIf(ctx context.Context, id string, from, to store.Status) (bool, error)
	UpdatePane(ctx context.Context, id, excerpt string) error
	ExitCode(ctx context.Context, id string) (int, bool)
	ClearExit(ctx context.Context, id string)
	Restore(ctx context.Context, sess *store.Session) error
}

// TerminalWatcher monitors terminal sessions (Kind==terminal) at their own
// cadence, independently of the agent poll loop. It checks liveness, captures
// pane output, and detects exit codes. OnChange and OnTransition mirror the
// Poller callbacks so the daemon can wire a single handler for both.
type TerminalWatcher struct {
	deps         TerminalDeps
	OnChange     func()
	OnTransition func(sess *store.Session, from, to store.Status)
}

// NewTerminalWatcher creates a TerminalWatcher backed by deps.
func NewTerminalWatcher(deps TerminalDeps) *TerminalWatcher {
	return &TerminalWatcher{deps: deps}
}

// Run ticks at interval until ctx is cancelled.
func (w *TerminalWatcher) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.tick(ctx)
		}
	}
}

// tick processes all terminal sessions for one poll cycle.
func (w *TerminalWatcher) tick(ctx context.Context) {
	sessions, err := w.deps.List(ctx)
	if err != nil {
		slog.Warn("terminal_watcher: list sessions failed", "err", err)
		return
	}
	for _, s := range sessions {
		if !s.IsTerminal() {
			continue
		}

		// Consume any recorded exit code. Phase 3 will wire the Restarter here.
		if code, ok := w.deps.ExitCode(ctx, s.ID); ok {
			slog.Info("terminal_watcher: exit code recorded", "terminal", s.ID, "code", code)
			w.deps.ClearExit(ctx, s.ID)
		}

		alive := w.deps.SessionAlive(ctx, s.TmuxSession)
		if !alive {
			if s.Status == store.StatusOrphaned {
				continue // already orphaned; no double-transition
			}
			prev := s.Status
			ok, err := w.deps.UpdateStatusIf(ctx, s.ID, prev, store.StatusOrphaned)
			if err != nil {
				slog.Warn("terminal_watcher: status update failed", "terminal", s.ID, "err", err)
				continue
			}
			if ok {
				if w.OnTransition != nil {
					w.OnTransition(s, prev, store.StatusOrphaned)
				}
				if w.OnChange != nil {
					w.OnChange()
				}
			}
			continue
		}

		// Alive: promote spawning → working.
		if s.Status == store.StatusSpawning {
			ok, err := w.deps.UpdateStatusIf(ctx, s.ID, store.StatusSpawning, store.StatusWorking)
			if err != nil {
				slog.Warn("terminal_watcher: status update failed", "terminal", s.ID, "err", err)
			} else if ok {
				if w.OnTransition != nil {
					w.OnTransition(s, store.StatusSpawning, store.StatusWorking)
				}
				s.Status = store.StatusWorking
				if w.OnChange != nil {
					w.OnChange()
				}
			}
		}

		// Capture pane; update excerpt if changed.
		captured, err := w.deps.CapturePane(ctx, s.TmuxSession)
		if err != nil {
			slog.Warn("terminal_watcher: capture pane failed", "terminal", s.ID, "err", err)
			continue
		}
		if excerpt := lastLines(captured, 20); excerpt != s.LastPaneExcerpt {
			if err := w.deps.UpdatePane(ctx, s.ID, excerpt); err != nil {
				slog.Warn("terminal_watcher: update pane failed", "terminal", s.ID, "err", err)
			}
			if w.OnChange != nil {
				w.OnChange()
			}
		}
	}
}

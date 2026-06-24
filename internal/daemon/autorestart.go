package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/srjn45/warden/internal/store"
)

type restartAction int

const (
	actionGiveUp restartAction = iota
	actionRestart
)

// decideRestart decides whether an errored, auto-restart-enabled agent should be
// restarted, and the counter to persist if so. A restart that happened >= reset
// ago (or never) means the prior run was sustained-healthy, so the counter resets
// to 0 — "a successful run resets the counter", defined as sustained health so a
// resume->instant-crash loop cannot evade the cap by briefly reaching working.
func decideRestart(count int, lastRestartAt, now time.Time, max int, reset time.Duration) (restartAction, int) {
	effective := count
	if lastRestartAt.IsZero() || now.Sub(lastRestartAt) >= reset {
		effective = 0
	}
	if effective >= max {
		return actionGiveUp, effective
	}
	return actionRestart, effective + 1
}

// Restarter auto-resumes an opted-in agent that reaches errored, bounded by a
// per-agent retry cap that resets after sustained health.
type Restarter struct {
	life  Lifecycle
	store store.Store
	max   int
	reset time.Duration
}

// NewRestarter builds a Restarter. The cap (max) and reset window are supplied
// by the caller from config (auto_restart_max / auto_restart_reset); the feature
// itself is opt-in per agent (Session.AutoRestart).
func NewRestarter(life Lifecycle, st store.Store, max int, reset time.Duration) *Restarter {
	return &Restarter{
		life:  life,
		store: st,
		max:   max,
		reset: reset,
	}
}

// OnTransition is wired as a callback on the poller's status-transition hook.
func (r *Restarter) OnTransition(sess *store.Session, _ store.Status, to store.Status) {
	r.onTransitionAt(sess, store.Status(""), to, time.Now().UTC())
}

// onTransitionAt is the testable core (now injected). It restarts a qualifying
// errored agent or records a give-up.
func (r *Restarter) onTransitionAt(sess *store.Session, _ store.Status, to store.Status, now time.Time) {
	if to != store.StatusErrored || !sess.AutoRestart || sess.PipelineID != "" {
		return
	}
	ctx := context.Background()
	var last time.Time
	if sess.LastRestartAt != nil {
		last = *sess.LastRestartAt
	}
	act, next := decideRestart(sess.RestartCount, last, now, r.max, r.reset)
	if act == actionGiveUp {
		r.appendEvent(ctx, sess.ID, fmt.Sprintf("auto-restart: giving up after %d attempts", r.max))
		return
	}
	if err := r.store.SetRestart(ctx, sess.ID, next, now); err != nil {
		slog.Warn("auto-restart: set restart failed", "agent", sess.ID, "err", err)
		return
	}
	r.appendEvent(ctx, sess.ID, fmt.Sprintf("auto-restart: attempt %d/%d", next, r.max))
	// errored = claude died, shell alive: kill the surviving session so Restore's
	// has-session guard passes; best-effort (Terminate ignores an already-dead session).
	_ = r.life.Terminate(ctx, sess.TmuxSession)
	if err := r.life.Restore(ctx, sess); err != nil {
		r.appendEvent(ctx, sess.ID, fmt.Sprintf("auto-restart: restore failed: %v", err))
		return
	}
	if _, err := r.store.UpdateStatusIf(ctx, sess.ID, store.StatusErrored, store.StatusSpawning); err != nil {
		slog.Warn("auto-restart: status update failed", "agent", sess.ID, "err", err)
	}
}

func (r *Restarter) appendEvent(ctx context.Context, id, detail string) {
	if err := r.store.AppendEvent(ctx, id, store.Event{Type: "auto-restart", Detail: detail}); err != nil {
		slog.Warn("auto-restart: append event failed", "agent", id, "err", err)
	}
}

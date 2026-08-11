package daemon

import (
	"context"
	"log/slog"
	"time"

	"github.com/srjn45/warden/internal/schedule"
)

// runScheduler is the daemon's native-scheduler reconcile loop (#15). It is a
// no-op when the feature gate is off. On each tick it fires every enabled due
// schedule, re-arms it (cron rolls forward, single-shot at goes inactive), and
// persists the result. It is fail-soft: a fire error is recorded in the
// schedule's LastError and logged, and never stops the loop. Recompute on the
// first tick also rebuilds each schedule's NextRun from the wall clock, so cron
// schedules resume at their next FUTURE occurrence (never backfilling missed
// ones) while a past-due single-shot fires once.
func (s *Server) runScheduler(ctx context.Context) {
	if !s.scheduler || s.schedStore == nil {
		return
	}
	interval := s.schedInterval
	if interval < time.Second {
		interval = time.Minute
	}
	// Startup reconcile: re-arm every schedule from the wall clock before the first
	// tick. A cron schedule's NextRun rolls forward to its next FUTURE occurrence,
	// so a daemon that was down across a scheduled time does NOT backfill the missed
	// run. A single-shot at schedule keeps its fixed (possibly past) NextRun, so a
	// past-due one stays due and fires once on the first tick below.
	s.reconcileScheduleNextRuns()
	// Tick once promptly at startup so a past-due single-shot fires without waiting
	// a full interval, then on the ticker cadence.
	s.scheduleTick(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.scheduleTick(ctx)
		}
	}
}

// reconcileScheduleNextRuns rolls each schedule's NextRun forward from the
// current wall clock (Recompute). For cron this is the next future occurrence
// (never backfilling); for at it re-derives the fixed instant. Persisted so a
// later read (CLI list) reflects the live next-fire. Best-effort per schedule.
func (s *Server) reconcileScheduleNextRuns() {
	now := time.Now()
	list, err := s.schedStore.List()
	if err != nil {
		slog.Warn("scheduler: startup list failed", "err", err)
		return
	}
	for _, sc := range list {
		if uerr := s.schedStore.Update(sc.ID, func(stored *schedule.Schedule) {
			if rerr := schedule.Recompute(stored, now); rerr != nil {
				slog.Warn("scheduler: startup recompute failed", "schedule", stored.ID, "err", rerr)
			}
		}); uerr != nil {
			slog.Warn("scheduler: startup persist failed", "schedule", sc.ID, "err", uerr)
		}
	}
}

// scheduleTick fires every due schedule once. For each due schedule it performs
// the side effect (agent spawn or pipeline create+start) and then advances the
// schedule under the store lock, recording the fire time and any error. A
// schedule whose NextRun has drifted stale (e.g. the daemon was down) is
// re-armed by Advance to its next future occurrence.
func (s *Server) scheduleTick(ctx context.Context) {
	now := time.Now()
	list, err := s.schedStore.List()
	if err != nil {
		slog.Warn("scheduler: list failed", "err", err)
		return
	}
	for _, sc := range list {
		// Refresh the durable last-run status from the live session even when this
		// schedule is not due to fire, so a row shows running → exited/error as the
		// prior run progresses (best-effort; skipped once the session is gone).
		s.refreshLastRunStatus(ctx, sc)
		if !schedule.Due(sc, now) {
			continue
		}
		sessionID, fireErr := s.fireSchedule(ctx, sc)
		if fireErr != nil {
			slog.Warn("scheduler: schedule fire failed", "schedule", sc.ID, "err", fireErr)
		} else {
			slog.Info("scheduler: fired schedule", "schedule", sc.ID, "mode", sc.Mode)
		}
		// Persist the fire under the store lock: stamp LastRun/LastError/last-run id
		// and re-arm (cron → next occurrence, at → inactive). Pass the same `now`
		// used for the due check so LastRun reflects this tick.
		if uerr := s.schedStore.Update(sc.ID, func(stored *schedule.Schedule) {
			schedule.Advance(stored, now, sessionID, fireErr)
		}); uerr != nil {
			slog.Warn("scheduler: persist after fire failed", "schedule", sc.ID, "err", uerr)
		}
	}
	s.notify()
}

// refreshLastRunStatus best-effort syncs a schedule's durable LastRunStatus from
// the live status of its last run. For an agent-mode fire LastRunSessionID is a
// session; for a pipeline-mode fire it is a pipeline. A missing record (the run
// was rotated or deleted) leaves the stored status untouched — the last-known
// value stays as the durable record. A no-op when nothing has fired yet or the
// status is unchanged, so it costs one store lookup per schedule per tick.
func (s *Server) refreshLastRunStatus(ctx context.Context, sc *schedule.Schedule) {
	if sc.LastRunSessionID == "" {
		return
	}
	var status string
	if sess, err := s.store.Get(ctx, sc.LastRunSessionID); err == nil && sess != nil {
		status = string(sess.Status)
	} else if s.exec != nil {
		if p, perr := s.exec.pstore.Get(sc.LastRunSessionID); perr == nil && p != nil {
			status = string(p.Status)
		}
	}
	if status == "" || status == sc.LastRunStatus {
		return
	}
	if uerr := s.schedStore.Update(sc.ID, func(stored *schedule.Schedule) {
		// Guard against a concurrent fire having moved on to a new run.
		if stored.LastRunSessionID == sc.LastRunSessionID {
			stored.LastRunStatus = status
		}
	}); uerr != nil {
		slog.Warn("scheduler: last-run status refresh failed", "schedule", sc.ID, "err", uerr)
	}
}

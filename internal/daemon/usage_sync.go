package daemon

import (
	"context"
	"log/slog"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/backendusage"
	"github.com/srjn45/warden/internal/store"
)

// usageSyncInterval is how often the daemon re-probes provider usage and
// projects the Snapshot into scoped BackendQuota records (Phase 3 / D5).
// Matches backendusage.FreshTTL so the process-local cache stays warm without
// hammering providers on every tick.
const usageSyncInterval = backendusage.FreshTTL

// runUsageSync probes live subscription usage once at startup, then on a ticker,
// and after every successful Snapshot writes per-scope BackendQuota rows via
// SyncToStore. A no-op when usage or the backend registry is unconfigured.
func (s *Server) runUsageSync(ctx context.Context) {
	if s.usage == nil || s.backends == nil {
		return
	}
	s.usageSyncOnce(ctx, true) // startup probe
	t := time.NewTicker(usageSyncInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.usageSyncOnce(ctx, true)
		}
	}
}

// usageSyncOnce fetches a usage Snapshot and projects it into the backend store.
// Panic-guarded so a provider-adapter bug cannot take down the daemon.
func (s *Server) usageSyncOnce(ctx context.Context, refresh bool) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("daemon: usage sync recovered panic", "panic", rec)
		}
	}()
	if s.usage == nil || s.backends == nil {
		return
	}
	snap, err := s.usage.Snapshot(ctx, refresh)
	if err != nil {
		slog.Warn("daemon: usage snapshot failed", "err", err)
		return
	}
	if err := backendusage.SyncToStore(snap, s.backends); err != nil {
		slog.Warn("daemon: quota sync failed", "err", err)
	}
	s.limitSessionsFromSnapshot(ctx, snap)
}

// syncUsageSnapshot projects snap into the backend store. Used by on-demand
// Snapshot call sites (GetUsage, recovery) so every successful Snapshot feeds
// scoped BackendQuota records, not only the periodic loop.
func (s *Server) syncUsageSnapshot(snap backendusage.Snapshot) {
	if s.backends == nil {
		return
	}
	if err := backendusage.SyncToStore(snap, s.backends); err != nil {
		slog.Warn("daemon: quota sync failed", "err", err)
	}
}

// limitSessionsFromSnapshot translates backendusage Snapshot rate-limit signals
// into StatusRateLimited on active sessions whose backends are pane-blind
// (no RateLimitDetector). Pane-based backends are skipped so classify() remains
// their sole transition source — this path must not double-fire.
func (s *Server) limitSessionsFromSnapshot(ctx context.Context, snap backendusage.Snapshot) {
	if s.rateLimitScheduler == nil || s.store == nil {
		return
	}
	// Build set of currently-limited backends from the snapshot.
	// backendID → earliest resetAt across scopes (zero when unknown).
	limited := map[string]time.Time{}
	for _, br := range snap.Backends {
		backendLimited := br.Status == backendusage.StatusRateLimited
		for _, ul := range br.Usage {
			stateLimited := ul.LimitState != nil && *ul.LimitState == "rate_limited"
			if !stateLimited && !backendLimited {
				continue
			}
			if stateLimited {
				backendLimited = true
			}
			resetAt := time.Time{}
			if ul.ResetsAt != nil {
				resetAt = *ul.ResetsAt
			}
			if cur, exists := limited[br.ID]; !exists || cur.IsZero() || (!resetAt.IsZero() && resetAt.Before(cur)) {
				limited[br.ID] = resetAt
			}
		}
		// Backend-level StatusRateLimited with no Usage rows still marks the backend.
		if backendLimited {
			if _, exists := limited[br.ID]; !exists {
				limited[br.ID] = time.Time{}
			}
		}
	}
	if len(limited) == 0 {
		return
	}
	sessions, err := s.store.List(ctx)
	if err != nil {
		return
	}
	for _, sess := range sessions {
		resetAt, isLimited := limited[sess.Backend]
		if !isLimited {
			continue
		}
		// Only transition non-terminal, non-already-limited sessions.
		switch sess.Status {
		case store.StatusWorking, store.StatusIdle, store.StatusWaitingForInput:
			// Check if backend implements RateLimitDetector — if it does, trust
			// the pane-based path and do NOT double-trigger from usage poll.
			if b, err := agentbackend.Get(sess.Backend); err == nil {
				if _, ok := b.(agentbackend.RateLimitDetector); ok {
					continue // pane-based detection handles this backend
				}
			}
			changed, _ := s.store.UpdateStatusIf(ctx, sess.ID, sess.Status, store.StatusRateLimited)
			if changed {
				if !resetAt.IsZero() {
					_ = s.store.SetRateLimit(ctx, sess.ID, resetAt, 0)
				}
				updated, _ := s.store.Get(ctx, sess.ID)
				if updated != nil && s.rateLimitScheduler != nil {
					s.rateLimitScheduler.OnTransition(updated, sess.Status, store.StatusRateLimited)
				}
			}
		}
	}
}

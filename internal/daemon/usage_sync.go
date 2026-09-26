package daemon

import (
	"context"
	"log/slog"
	"time"

	"github.com/srjn45/warden/internal/backendusage"
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

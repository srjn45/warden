package daemon

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/srjn45/warden/internal/branchtrack"
	"github.com/srjn45/warden/internal/collab"
	"github.com/srjn45/warden/internal/ctxstore"
	"github.com/srjn45/warden/internal/mailbox"
	"github.com/srjn45/warden/internal/notify"
	"github.com/srjn45/warden/internal/plugin"
	"github.com/srjn45/warden/internal/poller"
	"github.com/srjn45/warden/internal/savings"
	"github.com/srjn45/warden/internal/snapshot"
	"github.com/srjn45/warden/internal/spend"
	"github.com/srjn45/warden/internal/store"
)

func NewServer(st store.Store, life Lifecycle, p *poller.Poller, interval time.Duration, approvals bool, cstore *ctxstore.Store, mbox *mailbox.Store, exec *Executor) *Server {
	h := newHub()
	if p != nil {
		p.OnChange = h.publish
	}
	return &Server{
		store: st, life: life, poller: p, pollInterval: interval,
		hub: h, done: make(chan struct{}), approvals: approvals, cstore: cstore, mbox: mbox, exec: exec,
		collab: collab.NewMonitor(st, mbox), collabInterval: 10 * time.Second,
		// branchTracker is opt-in (outward GitHub integration): constructed with a
		// log-only notifier and a zero interval (disabled) until the daemon wires
		// the real notifier + interval from config.
		branchTracker: branchtrack.NewTracker(st, mbox, notify.New(false)),
	}
}

// SetCollabInterval sets the file-conflict poll interval. A non-positive value
// disables the collaboration monitor.
func (s *Server) SetCollabInterval(d time.Duration) { s.collabInterval = d }

// SetBranchTrackInterval sets the branch-tracker poll interval. A non-positive
// value disables the tracker (Run returns immediately).
func (s *Server) SetBranchTrackInterval(d time.Duration) { s.branchTrackInterval = d }

// SetBranchTrackNotifier wires the operator notifier the branch tracker fans CI
// failures out to. No-op if the tracker was never constructed.
func (s *Server) SetBranchTrackNotifier(n notify.Notifier) {
	if s.branchTracker != nil {
		s.branchTracker.SetNotifier(n)
	}
}

// SetSnapshots wires the snapshot manager (#46) and the config gate. enabled=false
// (or a nil manager) makes the snapshot endpoints return 403.
func (s *Server) SetSnapshots(enabled bool, m *snapshot.Manager) { s.snapshots = enabled; s.snap = m }

// SetPlugins wires the lifecycle-hook dispatcher (#47). A nil dispatcher (plugins
// off, the default) makes every dispatch call a no-op, so the server runs exactly
// as before. Dispatch is fail-open, so this never changes request control flow.
func (s *Server) SetPlugins(d *plugin.Dispatcher) { s.plugins = d }

// SetSavings wires the token-savings ledger and its config gates. enabled=false
// (or a nil store) makes recording a no-op and GET /savings return 403, so the
// server runs exactly as before. samples toggles the opt-in provenance capture
// (config `savings_samples`); it is wired into both the emit-site flag and the
// store's persistence gate so a sample is never retained unless both agree.
// Recording is always fail-open — a ledger write never alters the request it
// measures.
func (s *Server) SetSavings(enabled bool, store *savings.Store, samples bool) {
	s.savingsOn = enabled
	s.savings = store
	s.savingsSamples = samples
	if store != nil {
		store.SetSampling(samples)
	}
}

// SetSpend wires the per-session real-spend tracker that feeds the savings
// report's measured-spend denominator. A nil store makes RecordSpend a no-op and
// leaves the report's MeasuredSpend at 0 (the CLI then falls back to the
// context-reduction wording). Gated by the same savings switch as the ledger.
func (s *Server) SetSpend(store *spend.Store) { s.spend = store }

// shutdownGrace bounds how long Shutdown waits for in-flight requests to drain
// before returning (after which the process exits and any stragglers are cut).
const shutdownGrace = 5 * time.Second

// ListenAndServe blocks serving the API on addr until ctx is cancelled, then
// shuts down gracefully: it signals SSE handlers to stop, waits (bounded) for
// in-flight requests to drain, and drains the poller's background workers
// before returning — so the caller can safely close the store afterwards.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	// Derive a child context so we can stop the poller on every exit path,
	// including an early HTTP bind failure (where ctx itself isn't cancelled).
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// One-time backlog sweep: drop session records of already-completed pipeline
	// jobs that older builds left behind (reaped tmux, kept the record), which the
	// poller would otherwise keep flagging "orphaned". Runs before the poller's
	// first tick so those records never get re-classified.
	if s.exec != nil {
		if n, err := s.exec.SweepDoneJobSessions(runCtx); err != nil {
			slog.Warn("daemon: done-job session sweep failed", "err", err)
		} else if n > 0 {
			slog.Info("daemon: swept completed pipeline-job session records", "count", n)
		}
	}

	pollerDone := make(chan struct{})
	if s.poller != nil {
		go func() { defer close(pollerDone); s.poller.Run(runCtx, s.pollInterval) }()
	} else {
		close(pollerDone)
	}

	// Reap tombstoned parents whose sub-tree has gone fully terminal (agent
	// sub-tree grouping). Lazy reap fires on terminal transitions; this sweep is
	// the safety net for transitions that bypass the poller (SessionEnd hook,
	// operator terminate). Uses the poll cadence; falls back if unset.
	reapInterval := s.pollInterval
	if reapInterval <= 0 {
		reapInterval = 30 * time.Second
	}
	go s.runTombstoneReapSweep(runCtx, reapInterval)

	if s.life != nil {
		go s.runPressureSampler(runCtx)
		// Unattended worktree GC (worktree_auto_prune): sweep at startup + on a
		// slow ticker. Reclaims clean record-less orphans only; never touches
		// archived-owned worktrees (see runWorktreePruneSweep).
		if s.autoPruneWorktree {
			go s.runWorktreePruneSweep(runCtx)
		}
	}
	go s.runMetricsRecorder(runCtx)
	if s.collab != nil && s.collabInterval > 0 {
		go s.collab.Run(runCtx, s.collabInterval)
	}
	if s.branchTracker != nil && s.branchTrackInterval > 0 {
		go s.branchTracker.Run(runCtx, s.branchTrackInterval)
	}
	// Native scheduler (#15): fires due cron/at schedules. No-op when the
	// scheduler_enabled gate is off (the default).
	if s.scheduler && s.schedStore != nil {
		go s.runScheduler(runCtx)
	}

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           s.router(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()

	var retErr error
	select {
	case err := <-errCh:
		// Server stopped on its own (e.g. failed to bind).
		if err != nil && err != http.ErrServerClosed {
			retErr = err
		}
	case <-ctx.Done():
		if s.done != nil {
			close(s.done) // release long-lived SSE handlers
		}
		sctx, scancel := context.WithTimeout(context.Background(), shutdownGrace)
		retErr = httpSrv.Shutdown(sctx)
		scancel()
	}

	cancel()     // stop the poller (also covers the bind-failure path)
	<-pollerDone // wait for its summarizers to drain before returning
	return retErr
}

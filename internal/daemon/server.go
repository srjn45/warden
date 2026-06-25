package daemon

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/srjn45/warden/internal/collab"
	"github.com/srjn45/warden/internal/ctxstore"
	"github.com/srjn45/warden/internal/mailbox"
	"github.com/srjn45/warden/internal/plugin"
	"github.com/srjn45/warden/internal/poller"
	"github.com/srjn45/warden/internal/snapshot"
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
	}
}

// SetCollabInterval sets the file-conflict poll interval. A non-positive value
// disables the collaboration monitor.
func (s *Server) SetCollabInterval(d time.Duration) { s.collabInterval = d }

// SetSnapshots wires the snapshot manager (#46) and the config gate. enabled=false
// (or a nil manager) makes the snapshot endpoints return 403.
func (s *Server) SetSnapshots(enabled bool, m *snapshot.Manager) { s.snapshots = enabled; s.snap = m }

// SetPlugins wires the lifecycle-hook dispatcher (#47). A nil dispatcher (plugins
// off, the default) makes every dispatch call a no-op, so the server runs exactly
// as before. Dispatch is fail-open, so this never changes request control flow.
func (s *Server) SetPlugins(d *plugin.Dispatcher) { s.plugins = d }

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

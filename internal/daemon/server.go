package daemon

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/srjn45/warden/internal/ctxstore"
	"github.com/srjn45/warden/internal/mailbox"
	"github.com/srjn45/warden/internal/poller"
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
	}
}

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
			log.Printf("daemon: done-job session sweep: %v", err)
		} else if n > 0 {
			log.Printf("daemon: swept %d completed pipeline-job session record(s)", n)
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

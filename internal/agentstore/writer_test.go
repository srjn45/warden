package agentstore

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/store"
)

func TestWriterQueueReleasedOnCancelDoesNotGrow(t *testing.T) {
	s := seededStore(t, 2)
	g := slowWrite(t, "Update")
	go func() { _ = s.UpdateStatus(context.Background(), "a-0", store.StatusIdle) }()
	g.waitEntered(t)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			require.ErrorIs(t, s.UpdateStatus(ctx, "a-1", store.StatusIdle), context.DeadlineExceeded)
		}()
	}
	wg.Wait()
	d := s.Diagnostics()
	require.Zero(t, d.LockWaiters)
	require.Equal(t, "Update", d.HolderOp)
	require.Equal(t, uint64(20), d.CtxAbandoned["UpdateStatus/queued"]+d.CtxAbandoned["Update/queued"])
	g.release()
}

func TestWriterUpdateCallbackCancelAbortsBeforeCommit(t *testing.T) {
	s := seededStore(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	err := s.Update(ctx, "a-0", func(a *Agent) error {
		a.Status = store.StatusIdle
		cancel()
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	a, gerr := s.Get(context.Background(), "a-0")
	require.NoError(t, gerr)
	require.NotEqual(t, store.StatusIdle, a.Status)
}

func TestWriterCommittedWriteNeverReportsCtxError(t *testing.T) {
	s := seededStore(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancel is only observed pre-commit; a write that gets the slot with a
	// live ctx completes and returns nil.
	require.NoError(t, s.Update(ctx, "a-0", func(a *Agent) error { a.Status = store.StatusIdle; return nil }))
	cancel()
	a, err := s.Get(context.Background(), "a-0")
	require.NoError(t, err)
	require.Equal(t, store.StatusIdle, a.Status)
}

func TestWriterCloseReleasesQueued(t *testing.T) {
	s := seededStore(t, 2)
	g := slowWrite(t, "Update")
	go func() { _ = s.UpdateStatus(context.Background(), "a-0", store.StatusIdle) }()
	g.waitEntered(t)
	errc := make(chan error, 1)
	go func() { errc <- s.UpdateStatus(context.Background(), "a-1", store.StatusIdle) }()
	require.Eventually(t, func() bool { return s.Diagnostics().LockWaiters == 1 }, time.Second, 5*time.Millisecond)
	go func() { time.Sleep(50 * time.Millisecond); g.release() }()
	closeDone := make(chan struct{})
	go func() { _ = s.Close(); close(closeDone) }()
	select {
	case err := <-errc:
		require.True(t, errors.Is(err, ErrClosed) || err == nil, "got %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("queued writer not released by Close")
	}
	<-closeDone
}

func TestWriterDiagnosticsOpMetrics(t *testing.T) {
	s := seededStore(t, 1)
	ctx := context.Background()
	require.NoError(t, s.UpdateStatus(ctx, "a-0", store.StatusIdle))
	require.Error(t, s.Update(ctx, "missing", func(*Agent) error { return nil }))
	d := s.Diagnostics()
	require.Equal(t, 1, d.Agents)
	require.NotZero(t, d.SnapshotVer)
	require.Equal(t, StateOK, d.State)
	up := d.Ops["Update"]
	require.GreaterOrEqual(t, up.Count, uint64(2))
	require.Equal(t, uint64(1), up.Results["not_found"])
	require.Equal(t, "write", up.Class)
}

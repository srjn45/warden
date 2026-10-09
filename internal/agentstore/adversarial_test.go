package agentstore

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/store"
)

// A parked writer must not stall snapshot readers, and queued writers whose
// contexts expire must return promptly instead of piling up behind it.
func TestAdversarial_BlockedWriteDoesNotPileUp(t *testing.T) {
	s := seededStore(t, 20)
	g := slowWrite(t, "")

	holder := make(chan error, 1)
	go func() {
		holder <- s.UpdateStatus(context.Background(), "a-0", store.StatusIdle)
	}()
	g.waitEntered(t)

	// Readers stay fast while the write slot is held.
	for i := 0; i < 50; i++ {
		start := time.Now()
		list, err := s.List(context.Background())
		require.NoError(t, err)
		require.Len(t, list, 20)
		require.Less(t, time.Since(start), stallProbe)
	}

	// Queued writers with short deadlines abandon cleanly and quickly.
	var wg sync.WaitGroup
	var slow atomic.Int64
	for i := 1; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			start := time.Now()
			err := s.UpdateStatus(ctx, fmt.Sprintf("a-%d", i), store.StatusIdle)
			require.Error(t, err)
			if time.Since(start) > 2*time.Second {
				slow.Add(1)
			}
		}(i)
	}
	wg.Wait()
	require.Zero(t, slow.Load(), "queued writers must honour ctx deadlines")

	// Metrics are visible during the incident.
	d := s.Diagnostics()
	require.NotEmpty(t, d.HolderOp)
	require.Equal(t, 20, d.Agents)

	g.release()
	require.NoError(t, <-holder)

	// No data loss: the holder's write landed, abandoned writes did not corrupt.
	a, err := s.Get(context.Background(), "a-0")
	require.NoError(t, err)
	require.Equal(t, store.StatusIdle, a.Status)
	a, err = s.Get(context.Background(), "a-5")
	require.NoError(t, err)
	require.Equal(t, store.StatusWorking, a.Status)
	require.NotEmpty(t, s.Diagnostics().CtxAbandoned)
}

// A slow scan (audit) must not block writes or reads.
func TestAdversarial_DelayedScanIsolated(t *testing.T) {
	s := seededStore(t, 10)
	g := newGate()
	restore := SetAuditSeam(func(string) { g.hit() })
	t.Cleanup(func() { g.release(); restore() })
	a := NewAuditor(s, AuditorOptions{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = a.Audit(context.Background(), true)
	}()
	g.waitEntered(t)

	blocked, wait := stalled(stallProbe*4, func() error {
		return s.UpdateStatus(context.Background(), "a-1", store.StatusIdle)
	})
	_, err := wait()
	require.NoError(t, err)
	require.False(t, blocked, "write stalled behind a delayed scan")
	_, err = s.List(context.Background())
	require.NoError(t, err)

	g.release()
	<-done
}

// Hammer the store with concurrent writers and readers (REST/SSE/CLI/TUI/Web
// stand-ins), cancel a subset, then verify every acknowledged write persisted
// across a close/reopen (daemon restart).
func TestAdversarial_ConcurrentMixAndRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	require.NoError(t, err)
	const n = 40
	for i := 0; i < n; i++ {
		require.NoError(t, s.Insert(context.Background(), &Agent{
			ID: fmt.Sprintf("a-%d", i), Name: fmt.Sprintf("n-%d", i), Status: store.StatusWorking,
		}))
	}

	stop := make(chan struct{})
	var rw sync.WaitGroup
	for r := 0; r < 8; r++ {
		rw.Add(1)
		go func() {
			defer rw.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if l, err := s.List(context.Background()); err != nil || len(l) != n {
					t.Errorf("list: len=%d err=%v", len(l), err)
					return
				}
				_ = s.Diagnostics()
			}
		}()
	}

	var acked sync.Map
	var ww sync.WaitGroup
	for i := 0; i < n; i++ {
		ww.Add(1)
		go func(i int) {
			defer ww.Done()
			ctx := context.Background()
			if i%4 == 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel() // pre-cancelled: must not apply, must not hang
			}
			id := fmt.Sprintf("a-%d", i)
			if err := s.UpdateStatus(ctx, id, store.StatusIdle); err == nil {
				acked.Store(id, true)
			} else if i%4 != 0 {
				t.Errorf("unexpected write error for %s: %v", id, err)
			}
		}(i)
	}
	ww.Wait()
	close(stop)
	rw.Wait()
	require.NoError(t, s.Close())

	s2, err := New(dir)
	require.NoError(t, err)
	defer s2.Close()
	require.Equal(t, StateOK, s2.State())
	list, err := s2.List(context.Background())
	require.NoError(t, err)
	require.Len(t, list, n)
	for _, a := range list {
		if _, ok := acked.Load(a.ID); ok {
			require.Equal(t, store.StatusIdle, a.Status, a.ID)
		}
	}
}

// Writes racing the audit's Count/Scan must never be mistaken for corruption:
// the engine commit lands before the generation bump, so the auditor relies on
// the write-slot epoch to detect a torn pass.
func TestAdversarial_AuditNeverFalselyDegradesUnderWrites(t *testing.T) {
	s := seededStore(t, 0)
	a := NewAuditor(s, AuditorOptions{})
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 300; i++ {
			select {
			case <-stop:
				return
			default:
			}
			time.Sleep(time.Millisecond)
			_ = s.Insert(context.Background(), &Agent{
				ID: fmt.Sprintf("w-%d", i), Name: fmt.Sprintf("wn-%d", i), Status: store.StatusWorking,
			})
		}
	}()
	for i := 0; i < 40; i++ {
		_, err := a.Audit(context.Background(), true)
		require.NoError(t, err)
		require.NotEqual(t, StateDegraded, s.State(), "audit %d falsely degraded: %v", i, s.DegradedError())
	}
	close(stop)
	wg.Wait()
}

func TestWriteEpochOddWhileHeld(t *testing.T) {
	s := seededStore(t, 1)
	before := s.writeEpoch()
	require.Zero(t, before%2)
	g := slowWrite(t, "")
	done := make(chan error, 1)
	go func() { done <- s.UpdateStatus(context.Background(), "a-0", store.StatusIdle) }()
	g.waitEntered(t)
	require.Equal(t, uint64(1), s.writeEpoch()%2)
	g.release()
	require.NoError(t, <-done)
	require.Zero(t, s.writeEpoch()%2)
	require.Greater(t, s.writeEpoch(), before)
}

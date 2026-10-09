package agentstore

// Executable baseline for the Cockpit lock-contention incident; see
// docs/specs/2026-10-09-agent-store-lock-contention-contract.md.
//
// Every TestBaseline* test CHARACTERIZES CURRENT BEHAVIOR and passes today. A
// later task that fixes a defect must flip the matching baseline assertion in
// the same change (the failing baseline is the signal that the contract moved)
// and un-skip the matching TestContractContention* gate in
// contention_contract_test.go.

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/store"
)

// B1: a read holds no mutex, so writers complete without waiting for the in-flight read.
func TestBaselineSlowScanBlocksWriters(t *testing.T) {
	s := seededStore(t, 5)
	ctx := context.Background()
	g := slowRead(t)

	readDone := make(chan error, 1)
	go func() { _, err := s.List(ctx); readDone <- err }()
	g.waitEntered(t)

	writers := map[string]func() error{
		"Update": func() error { return s.UpdateStatus(ctx, "a-0", store.StatusIdle) },
		"UpdateStatusIf": func() error {
			_, err := s.UpdateStatusIf(ctx, "a-1", store.StatusWorking, store.StatusIdle)
			return err
		},
		"AppendEvent": func() error { return s.AppendEvent(ctx, "a-2", store.Event{Type: "x"}) },
		"Insert":      func() error { return s.Insert(ctx, &Agent{ID: "new", Name: "new"}) },
		"Delete":      func() error { return s.Delete(ctx, "a-4") },
		"Ping":        func() error { return s.Ping(ctx) },
	}
	waits := map[string]func() (time.Duration, error){}
	for name, fn := range writers {
		blocked, wait := stalled(stallProbe, fn)
		require.False(t, blocked, "%s must NOT be blocked behind the in-flight read", name)
		waits[name] = wait
	}
	g.release()
	require.NoError(t, <-readDone)
	for name, wait := range waits {
		_, err := wait()
		require.NoError(t, err, name)
	}
}

// B2: a slow write holds the write mutex, but readers load the snapshot
// and do not wait for the write to finish.
func TestBaselineSlowWriteBlocksReaders(t *testing.T) {
	s := seededStore(t, 3)
	ctx := context.Background()
	g := slowWrite(t, "Update")

	writeDone := make(chan error, 1)
	go func() { writeDone <- s.UpdateStatus(ctx, "a-0", store.StatusIdle) }()
	g.waitEntered(t)

	readers := map[string]func() error{
		"List":          func() error { _, err := s.List(ctx); return err },
		"Get":           func() error { _, err := s.Get(ctx, "a-1"); return err },
		"GetByNameOrID": func() error { _, err := s.GetByNameOrID(ctx, "n-1"); return err },
		"ListClosed":    func() error { _, err := s.ListClosed(ctx); return err },
		"Ping":          func() error { return s.Ping(ctx) },
	}
	waits := map[string]func() (time.Duration, error){}
	for name, fn := range readers {
		blocked, wait := stalled(stallProbe, fn)
		require.False(t, blocked, "%s must NOT be blocked behind the in-flight write", name)
		waits[name] = wait
	}
	g.release()
	require.NoError(t, <-writeDone)
	for name, wait := range waits {
		_, err := wait()
		require.NoError(t, err, name)
	}
}

// B3: readers run concurrently from the atomic snapshot pointer without serialization.
func TestBaselineReadersSerialize(t *testing.T) {
	s := seededStore(t, 3)
	restore := SetReadSeam(func(string) { time.Sleep(100 * time.Millisecond) })
	defer restore()
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = s.List(context.Background()) }()
	}
	wg.Wait()
	require.Less(t, time.Since(start), 180*time.Millisecond,
		"two concurrent List calls were serialized; expected parallel execution")
}

// B4: which methods pay for a full verified scan (O(N) under the mutex).
// With the snapshot read model, reads and Insert pay 0 collection scans.
func TestBaselineScanCountPerOperation(t *testing.T) {
	s := seededStore(t, 5)
	ctx := context.Background()
	n := countScans(t)
	cases := []struct {
		name string
		want int64
		fn   func() error
	}{
		{"Get", 0, func() error { _, err := s.Get(ctx, "a-1"); return err }},
		{"List", 0, func() error { _, err := s.List(ctx); return err }},
		{"GetByNameOrID(name hit)", 0, func() error { _, err := s.GetByNameOrID(ctx, "n-1"); return err }},
		{"GetByNameOrID(id fallback)", 0, func() error { _, err := s.GetByNameOrID(ctx, "a-1"); return err }},
		{"ListClosed", 0, func() error { _, err := s.ListClosed(ctx); return err }},
		{"ListClosedDegraded", 0, func() error { _, _, err := s.ListClosedDegraded(ctx); return err }},
		{"Insert", 0, func() error { return s.Insert(ctx, &Agent{ID: "x1", Name: "x1"}) }},
		{"Update", 0, func() error { return s.UpdateStatus(ctx, "a-0", store.StatusIdle) }},
		{"UpdateStatusIf", 0, func() error {
			_, err := s.UpdateStatusIf(ctx, "a-0", store.StatusIdle, store.StatusWorking)
			return err
		}},
		{"Archive", 0, func() error { return s.Archive(ctx, "a-4") }},
		{"Delete", 0, func() error { return s.Delete(ctx, "a-3") }},
	}
	for _, c := range cases {
		before := n.Load()
		require.NoError(t, c.fn(), c.name)
		require.Equal(t, c.want, n.Load()-before, "%s: full scans per call", c.name)
	}
}

// B5: ctx is checked only on entry. A caller whose deadline expires while it is
// queued on the mutex is not released at the deadline; it returns success long
// after, so cancellation does not bound request latency.
func TestBaselineContextIgnoredWhileQueued(t *testing.T) {
	s := seededStore(t, 2)
	g := slowWrite(t, "Update")
	go func() { _ = s.UpdateStatus(context.Background(), "a-0", store.StatusIdle) }()
	g.waitEntered(t)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	blocked, wait := stalled(300*time.Millisecond, func() error { return s.UpdateStatus(ctx, "a-1", store.StatusIdle) })
	require.True(t, blocked, "UpdateStatus returned at its 50ms deadline; baseline expected it to stay queued (BASELINE defect)")
	g.release()
	el, err := wait()
	require.NoError(t, err, "queued call with an expired ctx should currently still succeed")
	require.Greater(t, el, 250*time.Millisecond)
	require.Error(t, ctx.Err())
}

// B6: an Update whose fn is running does not block snapshot readers.
func TestBaselineUpdateFnRunsUnderLock(t *testing.T) {
	s := seededStore(t, 2)
	ctx := context.Background()
	in, out := make(chan struct{}), make(chan struct{})
	go func() {
		_ = s.Update(ctx, "a-0", func(a *Agent) error { close(in); <-out; return nil })
	}()
	<-in
	blocked, wait := stalled(stallProbe, func() error { _, err := s.List(ctx); return err })
	require.False(t, blocked, "reader must NOT wait for an Update callback")
	close(out)
	_, err := wait()
	require.NoError(t, err)
}

// B7: a degraded (preflight) store fails fast and never takes the mutex, so it
// is the one path that is NOT subject to the contention (and also never serves
// a stale view — contract §4).
func TestBaselineDegradedStoreFailsFastWithoutStale(t *testing.T) {
	s := seededStore(t, 2)
	s.preflight = newUnhealthy(integrityFailure("agents", "", "synthetic"))
	g := slowWrite(t, "")
	_ = g
	_, err := s.List(context.Background())
	require.ErrorIs(t, err, ErrUnhealthy)
	_, err = s.Get(context.Background(), "a-0") // Get has no preflight guard: it scans the scratch copy
	require.NoError(t, err)
}

// TestBaselineLatencyProfile prints the numbers quoted in the design record.
// Opt-in: WARDEN_BASELINE_PROFILE=1 go test ./internal/agentstore -run LatencyProfile -v
func TestBaselineLatencyProfile(t *testing.T) {
	if os.Getenv("WARDEN_BASELINE_PROFILE") == "" {
		t.Skip("set WARDEN_BASELINE_PROFILE=1 to print the contention profile")
	}
	ctx := context.Background()
	for _, n := range []int{50, 200, 1000} {
		s := seededStore(t, n)
		// Scan cost alone.
		const reps = 5
		var scan time.Duration
		for i := 0; i < reps; i++ {
			st := time.Now()
			_, _ = s.List(ctx)
			scan += time.Since(st)
		}
		// Idle-store write latency for contrast.
		var idle []time.Duration
		for i := 0; i < 30; i++ {
			st := time.Now()
			_ = s.UpdateStatus(ctx, fmt.Sprintf("a-%d", i%n), store.StatusIdle)
			idle = append(idle, time.Since(st))
		}
		sort.Slice(idle, func(i, j int) bool { return idle[i] < idle[j] })
		// Writer latency while 4 Cockpit-style pollers loop List().
		stop := make(chan struct{})
		var wg sync.WaitGroup
		for p := 0; p < 4; p++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
						_, _ = s.List(ctx)
					}
				}
			}()
		}
		var lat []time.Duration
		for i := 0; i < 60; i++ {
			st := time.Now()
			_ = s.UpdateStatus(ctx, fmt.Sprintf("a-%d", i%n), store.StatusIdle)
			lat = append(lat, time.Since(st))
		}
		close(stop)
		wg.Wait()
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		t.Logf("agents=%-5d scan=%-10v write-idle p50=%-10v write-under-4-pollers p50=%-10v p99=%-10v max=%v",
			n, scan/reps, idle[len(idle)/2], lat[len(lat)/2], lat[len(lat)*99/100], lat[len(lat)-1])
	}
}

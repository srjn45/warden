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

// seededStore opens a store in a temp dir holding n agents (named a-0..a-n-1).
func seededStore(t testing.TB, n int) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	for i := 0; i < n; i++ {
		require.NoError(t, s.Insert(context.Background(), &Agent{
			ID: fmt.Sprintf("a-%d", i), Name: fmt.Sprintf("n-%d", i), Status: store.StatusWorking,
		}))
	}
	return s
}

// gate is a one-shot latch used as a slow seam: the first caller signals
// entered and parks until release; later callers pass straight through.
type gate struct {
	entered chan struct{}
	rel     chan struct{}
	once    sync.Once
	relOnce sync.Once
}

func newGate() *gate { return &gate{entered: make(chan struct{}), rel: make(chan struct{})} }

func (g *gate) hit() {
	first := false
	g.once.Do(func() { first = true })
	if !first {
		return
	}
	close(g.entered)
	<-g.rel
}

func (g *gate) release() { g.relOnce.Do(func() { close(g.rel) }) }

func (g *gate) waitEntered(t testing.TB) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("seam was never reached")
	}
}

// slowScan parks the first scan of any collection until released.
func slowScan(t testing.TB) *gate {
	g := newGate()
	restore := SetScanSeam(func(string) { g.hit() })
	t.Cleanup(func() { g.release(); restore() })
	return g
}

// slowWrite parks the first write whose op matches (all ops when op == "").
func slowWrite(t testing.TB, op string) *gate {
	g := newGate()
	restore := SetWriteSeam(func(got string) {
		if op == "" || op == got {
			g.hit()
		}
	})
	t.Cleanup(func() { g.release(); restore() })
	return g
}

// countScans counts scans per collection for the duration of the test.
func countScans(t testing.TB) *atomic.Int64 {
	var n atomic.Int64
	restore := SetScanSeam(func(string) { n.Add(1) })
	t.Cleanup(restore)
	return &n
}

// stalled runs fn in a goroutine and reports whether it finished within d. The
// returned wait func blocks until fn returns and yields its result.
func stalled(d time.Duration, fn func() error) (blocked bool, wait func() (time.Duration, error)) {
	type res struct {
		err error
		el  time.Duration
	}
	start := time.Now()
	ch := make(chan res, 1)
	go func() { err := fn(); ch <- res{err, time.Since(start)} }()
	var r res
	var done bool
	select {
	case r = <-ch:
		done = true
	case <-time.After(d):
	}
	return !done, func() (time.Duration, error) {
		if !done {
			r = <-ch
		}
		return r.el, r.err
	}
}

const stallProbe = 150 * time.Millisecond

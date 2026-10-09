package agentstore

import (
	"context"
	"errors"
	"log"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// ErrClosed is returned to writers that are queued (or arrive) once the store
// is closing.
var ErrClosed = errors.New("agentstore: closed")

// slowHoldThreshold is the generous, observe-only bound above which a write
// critical section is logged and recorded in the diagnostics ring. It never
// throttles or rejects work.
const slowHoldThreshold = 250 * time.Millisecond

const slowHoldRing = 8

// writeGate serializes durable writes with a context-aware, closable
// single-slot semaphore. Unlike a sync.Mutex a queued caller is released the
// moment its ctx is done or the store closes, so a slow engine operation can
// never create an unbounded queue of abandoned waiters.
type writeGate struct {
	sem     chan struct{}
	closing chan struct{}
	once    sync.Once

	waiters   atomic.Int64
	holderOp  atomic.Pointer[string]
	heldSince atomic.Int64 // unix nanos; 0 when free

	metrics opMetrics
}

func newWriteGate() *writeGate {
	return &writeGate{sem: make(chan struct{}, 1), closing: make(chan struct{})}
}

func (g *writeGate) close() { g.once.Do(func() { close(g.closing) }) }

// writeTicket is a held write slot. release must be called exactly once.
type writeTicket struct {
	g       *writeGate
	op      string
	started time.Time // when the call began (includes queueing)
	held    time.Time // when the slot was acquired
	waited  time.Duration
}

// beginWrite acquires the write slot for op, returning ctx.Err() if ctx ends
// while queued (before any engine mutation) and ErrClosed if the store is
// closing. The caller must `defer t.release(&err)`.
func (s *Store) beginWrite(ctx context.Context, op string) (*writeTicket, error) {
	g := s.gate
	start := time.Now()
	if err := ctx.Err(); err != nil {
		g.metrics.abandon(op, "queued")
		g.metrics.observeOp(op, "write", err, time.Since(start))
		return nil, err
	}
	g.waiters.Add(1)
	select {
	case g.sem <- struct{}{}:
		g.waiters.Add(-1)
	case <-ctx.Done():
		g.waiters.Add(-1)
		w := time.Since(start)
		g.metrics.abandon(op, "queued")
		g.metrics.observeWait(op, w)
		g.metrics.observeOp(op, "write", ctx.Err(), w)
		logThrottled("agentstore: queued call abandoned op=%s waited_ms=%d phase=queued", op, w.Milliseconds())
		return nil, ctx.Err()
	case <-g.closing:
		g.waiters.Add(-1)
		g.metrics.observeOp(op, "write", ErrClosed, time.Since(start))
		return nil, ErrClosed
	}
	now := time.Now()
	select {
	case <-g.closing:
		<-g.sem
		g.metrics.observeOp(op, "write", ErrClosed, now.Sub(start))
		return nil, ErrClosed
	default:
	}
	// A select with several ready cases picks one at random; make an expired
	// ctx win deterministically so it never commits.
	if err := ctx.Err(); err != nil {
		<-g.sem
		g.metrics.abandon(op, "queued")
		g.metrics.observeOp(op, "write", err, now.Sub(start))
		return nil, err
	}
	g.holderOp.Store(&op)
	g.heldSince.Store(now.UnixNano())
	w := now.Sub(start)
	g.metrics.observeWait(op, w)
	return &writeTicket{g: g, op: op, started: start, held: now, waited: w}, nil
}

// release frees the slot and records op metrics using the final error.
func (t *writeTicket) release(errp *error) {
	g := t.g
	hold := time.Since(t.held)
	g.heldSince.Store(0)
	g.holderOp.Store(nil)
	<-g.sem
	var err error
	if errp != nil {
		err = *errp
	}
	g.metrics.observeHold(t.op, hold, t.held)
	g.metrics.observeOp(t.op, "write", err, time.Since(t.started))
	if hold >= slowHoldThreshold {
		logThrottled("agentstore: slow lock hold op=%s held_ms=%d waiters=%d", t.op, hold.Milliseconds(), g.waiters.Load())
	}
}

// ctxErr reports a done ctx before the commit point. Past the commit point a
// write completes and returns its true result.
func (t *writeTicket) ctxErr(ctx context.Context, phase string) error {
	if err := ctx.Err(); err != nil {
		t.g.metrics.abandon(t.op, phase)
		return err
	}
	return nil
}

var (
	logMu   sync.Mutex
	logLast time.Time
)

// logThrottled emits at most one line per second.
func logThrottled(format string, args ...any) {
	logMu.Lock()
	if time.Since(logLast) < time.Second {
		logMu.Unlock()
		return
	}
	logLast = time.Now()
	logMu.Unlock()
	log.Printf(format, args...)
}

// ---- metrics ---------------------------------------------------------------

var bucketBoundsMs = []float64{1, 2, 5, 10, 25, 50, 100, 250, 500, 1000, 5000}

type hist struct {
	counts [12]uint64 // len(bucketBoundsMs)+1
	n      uint64
	max    time.Duration
}

func (h *hist) observe(d time.Duration) {
	ms := float64(d) / float64(time.Millisecond)
	i := sort.SearchFloat64s(bucketBoundsMs, ms)
	h.counts[i]++
	h.n++
	if d > h.max {
		h.max = d
	}
}

// quantileMs returns the upper bucket bound containing quantile q (max for the
// overflow bucket).
func (h *hist) quantileMs(q float64) float64 {
	if h.n == 0 {
		return 0
	}
	target := uint64(float64(h.n)*q + 0.999999)
	var cum uint64
	for i, c := range h.counts {
		cum += c
		if cum >= target {
			if i < len(bucketBoundsMs) {
				return bucketBoundsMs[i]
			}
			return float64(h.max) / float64(time.Millisecond)
		}
	}
	return float64(h.max) / float64(time.Millisecond)
}

type opStat struct {
	results map[string]uint64
	class   string
	dur     hist
	wait    hist
	hold    hist
}

// SlowHold records one write critical section that exceeded the threshold.
type SlowHold struct {
	Op     string    `json:"op"`
	HeldMs int64     `json:"held_ms"`
	At     time.Time `json:"at"`
}

type opMetrics struct {
	mu        sync.Mutex
	ops       map[string]*opStat
	abandoned map[string]uint64 // "op/phase"
	slow      []SlowHold
}

func (m *opMetrics) stat(op, class string) *opStat {
	if m.ops == nil {
		m.ops = map[string]*opStat{}
	}
	st := m.ops[op]
	if st == nil {
		st = &opStat{results: map[string]uint64{}, class: class}
		m.ops[op] = st
	}
	return st
}

func resultLabel(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrNotFound):
		return "not_found"
	case errors.Is(err, ErrExists), errors.Is(err, ErrNameExists):
		return "conflict"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	}
	var u *UnhealthyError
	if errors.As(err, &u) {
		return "unhealthy"
	}
	return "error"
}

func (m *opMetrics) observeOp(op, class string, err error, d time.Duration) {
	m.mu.Lock()
	st := m.stat(op, class)
	st.results[resultLabel(err)]++
	st.dur.observe(d)
	m.mu.Unlock()
}

func (m *opMetrics) observeWait(op string, d time.Duration) {
	m.mu.Lock()
	m.stat(op, "write").wait.observe(d)
	m.mu.Unlock()
}

func (m *opMetrics) observeHold(op string, d time.Duration, at time.Time) {
	m.mu.Lock()
	m.stat(op, "write").hold.observe(d)
	if d >= slowHoldThreshold {
		m.slow = append(m.slow, SlowHold{Op: op, HeldMs: d.Milliseconds(), At: at.UTC()})
		if len(m.slow) > slowHoldRing {
			m.slow = m.slow[len(m.slow)-slowHoldRing:]
		}
	}
	m.mu.Unlock()
}

func (m *opMetrics) abandon(op, phase string) {
	m.mu.Lock()
	if m.abandoned == nil {
		m.abandoned = map[string]uint64{}
	}
	m.abandoned[op+"/"+phase]++
	m.mu.Unlock()
}

// OpStats is the per-operation summary exposed by Diagnostics.
type OpStats struct {
	Class     string            `json:"class"`
	Count     uint64            `json:"count"`
	Results   map[string]uint64 `json:"results"`
	Errors    uint64            `json:"errors"`
	Canceled  uint64            `json:"canceled"`
	P50Ms     float64           `json:"p50_ms"`
	P99Ms     float64           `json:"p99_ms"`
	WaitP99Ms float64           `json:"lock_wait_p99_ms"`
	HoldP99Ms float64           `json:"lock_hold_p99_ms"`
	HoldMaxMs float64           `json:"lock_hold_max_ms"`
}

// Diagnostics is a safe, lock-free-for-readers picture of the store: it never
// scans, never takes the write slot, and is safe during an incident.
type Diagnostics struct {
	CheckedAt     time.Time          `json:"checked_at"`
	State         State              `json:"state"`
	SnapshotVer   uint64             `json:"snapshot_version"`
	SnapshotAgeMs int64              `json:"snapshot_age_ms"`
	Agents        int                `json:"agents"`
	Closed        int                `json:"closed"`
	LockWaiters   int64              `json:"lock_waiters"`
	HolderOp      string             `json:"holder_op"`
	HeldMs        int64              `json:"held_ms"`
	SlowestHolds  []SlowHold         `json:"slowest_holds"`
	Ops           map[string]OpStats `json:"ops"`
	CtxAbandoned  map[string]uint64  `json:"ctx_abandoned"`
	Audit         AuditReport        `json:"audit"`
}

// Diagnostics assembles the current diagnostics payload.
func (s *Store) Diagnostics() Diagnostics {
	now := time.Now()
	d := Diagnostics{
		CheckedAt:    now.UTC(),
		State:        s.State(),
		Ops:          map[string]OpStats{},
		CtxAbandoned: map[string]uint64{},
		Audit:        s.AuditReport(),
	}
	if sn := s.snap.Load(); sn != nil {
		d.SnapshotVer = sn.Version
		d.SnapshotAgeMs = now.Sub(sn.BuiltAt).Milliseconds()
		d.Agents = len(sn.Agents)
		d.Closed = len(sn.Closed)
	}
	g := s.gate
	if g == nil {
		return d
	}
	d.LockWaiters = g.waiters.Load()
	if since := g.heldSince.Load(); since != 0 {
		d.HeldMs = now.Sub(time.Unix(0, since)).Milliseconds()
		if p := g.holderOp.Load(); p != nil {
			d.HolderOp = *p
		}
	}
	m := &g.metrics
	m.mu.Lock()
	defer m.mu.Unlock()
	d.SlowestHolds = append([]SlowHold(nil), m.slow...)
	for k, v := range m.abandoned {
		d.CtxAbandoned[k] = v
	}
	for op, st := range m.ops {
		os := OpStats{Class: st.class, Results: map[string]uint64{}, Count: st.dur.n,
			P50Ms: st.dur.quantileMs(0.5), P99Ms: st.dur.quantileMs(0.99),
			WaitP99Ms: st.wait.quantileMs(0.99), HoldP99Ms: st.hold.quantileMs(0.99),
			HoldMaxMs: float64(st.hold.max) / float64(time.Millisecond)}
		for r, c := range st.results {
			os.Results[r] = c
			switch r {
			case "canceled":
				os.Canceled = c
			case "error", "unhealthy":
				os.Errors += c
			}
		}
		d.Ops[op] = os
	}
	return d
}

// Package dbtx is Warden's narrow cross-collection transaction boundary over
// ScrivaDB's engine-level CommitXTx. It is the ONLY package allowed to touch
// engine.CommitXTx / XTxStatus / XTxOp (audit G13; enforced by
// TestImportFence). See docs/specs/2026-10-10-warden-db-transaction-boundary.md.
//
// Hard rules: atomic commit or a typed failure — never sequential writes,
// compensation or a partial write set; transactions cover internal DB state
// only (no external side effects inside the boundary).
package dbtx

import (
	"context"
	"fmt"
	"time"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/warden/internal/audit"
)

// Limits mirrored from the engine (audit G9); enforced before the engine call.
const (
	MaxOps          = 1000
	MaxParticipants = 16
	MaxBytes        = 64 << 20
	MaxKeyLen       = 128
)

// collection is the slice of *engine.Collection the boundary needs.
type collection interface {
	ReserveID() uint64
	Get(id uint64) (engine.Record, error)
}

// Engine is the injectable seam over the engine DB. Production uses
// FromEngine; tests wrap it to provoke failure classes that cannot be forced
// for real.
type Engine interface {
	CommitXTx(key string, ops []engine.XTxOp) (*engine.XTxResult, error)
	XTxStatus(txOrKey string) (engine.XTxStatus, string)
	Collection(name string) (collection, error)
}

type engineDB struct{ db *engine.DB }

// FromEngine adapts an *engine.DB to the Engine seam.
func FromEngine(db *engine.DB) Engine { return engineDB{db} }

func (e engineDB) CommitXTx(k string, ops []engine.XTxOp) (*engine.XTxResult, error) {
	return e.db.CommitXTx(k, ops)
}
func (e engineDB) XTxStatus(s string) (engine.XTxStatus, string) { return e.db.XTxStatus(s) }
func (e engineDB) Collection(name string) (collection, error) {
	c, err := e.db.Collection(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %q", engine.ErrCollectionNotFound, name)
	}
	return c, nil
}

// Event describes one finished Commit, for observers (metrics/audit attach
// here without reshaping the API). Observe must not block or panic.
type Event struct {
	Intent       string
	Key          string
	TxID         string
	Ops          int
	Participants int
	Outcome      string // "committed" or a Class
	Resolved     bool   // outcome was established via status lookup
	Duration     time.Duration
}

// Observer receives one Event per Commit call.
type Observer interface{ Observe(Event) }

// Option configures a Boundary.
type Option func(*Boundary)

func WithObserver(o Observer) Option { return func(b *Boundary) { b.obs = o } }
func WithAudit(w *audit.Writer) Option {
	return func(b *Boundary) {
		if w == nil {
			return
		}
		ao := NewAuditObserver(w)
		if b.obs == nil {
			b.obs = ao
		} else {
			b.obs = MultiObserver(b.obs, ao)
		}
	}
}
func WithTelemetry(c *MetricsCollector) Option {
	return func(b *Boundary) {
		if c == nil {
			return
		}
		if b.obs == nil {
			b.obs = c
		} else {
			b.obs = MultiObserver(b.obs, c)
		}
	}
}
func WithMaxAttempts(n int) Option             { return func(b *Boundary) { b.maxAttempts = n } }
func WithMaxStagingAge(d time.Duration) Option { return func(b *Boundary) { b.maxStaging = d } }
func WithCommitTimeout(d time.Duration) Option { return func(b *Boundary) { b.commitTimeout = d } }
func WithDeadlineGrace(d time.Duration) Option { return func(b *Boundary) { b.grace = d } }
func WithResolveWait(d time.Duration) Option   { return func(b *Boundary) { b.resolveWait = d } }
func WithBackoff(f func(attempt int) time.Duration) Option {
	return func(b *Boundary) { b.backoff = f }
}

// Boundary is safe for concurrent use.
type Boundary struct {
	eng           Engine
	obs           Observer
	maxAttempts   int
	maxStaging    time.Duration // generous: guards only fire at extremes
	commitTimeout time.Duration // applied when ctx has no deadline
	grace         time.Duration // wait for an in-flight commit after ctx expiry
	resolveWait   time.Duration // max wait for a PENDING status
	pollInterval  time.Duration
	backoff       func(int) time.Duration
}

// New builds a boundary over an embedded scriva DB.
func New(db *scriva.DB, opts ...Option) (*Boundary, error) {
	if db == nil || db.Engine() == nil {
		return nil, internal(ErrAtomicUnavailable)
	}
	return NewWithEngine(FromEngine(db.Engine()), opts...)
}

// NewWithEngine builds a boundary over an Engine seam.
func NewWithEngine(eng Engine, opts ...Option) (*Boundary, error) {
	if eng == nil {
		return nil, internal(ErrAtomicUnavailable)
	}
	b := &Boundary{
		eng: eng, maxAttempts: 4, maxStaging: 2 * time.Minute,
		commitTimeout: 30 * time.Second, grace: 2 * time.Second,
		resolveWait: 5 * time.Second, pollInterval: 10 * time.Millisecond,
		backoff: func(a int) time.Duration { return time.Duration(5<<uint(a-1)) * time.Millisecond },
	}
	for _, o := range opts {
		o(b)
	}
	if b.maxAttempts < 1 {
		b.maxAttempts = 1
	}
	return b, nil
}

// Outcome reports the engine's recorded status for a commit key.
func (b *Boundary) Outcome(key string) (Outcome, string) {
	st, tx := b.eng.XTxStatus(key)
	return toOutcome(st), tx
}

func toOutcome(s engine.XTxStatus) Outcome {
	switch s {
	case engine.XTxCommitted:
		return OutcomeCommitted
	case engine.XTxAborted:
		return OutcomeAborted
	case engine.XTxPending:
		return OutcomePending
	}
	return OutcomeUnknown
}

// resolve polls status while PENDING, bounded by resolveWait.
func (b *Boundary) resolve(key string) (Outcome, string) {
	deadline := time.Now().Add(b.resolveWait)
	for {
		o, tx := b.Outcome(key)
		if o != OutcomePending || !time.Now().Before(deadline) {
			return o, tx
		}
		time.Sleep(b.pollInterval)
	}
}

// Run executes build + commit with bounded retry (audit G6). build must
// re-read the state it depends on and stage every write on tx each time it is
// called — it is re-invoked from scratch on each attempt, never replayed.
// Retries happen only for retry-safe classes (Conflict, Exhausted,
// Durability) and for an outcome-unknown commit that status lookup resolved
// to ABORTED/UNKNOWN. If build returns ErrNoChange, Run rolls back and
// returns a Result with NoChange set.
func (b *Boundary) Run(ctx context.Context, intent string, build func(ctx context.Context, tx *Tx) error) (*Result, error) {
	for attempt := 1; ; attempt++ {
		tx, err := b.Begin(ctx)
		if err != nil {
			return nil, err
		}
		tx.SetIntent(intent)
		if err := build(tx.Context(), tx); err != nil {
			_ = tx.Rollback()
			if err == ErrNoChange {
				return &Result{NoChange: true}, nil
			}
			return nil, err
		}
		res, err := tx.Commit(ctx, intent)
		if err == nil {
			return res, nil
		}
		be, ok := err.(*Error)
		if !ok || !be.SafeToRetry || attempt >= b.maxAttempts {
			if ok {
				be.Attempts = attempt
			}
			return nil, err
		}
		select {
		case <-ctx.Done():
			be.Attempts = attempt
			return nil, err
		case <-time.After(b.backoff(attempt)):
		}
	}
}

package dbtx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/srjn45/scriva/engine"
)

type txCtxKey struct{}

// Tx accumulates an in-memory typed op list. It holds no engine resource: no
// lock, no snapshot, no open engine transaction (audit G2). A Tx is used from
// one goroutine at a time.
type Tx struct {
	b       *Boundary
	ctx     context.Context
	started time.Time

	mu       sync.Mutex
	finished bool
	ops      []engine.XTxOp
	seen     map[string]map[uint64]struct{}
}

// Begin starts a transaction. A Begin whose ctx descends from a still-open Tx
// is rejected (ErrNestedTx): flatten into one op list instead.
func (b *Boundary) Begin(ctx context.Context) (*Tx, error) {
	if parent, ok := ctx.Value(txCtxKey{}).(*Tx); ok && !parent.isFinished() {
		return nil, internal(ErrNestedTx)
	}
	t := &Tx{b: b, started: time.Now(), seen: map[string]map[uint64]struct{}{}}
	t.ctx = context.WithValue(ctx, txCtxKey{}, t)
	return t, nil
}

// Begin on an open Tx is always nested.
func (t *Tx) Begin() (*Tx, error) { return nil, internal(ErrNestedTx) }

// Context is the context to pass to code running inside the transaction; it
// carries the re-entrancy marker.
func (t *Tx) Context() context.Context { return t.ctx }

func (t *Tx) isFinished() bool { t.mu.Lock(); defer t.mu.Unlock(); return t.finished }

// Rollback discards staged ops. It is idempotent and a no-op after Commit, so
// `defer tx.Rollback()` is safe.
func (t *Tx) Rollback() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.finished = true
	t.ops = nil
	return nil
}

// Len is the number of staged ops.
func (t *Tx) Len() int { t.mu.Lock(); defer t.mu.Unlock(); return len(t.ops) }

func (t *Tx) stage(b *Boundary, op engine.XTxOp) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return internal(ErrTxFinished)
	}
	if b != t.b {
		return internal(fmt.Errorf("%w: %q (handle bound to a different boundary)", ErrUndeclared, op.Collection))
	}
	if time.Since(t.started) > t.b.maxStaging {
		return internal(ErrStagingExpired)
	}
	ids := t.seen[op.Collection]
	if ids == nil {
		ids = map[uint64]struct{}{}
		t.seen[op.Collection] = ids
	}
	if _, dup := ids[op.ID]; dup {
		return internal(fmt.Errorf("%w: %s/%d", ErrDuplicateOp, op.Collection, op.ID))
	}
	ids[op.ID] = struct{}{}
	t.ops = append(t.ops, op)
	return nil
}

// OpResult is one committed op. Rev is 0 when !Result.RevsAuthoritative.
type OpResult struct {
	Collection string
	ID         uint64
	Rev        uint64
	Kind       string
}

// Result of a successful commit.
type Result struct {
	TxID string
	Key  string
	Ops  []OpResult
	// RevsAuthoritative is false when the commit was replayed or confirmed by
	// status lookup: the engine's replay revs are untrusted (audit C3/G4), so
	// Rev is zeroed and callers must re-read.
	RevsAuthoritative bool
	Replayed          bool
	NoChange          bool
}

func estimate(ops []engine.XTxOp) (participants int, bytes int64) {
	parts := map[string]struct{}{}
	for _, o := range ops {
		parts[o.Collection] = struct{}{}
		bytes += 128 + int64(len(o.Collection))
		if o.Data != nil {
			bytes += 256
		}
	}
	return len(parts), bytes
}

// keyFor builds `<intent>/<payload-hash>`: a key can never be reused for a
// different op set (audit G5).
func keyFor(intent string, ops []engine.XTxOp) (string, error) {
	raw, err := json.Marshal(ops)
	if err != nil {
		return "", fmt.Errorf("%w: unencodable payload: %v", ErrInvalidOp, err)
	}
	sum := sha256.Sum256(raw)
	return intent + "/" + hex.EncodeToString(sum[:12]), nil
}

// Commit makes exactly one engine CommitXTx call (unless the key is already
// recorded COMMITTED, in which case no call is made). intent is the caller's
// stable operation identity (e.g. `<operation-journal-id>/<step>`).
func (t *Tx) Commit(ctx context.Context, intent string) (res *Result, err error) {
	t.mu.Lock()
	if t.finished {
		t.mu.Unlock()
		return nil, internal(ErrTxFinished)
	}
	t.finished = true
	ops := t.ops
	t.ops = nil
	started := t.started
	t.mu.Unlock()

	b := t.b
	begin := time.Now()
	ev := Event{Intent: intent, Ops: len(ops)}
	ev.Participants, _ = estimate(ops)
	defer func() {
		if b.obs == nil {
			return
		}
		ev.Duration = time.Since(begin)
		if err == nil {
			ev.Outcome, ev.Key = "committed", res.Key
			ev.TxID = res.TxID
			ev.Resolved = res.Replayed
		} else {
			ev.Outcome = string(Classify(err))
			var be *Error
			if errors.As(err, &be) {
				ev.Key, ev.TxID = be.Key, be.TxID
				ev.Resolved = be.Outcome != ""
			}
		}
		b.obs.Observe(ev)
	}()

	if intent == "" {
		return nil, internal(ErrKeyRequired)
	}
	if len(ops) == 0 {
		return nil, internal(ErrEmptyTx)
	}
	if time.Since(started) > b.maxStaging {
		return nil, internal(ErrStagingExpired)
	}
	if n, sz := estimate(ops); len(ops) > MaxOps || n > MaxParticipants || sz > MaxBytes {
		return nil, &Error{Class: ClassTooLarge, Err: fmt.Errorf("%w: %d ops, %d participants, ~%d bytes", ErrTooLarge, len(ops), n, sz)}
	}
	key, kerr := keyFor(intent, ops)
	if kerr != nil {
		return nil, internal(kerr)
	}
	if len(key) > MaxKeyLen {
		return nil, &Error{Class: ClassTooLarge, Key: key, Err: fmt.Errorf("%w: idempotency key %d bytes > %d", ErrTooLarge, len(key), MaxKeyLen)}
	}
	if err := ctx.Err(); err != nil {
		return nil, &Error{Class: ClassInternal, Key: key, Err: err}
	}
	// Key already COMMITTED (same intent + same payload hash): confirmed by
	// status, nothing to apply.
	if st, tx := b.eng.XTxStatus(key); st == engine.XTxCommitted {
		return replayed(key, tx, ops), nil
	}
	return b.execute(ctx, key, ops)
}

func replayed(key, txid string, ops []engine.XTxOp) *Result {
	r := &Result{TxID: txid, Key: key, Replayed: true}
	for _, o := range ops {
		r.Ops = append(r.Ops, OpResult{Collection: o.Collection, ID: o.ID, Kind: string(o.Op)})
	}
	return r
}

type commitRet struct {
	res *engine.XTxResult
	err error
}

func (b *Boundary) execute(ctx context.Context, key string, ops []engine.XTxOp) (*Result, error) {
	if _, hasDL := ctx.Deadline(); !hasDL && b.commitTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, b.commitTimeout)
		defer cancel()
	}
	// CommitXTx takes no ctx and may block on participant locks (audit C2/G7):
	// run it in a goroutine and never report a deadline without resolving the
	// outcome via key status.
	ch := make(chan commitRet, 1)
	go func() {
		r, err := b.eng.CommitXTx(key, ops)
		ch <- commitRet{r, err}
	}()
	var ret commitRet
	select {
	case ret = <-ch:
	case <-ctx.Done():
		select {
		case ret = <-ch:
		case <-time.After(b.grace):
			if st, tx := b.eng.XTxStatus(key); st == engine.XTxCommitted {
				return replayed(key, tx, ops), nil
			}
			// The goroutine may still commit: outcome genuinely unknown, and
			// NOT safe to retry.
			return nil, &Error{Class: ClassOutcomeUnknown, Key: key, Outcome: OutcomeUnknown,
				Err: fmt.Errorf("%w: %w", ErrCommitDeadline, ctx.Err())}
		}
	}
	if ret.err == nil {
		r := &Result{TxID: ret.res.TxID, Key: key, RevsAuthoritative: true}
		for _, o := range ret.res.Ops {
			r.Ops = append(r.Ops, OpResult{Collection: o.Collection, ID: o.ID, Rev: o.Rev, Kind: string(o.Op)})
		}
		return r, nil
	}
	return b.fail(key, ops, ret.err)
}

func (b *Boundary) fail(key string, ops []engine.XTxOp, err error) (*Result, error) {
	class := Classify(err)
	be := &Error{Class: class, Key: key, Err: err}
	var xe *engine.XTxError
	if errors.As(err, &xe) {
		be.TxID = xe.Tx
	}
	switch {
	case errors.Is(err, engine.ErrXTxUnsupported), errors.Is(err, engine.ErrFormatTooNew),
		errors.Is(err, engine.ErrReadOnly):
		be.Err = fmt.Errorf("%w: %w", ErrAtomicUnavailable, err)
	case class == ClassOutcomeUnknown:
		// G4: never succeed without status confirmation.
		inProgress := errors.Is(err, engine.ErrXTxInProgress)
		o, tx := b.resolve(key)
		be.Outcome = o
		if tx != "" {
			be.TxID = tx
		}
		switch o {
		case OutcomeCommitted:
			return replayed(key, tx, ops), nil
		case OutcomeAborted, OutcomeUnknown:
			// Definitely (ABORTED) or presumably (UNKNOWN, audit G4) not
			// applied: a rebuilt, re-read retry is allowed — but not for
			// another executor's in-flight key.
			be.SafeToRetry = !inProgress
		}
	default:
		be.SafeToRetry = class.Retryable() && engine.XTxRetrySafe(err)
	}
	return nil, be
}

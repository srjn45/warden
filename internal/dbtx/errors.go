package dbtx

import (
	"context"
	"errors"

	"github.com/srjn45/scriva/engine"
)

// Class is the closed set of failure classes the boundary reports. Every error
// leaving Commit/Run carries exactly one (audit G6).
type Class string

const (
	ClassConflict           Class = "conflict"
	ClassExhausted          Class = "exhausted"
	ClassTooLarge           Class = "too_large"
	ClassDurability         Class = "durability"
	ClassOutcomeUnknown     Class = "outcome_unknown"
	ClassCollectionNotFound Class = "collection_not_found"
	ClassInternal           Class = "internal"
)

// Retryable reports whether a failed commit of this class may be re-run as a
// fresh transaction after the caller re-reads state. Everything else is
// fail-closed.
func (c Class) Retryable() bool {
	switch c {
	case ClassConflict, ClassExhausted, ClassDurability:
		return true
	}
	return false
}

// Outcome is the resolved fate of a commit key.
type Outcome string

const (
	OutcomeCommitted Outcome = "COMMITTED"
	OutcomeAborted   Outcome = "ABORTED"
	OutcomePending   Outcome = "PENDING"
	OutcomeUnknown   Outcome = "UNKNOWN"
)

// Boundary-level sentinels (all wrapped in *Error; test with errors.Is).
var (
	ErrTxFinished        = errors.New("dbtx: transaction already committed or rolled back")
	ErrNestedTx          = errors.New("dbtx: nested or re-entrant Begin is not supported")
	ErrEmptyTx           = errors.New("dbtx: cannot commit an empty transaction")
	ErrUndeclared        = errors.New("dbtx: collection is not declared on this boundary")
	ErrDuplicateOp       = errors.New("dbtx: collection/id already staged in this transaction")
	ErrInvalidOp         = errors.New("dbtx: invalid operation")
	ErrKeyRequired       = errors.New("dbtx: commit requires a non-empty intent (idempotency key)")
	ErrStagingExpired    = errors.New("dbtx: transaction exceeded its maximum staging age")
	ErrCommitDeadline    = errors.New("dbtx: commit deadline exceeded before the outcome was known")
	ErrTooLarge          = errors.New("dbtx: transaction exceeds limits")
	ErrAtomicUnavailable = errors.New("dbtx: atomic cross-collection commit is unavailable")
	ErrNoChange          = errors.New("dbtx: nothing to commit")
)

// Error is the single error type returned by the boundary.
type Error struct {
	Class   Class
	Key     string  // idempotency key of the failing commit, when known
	TxID    string  // engine txid, when known
	Outcome Outcome // resolved outcome for ClassOutcomeUnknown, when determined
	// SafeToRetry is true only when re-running the whole operation (re-read +
	// restage) cannot double-apply. Run honours it; direct Commit callers may
	// too.
	SafeToRetry bool
	Attempts    int // attempts made by Run, 0 for a bare Commit
	Err         error
}

func (e *Error) Error() string { return "dbtx: " + string(e.Class) + ": " + e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

// Classify maps any error to its Class. Untyped/unknown errors are Internal
// (fail closed).
func Classify(err error) Class {
	var be *Error
	if errors.As(err, &be) {
		return be.Class
	}
	switch {
	case err == nil:
		return ClassInternal
	case errors.Is(err, engine.ErrXTxOutcomeUnknown), errors.Is(err, engine.ErrXTxInProgress):
		return ClassOutcomeUnknown
	case errors.Is(err, engine.ErrXTxTooLarge), errors.Is(err, ErrTooLarge):
		return ClassTooLarge
	case errors.Is(err, engine.ErrXTxConflict):
		return ClassConflict
	case errors.Is(err, engine.ErrResourceExhausted), errors.Is(err, engine.ErrQuotaExceeded):
		return ClassExhausted
	case errors.Is(err, engine.ErrXTxDurability):
		return ClassDurability
	case errors.Is(err, engine.ErrCollectionNotFound):
		return ClassCollectionNotFound
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return ClassInternal
	}
	return ClassInternal
}

func internal(err error) *Error { return &Error{Class: ClassInternal, Err: err} }

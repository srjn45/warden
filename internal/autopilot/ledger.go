package autopilot

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ledgerWriter is the provenance stamped on daemon-side ledger writes. Agents
// write their own keys under their own identity; the daemon (landings, digest
// bookkeeping) writes as "daemon".
const ledgerWriter = "daemon"

// ErrLedgerMissing is returned by a ledger read when the key does not exist yet.
// A fresh run has no ledger keys, so callers treat it as an empty collection.
var ErrLedgerMissing = errors.New("autopilot: ledger key not found")

// ErrLedgerConflict is returned by a CAS write when the current value no longer
// matches the caller's expectation — another writer won the race. The caller
// re-reads and retries (autopilot.md §4).
var ErrLedgerConflict = errors.New("autopilot: ledger value conflict")

// CtxStore is the narrow slice of the daemon's shared-context blackboard the
// ledger needs (ctxstore.Store's Get/Set/CompareAndSet). It is an interface so
// the ledger is unit-testable without a ScrivaDB, and so the autopilot package
// stays decoupled from the daemon. Implementations MUST map "key absent" to
// ErrLedgerMissing on Get and "expectation mismatch" to ErrLedgerConflict on
// CompareAndSet; the daemon adapter (internal/daemon) does this translation.
type CtxStore interface {
	// Get returns the raw JSON value at key, or ErrLedgerMissing when absent.
	Get(key string) (string, error)
	// Set writes value at key under the writer provenance by.
	Set(key, value, by string) error
	// CompareAndSet writes value at key only if the current value equals expected
	// (expected "" means "the key must be absent"), else ErrLedgerConflict.
	CompareAndSet(key, expected, value, by string) error
}

// LedgerTask is one task row in the run ledger (autopilot.md §4). Written by the
// brain via ctx_cas; read by the daemon when composing the recovery digest and
// the status task rollup. State must be a canonical LedgerState on write.
type LedgerTask struct {
	ID        string      `json:"id"`
	State     LedgerState `json:"state"`
	WorkerID  string      `json:"worker_id,omitempty"`
	Branch    string      `json:"branch,omitempty"`
	PR        int         `json:"pr,omitempty"`
	Note      string      `json:"note,omitempty"`
	UpdatedAt string      `json:"updated_at,omitempty"`
}

// Landing is one recorded merge into the integration branch. Written
// authoritatively by the daemon inside the `land` handler (S4), never trusted to
// the brain — it is the idempotency ledger for `land` (autopilot.md §4, §6).
type Landing struct {
	Branch   string `json:"branch"`
	SHA      string `json:"sha"`
	PR       int    `json:"pr,omitempty"`
	LandedAt string `json:"landed_at"`
}

// JournalEntry is one row of the brain's rolling decision log, newest-first. It
// enriches a successor brain's digest but a lapse never affects correctness
// (autopilot.md §4).
type JournalEntry struct {
	At   string `json:"at"`
	Note string `json:"note"`
}

// Ledger is a typed read/write facade over the reserved ledger keys for one
// run (autopilot.md §4). Keys are DOT-separated (`autopilot.<run_id>.tasks`, …)
// because the ctx store rejects "/".
type Ledger struct {
	store CtxStore
	runID string
}

// NewLedger returns a ledger over store scoped to runID.
func NewLedger(store CtxStore, runID string) *Ledger {
	return &Ledger{store: store, runID: runID}
}

// key builds a dot-namespaced ledger key for this run.
func (l *Ledger) key(kind string) string {
	return "autopilot." + l.runID + "." + kind
}

// TasksKey/LandingsKey/JournalKey expose the on-the-wire ctx keys (dot form) so
// the brain persona docs and the land handler reference the exact same strings.
func (l *Ledger) TasksKey() string    { return l.key("tasks") }
func (l *Ledger) LandingsKey() string { return l.key("landings") }
func (l *Ledger) JournalKey() string  { return l.key("journal") }

// TaskStateKey is the optional per-task overlay used for TUI segmentation:
// autopilot.<run_id>.tasks.<taskID>.state. The JSON array at TasksKey remains
// the source of truth; this key is a convenience fan-out, not required.
func (l *Ledger) TaskStateKey(taskID string) string {
	return l.key("tasks." + taskID + ".state")
}

// TaskBranchKey is the optional per-task overlay of the worker branch:
// autopilot.<run_id>.tasks.<taskID>.branch.
func (l *Ledger) TaskBranchKey(taskID string) string {
	return l.key("tasks." + taskID + ".branch")
}

// Tasks reads the task ledger, returning an empty slice for an absent key.
func (l *Ledger) Tasks() ([]LedgerTask, error) {
	var out []LedgerTask
	if err := l.readJSON(l.TasksKey(), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// WriteTasks replaces the task ledger unconditionally (a plain Set). Used when a
// lost race is not a concern; prefer CASTasks for the brain's read-modify-write.
// Each row's State must be a canonical LedgerState; unknown values are rejected
// before the store write. Reads of pre-enum data are not validated.
func (l *Ledger) WriteTasks(tasks []LedgerTask, by string) error {
	if err := validateLedgerTasks(tasks); err != nil {
		return err
	}
	raw, err := marshalList(tasks)
	if err != nil {
		return err
	}
	return l.store.Set(l.TasksKey(), raw, writerOrDefault(by))
}

// CASTasks atomically replaces the task ledger, guarding against a concurrent
// writer: expected is the task list the caller last read (nil = the key is
// absent). `next` is validated; `expected` is not, so a CAS from pre-enum
// data onto a canonical list can succeed. It returns ErrLedgerConflict when the
// store moved under the caller, who should re-read via Tasks and retry
// (autopilot.md §4).
func (l *Ledger) CASTasks(expected, next []LedgerTask, by string) error {
	if err := validateLedgerTasks(next); err != nil {
		return err
	}
	expRaw, err := marshalExpected(expected)
	if err != nil {
		return err
	}
	nextRaw, err := marshalList(next)
	if err != nil {
		return err
	}
	return l.store.CompareAndSet(l.TasksKey(), expRaw, nextRaw, writerOrDefault(by))
}

// WriteTaskState writes the optional TUI overlay for one task's canonical state.
// It does not update the JSON array at TasksKey — callers that own the ledger
// still WriteTasks/CASTasks the array as the source of truth.
func (l *Ledger) WriteTaskState(taskID string, state LedgerState, by string) error {
	if strings.TrimSpace(taskID) == "" {
		return fmt.Errorf("%w: empty id", ErrInvalidLedgerTask)
	}
	if !state.Valid() {
		return invalidLedgerStateError(state)
	}
	return l.store.Set(l.TaskStateKey(taskID), string(state), writerOrDefault(by))
}

// TaskState reads the optional TUI overlay for one task. An absent key is
// ErrLedgerMissing — callers fall back to the tasks array.
func (l *Ledger) TaskState(taskID string) (LedgerState, error) {
	raw, err := l.store.Get(l.TaskStateKey(taskID))
	if err != nil {
		return "", err
	}
	return ParseLedgerState(raw)
}

// Landings reads the append-only landings ledger (empty when absent).
func (l *Ledger) Landings() ([]Landing, error) {
	var out []Landing
	if err := l.readJSON(l.LandingsKey(), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// AppendLanding appends one landing under the daemon writer via a read-modify-CAS
// retry loop, so concurrent daemon writes never clobber each other. It backs the
// authoritative landings write the `land` handler performs (S4); provided here so
// the ledger owns the encoding in one place.
func (l *Ledger) AppendLanding(land Landing) error {
	for {
		cur, err := l.Landings()
		if err != nil {
			return err
		}
		expRaw, err := marshalExpected(landingsOrNil(cur))
		if err != nil {
			return err
		}
		next := append(append([]Landing{}, cur...), land)
		nextRaw, err := marshalList(next)
		if err != nil {
			return err
		}
		err = l.store.CompareAndSet(l.LandingsKey(), expRaw, nextRaw, ledgerWriter)
		if errors.Is(err, ErrLedgerConflict) {
			continue // lost the race — re-read and retry
		}
		return err
	}
}

// Journal reads the rolling decision log (empty when absent).
func (l *Ledger) Journal() ([]JournalEntry, error) {
	var out []JournalEntry
	if err := l.readJSON(l.JournalKey(), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// readJSON loads key and unmarshals it into v. An absent key is not an error: v
// is left at its zero value (an empty collection) so callers need not branch.
func (l *Ledger) readJSON(key string, v any) error {
	raw, err := l.store.Get(key)
	if errors.Is(err, ErrLedgerMissing) {
		return nil
	}
	if err != nil {
		return err
	}
	if raw == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(raw), v); err != nil {
		return fmt.Errorf("autopilot: ledger %s: decode: %w", key, err)
	}
	return nil
}

// marshalList encodes a slice to a compact JSON string, normalizing a nil slice
// to "[]" (not "null") so a written-then-read empty ledger round-trips cleanly
// and CAS comparisons stay stable regardless of nil-vs-empty on the write side.
func marshalList[T any](v []T) (string, error) {
	if v == nil {
		v = []T{}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// marshalExpected encodes the CAS "expected" value: a nil slice means "the key
// must be absent" (the empty-string sentinel the ctx store's CAS understands),
// while any non-nil value is compared byte-for-byte against the stored JSON — so
// it must use the same normalization as the stored form (marshalList).
func marshalExpected[T any](v []T) (string, error) {
	if v == nil {
		return "", nil
	}
	return marshalList(v)
}

// landingsOrNil preserves the nil-vs-empty distinction for AppendLanding's CAS:
// an absent key reads back as a nil slice (expected ""), while an existing empty
// list is a real "[]".
func landingsOrNil(xs []Landing) []Landing {
	return xs
}

// writerOrDefault falls back to the daemon writer provenance when unset.
func writerOrDefault(by string) string {
	if by == "" {
		return ledgerWriter
	}
	return by
}

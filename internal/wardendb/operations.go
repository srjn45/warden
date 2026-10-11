package wardendb

// The operation journal: the durable `operations` collection and its lifecycle
// primitives, as specified by
// docs/specs/2026-10-10-warden-db-operation-journal-and-reconciliation.md.
//
// An operation is the committed intent to create, mutate or remove a resource
// outside warden-db. The journal owns the record, its idempotency key, its
// state machine, its lease and its recovery queries. It issues no external
// effect, runs no executor and is wired into nothing: these are primitives.
//
// Every mutation is one dbtx commit conditioned on the record revision (CAS).
// A caller that must commit its own collections together with a journal change
// (spec §1.1) passes a TxStage, which is staged into the same commit.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"

	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/dbtx"
)

const (
	// OperationsCollection is the catalogue name of the operation journal.
	OperationsCollection = "operations"
	// OperationSchemaVersion is the record-schema version of an Operation.
	OperationSchemaVersion = 1
)

// Bounds of one journal row. Large output belongs in an external blob with a
// pointer in Result, never in the row.
const (
	// MaxOperationPayload bounds the encoded size of each of Intent,
	// Compensation, Observation, Result and Checkpoint.
	MaxOperationPayload = 16 << 10
	// MaxOperationError bounds LastError; longer text is truncated.
	MaxOperationError = 4 << 10
	// MaxOperationHistory bounds the retained history; the oldest entries are
	// dropped first and AuditSeq keeps counting.
	MaxOperationHistory = 64
)

// OperationState is a state of the journal state machine (spec §2).
type OperationState string

const (
	OperationPending            OperationState = "pending"
	OperationReady              OperationState = "ready"
	OperationRunning            OperationState = "running"
	OperationRetryWait          OperationState = "retry_wait"
	OperationReconciling        OperationState = "reconciling"
	OperationCompensating       OperationState = "compensating"
	OperationSucceeded          OperationState = "succeeded"
	OperationCompensated        OperationState = "compensated"
	OperationCanceled           OperationState = "canceled"
	OperationFailed             OperationState = "failed"
	OperationManualIntervention OperationState = "manual_intervention"
)

// operationTransitions is the "allowed next states" column of spec §2.
var operationTransitions = map[OperationState][]OperationState{
	OperationPending:            {OperationReady, OperationCanceled, OperationFailed},
	OperationReady:              {OperationRunning, OperationCanceled, OperationFailed},
	OperationRunning:            {OperationSucceeded, OperationRetryWait, OperationReconciling, OperationCompensating, OperationFailed},
	OperationRetryWait:          {OperationReady, OperationFailed, OperationCanceled},
	OperationReconciling:        {OperationSucceeded, OperationReady, OperationCompensating, OperationManualIntervention, OperationFailed},
	OperationCompensating:       {OperationCompensated, OperationRetryWait, OperationManualIntervention, OperationFailed},
	OperationSucceeded:          nil,
	OperationCompensated:        nil,
	OperationCanceled:           nil,
	OperationFailed:             nil,
	OperationManualIntervention: nil,
}

// OperationStates returns every state, in spec order.
func OperationStates() []OperationState {
	return []OperationState{
		OperationPending, OperationReady, OperationRunning, OperationRetryWait,
		OperationReconciling, OperationCompensating, OperationSucceeded,
		OperationCompensated, OperationCanceled, OperationFailed, OperationManualIntervention,
	}
}

// Valid reports whether s is a state of the machine.
func (s OperationState) Valid() bool {
	_, ok := operationTransitions[s]
	return ok
}

// Terminal reports whether s never changes again.
func (s OperationState) Terminal() bool {
	next, ok := operationTransitions[s]
	return ok && len(next) == 0
}

// Leased reports whether s is a state in which only the lease holder may
// write: running, reconciling and compensating.
func (s OperationState) Leased() bool {
	return s == OperationRunning || s == OperationReconciling || s == OperationCompensating
}

// Next returns the states s may move to.
func (s OperationState) Next() []OperationState {
	return append([]OperationState(nil), operationTransitions[s]...)
}

// CanTransition reports whether the machine allows s -> to.
func (s OperationState) CanTransition(to OperationState) bool {
	return slices.Contains(operationTransitions[s], to)
}

// OperationEvent is one entry of an operation's append-only history. AuditID
// is the identifier carried by the audit event emitted for the same change.
type OperationEvent struct {
	Seq     uint64         `json:"seq"`
	AuditID string         `json:"audit_id"`
	At      time.Time      `json:"at"`
	Event   string         `json:"event"`
	From    OperationState `json:"from,omitempty"`
	To      OperationState `json:"to"`
	Attempt int            `json:"attempt"`
	Owner   string         `json:"owner,omitempty"`
	Detail  string         `json:"detail,omitempty"`
}

// History event names.
const (
	OperationEventBegin      = "begin"
	OperationEventTransition = "transition"
	OperationEventClaim      = "claim"    // ready -> running under a new lease
	OperationEventAcquire    = "acquire"  // lease taken on an unleased row
	OperationEventTakeover   = "takeover" // an expired or released lease was taken over
	OperationEventRelease    = "release"
)

// Operation is one journal row. The field names and classes are those of spec
// §1.2; the trailing block holds what the primitives need beyond it.
type Operation struct {
	// Immutable intent and identity.
	ID             string         `json:"id"`
	Kind           string         `json:"kind"`
	SubjectType    string         `json:"subject_type"`
	SubjectID      string         `json:"subject_id"`
	Intent         map[string]any `json:"intent,omitempty"`
	IdempotencyKey string         `json:"idempotency_key"`
	DependsOn      []string       `json:"depends_on,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	DeadlineAt     time.Time      `json:"deadline_at,omitzero"`
	Compensation   map[string]any `json:"compensation,omitempty"`

	// Journal coordination and outcome.
	State          OperationState `json:"state"`
	Attempt        int            `json:"attempt"`
	NextAttemptAt  time.Time      `json:"next_attempt_at,omitzero"`
	LeaseOwner     string         `json:"lease_owner,omitempty"`
	LeaseExpiresAt time.Time      `json:"lease_expires_at,omitzero"`
	LastError      string         `json:"last_error,omitempty"`
	FinishedAt     time.Time      `json:"finished_at,omitzero"`
	Supersedes     string         `json:"supersedes,omitempty"`

	// Last verified external fact.
	Observation map[string]any `json:"observation,omitempty"`
	ObservedAt  time.Time      `json:"observed_at,omitzero"`
	ExternalRef string         `json:"external_ref,omitempty"`
	Result      map[string]any `json:"result,omitempty"`

	UpdatedAt time.Time `json:"updated_at"`

	// SchemaVersion is the OperationSchemaVersion the row was written with.
	SchemaVersion int `json:"schema_version"`
	// IntentFingerprint identifies the canonical intent behind the
	// idempotency key; a reuse of the key with another fingerprint conflicts.
	IntentFingerprint string `json:"intent_fingerprint"`
	// IdempotencyRef is the unique (kind, idempotency_key) index value.
	IdempotencyRef string `json:"idempotency_ref"`
	// LeaseEpoch is the fencing token: it grows with every lease grant.
	LeaseEpoch uint64 `json:"lease_epoch"`
	// Step and Checkpoint are the holder's progress within the attempt.
	Step       string         `json:"step,omitempty"`
	Checkpoint map[string]any `json:"checkpoint,omitempty"`
	// AuditSeq counts the history entries ever written; History holds the
	// most recent MaxOperationHistory of them.
	AuditSeq uint64           `json:"audit_seq"`
	History  []OperationEvent `json:"history,omitempty"`
}

// AuditIDs returns the audit identifiers of the retained history, oldest first.
func (o Operation) AuditIDs() []string {
	out := make([]string, len(o.History))
	for i, h := range o.History {
		out[i] = h.AuditID
	}
	return out
}

// LeaseLive reports whether the row has a lease that has not expired at now.
func (o Operation) LeaseLive(now time.Time) bool {
	return o.LeaseOwner != "" && now.Before(o.LeaseExpiresAt)
}

// OperationSpec is the immutable intent of a new operation.
type OperationSpec struct {
	// Kind is the effect kind, e.g. "tmux.session.create".
	Kind string
	// SubjectType and SubjectID are the auditable subject reference.
	SubjectType string
	SubjectID   string
	// IdempotencyKey is unique per Kind for the retained life of the row.
	IdempotencyKey string
	// Intent is the stable, non-secret desired state.
	Intent map[string]any
	// DependsOn names earlier operations that must have succeeded before this
	// one may become ready.
	DependsOn []string
	// DeadlineAt is optional and is not part of the intent fingerprint.
	DeadlineAt time.Time
	// Compensation is the operation's declared compensation policy.
	Compensation map[string]any
	// Supersedes names the terminal operation this one replaces.
	Supersedes string
}

// OperationUpdate carries what a transition records.
type OperationUpdate struct {
	// Lease is required when the operation is leaving a leased state.
	Lease *Lease
	// Error becomes LastError (redacted and bounded).
	Error string
	// NextAttemptAt is required when entering retry_wait.
	NextAttemptAt time.Time
	// Observation, ObservedAt, ExternalRef and Result record the observed
	// external fact. ObservedAt defaults to now when Observation is set.
	Observation map[string]any
	ObservedAt  time.Time
	ExternalRef string
	Result      map[string]any
	// Reason is recorded in the history entry and the audit event.
	Reason string
}

// OperationProgress is a step/checkpoint update within an attempt.
type OperationProgress struct {
	Step        string
	Checkpoint  map[string]any
	Observation map[string]any
	ObservedAt  time.Time
	ExternalRef string
}

// Journal errors. The typed errors below wrap them.
var (
	// ErrOperationNotFound: no operation with that id or key.
	ErrOperationNotFound = errors.New("wardendb: operation not found")
	// ErrInvalidOperation: a spec, update or argument is malformed or over a bound.
	ErrInvalidOperation = errors.New("wardendb: invalid operation")
	// ErrIdempotencyConflict is wrapped by every *IdempotencyConflictError.
	ErrIdempotencyConflict = errors.New("wardendb: idempotency key reused with a different intent")
	// ErrIllegalTransition is wrapped by every *TransitionError.
	ErrIllegalTransition = errors.New("wardendb: illegal operation transition")
	// ErrDependencyUnsatisfied is wrapped by every *DependencyError.
	ErrDependencyUnsatisfied = errors.New("wardendb: operation dependency not satisfied")
	// ErrOperationSchema: a row was written by a newer schema version.
	ErrOperationSchema = errors.New("wardendb: unsupported operation schema version")
)

// IdempotencyConflictError reports a (kind, idempotency key) that already
// names an operation with a different intent.
type IdempotencyConflictError struct {
	Kind, IdempotencyKey string
	// ExistingID is the operation that owns the key.
	ExistingID string
	// Existing and Got are the two intent fingerprints.
	Existing, Got string
}

func (e *IdempotencyConflictError) Error() string {
	return fmt.Sprintf("wardendb: idempotency key %q of kind %q already names operation %s with a different intent (fingerprint %s, got %s); a changed intent needs a new key",
		e.IdempotencyKey, e.Kind, e.ExistingID, e.Existing, e.Got)
}

// Unwrap exposes ErrIdempotencyConflict.
func (e *IdempotencyConflictError) Unwrap() error { return ErrIdempotencyConflict }

// TransitionError reports a transition the state machine or its guards refuse.
type TransitionError struct {
	ID       string
	From, To OperationState
	// Reason is empty when the machine has no such edge.
	Reason string
}

func (e *TransitionError) Error() string {
	msg := fmt.Sprintf("wardendb: operation %s cannot move from %s to %s", e.ID, e.From, e.To)
	if e.Reason != "" {
		msg += ": " + e.Reason
	}
	return msg
}

// Unwrap exposes ErrIllegalTransition.
func (e *TransitionError) Unwrap() error { return ErrIllegalTransition }

// DependencyError reports a predecessor that blocks pending -> ready.
type DependencyError struct {
	ID        string
	DependsOn string
	// State is the predecessor's state; empty when it does not exist.
	State OperationState
}

func (e *DependencyError) Error() string {
	if e.ID == "" {
		return fmt.Sprintf("wardendb: new operation depends on %s, which does not exist", e.DependsOn)
	}
	if e.State == "" {
		return fmt.Sprintf("wardendb: operation %s depends on %s, which does not exist", e.ID, e.DependsOn)
	}
	return fmt.Sprintf("wardendb: operation %s depends on %s, which is %s, not %s", e.ID, e.DependsOn, e.State, OperationSucceeded)
}

// Unwrap exposes ErrDependencyUnsatisfied.
func (e *DependencyError) Unwrap() error { return ErrDependencyUnsatisfied }

// Audit actions of the journal. Each is also a counter name, with the
// collection label OperationsCollection. The audit event's Target is the
// operation id and its detail carries the audit_id recorded in the history.
const (
	ActionOperationBegin      = "wardendb.operation.begin"
	ActionOperationTransition = "wardendb.operation.transition"
	ActionOperationLease      = "wardendb.operation.lease"
	// ActionOperationFenced: a write was refused because its lease is stale
	// or expired.
	ActionOperationFenced = "wardendb.operation.fenced"
)

// TxStage stages a caller's own writes into the commit of a journal change,
// so the operation and the internal state that makes it necessary commit or
// roll back together. Bind the caller's collections on Journal.Boundary. A
// stage may be invoked more than once (a conflicting commit is rebuilt) and
// is not invoked at all when Begin returns an existing operation.
type TxStage func(ctx context.Context, tx *dbtx.Tx) error

// JournalOption configures OpenJournal.
type JournalOption func(*Journal)

// WithOperationClock injects the journal's time source.
func WithOperationClock(now func() time.Time) JournalOption {
	return func(j *Journal) {
		if now != nil {
			j.clock = now
		}
	}
}

// Journal is the typed owner of the operations collection. It is safe for
// concurrent use; its writes are serialized.
type Journal struct {
	db    *DB
	col   *Collection[Operation]
	b     *dbtx.Boundary
	coll  *dbtx.Coll[Operation]
	clock func() time.Time

	mu sync.Mutex
}

// operationIndexes are the indexed fields required by spec §1.2. The unique
// (kind, idempotency_key) pair is carried by idempotency_ref.
var operationIndexes = []string{
	"kind", "state", "subject_type", "subject_id", "idempotency_key",
	"next_attempt_at", "created_at", "finished_at",
}

// OpenJournal registers the operations collection on db under the strict
// policy and returns its journal. It can be opened once per handle
// (ErrDuplicateCollection afterwards).
func OpenJournal(db *DB, opts ...JournalOption) (*Journal, error) {
	col, err := Register[Operation](db, Spec{
		Name:    OperationsCollection,
		Version: OperationSchemaVersion,
		Options: []scriva.CollectionOption{scriva.WithUniqueIndex("id", "idempotency_ref")},
	})
	if err != nil {
		return nil, err
	}
	eng, err := col.Engine()
	if err != nil {
		return nil, err
	}
	for _, f := range operationIndexes {
		if err := eng.EnsureIndex(f); err != nil {
			return nil, fmt.Errorf("wardendb: operations index %q: %w", f, err)
		}
	}
	h, err := db.Handle()
	if err != nil {
		return nil, err
	}
	b, err := dbtx.New(h)
	if err != nil {
		return nil, fmt.Errorf("wardendb: operations boundary: %w", err)
	}
	coll, err := dbtx.Bind(b, OperationsCollection, operationCodec{})
	if err != nil {
		return nil, fmt.Errorf("wardendb: bind operations: %w", err)
	}
	j := &Journal{db: db, col: col, b: b, coll: coll, clock: time.Now}
	for _, o := range opts {
		if o != nil {
			o(j)
		}
	}
	return j, nil
}

// Boundary returns the transaction boundary the journal commits on, for
// binding the collections a TxStage writes.
func (j *Journal) Boundary() *dbtx.Boundary { return j.b }

func (j *Journal) now() time.Time { return j.clock().UTC().Round(0) }

// operationCodec maps an Operation to its stored document through JSON.
type operationCodec struct{}

func (operationCodec) Encode(op Operation) (map[string]any, error) {
	raw, err := json.Marshal(op)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func (operationCodec) Decode(m map[string]any) (Operation, error) {
	var op Operation
	raw, err := json.Marshal(m)
	if err != nil {
		return op, err
	}
	if err := json.Unmarshal(raw, &op); err != nil {
		return op, fmt.Errorf("wardendb: decode operation: %w", err)
	}
	if op.SchemaVersion > OperationSchemaVersion {
		return op, fmt.Errorf("%w: operation %s has version %d, this build reads up to %d", ErrOperationSchema, op.ID, op.SchemaVersion, OperationSchemaVersion)
	}
	return op, nil
}

// newOperationID mints `op-<ULID>`: 48 bits of millisecond time and 80 random
// bits in Crockford base32, so ids sort by creation time.
func newOperationID(now time.Time) (string, error) {
	var raw [16]byte
	binary.BigEndian.PutUint64(raw[:8], uint64(now.UnixMilli())<<16)
	if _, err := rand.Read(raw[6:]); err != nil {
		return "", err
	}
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	hi, lo := binary.BigEndian.Uint64(raw[:8]), binary.BigEndian.Uint64(raw[8:])
	var out [26]byte
	for i := 25; i >= 0; i-- {
		out[i] = alphabet[lo&31]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return "op-" + string(out[:]), nil
}

func idempotencyRef(kind, key string) string {
	sum := sha256.Sum256([]byte(strconv.Itoa(len(kind)) + ":" + kind + key))
	return hex.EncodeToString(sum[:])
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidOperation, fmt.Sprintf(format, args...))
}

// bounded checks that a payload is encodable and within MaxOperationPayload.
func bounded(field string, v map[string]any) error {
	if v == nil {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return invalid("%s is not encodable: %v", field, err)
	}
	if len(raw) > MaxOperationPayload {
		return invalid("%s is %d bytes, over the %d byte bound; store large payloads externally and reference them", field, len(raw), MaxOperationPayload)
	}
	return nil
}

func boundedError(s string) string {
	s = audit.RedactSecrets(s)
	if len(s) > MaxOperationError {
		s = s[:MaxOperationError]
	}
	return s
}

func cleanToken(field, v string, required bool) error {
	if v == "" {
		if required {
			return invalid("%s is required", field)
		}
		return nil
	}
	if len(v) > 256 || strings.ContainsFunc(v, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return invalid("%s must be at most 256 bytes without control characters", field)
	}
	return nil
}

// fingerprint canonicalizes the intent of spec (encoding/json sorts map keys)
// and hashes it. DeadlineAt is deliberately excluded: a timestamp is not part
// of the logical desired effect.
func (s OperationSpec) fingerprint() (string, error) {
	deps := append([]string(nil), s.DependsOn...)
	sort.Strings(deps)
	raw, err := json.Marshal(struct {
		Kind         string         `json:"kind"`
		SubjectType  string         `json:"subject_type"`
		SubjectID    string         `json:"subject_id"`
		Intent       map[string]any `json:"intent"`
		DependsOn    []string       `json:"depends_on"`
		Compensation map[string]any `json:"compensation"`
		Supersedes   string         `json:"supersedes"`
	}{s.Kind, s.SubjectType, s.SubjectID, s.Intent, deps, s.Compensation, s.Supersedes})
	if err != nil {
		return "", invalid("intent is not encodable: %v", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func (s OperationSpec) validate() error {
	for _, f := range []struct {
		name, v  string
		required bool
	}{
		{"kind", s.Kind, true}, {"subject_type", s.SubjectType, true}, {"subject_id", s.SubjectID, true},
		{"idempotency_key", s.IdempotencyKey, true}, {"supersedes", s.Supersedes, false},
	} {
		if err := cleanToken(f.name, f.v, f.required); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, d := range s.DependsOn {
		if d == "" || seen[d] {
			return invalid("depends_on has an empty or repeated id %q", d)
		}
		seen[d] = true
	}
	if err := bounded("intent", s.Intent); err != nil {
		return err
	}
	return bounded("compensation", s.Compensation)
}

// usable fails once the handle is closed or retired.
func (j *Journal) usable() (*engine.Collection, error) {
	if j == nil {
		return nil, ErrClosed
	}
	return j.col.Engine()
}

// find resolves an indexed unique field to its record. ok is false when no
// record carries the value.
func (j *Journal) find(field, value string) (dbtx.Versioned[Operation], bool, error) {
	eng, err := j.usable()
	if err != nil {
		return dbtx.Versioned[Operation]{}, false, err
	}
	ids, ok := eng.IndexLookup(field, value)
	if !ok {
		return dbtx.Versioned[Operation]{}, false, fmt.Errorf("wardendb: operations index %q is missing", field)
	}
	if len(ids) == 0 {
		return dbtx.Versioned[Operation]{}, false, nil
	}
	v, err := j.coll.Get(ids[0])
	if err != nil {
		return v, false, fmt.Errorf("wardendb: read operation: %w", err)
	}
	return v, true, nil
}

func (j *Journal) byID(id string) (dbtx.Versioned[Operation], error) {
	v, ok, err := j.find("id", id)
	if err != nil {
		return v, err
	}
	if !ok {
		return v, fmt.Errorf("%w: %s", ErrOperationNotFound, id)
	}
	return v, nil
}

// Get returns the operation with the given id.
func (j *Journal) Get(id string) (Operation, error) {
	v, err := j.byID(id)
	return v.Value, err
}

// Lookup returns the operation that owns (kind, idempotencyKey).
func (j *Journal) Lookup(kind, idempotencyKey string) (Operation, error) {
	v, ok, err := j.find("idempotency_ref", idempotencyRef(kind, idempotencyKey))
	if err != nil {
		return Operation{}, err
	}
	if !ok {
		return Operation{}, fmt.Errorf("%w: kind %q key %q", ErrOperationNotFound, kind, idempotencyKey)
	}
	return v.Value, nil
}

// record appends a history entry to op and returns it.
func record(op *Operation, at time.Time, event string, from OperationState, owner, detail string) OperationEvent {
	op.AuditSeq++
	ev := OperationEvent{
		Seq: op.AuditSeq, AuditID: op.ID + "#" + strconv.FormatUint(op.AuditSeq, 10),
		At: at, Event: event, From: from, To: op.State, Attempt: op.Attempt, Owner: owner,
		Detail: boundedError(detail),
	}
	op.History = append(op.History, ev)
	if n := len(op.History); n > MaxOperationHistory {
		op.History = append([]OperationEvent(nil), op.History[n-MaxOperationHistory:]...)
	}
	return ev
}

// emit sends the committed history entry to the handle's sinks.
func (j *Journal) emit(action string, op Operation, ev OperationEvent) {
	m := j.db.mon
	if m.metrics != nil {
		m.metrics.Inc(action, OperationsCollection)
	}
	if m.auditor == nil {
		return
	}
	detail := map[string]string{
		"audit_id":     ev.AuditID,
		"event":        ev.Event,
		"kind":         op.Kind,
		"subject_type": op.SubjectType,
		"subject_id":   op.SubjectID,
		"to":           string(ev.To),
		"attempt":      strconv.Itoa(ev.Attempt),
	}
	if ev.From != "" {
		detail["from"] = string(ev.From)
	}
	if ev.Owner != "" {
		detail["lease_owner"] = ev.Owner
		detail["lease_epoch"] = strconv.FormatUint(op.LeaseEpoch, 10)
	}
	if ev.Detail != "" {
		detail["detail"] = ev.Detail
	}
	m.auditor.Log(audit.Event{Time: ev.At, Action: action, Actor: "wardendb", Target: op.ID, Detail: detail})
}

// Begin records the intent of spec as a new pending operation and returns it
// with created=true. If (Kind, IdempotencyKey) already names an operation,
// Begin writes nothing: it returns that operation with created=false when the
// intent is the same, and *IdempotencyConflictError when it differs.
func (j *Journal) Begin(ctx context.Context, spec OperationSpec, stages ...TxStage) (Operation, bool, error) {
	if err := spec.validate(); err != nil {
		return Operation{}, false, err
	}
	fp, err := spec.fingerprint()
	if err != nil {
		return Operation{}, false, err
	}
	ref := idempotencyRef(spec.Kind, spec.IdempotencyKey)

	j.mu.Lock()
	defer j.mu.Unlock()
	existing, found, err := j.find("idempotency_ref", ref)
	if err != nil {
		return Operation{}, false, err
	}
	if found {
		if existing.Value.IntentFingerprint != fp {
			return Operation{}, false, &IdempotencyConflictError{
				Kind: spec.Kind, IdempotencyKey: spec.IdempotencyKey,
				ExistingID: existing.Value.ID, Existing: existing.Value.IntentFingerprint, Got: fp,
			}
		}
		return existing.Value, false, nil
	}

	for _, dep := range spec.DependsOn {
		if _, ok, err := j.find("id", dep); err != nil {
			return Operation{}, false, err
		} else if !ok {
			return Operation{}, false, &DependencyError{DependsOn: dep}
		}
	}
	if spec.Supersedes != "" {
		prev, err := j.byID(spec.Supersedes)
		if err != nil {
			return Operation{}, false, err
		}
		if !prev.Value.State.Terminal() {
			return Operation{}, false, invalid("supersedes %s, which is %s and not terminal", spec.Supersedes, prev.Value.State)
		}
	}

	now := j.now()
	id, err := newOperationID(now)
	if err != nil {
		return Operation{}, false, fmt.Errorf("wardendb: mint operation id: %w", err)
	}
	op := Operation{
		ID: id, Kind: spec.Kind, SubjectType: spec.SubjectType, SubjectID: spec.SubjectID,
		Intent: spec.Intent, IdempotencyKey: spec.IdempotencyKey,
		DependsOn: append([]string(nil), spec.DependsOn...),
		CreatedAt: now, Compensation: spec.Compensation, Supersedes: spec.Supersedes,
		State: OperationPending, UpdatedAt: now,
		SchemaVersion: OperationSchemaVersion, IntentFingerprint: fp, IdempotencyRef: ref,
	}
	if !spec.DeadlineAt.IsZero() {
		op.DeadlineAt = spec.DeadlineAt.UTC().Round(0)
	}
	ev := record(&op, now, OperationEventBegin, "", "", "")
	if _, err := j.b.Run(ctx, "op.begin/"+ref[:32], func(ctx context.Context, tx *dbtx.Tx) error {
		if _, err := j.coll.Insert(tx, op); err != nil {
			return err
		}
		return runStages(ctx, tx, stages)
	}); err != nil {
		return Operation{}, false, err
	}
	j.emit(ActionOperationBegin, op, ev)
	// Return what was stored, so the value equals a later Get.
	stored, err := j.byID(id)
	if err != nil {
		return op, true, nil
	}
	return stored.Value, true, nil
}

func runStages(ctx context.Context, tx *dbtx.Tx, stages []TxStage) error {
	for _, s := range stages {
		if s == nil {
			continue
		}
		if err := s(ctx, tx); err != nil {
			return err
		}
	}
	return nil
}

// change is what one mutation did: the audit action and the history entry.
type change struct {
	action string
	ev     OperationEvent
}

// mutate is the single write path: it re-reads the operation, applies fn and
// commits the result conditioned on the revision it read, together with the
// caller's stages. fn returns the change to audit, or nil for a silent write.
func (j *Journal) mutate(ctx context.Context, id, verb string, stages []TxStage, fn func(op *Operation, now time.Time) (*change, error)) (Operation, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, err := j.usable(); err != nil {
		return Operation{}, err
	}
	var (
		out  Operation
		done *change
	)
	_, err := j.b.Run(ctx, id+"/"+verb, func(ctx context.Context, tx *dbtx.Tx) error {
		cur, err := j.byID(id)
		if err != nil {
			return err
		}
		op, now := cur.Value, j.now()
		c, err := fn(&op, now)
		if err != nil {
			return err
		}
		op.UpdatedAt = now
		if err := j.coll.Update(tx, cur.ID, op, cur.Rev); err != nil {
			return err
		}
		out, done = op, c
		return runStages(ctx, tx, stages)
	})
	if err != nil {
		var le *LeaseError
		if errors.As(err, &le) && (errors.Is(err, ErrLeaseFenced) || errors.Is(err, ErrLeaseExpired)) {
			j.fenced(le)
		}
		return Operation{}, err
	}
	if done != nil {
		j.emit(done.action, out, done.ev)
	}
	return out, nil
}

// Transition moves the operation to state `to`, recording u. The move must be
// an edge of the state machine (*TransitionError otherwise) and:
//
//   - running is entered only through Acquire;
//   - leaving running, reconciling or compensating requires the live lease in
//     u.Lease (*LeaseError), and entering a state that is not leased drops it;
//   - pending -> ready requires every DependsOn operation to have succeeded
//     (*DependencyError);
//   - retry_wait needs u.NextAttemptAt, and retry_wait -> ready is refused
//     before that time;
//   - a terminal state needs a result or an error, and stamps FinishedAt.
func (j *Journal) Transition(ctx context.Context, id string, to OperationState, u OperationUpdate, stages ...TxStage) (Operation, error) {
	if !to.Valid() {
		return Operation{}, invalid("unknown state %q", to)
	}
	for field, v := range map[string]map[string]any{"observation": u.Observation, "result": u.Result} {
		if err := bounded(field, v); err != nil {
			return Operation{}, err
		}
	}
	if err := cleanToken("external_ref", u.ExternalRef, false); err != nil {
		return Operation{}, err
	}
	return j.mutate(ctx, id, "transition", stages, func(op *Operation, now time.Time) (*change, error) {
		from := op.State
		refuse := func(reason string) error {
			return &TransitionError{ID: op.ID, From: from, To: to, Reason: reason}
		}
		switch {
		case !from.CanTransition(to):
			return nil, refuse("")
		case to == OperationRunning:
			return nil, refuse("running is entered by acquiring the lease")
		}
		owner := ""
		if from.Leased() {
			if u.Lease == nil {
				return nil, &LeaseError{OperationID: op.ID, Holder: op.LeaseOwner, HolderEpoch: op.LeaseEpoch, ExpiresAt: op.LeaseExpiresAt, cause: ErrLeaseRequired}
			}
			if err := checkLease(op, *u.Lease, now); err != nil {
				return nil, err
			}
			owner = op.LeaseOwner
		}
		switch to {
		case OperationReady:
			if from == OperationPending {
				for _, dep := range op.DependsOn {
					d, ok, err := j.find("id", dep)
					if err != nil {
						return nil, err
					}
					if !ok || d.Value.State != OperationSucceeded {
						return nil, &DependencyError{ID: op.ID, DependsOn: dep, State: d.Value.State}
					}
				}
			}
			if from == OperationRetryWait && now.Before(op.NextAttemptAt) {
				return nil, refuse("not due until " + op.NextAttemptAt.Format(time.RFC3339Nano))
			}
		case OperationRetryWait:
			if u.NextAttemptAt.IsZero() {
				return nil, invalid("retry_wait needs a next attempt time")
			}
		}
		if to.Terminal() && u.Error == "" && u.Result == nil && op.Result == nil {
			return nil, invalid("terminal state %s needs a result or an error", to)
		}

		op.State = to
		op.NextAttemptAt = time.Time{}
		if to == OperationRetryWait {
			op.NextAttemptAt = u.NextAttemptAt.UTC().Round(0)
		}
		if u.Error != "" {
			op.LastError = boundedError(u.Error)
		}
		if u.Observation != nil {
			op.Observation = u.Observation
			op.ObservedAt = now
			if !u.ObservedAt.IsZero() {
				op.ObservedAt = u.ObservedAt.UTC().Round(0)
			}
		}
		if u.ExternalRef != "" {
			op.ExternalRef = u.ExternalRef
		}
		if u.Result != nil {
			op.Result = u.Result
		}
		if !to.Leased() {
			op.LeaseOwner, op.LeaseExpiresAt = "", time.Time{}
		}
		if to.Terminal() {
			op.FinishedAt = now
		}
		detail := u.Reason
		if detail == "" {
			detail = u.Error
		}
		return &change{ActionOperationTransition, record(op, now, OperationEventTransition, from, owner, detail)}, nil
	})
}

// Progress records a step/checkpoint update of the attempt the lease holds.
// It does not change state and writes no history entry.
func (j *Journal) Progress(ctx context.Context, lease Lease, p OperationProgress, stages ...TxStage) (Operation, error) {
	for field, v := range map[string]map[string]any{"checkpoint": p.Checkpoint, "observation": p.Observation} {
		if err := bounded(field, v); err != nil {
			return Operation{}, err
		}
	}
	if err := cleanToken("step", p.Step, false); err != nil {
		return Operation{}, err
	}
	if err := cleanToken("external_ref", p.ExternalRef, false); err != nil {
		return Operation{}, err
	}
	return j.mutate(ctx, lease.OperationID, "progress", stages, func(op *Operation, now time.Time) (*change, error) {
		if err := checkLease(op, lease, now); err != nil {
			return nil, err
		}
		if p.Step != "" {
			op.Step = p.Step
		}
		if p.Checkpoint != nil {
			op.Checkpoint = p.Checkpoint
		}
		if p.Observation != nil {
			op.Observation = p.Observation
			op.ObservedAt = now
			if !p.ObservedAt.IsZero() {
				op.ObservedAt = p.ObservedAt.UTC().Round(0)
			}
		}
		if p.ExternalRef != "" {
			op.ExternalRef = p.ExternalRef
		}
		return nil, nil
	})
}

// list returns the operations in the given states that keep(op) accepts,
// ordered by id (creation time).
func (j *Journal) list(states []OperationState, keep func(Operation) bool) ([]Operation, error) {
	eng, err := j.usable()
	if err != nil {
		return nil, err
	}
	var out []Operation
	for _, s := range states {
		ids, ok := eng.IndexLookup("state", string(s))
		if !ok {
			return nil, errors.New(`wardendb: operations index "state" is missing`)
		}
		for _, id := range ids {
			v, err := j.coll.Get(id)
			if err != nil {
				return nil, fmt.Errorf("wardendb: read operation: %w", err)
			}
			if v.Value.State == s && (keep == nil || keep(v.Value)) {
				out = append(out, v.Value)
			}
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out, nil
}

func nonTerminalStates() []OperationState {
	var out []OperationState
	for _, s := range OperationStates() {
		if !s.Terminal() {
			out = append(out, s)
		}
	}
	return out
}

// ListByState returns the operations currently in any of the given states.
func (j *Journal) ListByState(states ...OperationState) ([]Operation, error) {
	return j.list(states, nil)
}

// Incomplete returns every non-terminal operation: the boot reconciler's scan
// set (spec §5).
func (j *Journal) Incomplete() ([]Operation, error) {
	return j.list(nonTerminalStates(), nil)
}

// LeaseExpired returns the operations in a leased state whose lease has
// expired or was released: nobody is working on them.
func (j *Journal) LeaseExpired() ([]Operation, error) {
	now := j.now()
	return j.list([]OperationState{OperationRunning, OperationReconciling, OperationCompensating},
		func(op Operation) bool { return !op.LeaseLive(now) })
}

// NeedsReconciliation returns the operations whose external outcome must be
// observed before anything is reissued: every reconciling row, and every
// running row without a live lease (Acquire moves the latter to reconciling).
func (j *Journal) NeedsReconciliation() ([]Operation, error) {
	now := j.now()
	return j.list([]OperationState{OperationRunning, OperationReconciling}, func(op Operation) bool {
		return op.State == OperationReconciling || !op.LeaseLive(now)
	})
}

// Due returns the operations an executor may act on now: ready rows, and
// retry_wait rows whose next attempt time has passed.
func (j *Journal) Due() ([]Operation, error) {
	now := j.now()
	return j.list([]OperationState{OperationReady, OperationRetryWait}, func(op Operation) bool {
		return op.State == OperationReady || !now.Before(op.NextAttemptAt)
	})
}

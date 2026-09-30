package planstore

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
)

// EventKind is the closed set of event types that may appear in the
// PlanExecution audit log. No other values are valid.
type EventKind string

const (
	EventKindExecutionStarted     EventKind = "execution_started"
	EventKindExecutorCreated      EventKind = "executor_created"
	EventKindAgentSpawned         EventKind = "agent_spawned"
	EventKindAgentFinished        EventKind = "agent_finished"
	EventKindTaskAssigned         EventKind = "task_assigned"
	EventKindTaskEvidenceVerified EventKind = "task_evidence_verified"
	EventKindCheckCompleted       EventKind = "check_completed"
	EventKindCommitCreated        EventKind = "commit_created"
	EventKindBranchPushed         EventKind = "branch_pushed"
	EventKindPROpened             EventKind = "pr_opened"
	EventKindPRMerged             EventKind = "pr_merged"
	EventKindBranchLanded         EventKind = "branch_landed"
	EventKindWorktreeRemoved      EventKind = "worktree_removed"
	EventKindExecutionPaused      EventKind = "execution_paused"
	EventKindExecutionFailed      EventKind = "execution_failed"
	EventKindExecutionStopped     EventKind = "execution_stopped"
	EventKindCompletionVerified   EventKind = "completion_verified"
)

// EventPayload carries structured fields for a PlanExecutionEvent.
// Only the fields relevant to the event Kind are populated.
type EventPayload struct {
	// Task/agent context.
	TaskID  string `json:"task_id,omitempty"`
	AgentID string `json:"agent_id,omitempty"`

	// Executor identity — populated on execution_started and executor_created.
	ExecutorID    string `json:"executor_id,omitempty"`
	ExecutionMode string `json:"execution_mode,omitempty"` // PlanExecutionMode value

	// Plan metadata — embedded in execution_started so the reducer can produce
	// an ExecutionSummary without a separate Plan lookup.
	PlanName   string `json:"plan_name,omitempty"`
	Goal       string `json:"goal,omitempty"`
	TasksTotal int    `json:"tasks_total,omitempty"`

	// Git / PR evidence.
	Branch    string `json:"branch,omitempty"`
	CommitSHA string `json:"commit_sha,omitempty"`
	PRURL     string `json:"pr_url,omitempty"`
	PRNumber  int    `json:"pr_number,omitempty"`

	// Check evidence.
	CheckName string `json:"check_name,omitempty"`

	// Terminal-state detail.
	FailureReason string `json:"failure_reason,omitempty"` // execution_failed
	StopReason    string `json:"stop_reason,omitempty"`    // execution_stopped
}

// PlanExecutionEvent is a timestamped, append-only audit event for a
// PlanExecution. Events are never edited or deleted. Kind is a closed set;
// see the EventKind constants.
//
// The event is keyed by its dedup identifier (DedupKey if set, else ID) in
// the "plan-events" ScrivaDB collection. Appending the same dedup key twice is
// silently idempotent.
type PlanExecutionEvent struct {
	ID          string        `json:"id"`                  // stable event ID
	DedupKey    string        `json:"dedup_key,omitempty"` // alternative stable dedup key
	PlanID      string        `json:"plan_id"`             // parent plan
	ExecutionID string        `json:"execution_id"`        // pe-<8hex>
	Kind        EventKind     `json:"kind"`
	Seq         int64         `json:"seq"`         // store-assigned monotonic counter
	OccurredAt  time.Time     `json:"occurred_at"` // caller-provided event time
	Payload     *EventPayload `json:"payload,omitempty"`
}

// ExecutionNote is an attributed prose note written by an agent. Notes are
// separate from typed PlanExecutionEvents and NEVER overwrite factual fields.
type ExecutionNote struct {
	ID          string    `json:"id"`
	PlanID      string    `json:"plan_id"`
	ExecutionID string    `json:"execution_id"`
	AgentID     string    `json:"agent_id"` // attribution — which agent wrote this
	Content     string    `json:"content"`  // free-form agent prose
	CreatedAt   time.Time `json:"created_at"`
}

// NewEventID returns a fresh ev-<16hex> event identifier.
func NewEventID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("planstore: crypto/rand unavailable: " + err.Error())
	}
	return fmt.Sprintf("ev-%x", b)
}

// NewNoteID returns a fresh note-<16hex> note identifier.
func NewNoteID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("planstore: crypto/rand unavailable: " + err.Error())
	}
	return fmt.Sprintf("note-%x", b)
}

// AppendEvent appends ev to the plan-events collection. It is idempotent:
// if the same event has already been stored (matched by DedupKey, falling
// back to ID), the call returns nil without creating a duplicate.
//
// The store assigns ev.Seq and fills ev.OccurredAt if zero. Callers should
// treat the event as modified in place after a successful call.
//
// Preconditions: ev.PlanID, ev.ExecutionID, and ev.Kind must be non-empty.
func (s *Store) AppendEvent(ctx context.Context, ev *PlanExecutionEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ev.PlanID == "" {
		return errors.New("planstore: event missing plan_id")
	}
	if ev.ExecutionID == "" {
		return errors.New("planstore: event missing execution_id")
	}
	if ev.Kind == "" {
		return errors.New("planstore: event missing kind")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Determine document key for dedup (DedupKey overrides ID).
	key := ev.DedupKey
	if key == "" {
		key = ev.ID
	}
	if key == "" {
		key = NewEventID()
		ev.ID = key
	} else if ev.ID == "" {
		ev.ID = key
	}

	// Assign store-controlled fields.
	s.seqCounter++
	ev.Seq = s.seqCounter
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = time.Now().UTC()
	}

	rec, err := encodeEvent(ev)
	if err != nil {
		return err
	}
	_, _, err = s.events.InsertWithKey(key, rec)
	if errors.Is(err, engine.ErrDuplicateKey) {
		return nil // idempotent: already present
	}
	return err
}

// ListEvents returns all events for the given plan execution, ordered by
// ascending Seq (with OccurredAt as the tiebreaker for equal Seq values).
// The listing is deterministic for any given set of stored events.
func (s *Store) ListEvents(ctx context.Context, planID, executionID string) ([]*PlanExecutionEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.events.Scan(query.MatchAll)
	if err != nil {
		return nil, err
	}

	var out []*PlanExecutionEvent
	for _, row := range rows {
		ev, err := decodeEvent(row.Data)
		if err != nil {
			return nil, fmt.Errorf("planstore: decode event: %w", err)
		}
		if ev.PlanID == planID && ev.ExecutionID == executionID {
			out = append(out, ev)
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Seq != out[j].Seq {
			return out[i].Seq < out[j].Seq
		}
		return out[i].OccurredAt.Before(out[j].OccurredAt)
	})
	return out, nil
}

// ListAllEvents returns every PlanExecutionEvent for planID across all
// executions, ordered by ascending Seq (OccurredAt tiebreaker). Used by Plan
// backup export so audit history survives machine transfer.
func (s *Store) ListAllEvents(ctx context.Context, planID string) ([]*PlanExecutionEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.events.Scan(query.MatchAll)
	if err != nil {
		return nil, err
	}
	var out []*PlanExecutionEvent
	for _, row := range rows {
		ev, err := decodeEvent(row.Data)
		if err != nil {
			return nil, fmt.Errorf("planstore: decode event: %w", err)
		}
		if ev.PlanID == planID {
			out = append(out, ev)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Seq != out[j].Seq {
			return out[i].Seq < out[j].Seq
		}
		return out[i].OccurredAt.Before(out[j].OccurredAt)
	})
	return out, nil
}

// RestoreEvent inserts an event from a backup bundle, preserving ID / DedupKey /
// Seq / OccurredAt. Idempotent: a duplicate key is a no-op. Advances the store
// sequence counter past the restored Seq so later AppendEvent calls stay monotonic.
func (s *Store) RestoreEvent(ctx context.Context, ev *PlanExecutionEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ev == nil || ev.PlanID == "" || ev.ExecutionID == "" || ev.Kind == "" {
		return errors.New("planstore: restore event missing required fields")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	key := ev.DedupKey
	if key == "" {
		key = ev.ID
	}
	if key == "" {
		return errors.New("planstore: restore event missing id")
	}
	if ev.ID == "" {
		ev.ID = key
	}
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = time.Now().UTC()
	}
	if ev.Seq > s.seqCounter {
		s.seqCounter = ev.Seq
	}
	rec, err := encodeEvent(ev)
	if err != nil {
		return err
	}
	_, _, err = s.events.InsertWithKey(key, rec)
	if errors.Is(err, engine.ErrDuplicateKey) {
		return nil
	}
	return err
}

// AppendNote stores an attributed agent prose note for the given execution.
// If note.ID is empty, a fresh ID is generated and set on the note.
// AppendNote is NOT idempotent — each call creates a new note record.
func (s *Store) AppendNote(ctx context.Context, note *ExecutionNote) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if note.PlanID == "" {
		return errors.New("planstore: note missing plan_id")
	}
	if note.ExecutionID == "" {
		return errors.New("planstore: note missing execution_id")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if note.ID == "" {
		note.ID = NewNoteID()
	}
	if note.CreatedAt.IsZero() {
		note.CreatedAt = time.Now().UTC()
	}

	rec, err := encodeNote(note)
	if err != nil {
		return err
	}
	_, _, err = s.notes.InsertWithKey(note.ID, rec)
	return err
}

// ListNotes returns all attributed notes for the given plan execution,
// ordered by ascending CreatedAt.
func (s *Store) ListNotes(ctx context.Context, planID, executionID string) ([]*ExecutionNote, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.notes.Scan(query.MatchAll)
	if err != nil {
		return nil, err
	}

	var out []*ExecutionNote
	for _, row := range rows {
		n, err := decodeNote(row.Data)
		if err != nil {
			return nil, fmt.Errorf("planstore: decode note: %w", err)
		}
		if n.PlanID == planID && n.ExecutionID == executionID {
			out = append(out, n)
		}
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// ListAllNotes returns every ExecutionNote for planID across all executions,
// ordered by ascending CreatedAt. Used by Plan backup export.
func (s *Store) ListAllNotes(ctx context.Context, planID string) ([]*ExecutionNote, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.notes.Scan(query.MatchAll)
	if err != nil {
		return nil, err
	}
	var out []*ExecutionNote
	for _, row := range rows {
		n, err := decodeNote(row.Data)
		if err != nil {
			return nil, fmt.Errorf("planstore: decode note: %w", err)
		}
		if n.PlanID == planID {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// RestoreNote inserts a note from a backup bundle by stable ID. Idempotent:
// duplicate key is a no-op (safe retry).
func (s *Store) RestoreNote(ctx context.Context, note *ExecutionNote) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if note == nil || note.PlanID == "" || note.ExecutionID == "" {
		return errors.New("planstore: restore note missing required fields")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if note.ID == "" {
		return errors.New("planstore: restore note missing id")
	}
	if note.CreatedAt.IsZero() {
		note.CreatedAt = time.Now().UTC()
	}
	rec, err := encodeNote(note)
	if err != nil {
		return err
	}
	_, _, err = s.notes.InsertWithKey(note.ID, rec)
	if errors.Is(err, engine.ErrDuplicateKey) {
		return nil
	}
	return err
}

// encodeEvent marshals ev through JSON into a map[string]any for ScrivaDB storage.
func encodeEvent(ev *PlanExecutionEvent) (map[string]any, error) {
	b, err := json.Marshal(ev)
	if err != nil {
		return nil, err
	}
	var rec map[string]any
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// decodeEvent unmarshals a ScrivaDB record into a PlanExecutionEvent.
func decodeEvent(rec map[string]any) (*PlanExecutionEvent, error) {
	b, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	var ev PlanExecutionEvent
	if err := json.Unmarshal(b, &ev); err != nil {
		return nil, err
	}
	return &ev, nil
}

// encodeNote marshals n through JSON into a map[string]any for ScrivaDB storage.
func encodeNote(n *ExecutionNote) (map[string]any, error) {
	b, err := json.Marshal(n)
	if err != nil {
		return nil, err
	}
	var rec map[string]any
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// decodeNote unmarshals a ScrivaDB record into an ExecutionNote.
func decodeNote(rec map[string]any) (*ExecutionNote, error) {
	b, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	var n ExecutionNote
	if err := json.Unmarshal(b, &n); err != nil {
		return nil, err
	}
	return &n, nil
}

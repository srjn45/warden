package planstore

import (
	"context"
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ─── helpers ──────────────────────────────────────────────────────────────────

func newTestEvent(planID, execID string, kind EventKind) *PlanExecutionEvent {
	return &PlanExecutionEvent{
		ID:          NewEventID(),
		PlanID:      planID,
		ExecutionID: execID,
		Kind:        kind,
		OccurredAt:  time.Now().UTC(),
	}
}

// ─── EventStore: idempotent append ────────────────────────────────────────────

// TestAppendEvent_idempotentByID verifies that appending the same event ID
// twice does not create a duplicate.
func TestAppendEvent_idempotentByID(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	ev := newTestEvent("plan-aabb", "pe-11223344", EventKindAgentSpawned)
	ev.Payload = &EventPayload{AgentID: "agent-001", TaskID: "task-alpha"}

	require.NoError(t, s.AppendEvent(ctx, ev))
	require.NoError(t, s.AppendEvent(ctx, ev)) // same ID → silently ignored

	got, err := s.ListEvents(ctx, "plan-aabb", "pe-11223344")
	require.NoError(t, err)
	require.Len(t, got, 1, "duplicate append must not double-insert")
}

// TestAppendEvent_idempotentByDedupKey verifies that DedupKey overrides ID
// for dedup purposes.
func TestAppendEvent_idempotentByDedupKey(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Two distinct event IDs but same DedupKey.
	ev1 := &PlanExecutionEvent{
		ID:          NewEventID(),
		DedupKey:    "dedup-unique-key-xyz",
		PlanID:      "plan-aabb",
		ExecutionID: "pe-11223344",
		Kind:        EventKindCommitCreated,
		OccurredAt:  time.Now().UTC(),
		Payload:     &EventPayload{CommitSHA: "abc123"},
	}
	ev2 := &PlanExecutionEvent{
		ID:          NewEventID(),           // different ID
		DedupKey:    "dedup-unique-key-xyz", // same dedup key
		PlanID:      "plan-aabb",
		ExecutionID: "pe-11223344",
		Kind:        EventKindCommitCreated,
		OccurredAt:  time.Now().UTC(),
		Payload:     &EventPayload{CommitSHA: "abc123"},
	}

	require.NoError(t, s.AppendEvent(ctx, ev1))
	require.NoError(t, s.AppendEvent(ctx, ev2)) // same DedupKey → silently ignored

	got, err := s.ListEvents(ctx, "plan-aabb", "pe-11223344")
	require.NoError(t, err)
	require.Len(t, got, 1, "same DedupKey must not double-insert")
}

// TestAppendEvent_preconditions verifies that missing required fields return
// an error rather than inserting a malformed event.
func TestAppendEvent_preconditions(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	cases := []struct {
		name string
		ev   *PlanExecutionEvent
	}{
		{"missing plan_id", &PlanExecutionEvent{ExecutionID: "pe-x", Kind: EventKindAgentSpawned}},
		{"missing execution_id", &PlanExecutionEvent{PlanID: "plan-x", Kind: EventKindAgentSpawned}},
		{"missing kind", &PlanExecutionEvent{PlanID: "plan-x", ExecutionID: "pe-x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Error(t, s.AppendEvent(ctx, tc.ev))
		})
	}
}

// ─── EventStore: ordered listing ──────────────────────────────────────────────

// TestListEvents_order verifies that events are returned ordered by Seq even
// when they are appended with the same OccurredAt timestamp.
func TestListEvents_order(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	planID, execID := "plan-order", "pe-order01"

	// Append five events at the same instant so OccurredAt cannot break ties.
	fixedTime := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	kinds := []EventKind{
		EventKindExecutionStarted,
		EventKindExecutorCreated,
		EventKindAgentSpawned,
		EventKindTaskAssigned,
		EventKindBranchPushed,
	}
	for _, k := range kinds {
		ev := &PlanExecutionEvent{
			PlanID:      planID,
			ExecutionID: execID,
			Kind:        k,
			OccurredAt:  fixedTime,
		}
		require.NoError(t, s.AppendEvent(ctx, ev))
	}

	got, err := s.ListEvents(ctx, planID, execID)
	require.NoError(t, err)
	require.Len(t, got, len(kinds))

	// Verify Seq is strictly ascending.
	for i := 1; i < len(got); i++ {
		require.Greater(t, got[i].Seq, got[i-1].Seq,
			"events must be ordered by ascending Seq")
	}
	// Verify kinds match insertion order.
	for i, ev := range got {
		require.Equal(t, kinds[i], ev.Kind)
	}
}

// TestListEvents_isolatesByExecution verifies that listing for one execution
// does not return events belonging to a different execution of the same plan.
func TestListEvents_isolatesByExecution(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	planID := "plan-iso"

	ev1 := newTestEvent(planID, "pe-exec-A", EventKindAgentSpawned)
	ev2 := newTestEvent(planID, "pe-exec-B", EventKindAgentSpawned)
	require.NoError(t, s.AppendEvent(ctx, ev1))
	require.NoError(t, s.AppendEvent(ctx, ev2))

	gotA, err := s.ListEvents(ctx, planID, "pe-exec-A")
	require.NoError(t, err)
	require.Len(t, gotA, 1)
	require.Equal(t, "pe-exec-A", gotA[0].ExecutionID)

	gotB, err := s.ListEvents(ctx, planID, "pe-exec-B")
	require.NoError(t, err)
	require.Len(t, gotB, 1)
	require.Equal(t, "pe-exec-B", gotB[0].ExecutionID)
}

// ─── EventStore: seq counter persists across Store restarts ───────────────────

// TestSeqCounter_persists verifies that seqCounter is initialized from
// existing events so a newly opened Store continues from the high-water mark
// rather than resetting to 0 and colliding with stored Seq values.
func TestSeqCounter_persists(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// First store: append three events.
	s1, err := New(dir)
	require.NoError(t, err)
	for range 3 {
		ev := newTestEvent("plan-persist", "pe-persist01", EventKindAgentSpawned)
		require.NoError(t, s1.AppendEvent(ctx, ev))
	}
	require.NoError(t, s1.Close())

	// Second store: seqCounter must start above 3.
	s2, err := New(dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s2.Close()) })

	ev := newTestEvent("plan-persist", "pe-persist01", EventKindAgentFinished)
	require.NoError(t, s2.AppendEvent(ctx, ev))

	all, err := s2.ListEvents(ctx, "plan-persist", "pe-persist01")
	require.NoError(t, err)
	require.Len(t, all, 4)
	// The last event must have a higher Seq than all previous ones.
	require.Greater(t, all[3].Seq, all[2].Seq)
}

// ─── Notes ────────────────────────────────────────────────────────────────────

// TestAppendNote_basic verifies that attributed notes are stored and listed
// separately from typed events.
func TestAppendNote_basic(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	planID, execID := "plan-notes", "pe-notes01"

	// Notes must not appear in event listing.
	note := &ExecutionNote{
		PlanID:      planID,
		ExecutionID: execID,
		AgentID:     "agent-writer",
		Content:     "All checks passed; branch looks good to merge.",
	}
	require.NoError(t, s.AppendNote(ctx, note))
	require.NotEmpty(t, note.ID, "AppendNote must assign a note ID")

	events, err := s.ListEvents(ctx, planID, execID)
	require.NoError(t, err)
	require.Empty(t, events, "notes must not appear in event listing")

	notes, err := s.ListNotes(ctx, planID, execID)
	require.NoError(t, err)
	require.Len(t, notes, 1)
	require.Equal(t, "agent-writer", notes[0].AgentID)
	require.Equal(t, note.Content, notes[0].Content)
}

// ─── Reducer: duplicate delivery ─────────────────────────────────────────────

// TestReduceEvents_duplicateDelivery verifies that delivering the same event
// twice (same ID) results in the same summary as delivering it once.
func TestReduceEvents_duplicateDelivery(t *testing.T) {
	now := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)

	evStart := &PlanExecutionEvent{
		ID:          "ev-start-001",
		PlanID:      "plan-dup",
		ExecutionID: "pe-dup01",
		Kind:        EventKindExecutionStarted,
		Seq:         1,
		OccurredAt:  now,
		Payload: &EventPayload{
			PlanName:      "dup-test",
			Goal:          "test dedup",
			ExecutionMode: string(PlanModeAutopilot),
			TasksTotal:    2,
		},
	}
	evTask := &PlanExecutionEvent{
		ID:          "ev-task-001",
		PlanID:      "plan-dup",
		ExecutionID: "pe-dup01",
		Kind:        EventKindTaskEvidenceVerified,
		Seq:         2,
		OccurredAt:  now.Add(time.Minute),
		Payload:     &EventPayload{TaskID: "task-1"},
	}

	// Deliver evTask twice (duplicate).
	events := []*PlanExecutionEvent{evStart, evTask, evTask}

	s := ReduceEvents(events)
	require.Equal(t, "plan-dup", s.PlanID)
	require.Equal(t, 1, s.TasksDone, "duplicate task_evidence_verified must not double-count")
	require.Equal(t, 2, s.TasksTotal)
	require.Equal(t, "dup-test", s.PlanName)
}

// ─── Reducer: out-of-order reads ──────────────────────────────────────────────

// TestReduceEvents_outOfOrder verifies that the reducer produces the same
// summary regardless of the order in which events are passed in.
func TestReduceEvents_outOfOrder(t *testing.T) {
	now := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)

	evStart := &PlanExecutionEvent{
		ID:          "ev-oo-start",
		PlanID:      "plan-oo",
		ExecutionID: "pe-oo01",
		Kind:        EventKindExecutionStarted,
		Seq:         1,
		OccurredAt:  now,
		Payload: &EventPayload{
			PlanName:      "out-of-order plan",
			ExecutionMode: string(PlanModePipeline),
			TasksTotal:    3,
		},
	}
	evExec := &PlanExecutionEvent{
		ID:          "ev-oo-exec",
		PlanID:      "plan-oo",
		ExecutionID: "pe-oo01",
		Kind:        EventKindExecutorCreated,
		Seq:         2,
		OccurredAt:  now.Add(time.Second),
		Payload:     &EventPayload{ExecutorID: "pipe-abc"},
	}
	evTask1 := &PlanExecutionEvent{
		ID:          "ev-oo-t1",
		PlanID:      "plan-oo",
		ExecutionID: "pe-oo01",
		Kind:        EventKindTaskEvidenceVerified,
		Seq:         3,
		OccurredAt:  now.Add(2 * time.Minute),
		Payload:     &EventPayload{TaskID: "t1"},
	}
	evTask2 := &PlanExecutionEvent{
		ID:          "ev-oo-t2",
		PlanID:      "plan-oo",
		ExecutionID: "pe-oo01",
		Kind:        EventKindTaskEvidenceVerified,
		Seq:         4,
		OccurredAt:  now.Add(3 * time.Minute),
		Payload:     &EventPayload{TaskID: "t2"},
	}
	evDone := &PlanExecutionEvent{
		ID:          "ev-oo-done",
		PlanID:      "plan-oo",
		ExecutionID: "pe-oo01",
		Kind:        EventKindCompletionVerified,
		Seq:         5,
		OccurredAt:  now.Add(4 * time.Minute),
	}

	canonical := []*PlanExecutionEvent{evStart, evExec, evTask1, evTask2, evDone}
	canonicalSummary := ReduceEvents(canonical)

	// Shuffle the same events and verify the summary is identical.
	shuffled := make([]*PlanExecutionEvent, len(canonical))
	copy(shuffled, canonical)
	rng := rand.New(rand.NewSource(42)) //nolint:gosec // test-only RNG
	rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

	shuffledSummary := ReduceEvents(shuffled)

	require.Equal(t, canonicalSummary.PlanID, shuffledSummary.PlanID)
	require.Equal(t, canonicalSummary.PlanName, shuffledSummary.PlanName)
	require.Equal(t, canonicalSummary.ExecutionMode, shuffledSummary.ExecutionMode)
	require.Equal(t, canonicalSummary.ExecutorID, shuffledSummary.ExecutorID)
	require.Equal(t, canonicalSummary.TasksTotal, shuffledSummary.TasksTotal)
	require.Equal(t, canonicalSummary.TasksDone, shuffledSummary.TasksDone)
	require.Equal(t, 2, shuffledSummary.TasksDone)
	require.NotNil(t, shuffledSummary.CompletedAt)
	require.Equal(t, canonicalSummary.CompletedAt.UTC(), shuffledSummary.CompletedAt.UTC())
}

// ─── Reducer: executor deleted after emitting events ─────────────────────────

// TestReduceEvents_executorDeleted verifies that even when the executor that
// emitted events no longer exists, the events remain in the store and the
// derived summary is still correct.
//
// This test simulates: executor emits events → executor is "deleted" (its Plan
// record / executor_id link is cleared) → events are listed and reduced →
// summary still reflects what actually happened.
func TestReduceEvents_executorDeleted(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	planID := "plan-survivor"
	execID := "pe-survivor01"
	executorID := "pipe-to-be-deleted"

	now := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)

	// Executor emits events.
	events := []*PlanExecutionEvent{
		{
			ID: "ev-sur-start", PlanID: planID, ExecutionID: execID,
			Kind: EventKindExecutionStarted, Seq: 0, OccurredAt: now,
			Payload: &EventPayload{
				PlanName:      "survivor-plan",
				Goal:          "outlast the executor",
				ExecutionMode: string(PlanModePipeline),
				TasksTotal:    2,
			},
		},
		{
			ID: "ev-sur-exec", PlanID: planID, ExecutionID: execID,
			Kind: EventKindExecutorCreated, Seq: 0, OccurredAt: now.Add(time.Second),
			Payload: &EventPayload{ExecutorID: executorID},
		},
		{
			ID: "ev-sur-t1", PlanID: planID, ExecutionID: execID,
			Kind: EventKindTaskEvidenceVerified, Seq: 0, OccurredAt: now.Add(time.Minute),
			Payload: &EventPayload{TaskID: "task-1"},
		},
		{
			ID: "ev-sur-t2", PlanID: planID, ExecutionID: execID,
			Kind: EventKindTaskEvidenceVerified, Seq: 0, OccurredAt: now.Add(2 * time.Minute),
			Payload: &EventPayload{TaskID: "task-2"},
		},
		{
			ID: "ev-sur-done", PlanID: planID, ExecutionID: execID,
			Kind: EventKindCompletionVerified, Seq: 0, OccurredAt: now.Add(3 * time.Minute),
		},
	}
	for _, ev := range events {
		require.NoError(t, s.AppendEvent(ctx, ev))
	}

	// Simulate executor deletion: create the Plan record, then delete it.
	plan := &Plan{
		ID:            planID,
		ProjectID:     "proj-sur",
		Name:          "survivor-plan",
		FilePath:      "plans/in_progress/survivor-plan.yaml",
		Status:        PlanStatusInProgress,
		ExecutionMode: PlanModePipeline,
		PipelineID:    executorID,
	}
	require.NoError(t, s.Create(ctx, plan))
	require.NoError(t, s.Delete(ctx, planID)) // executor "deleted"

	// Plan record is gone.
	_, err := s.Get(ctx, planID)
	require.ErrorIs(t, err, ErrNotFound, "plan record should be deleted")

	// But events survive — they are in a separate collection.
	listed, err := s.ListEvents(ctx, planID, execID)
	require.NoError(t, err)
	require.Len(t, listed, 5, "events must survive plan record deletion")

	// And the reducer still produces a correct summary.
	summary := ReduceEvents(listed)
	require.Equal(t, planID, summary.PlanID)
	require.Equal(t, "survivor-plan", summary.PlanName)
	require.Equal(t, executorID, summary.ExecutorID)
	require.Equal(t, 2, summary.TasksDone)
	require.Equal(t, 2, summary.TasksTotal)
	require.NotNil(t, summary.CompletedAt)
	require.Equal(t, "completed", summary.OutcomeNote)
}

// ─── Reducer: terminal state precedence ───────────────────────────────────────

// TestReduceEvents_failureTerminal verifies that execution_failed sets the
// terminal state and carries the failure reason.
func TestReduceEvents_failureTerminal(t *testing.T) {
	now := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)

	events := []*PlanExecutionEvent{
		{
			ID: "ev-f-start", PlanID: "plan-fail", ExecutionID: "pe-fail01",
			Kind: EventKindExecutionStarted, Seq: 1, OccurredAt: now,
			Payload: &EventPayload{PlanName: "fail-plan", TasksTotal: 1},
		},
		{
			ID: "ev-f-fail", PlanID: "plan-fail", ExecutionID: "pe-fail01",
			Kind: EventKindExecutionFailed, Seq: 2, OccurredAt: now.Add(5 * time.Minute),
			Payload: &EventPayload{FailureReason: "wd check failed: lint errors"},
		},
	}

	s := ReduceEvents(events)
	require.NotNil(t, s.CompletedAt)
	require.Equal(t, "wd check failed: lint errors", s.OutcomeNote)
	require.Equal(t, now.Add(5*time.Minute).UTC(), s.CompletedAt.UTC())
}

// TestReduceEvents_firstTerminalWins verifies that the first terminal event
// (by Seq) wins; subsequent terminal events of a different kind are ignored.
func TestReduceEvents_firstTerminalWins(t *testing.T) {
	now := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)

	events := []*PlanExecutionEvent{
		{
			ID: "ev-tw-start", PlanID: "plan-tw", ExecutionID: "pe-tw01",
			Kind: EventKindExecutionStarted, Seq: 1, OccurredAt: now,
			Payload: &EventPayload{PlanName: "terminal-wins"},
		},
		{
			ID: "ev-tw-fail", PlanID: "plan-tw", ExecutionID: "pe-tw01",
			Kind: EventKindExecutionFailed, Seq: 2, OccurredAt: now.Add(time.Minute),
			Payload: &EventPayload{FailureReason: "first failure"},
		},
		{
			ID: "ev-tw-stop", PlanID: "plan-tw", ExecutionID: "pe-tw01",
			Kind: EventKindExecutionStopped, Seq: 3, OccurredAt: now.Add(2 * time.Minute),
			Payload: &EventPayload{StopReason: "operator stopped"},
		},
	}

	s := ReduceEvents(events)
	require.Equal(t, "first failure", s.OutcomeNote, "first terminal event must win")
	require.Equal(t, now.Add(time.Minute).UTC(), s.CompletedAt.UTC())
}

// ─── NewEventID / NewNoteID ───────────────────────────────────────────────────

func TestNewEventID_format(t *testing.T) {
	id := NewEventID()
	require.True(t, len(id) > 3, "expected ev-<16hex>")
	require.Equal(t, "ev-", id[:3])
	require.NotEqual(t, id, NewEventID(), "IDs must not collide")
}

func TestNewNoteID_format(t *testing.T) {
	id := NewNoteID()
	require.True(t, len(id) > 5, "expected note-<16hex>")
	require.Equal(t, "note-", id[:5])
	require.NotEqual(t, id, NewNoteID(), "IDs must not collide")
}

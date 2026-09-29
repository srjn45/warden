package planstore

import (
	"sort"
)

// ReduceEvents derives an ExecutionSummary deterministically from a slice of
// PlanExecutionEvents. The function is pure (no I/O) and order-independent:
// events are sorted internally by Seq before processing, so the same multiset
// of events always produces the same summary regardless of the order in which
// they are passed in.
//
// The EventStore guarantees that duplicate events (same dedup key) are never
// stored, so callers do not need to pre-deduplicate before calling ReduceEvents.
// However, ReduceEvents is also safe to call on a slice that still contains
// duplicates — duplicate event IDs are silently collapsed.
//
// ExecutionSummary.PlanName and ExecutionSummary.Goal are populated only when
// an execution_started event carries them in its Payload. If no such event is
// present, those fields are empty; callers may overlay them from the Plan record
// if needed.
func ReduceEvents(events []*PlanExecutionEvent) ExecutionSummary {
	// Deduplicate by event ID (the store guarantees this for normal paths, but
	// the reducer is also testable in isolation with raw slices).
	deduped := dedupEvents(events)

	// Sort by Seq ascending; use OccurredAt as tiebreaker for equal Seq.
	sorted := make([]*PlanExecutionEvent, len(deduped))
	copy(sorted, deduped)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Seq != sorted[j].Seq {
			return sorted[i].Seq < sorted[j].Seq
		}
		return sorted[i].OccurredAt.Before(sorted[j].OccurredAt)
	})

	var (
		summary     ExecutionSummary
		tasksDone   = make(map[string]bool)
		terminalSet bool
	)

	for _, ev := range sorted {
		switch ev.Kind {

		case EventKindExecutionStarted:
			// First execution_started wins; subsequent ones (should not exist,
			// but handled defensively) are ignored.
			if summary.PlanID == "" {
				summary.PlanID = ev.PlanID
			}
			if summary.StartedAt.IsZero() {
				summary.StartedAt = ev.OccurredAt
			}
			if p := ev.Payload; p != nil {
				if summary.ExecutionMode == "" && p.ExecutionMode != "" {
					summary.ExecutionMode = PlanExecutionMode(p.ExecutionMode)
				}
				if summary.PlanName == "" && p.PlanName != "" {
					summary.PlanName = p.PlanName
				}
				if summary.Goal == "" && p.Goal != "" {
					summary.Goal = p.Goal
				}
				if summary.TasksTotal == 0 && p.TasksTotal > 0 {
					summary.TasksTotal = p.TasksTotal
				}
			}

		case EventKindExecutorCreated:
			if p := ev.Payload; p != nil && summary.ExecutorID == "" && p.ExecutorID != "" {
				summary.ExecutorID = p.ExecutorID
			}

		case EventKindTaskEvidenceVerified:
			// Each distinct task ID counts once toward TasksDone.
			if p := ev.Payload; p != nil && p.TaskID != "" {
				tasksDone[p.TaskID] = true
			}

		case EventKindExecutionFailed:
			if !terminalSet {
				terminalSet = true
				t := ev.OccurredAt
				summary.CompletedAt = &t
				if p := ev.Payload; p != nil && p.FailureReason != "" {
					summary.OutcomeNote = p.FailureReason
				} else {
					summary.OutcomeNote = "execution failed"
				}
			}

		case EventKindExecutionStopped:
			if !terminalSet {
				terminalSet = true
				t := ev.OccurredAt
				summary.CompletedAt = &t
				if p := ev.Payload; p != nil && p.StopReason != "" {
					summary.OutcomeNote = p.StopReason
				} else {
					summary.OutcomeNote = "execution stopped"
				}
			}

		case EventKindCompletionVerified:
			if !terminalSet {
				terminalSet = true
				t := ev.OccurredAt
				summary.CompletedAt = &t
				if summary.OutcomeNote == "" {
					summary.OutcomeNote = "completed"
				}
			}
		}
	}

	summary.TasksDone = len(tasksDone)
	return summary
}

// dedupEvents removes events with duplicate IDs, keeping the first occurrence
// per ID. Events with empty IDs are never collapsed.
func dedupEvents(events []*PlanExecutionEvent) []*PlanExecutionEvent {
	seen := make(map[string]bool, len(events))
	out := make([]*PlanExecutionEvent, 0, len(events))
	for _, ev := range events {
		if ev.ID == "" {
			out = append(out, ev)
			continue
		}
		if !seen[ev.ID] {
			seen[ev.ID] = true
			out = append(out, ev)
		}
	}
	return out
}

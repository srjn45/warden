package planstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrCleanupIncomplete is the sentinel wrapped by CleanupIncompleteError.
var ErrCleanupIncomplete = errors.New("plan executor cleanup incomplete")

// CleanupIncompleteError is returned by Finalize when the immutable summary was
// persisted but disposable-executor teardown did not fully succeed. The Plan
// remains in_progress with CleanupEvidence set so a later Finalize call can
// retry cleanup without re-reducing (or losing) the summary.
type CleanupIncompleteError struct {
	Evidence CleanupEvidence
	Summary  ExecutionSummary
}

func (e *CleanupIncompleteError) Error() string {
	if e == nil {
		return ErrCleanupIncomplete.Error()
	}
	parts := append([]string{}, e.Evidence.Errors...)
	parts = append(parts, e.Evidence.WorktreeErrors...)
	if len(e.Evidence.PendingIDs) > 0 {
		parts = append(parts, "pending: "+strings.Join(e.Evidence.PendingIDs, ", "))
	}
	if len(parts) == 0 {
		return ErrCleanupIncomplete.Error()
	}
	return "cleanup incomplete: " + strings.Join(parts, "; ")
}

func (e *CleanupIncompleteError) Unwrap() error { return ErrCleanupIncomplete }

// ExecutorCleanupFunc tears down disposable executors owned by a plan
// (Autopilot / Pipeline / plan-bound root Agent, workers, worktrees, branches).
// It must preserve PR references, PlanExecutionEvents, ExecutionSummary, and
// global audit entries. Return a Failed CleanupEvidence to keep the Plan
// in_progress; return a non-failed (possibly empty) evidence on success.
type ExecutorCleanupFunc func(ctx context.Context, p *Plan) CleanupEvidence

// FinalizeResult is the outcome of a successful Finalize (plan completed).
type FinalizeResult struct {
	Plan      *Plan
	Summary   ExecutionSummary
	Reconcile ReconcileReport
}

// Finalize is the daemon-owned Plan finalization workflow:
//
//  1. Reconcile Git/GitHub observed evidence
//  2. Seal ActiveExecution with completion_verified (idempotent)
//  3. Validate CompletionRequirements (structured unmet error on failure)
//  4. Reduce and persist the immutable ExecutionSummary (never overwrite)
//  5. Run cleanup (disposable executors) — on failure keep in_progress + evidence
//  6. Stamp Status=completed, archive ActiveExecution (no replica file moves)
//
// Retry-safe: a second call with an existing summary + cleanup evidence skips
// re-validation and retries cleanup, then commits.
func (s *PlanService) Finalize(ctx context.Context, planID string, cleanup ExecutorCleanupFunc) (*FinalizeResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p, err := s.store.Get(ctx, planID)
	if err != nil {
		return nil, err
	}
	if p.Status == PlanStatusCompleted {
		summary := ExecutionSummary{}
		if p.ExecutionSummary != nil {
			summary = *p.ExecutionSummary
		}
		return &FinalizeResult{Plan: p, Summary: summary}, nil
	}
	if p.Status != PlanStatusInProgress {
		return nil, &InvalidTransitionError{From: p.Status, To: PlanStatusCompleted}
	}

	var reconcile ReconcileReport
	retrying := p.ExecutionSummary != nil && p.CleanupEvidence != nil && p.CleanupEvidence.Failed()

	if !retrying {
		reconcile, err = s.ReconcileObservedEvidence(ctx, planID)
		if err != nil {
			return nil, fmt.Errorf("planstore: reconcile before finalize: %w", err)
		}
		if err := s.sealActiveExecution(ctx, planID); err != nil {
			return nil, fmt.Errorf("planstore: seal execution: %w", err)
		}
		p, err = s.store.Get(ctx, planID)
		if err != nil {
			return nil, err
		}
		if err := s.evaluateCompletion(ctx, p); err != nil {
			return nil, err
		}
		if _, err := s.persistExecutionSummary(ctx, planID); err != nil {
			return nil, err
		}
		p, err = s.store.Get(ctx, planID)
		if err != nil {
			return nil, err
		}
	}

	summary := *p.ExecutionSummary

	if cleanup != nil {
		ev := cleanup(ctx, p)
		if ev.AttemptedAt.IsZero() {
			ev.AttemptedAt = s.clock()
		}
		if ev.Failed() {
			_ = s.store.Update(ctx, planID, func(pl *Plan) error {
				cp := ev
				pl.CleanupEvidence = &cp
				return nil
			})
			return nil, &CleanupIncompleteError{Evidence: ev, Summary: summary}
		}
		_ = s.store.Update(ctx, planID, func(pl *Plan) error {
			pl.CleanupEvidence = nil
			return nil
		})
	}

	completed, err := s.commitFinalize(ctx, planID)
	if err != nil {
		return nil, err
	}
	return &FinalizeResult{Plan: completed, Summary: summary, Reconcile: reconcile}, nil
}

// sealActiveExecution appends completion_verified for the active execution when
// no terminal event exists yet. Idempotent. Operator/daemon finalization is the
// authoritative completion signal for every execution mode.
func (s *PlanService) sealActiveExecution(ctx context.Context, planID string) error {
	p, err := s.store.Get(ctx, planID)
	if err != nil {
		return err
	}
	if p.ActiveExecution == nil || p.ActiveExecution.ID == "" {
		return nil
	}
	events, err := s.store.ListEvents(ctx, p.ID, p.ActiveExecution.ID)
	if err != nil {
		return err
	}
	for _, ev := range events {
		switch ev.Kind {
		case EventKindCompletionVerified, EventKindExecutionFailed, EventKindExecutionStopped:
			return nil
		}
	}
	mode := p.ActiveExecution.ExecutionMode
	if mode == "" {
		mode = p.ExecutionMode
	}
	now := s.clock()
	if err := s.store.AppendEvent(ctx, &PlanExecutionEvent{
		DedupKey:    p.ID + ":" + p.ActiveExecution.ID + ":completion_verified",
		PlanID:      p.ID,
		ExecutionID: p.ActiveExecution.ID,
		Kind:        EventKindCompletionVerified,
		OccurredAt:  now,
		Payload: &EventPayload{
			ExecutorID:    p.ActiveExecution.ExecutorID,
			ExecutionMode: string(mode),
		},
	}); err != nil {
		return err
	}
	return s.store.Update(ctx, planID, func(pl *Plan) error {
		if pl.ActiveExecution == nil {
			return nil
		}
		pl.ActiveExecution.TerminalStatus = ExecutionStatusCompleted
		ts := now
		pl.ActiveExecution.CompletedAt = &ts
		return nil
	})
}

// persistExecutionSummary reduces ActiveExecution events into an immutable
// ExecutionSummary and stores it on the Plan. If a summary is already present
// it is left untouched (never lose a completed summary).
func (s *PlanService) persistExecutionSummary(ctx context.Context, planID string) (ExecutionSummary, error) {
	p, err := s.store.Get(ctx, planID)
	if err != nil {
		return ExecutionSummary{}, err
	}
	if p.ExecutionSummary != nil {
		return *p.ExecutionSummary, nil
	}

	var events []*PlanExecutionEvent
	if p.ActiveExecution != nil && p.ActiveExecution.ID != "" {
		events, err = s.store.ListEvents(ctx, p.ID, p.ActiveExecution.ID)
		if err != nil {
			return ExecutionSummary{}, fmt.Errorf("planstore: list events for summary: %w", err)
		}
	}
	summary := ReduceEvents(events)
	if summary.PlanID == "" {
		summary.PlanID = p.ID
	}
	if summary.PlanName == "" {
		summary.PlanName = p.Name
	}
	if summary.ExecutionMode == "" {
		summary.ExecutionMode = p.ExecutionMode
		if p.ActiveExecution != nil && p.ActiveExecution.ExecutionMode != "" {
			summary.ExecutionMode = p.ActiveExecution.ExecutionMode
		}
	}
	if summary.ExecutorID == "" && p.ActiveExecution != nil {
		summary.ExecutorID = p.ActiveExecution.ExecutorID
	}
	if summary.StartedAt.IsZero() {
		if p.ActiveExecution != nil && !p.ActiveExecution.StartedAt.IsZero() {
			summary.StartedAt = p.ActiveExecution.StartedAt
		} else if p.StartedAt != nil {
			summary.StartedAt = *p.StartedAt
		}
	}
	if summary.CompletedAt == nil {
		ts := s.clock()
		summary.CompletedAt = &ts
		if summary.OutcomeNote == "" {
			summary.OutcomeNote = "completed"
		}
	}
	if summary.TasksTotal == 0 {
		if p.ActiveExecution != nil && p.ActiveExecution.Snapshot != nil {
			summary.TasksTotal = len(p.ActiveExecution.Snapshot.Tasks)
		} else {
			summary.TasksTotal = len(p.Tasks)
		}
	}
	if summary.TasksDone == 0 && p.TaskProgress != nil {
		for _, st := range p.TaskProgress {
			if st == "done" || st == "skipped" {
				summary.TasksDone++
			}
		}
	}

	cp := summary
	if err := s.store.Update(ctx, planID, func(pl *Plan) error {
		if pl.ExecutionSummary != nil {
			cp = *pl.ExecutionSummary // concurrent writer won — keep theirs
			return nil
		}
		pl.ExecutionSummary = &cp
		return nil
	}); err != nil {
		return ExecutionSummary{}, err
	}
	return cp, nil
}

// commitFinalize archives ActiveExecution into ExecutionHistory, clears
// disposable executor link fields, and stamps completed via UpdateIf.
// Repository replicas are not moved (canonical Status is authority).
func (s *PlanService) commitFinalize(ctx context.Context, planID string) (*Plan, error) {
	p, err := s.store.Get(ctx, planID)
	if err != nil {
		return nil, err
	}
	if p.Status == PlanStatusCompleted {
		return p, nil
	}
	if p.Status != PlanStatusInProgress {
		return nil, &InvalidTransitionError{From: p.Status, To: PlanStatusCompleted}
	}
	if p.ExecutionSummary == nil {
		return nil, errors.New("planstore: cannot commit finalize without execution summary")
	}

	now := s.clock()
	if err := s.store.UpdateIf(ctx, planID, p.Revision, func(pl *Plan) error {
		if pl.Status == PlanStatusCompleted {
			return nil
		}
		if pl.Status != PlanStatusInProgress {
			return &InvalidTransitionError{From: pl.Status, To: PlanStatusCompleted}
		}
		if pl.ExecutionSummary == nil {
			return errors.New("planstore: cannot commit finalize without execution summary")
		}
		pl.Status = PlanStatusCompleted
		if pl.CompletedAt == nil {
			ts := now
			pl.CompletedAt = &ts
		}
		pl.CleanupEvidence = nil
		if pl.ActiveExecution != nil {
			archived := *pl.ActiveExecution
			if archived.TerminalStatus == "" || archived.TerminalStatus == ExecutionStatusRunning {
				archived.TerminalStatus = ExecutionStatusCompleted
			}
			if archived.CompletedAt == nil {
				ts := now
				archived.CompletedAt = &ts
			}
			pl.ExecutionHistory = append(pl.ExecutionHistory, archived)
			pl.ActiveExecution = nil
		}
		// Clear disposable executor back-refs; Plan + summary + events remain.
		pl.AutopilotRunID = ""
		pl.PipelineID = ""
		pl.OrchestratorID = ""
		return nil
	}); err != nil {
		return nil, err
	}
	return s.store.Get(ctx, planID)
}

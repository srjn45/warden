package daemon

import (
	"context"
	"log/slog"
	"time"

	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/planstore"
)

// runPlanCompletionWatcher periodically advances in_progress plans whose
// execution entity has finished and reaps Autopilot executors orphaned by
// already-completed plans. Runs as a background goroutine started from
// ListenAndServe; exits when ctx is done.
//
// Watched: autopilot (StateComplete / sealed ActiveExecution) and pipeline
// (StatusDone). Not watched: orchestrator_worker and manual — the user calls
// wd plan status.
func (s *Server) runPlanCompletionWatcher(ctx context.Context, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.tickPlanCompletion(ctx)
		}
	}
}

func (s *Server) tickPlanCompletion(ctx context.Context) {
	if s.plans == nil {
		return
	}
	all, err := s.plans.List(ctx)
	if err != nil {
		slog.Warn("plan completion watcher: list plans failed", "err", err)
		return
	}
	completed := make(map[string]*planstore.Plan)
	for _, p := range all {
		if p.Status == planstore.PlanStatusCompleted {
			completed[p.ID] = p
			continue
		}
		if p.Status != planstore.PlanStatusInProgress {
			continue
		}
		switch {
		case p.AutopilotRunID != "" && s.autopilot != nil:
			s.maybeCompleteAutopilotPlan(ctx, p)
		case p.PipelineID != "" && s.exec != nil:
			s.maybeCompletePipelinePlan(ctx, p)
		}
	}
	s.reapCompletedPlanAutopilots(ctx, completed)
}

// reapCompletedPlanAutopilots repairs executor rows left behind by older
// completion paths or interrupted cleanup. A completed Plan is the durable
// audit record; its plan-bound Autopilot executors are disposable. Look in both
// stores because a live row can outlast its legacy RunStatus (and vice versa).
func (s *Server) reapCompletedPlanAutopilots(ctx context.Context, completed map[string]*planstore.Plan) {
	if s.autopilot == nil || len(completed) == 0 {
		return
	}
	seen := make(map[string]bool)
	reap := func(id, planID string) {
		p := completed[planID]
		if p == nil || id == "" || seen[id] {
			return
		}
		seen[id] = true
		if err := s.teardownPlanAutopilot(ctx, id, p.ProjectID); err != nil {
			slog.Warn("plan completion watcher: reap autopilot failed", "plan", planID, "autopilot", id, "err", err)
		}
	}
	live, err := s.autopilot.LiveAutopilots(ctx)
	if err != nil {
		slog.Warn("plan completion watcher: list live autopilots failed", "err", err)
	} else {
		for _, a := range live {
			if a != nil {
				reap(a.ID, a.PlanID)
			}
		}
	}
	for _, run := range s.autopilot.Status().Runs {
		reap(run.RunID, run.PlanID)
	}
}

func (s *Server) maybeCompleteAutopilotPlan(ctx context.Context, p *planstore.Plan) {
	if p == nil || s.autopilot == nil {
		return
	}
	st := s.autopilot.Status()
	sealed := p.ActiveExecution != nil &&
		p.ActiveExecution.TerminalStatus == planstore.ExecutionStatusCompleted

	var completeIDs []string
	currentComplete := false
	seen := make(map[string]bool)
	for _, rs := range st.Runs {
		matches := rs.PlanID == p.ID || (p.AutopilotRunID != "" && rs.RunID == p.AutopilotRunID)
		if !matches || rs.State != autopilot.StateComplete || rs.RunID == "" || seen[rs.RunID] {
			continue
		}
		seen[rs.RunID] = true
		completeIDs = append(completeIDs, rs.RunID)
		if rs.RunID == p.AutopilotRunID {
			currentComplete = true
		}
	}

	// Drive Finalize when the current executor finished or an earlier Finalize
	// attempt already sealed ActiveExecution (e.g. blocked on cleanup gates).
	if currentComplete || sealed {
		s.advancePlanToCompleted(ctx, p)
	}

	// Autopilot executors are disposable. A StateComplete run must leave the
	// live UI even when Finalize is still blocked (open PRs, cleanup retry).
	// Includes orphaned complete runs superseded by a newer AutopilotRunID.
	for _, id := range completeIDs {
		if err := s.teardownPlanAutopilot(ctx, id, p.ProjectID); err != nil {
			slog.Warn("plan completion watcher: teardown complete autopilot failed",
				"plan", p.ID, "autopilot", id, "err", err)
		}
	}
}

func (s *Server) maybeCompletePipelinePlan(ctx context.Context, p *planstore.Plan) {
	pl, err := s.exec.pstore.Get(p.PipelineID)
	if err != nil {
		return
	}
	if pl.Status == pipeline.StatusDone {
		s.advancePlanToCompleted(ctx, p)
	}
}

// advancePlanToCompleted runs the shared FinalizePlan workflow (summary →
// cleanup → completed). Failures are logged; the watcher retries next tick.
func (s *Server) advancePlanToCompleted(ctx context.Context, p *planstore.Plan) {
	if p == nil {
		return
	}
	if _, err := s.FinalizePlan(ctx, p.ID); err != nil {
		slog.Warn("plan completion watcher: finalize failed", "plan", p.ID, "err", err)
	}
}

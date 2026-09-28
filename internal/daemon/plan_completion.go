package daemon

import (
	"context"
	"log/slog"
	"time"

	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/planstore"
)

// runPlanCompletionWatcher periodically checks in_progress plans and advances
// any whose execution entity has finished to plans/completed/. Runs as a
// background goroutine started from ListenAndServe; exits when ctx is done.
//
// Watched: autopilot (StateComplete) and pipeline (StatusDone).
// Not watched: orchestrator_worker and manual — the user calls wd plan status.
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
	for _, p := range all {
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
}

func (s *Server) maybeCompleteAutopilotPlan(ctx context.Context, p *planstore.Plan) {
	st := s.autopilot.Status()
	for _, rs := range st.Runs {
		if rs.RunID != p.AutopilotRunID {
			continue
		}
		if rs.State == autopilot.StateComplete {
			s.advancePlanToCompleted(ctx, p)
		}
		return
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

// advancePlanToCompleted git-mv's the plan YAML to plans/completed/, commits,
// and marks the plan completed in the DB.
func (s *Server) advancePlanToCompleted(ctx context.Context, p *planstore.Plan) {
	root := s.resolvePlanRoot(p.ProjectID)
	if root == "" {
		slog.Warn("plan completion watcher: cannot resolve project root", "plan", p.ID, "project", p.ProjectID)
		return
	}
	newPath, err := gitMvPlanStatus(ctx, root, p.FilePath, planstore.PlanStatusCompleted)
	if err != nil {
		slog.Warn("plan completion watcher: git mv to completed failed", "plan", p.ID, "err", err)
		return
	}
	now := time.Now().UTC()
	if updateErr := s.plans.Update(ctx, p.ID, func(pl *planstore.Plan) error {
		pl.FilePath = newPath
		pl.Status = planstore.PlanStatusCompleted
		if pl.CompletedAt == nil {
			pl.CompletedAt = &now
		}
		return nil
	}); updateErr != nil {
		slog.Warn("plan completion watcher: DB update failed", "plan", p.ID, "err", updateErr)
	}
}

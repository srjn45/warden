package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/planstore"
)

// seedPlanOutcome writes integration_branch / default_branch onto the plan when
// an autopilot run starts (plan-finish-flow §6). Idempotent.
func (s *Server) seedPlanOutcome(ctx context.Context, planID, integration, defaultBranch string) {
	if s.plans == nil || planID == "" || integration == "" {
		return
	}
	_ = s.plans.Update(ctx, planID, func(pl *planstore.Plan) error {
		if pl.Outcome == nil {
			pl.Outcome = &planstore.PlanOutcome{}
		}
		if pl.Outcome.IntegrationBranch == "" {
			pl.Outcome.IntegrationBranch = integration
		}
		if defaultBranch != "" && pl.Outcome.DefaultBranch == "" {
			pl.Outcome.DefaultBranch = defaultBranch
		}
		return nil
	})
}

// mirrorPlanFinalPR copies the run's final PR onto the plan outcome while the
// plan is still in_progress (plan-finish-flow §6).
func (s *Server) mirrorPlanFinalPR(ctx context.Context, p *planstore.Plan, rs autopilot.RunStatus) {
	if s.plans == nil || p == nil || rs.FinalPR == nil || rs.FinalPR.Number <= 0 {
		return
	}
	fp := rs.FinalPR
	_ = s.plans.Update(ctx, p.ID, func(pl *planstore.Plan) error {
		if pl.Outcome == nil {
			pl.Outcome = &planstore.PlanOutcome{}
		}
		if pl.Outcome.IntegrationBranch == "" && rs.IntegrationBranch != "" {
			pl.Outcome.IntegrationBranch = rs.IntegrationBranch
		}
		pl.Outcome.FinalPR = &planstore.PlanOutcomeFinalPR{
			Number:   fp.Number,
			URL:      fp.URL,
			State:    firstNonEmpty(fp.State, "open"),
			HeadSHA:  fp.HeadSHA,
			MergedAt: fp.MergedAt,
		}
		return nil
	})
}

// deleteIntegrationBranchAfterMerge deletes the plan's integration branch
// (local + origin) once the final PR is confirmed merged (plan-finish-flow §4).
// Failure never blocks or reverts completion; it is recorded and retried.
func (s *Server) deleteIntegrationBranchAfterMerge(ctx context.Context, p *planstore.Plan) {
	if s.plans == nil || p == nil || p.Outcome == nil {
		return
	}
	o := p.Outcome
	switch o.BranchFate {
	case planstore.BranchFateDeleted, planstore.BranchFateKeptUnmerged, planstore.BranchFateAbandoned:
		return
	case planstore.BranchFateDeleteFailed:
		if o.BranchDeleteAttempts >= planstore.MaxBranchDeleteAttempts {
			return
		}
	}
	branch := strings.TrimSpace(o.IntegrationBranch)
	if branch == "" {
		return
	}
	def := strings.TrimSpace(o.DefaultBranch)
	if def == "" {
		def = "main"
	}
	repo := strings.TrimSpace(p.ProjectID)
	if repo == "" {
		return
	}

	// Safety: only delete when the merged final PR's head equals the tip (or the
	// tip is already reachable from the default branch). Commits pushed after
	// the merge → keep.
	if keep, why := s.shouldKeepIntegrationBranch(ctx, repo, branch, def, o.FinalPR); keep {
		_ = s.plans.Update(ctx, p.ID, func(pl *planstore.Plan) error {
			ensureOutcome(pl)
			pl.Outcome.BranchFate = planstore.BranchFateKeptUnmerged
			pl.Outcome.BranchDeleteError = why
			return nil
		})
		return
	}

	delErr := s.doDeleteIntegrationBranch(ctx, repo, branch)
	now := time.Now().UTC()
	_ = s.plans.Update(ctx, p.ID, func(pl *planstore.Plan) error {
		ensureOutcome(pl)
		pl.Outcome.BranchDeleteAttempts++
		if delErr != nil {
			pl.Outcome.BranchFate = planstore.BranchFateDeleteFailed
			pl.Outcome.BranchDeleteError = delErr.Error()
			return nil
		}
		pl.Outcome.BranchFate = planstore.BranchFateDeleted
		pl.Outcome.BranchDeletedAt = &now
		pl.Outcome.BranchDeleteError = ""
		return nil
	})
	if delErr != nil {
		slog.Warn("plan completion: integration branch delete failed",
			"plan", p.ID, "branch", branch, "err", delErr)
	}
}

func ensureOutcome(pl *planstore.Plan) {
	if pl.Outcome == nil {
		pl.Outcome = &planstore.PlanOutcome{}
	}
}

// shouldKeepIntegrationBranch reports whether the tip has unique commits that
// the merged final PR did not cover (commits pushed after the merge).
func (s *Server) shouldKeepIntegrationBranch(ctx context.Context, repo, branch, def string, fp *planstore.PlanOutcomeFinalPR) (bool, string) {
	_, _ = gitIn(ctx, repo, "fetch", "origin")
	tip, err := gitIn(ctx, repo, "rev-parse", "--verify", "refs/heads/"+branch)
	if err != nil {
		tip, err = gitIn(ctx, repo, "rev-parse", "--verify", "origin/"+branch)
		if err != nil {
			return false, "" // already gone — treat as deletable (no-op)
		}
	}
	tip = strings.TrimSpace(tip)

	ahead := 0
	if out, err := gitIn(ctx, repo, "rev-list", "--count", "origin/"+def+".."+tip); err == nil {
		ahead, _ = strconv.Atoi(strings.TrimSpace(out))
	}
	if ahead == 0 {
		return false, "" // tip is on/behind the default branch
	}
	if fp != nil && fp.HeadSHA != "" && tip == fp.HeadSHA {
		return false, "" // tip is exactly the merged PR head
	}
	short := tip
	if len(short) > 7 {
		short = short[:7]
	}
	return true, fmt.Sprintf("tip %s has %d commit(s) not covered by the merged PR", short, ahead)
}

func (s *Server) doDeleteIntegrationBranch(ctx context.Context, repo, branch string) error {
	var errs []string
	if out, err := gitIn(ctx, repo, "branch", "-D", branch); err != nil {
		if !strings.Contains(out, "not found") {
			errs = append(errs, fmt.Sprintf("local: %v (%s)", err, strings.TrimSpace(out)))
		}
	}
	if out, err := gitIn(ctx, repo, "push", "--no-verify", "origin", "--delete", branch); err != nil {
		msg := strings.ToLower(out + " " + err.Error())
		missing := strings.Contains(msg, "remote ref does not exist") ||
			strings.Contains(msg, "not found") ||
			strings.Contains(msg, "does not appear to be a git repository") ||
			strings.Contains(msg, "no such remote")
		if !missing {
			errs = append(errs, fmt.Sprintf("origin: %v (%s)", err, strings.TrimSpace(out)))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(errs, "; "))
}

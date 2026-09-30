package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/handoff"
	"github.com/srjn45/warden/internal/planstore"
)

// recordPlanBoundAgentFinished appends agent_finished for a plan-bound agent that
// reached a terminal lifecycle state. Best-effort / idempotent.
func (s *Server) recordPlanBoundAgentFinished(sess *agentstore.Agent, reason string) {
	if sess == nil {
		return
	}
	payload := &planstore.EventPayload{AgentID: sess.ID}
	if reason != "" {
		payload.StopReason = reason
	}
	s.recordPlanBoundAgentEvent(sess, planstore.EventKindAgentFinished, payload, sess.ID)
}

// recordPlanBoundWorktreeRemoved appends worktree_removed (covers branch deletion
// when the lifecycle path also drops the branch). Best-effort / idempotent.
func (s *Server) recordPlanBoundWorktreeRemoved(sess *agentstore.Agent) {
	if sess == nil {
		return
	}
	branch := strings.TrimSpace(sess.Branch)
	suffix := sess.ID
	if branch != "" {
		suffix = branch
	}
	s.recordPlanBoundAgentEvent(sess, planstore.EventKindWorktreeRemoved, &planstore.EventPayload{
		AgentID: sess.ID,
		Branch:  branch,
	}, suffix)
}

// trackPlanBranch records branch on Plan.Branches / ActiveExecution.PlanBranches
// so completion reconciliation has a stable reference set. Idempotent.
func (s *Server) trackPlanBranch(sess *agentstore.Agent, branch string) {
	branch = strings.TrimSpace(branch)
	if s.plans == nil || sess == nil || strings.TrimSpace(sess.PlanID) == "" || branch == "" {
		return
	}
	ctx := context.Background()
	_ = s.plans.Update(ctx, sess.PlanID, func(p *planstore.Plan) error {
		p.Branches = appendUniqueString(p.Branches, branch)
		if p.ActiveExecution != nil {
			p.ActiveExecution.PlanBranches = appendUniqueString(p.ActiveExecution.PlanBranches, branch)
		}
		return nil
	})
}

func appendUniqueString(list []string, v string) []string {
	for _, e := range list {
		if e == v {
			return list
		}
	}
	return append(list, v)
}

// recordPlanBoundHandoffNote stores agent handoff prose as an attributed
// ExecutionNote — never as a PlanExecutionEvent or ExecutionSummary mutation.
func (s *Server) recordPlanBoundHandoffNote(sess *agentstore.Agent, h handoff.Handoff, handoffPath string) {
	if s.plans == nil || sess == nil || strings.TrimSpace(sess.PlanID) == "" {
		return
	}
	ctx := context.Background()
	p, err := s.plans.Get(ctx, sess.PlanID)
	if err != nil || p == nil || p.ActiveExecution == nil || p.ActiveExecution.ID == "" {
		return
	}
	content := formatHandoffNote(h, handoffPath)
	if strings.TrimSpace(content) == "" {
		return
	}
	note := &planstore.ExecutionNote{
		PlanID:      p.ID,
		ExecutionID: p.ActiveExecution.ID,
		AgentID:     sess.ID,
		Content:     content,
		CreatedAt:   time.Now().UTC(),
	}
	if err := s.plans.AppendNote(ctx, note); err != nil {
		slog.Debug("plan handoff note append failed", "plan", p.ID, "agent", sess.ID, "err", err)
	}
}

func formatHandoffNote(h handoff.Handoff, path string) string {
	var b strings.Builder
	b.WriteString("agent handoff")
	if h.Reason != "" {
		fmt.Fprintf(&b, " (reason=%s)", h.Reason)
	}
	if h.Backend != "" || h.SuccessorBackend != "" {
		fmt.Fprintf(&b, ": %s → %s", h.Backend, h.SuccessorBackend)
	}
	b.WriteByte('\n')
	if path != "" {
		fmt.Fprintf(&b, "path: %s\n", path)
	}
	if g := strings.TrimSpace(h.Goal); g != "" {
		fmt.Fprintf(&b, "goal: %s\n", truncateNote(g, 400))
	}
	if n := strings.TrimSpace(h.NextStep); n != "" {
		fmt.Fprintf(&b, "next: %s\n", truncateNote(n, 400))
	}
	return strings.TrimSpace(b.String())
}

func truncateNote(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// recordPlanBoundLandEvents appends pr_merged + branch_landed (and optionally
// worktree_removed when the land deleted the branch) for a plan-bound worker.
func (s *Server) recordPlanBoundLandEvents(sess *agentstore.Agent, branch string, pr int, sha string, deleteBranch bool) {
	if sess == nil {
		return
	}
	prURL := ""
	if pr > 0 {
		prURL = fmt.Sprintf("pr/%d", pr)
	}
	suffix := branch
	if prURL != "" {
		suffix = prURL
	}
	s.recordPlanBoundAgentEvent(sess, planstore.EventKindPRMerged, &planstore.EventPayload{
		AgentID:   sess.ID,
		Branch:    branch,
		PRNumber:  pr,
		PRURL:     prURL,
		CommitSHA: sha,
	}, suffix)
	landSuffix := branch
	if sha != "" {
		landSuffix = branch + ":" + sha
	}
	s.recordPlanBoundAgentEvent(sess, planstore.EventKindBranchLanded, &planstore.EventPayload{
		AgentID:   sess.ID,
		Branch:    branch,
		PRNumber:  pr,
		PRURL:     prURL,
		CommitSHA: sha,
	}, landSuffix)
	if deleteBranch {
		s.recordPlanBoundAgentEvent(sess, planstore.EventKindWorktreeRemoved, &planstore.EventPayload{
			AgentID: sess.ID,
			Branch:  branch,
		}, branch)
	}
}

// reconcilePlanEvidenceBeforeComplete runs the Git/GitHub reconciliation pass
// that repairs missing observed events before CompletionRequirements are
// evaluated. Best-effort: failures are logged and do not block the transition
// (evaluateCompletion still enforces the gates).
func (s *Server) reconcilePlanEvidenceBeforeComplete(ctx context.Context, planID string) {
	svc := s.planSvc()
	if svc == nil || strings.TrimSpace(planID) == "" {
		return
	}
	rep, err := svc.ReconcileObservedEvidence(ctx, planID)
	if err != nil {
		slog.Warn("plan evidence reconcile failed", "plan", planID, "err", err)
		return
	}
	if rep.Repaired > 0 || len(rep.Errors) > 0 {
		slog.Info("plan evidence reconcile",
			"plan", planID,
			"repaired", rep.Repaired,
			"branches", len(rep.Branches),
			"errors", len(rep.Errors),
		)
	}
}

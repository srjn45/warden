package daemon

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/planstore"
)

// planLeftover describes a completed autopilot plan whose integration branch
// still has commits the default branch lacks (plan-finish-flow §9).
type planLeftover struct {
	branch  string
	def     string
	commits int
}

// integrationLeftover computes the §9 read-time leftover for a completed
// autopilot plan with no recorded branch fate. Local git only; no network.
// Returns nil when there is nothing to report or git cannot say.
func (s *Server) integrationLeftover(ctx context.Context, p *planstore.Plan) *planLeftover {
	if p == nil || p.Status != planstore.PlanStatusCompleted {
		return nil
	}
	if p.Outcome != nil && p.Outcome.BranchFate != "" {
		return nil // fate is recorded; nothing to compute
	}
	autopilotPlan := p.ExecutionMode == planstore.PlanModeAutopilot || p.AutopilotRunID != "" ||
		(p.Outcome != nil && p.Outcome.IntegrationBranch != "")
	if !autopilotPlan {
		return nil
	}
	repo := strings.TrimSpace(p.ProjectID)
	if repo == "" {
		return nil
	}
	branch, def := "", "main"
	if p.Outcome != nil {
		branch = strings.TrimSpace(p.Outcome.IntegrationBranch)
		if d := strings.TrimSpace(p.Outcome.DefaultBranch); d != "" {
			def = d
		}
	}
	if branch == "" {
		b, err := autopilot.ResolveInitIntegrationBranch(p.Name, "")
		if err != nil {
			return nil
		}
		branch = b
	}
	tip, err := gitIn(ctx, repo, "rev-parse", "--verify", "refs/heads/"+branch)
	if err != nil {
		tip, err = gitIn(ctx, repo, "rev-parse", "--verify", "refs/remotes/origin/"+branch)
		if err != nil {
			return nil
		}
	}
	tip = strings.TrimSpace(tip)
	out, err := gitIn(ctx, repo, "rev-list", "--count", "refs/remotes/origin/"+def+".."+tip)
	if err != nil {
		return nil
	}
	n, _ := strconv.Atoi(strings.TrimSpace(out))
	if n <= 0 {
		return nil
	}
	return &planLeftover{branch: branch, def: def, commits: n}
}

// leftoverMergedPR reports whether a merged PR branch→def has head == tip, i.e.
// the branch was squash-merged and is not actually leftover. One gh lookup,
// used only by `plan show` (§9).
func (s *Server) leftoverMergedPR(ctx context.Context, repo string, lo *planLeftover) bool {
	tip, err := gitIn(ctx, repo, "rev-parse", "--verify", "refs/heads/"+lo.branch)
	if err != nil {
		tip, err = gitIn(ctx, repo, "rev-parse", "--verify", "refs/remotes/origin/"+lo.branch)
		if err != nil {
			return false
		}
	}
	tip = strings.TrimSpace(tip)
	host := daemonLandHost{s: s, dir: repo}
	out, err := host.runGH(ctx, "pr", "list", "--head", lo.branch, "--base", lo.def,
		"--state", "merged", "--json", "headRefOid", "--limit", "5")
	if err != nil {
		return false
	}
	var prs []struct {
		HeadRefOid string `json:"headRefOid"`
	}
	if json.Unmarshal([]byte(out), &prs) != nil {
		return false
	}
	for _, pr := range prs {
		if pr.HeadRefOid == tip {
			return true
		}
	}
	return false
}

// leftoverOutcome returns a copy of o (or a fresh outcome) carrying the
// computed integration branch and branch_fate=leftover_unmerged. The stored
// record is never modified.
func leftoverOutcome(o *planstore.PlanOutcome, lo *planLeftover) *planstore.PlanOutcome {
	var out planstore.PlanOutcome
	if o != nil {
		out = *o
	}
	out.IntegrationBranch = lo.branch
	out.DefaultBranch = lo.def
	out.BranchFate = planstore.BranchFateLeftoverUnmerged
	return &out
}

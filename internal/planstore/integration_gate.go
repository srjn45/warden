package planstore

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrIntegrationUnmerged is returned when completing a plan whose integration
// branch still has commits not on the default branch and no matching merged PR
// (plan-finish-flow §5).
var ErrIntegrationUnmerged = errors.New("integration branch has unmerged commits")

// IntegrationUnmergedError names the branch, commit count, default branch and
// optional open final PR so the operator can merge or pass --abandon-unmerged.
type IntegrationUnmergedError struct {
	Branch        string
	DefaultBranch string
	CommitCount   int
	FinalPRNumber int // open PR number when known; 0 when none
	HasOpenPR     bool
}

func (e *IntegrationUnmergedError) Error() string {
	if e == nil {
		return ErrIntegrationUnmerged.Error()
	}
	pr := "no final PR"
	if e.HasOpenPR && e.FinalPRNumber > 0 {
		pr = fmt.Sprintf("final PR #%d is open", e.FinalPRNumber)
	} else if e.FinalPRNumber > 0 {
		pr = fmt.Sprintf("final PR #%d", e.FinalPRNumber)
	}
	return fmt.Sprintf(
		"integration branch %s has %d commits that are not on %s (%s). Merge the PR, or pass --abandon-unmerged to complete the plan and keep the branch.",
		e.Branch, e.CommitCount, e.DefaultBranch, pr,
	)
}

func (e *IntegrationUnmergedError) Unwrap() error { return ErrIntegrationUnmerged }

// FinalizeOptions controls optional Finalize behaviour (plan-finish-flow §5).
type FinalizeOptions struct {
	// AbandonUnmerged completes the plan even when the integration branch has
	// commits not on the default branch; the branch is kept and recorded as
	// abandoned. An open final PR is left open.
	AbandonUnmerged bool
}

// checkIntegrationMerged refuses Finalize when the plan's integration branch
// still has unique commits and no merged PR covers the tip (plan-finish-flow §5).
// No-op when the plan has no integration branch recorded.
func (s *PlanService) checkIntegrationMerged(ctx context.Context, p *Plan) error {
	if p == nil || p.Outcome == nil {
		return nil
	}
	branch := strings.TrimSpace(p.Outcome.IntegrationBranch)
	if branch == "" {
		return nil
	}
	def := strings.TrimSpace(p.Outcome.DefaultBranch)
	if def == "" {
		def = "main"
	}
	root, err := s.root(p.ProjectID)
	if err != nil {
		return nil // cannot probe — do not block automatic paths on a missing root
	}
	_, _ = s.run(ctx, root, "git", "fetch", "origin")

	tipRef := "refs/heads/" + branch
	tip, err := s.run(ctx, root, "git", "rev-parse", "--verify", tipRef)
	if err != nil {
		tip, err = s.run(ctx, root, "git", "rev-parse", "--verify", "origin/"+branch)
		if err != nil {
			return nil // branch gone — nothing to gate
		}
	}
	tip = strings.TrimSpace(tip)

	countOut, err := s.run(ctx, root, "git", "rev-list", "--count", "origin/"+def+".."+tip)
	if err != nil {
		return nil
	}
	n, _ := strconv.Atoi(strings.TrimSpace(countOut))
	if n == 0 {
		return nil
	}

	// A merged PR whose head is the tip passes (squash merges included).
	if s.integrationTipMerged(ctx, root, branch, def, tip) {
		return nil
	}

	prNum, open := 0, false
	if p.Outcome.FinalPR != nil && p.Outcome.FinalPR.Number > 0 {
		prNum = p.Outcome.FinalPR.Number
		open = strings.EqualFold(p.Outcome.FinalPR.State, "open")
	} else if out, err := s.run(ctx, root, "gh", "pr", "list", "--head", branch, "--base", def,
		"--state", "open", "--json", "number", "--limit", "1"); err == nil {
		prNum = firstPRNumber(out)
		open = prNum > 0
	}
	return &IntegrationUnmergedError{
		Branch: branch, DefaultBranch: def, CommitCount: n,
		FinalPRNumber: prNum, HasOpenPR: open,
	}
}

// integrationTipMerged reports whether a merged PR branch→def has head == tip.
func (s *PlanService) integrationTipMerged(ctx context.Context, root, branch, def, tip string) bool {
	out, err := s.run(ctx, root, "gh", "pr", "list", "--head", branch, "--base", def,
		"--state", "merged", "--json", "number,headRefOid", "--limit", "5")
	if err != nil {
		return false
	}
	return strings.Contains(out, tip) || strings.Contains(out, `"headRefOid":"`+tip+`"`)
}

func firstPRNumber(jsonOut string) int {
	// Tiny extract: look for "number": N without pulling in a second decoder path.
	const key = `"number":`
	i := strings.Index(jsonOut, key)
	if i < 0 {
		return 0
	}
	rest := strings.TrimSpace(jsonOut[i+len(key):])
	n := 0
	for _, c := range rest {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func ensurePlanOutcome(pl *Plan) {
	if pl.Outcome == nil {
		pl.Outcome = &PlanOutcome{}
	}
}

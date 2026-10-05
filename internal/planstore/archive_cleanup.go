package planstore

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// KeptBranch is a plan branch archive left in place because it still carries
// commits that are not on the default branch.
type KeptBranch struct {
	Branch  string `json:"branch"`
	Commits int    `json:"commits"`
}

// ArchiveCleanupReport says what archiving tore down and what it kept
// (plan-finish-flow §7).
type ArchiveCleanupReport struct {
	RemovedBranches []string     `json:"removed_branches,omitempty"`
	KeptBranches    []KeptBranch `json:"kept_branches,omitempty"`
}

// DefaultBranch resolves the repository default branch for a plan: the
// recorded outcome value, else origin/HEAD, else "main".
func (s *PlanService) DefaultBranch(ctx context.Context, p *Plan, root string) string {
	if p != nil && p.Outcome != nil {
		if d := strings.TrimSpace(p.Outcome.DefaultBranch); d != "" {
			return d
		}
	}
	if out, err := s.run(ctx, root, "git", "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if d := strings.TrimPrefix(strings.TrimSpace(out), "origin/"); d != "" {
			return d
		}
	}
	return "main"
}

// UnmergedCommits returns how many commits branch has that are not on the
// default branch; 0 when the branch is gone, empty, or a merged PR covers its
// tip (squash merges included). Errors probing err on the side of keeping:
// an unreadable count is reported as 1.
func (s *PlanService) UnmergedCommits(ctx context.Context, root, def, branch string) int {
	tipOut, err := s.run(ctx, root, "git", "rev-parse", "--verify", "refs/heads/"+branch)
	if err != nil {
		tipOut, err = s.run(ctx, root, "git", "rev-parse", "--verify", "origin/"+branch)
		if err != nil {
			return 0 // branch gone
		}
	}
	tip := strings.TrimSpace(tipOut)
	out, err := s.run(ctx, root, "git", "rev-list", "--count", "origin/"+def+".."+tip)
	if err != nil {
		return 1
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 1
	}
	if n == 0 {
		return 0
	}
	if s.integrationTipMerged(ctx, root, branch, def, tip) {
		return 0
	}
	return n
}

// CleanupWorktreesKeepUnmerged is CleanupWorktrees for archive: every plan
// branch (and the integration branch) loses its worktree, but the local and
// remote branch are deleted only when UnmergedCommits is 0.
func (s *PlanService) CleanupWorktreesKeepUnmerged(ctx context.Context, plan *Plan) (ArchiveCleanupReport, error) {
	var rep ArchiveCleanupReport
	if plan == nil {
		return rep, nil
	}
	branches := append([]string(nil), plan.Branches...)
	if plan.Outcome != nil && strings.TrimSpace(plan.Outcome.IntegrationBranch) != "" {
		branches = append(branches, plan.Outcome.IntegrationBranch)
	}
	if len(branches) == 0 {
		return rep, nil
	}
	root, err := s.root(plan.ProjectID)
	if err != nil {
		return rep, err
	}
	def := s.DefaultBranch(ctx, plan, root)
	_, _ = s.run(ctx, root, "git", "fetch", "origin")
	porcelain, _ := s.run(ctx, root, "git", "worktree", "list", "--porcelain")
	worktrees := parseWorktreeList(porcelain)

	var errs []error
	seen := map[string]bool{}
	for _, branch := range branches {
		branch = strings.TrimSpace(branch)
		if branch == "" || seen[branch] {
			continue
		}
		seen[branch] = true
		for _, wt := range worktrees {
			if wt.branch != branch || wt.path == "" || samePath(wt.path, root) {
				continue
			}
			if out, err := s.run(ctx, root, "git", "worktree", "remove", wt.path); err != nil {
				errs = append(errs, fmt.Errorf("git worktree remove %s: %w (%s)", wt.path, err, strings.TrimSpace(out)))
			}
		}
		if n := s.UnmergedCommits(ctx, root, def, branch); n > 0 {
			rep.KeptBranches = append(rep.KeptBranches, KeptBranch{Branch: branch, Commits: n})
			continue
		}
		if out, err := s.run(ctx, root, "git", "branch", "-D", branch); err != nil {
			if !strings.Contains(out, "not found") {
				errs = append(errs, fmt.Errorf("git branch -D %s: %w (%s)", branch, err, strings.TrimSpace(out)))
				continue
			}
		}
		if out, err := s.run(ctx, root, "git", "push", "--no-verify", "origin", "--delete", branch); err != nil {
			if !strings.Contains(out, "remote ref does not exist") && !strings.Contains(out, "not found") {
				errs = append(errs, fmt.Errorf("git push --no-verify origin --delete %s: %w (%s)", branch, err, strings.TrimSpace(out)))
				continue
			}
		}
		rep.RemovedBranches = append(rep.RemovedBranches, branch)
	}
	return rep, errors.Join(errs...)
}

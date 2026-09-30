package planstore

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// ReconcileReport summarizes one Git/GitHub evidence repair pass.
type ReconcileReport struct {
	Repaired int      `json:"repaired"`
	Branches []string `json:"branches,omitempty"`
	Errors   []string `json:"errors,omitempty"`
}

// ghPRJSON is the subset of `gh pr list/view --json` used by reconciliation.
type ghPRJSON struct {
	Number      int    `json:"number"`
	URL         string `json:"url"`
	State       string `json:"state"` // OPEN | MERGED | CLOSED (gh) or open/merged/closed
	HeadRefName string `json:"headRefName,omitempty"`
	MergeCommit struct {
		Oid string `json:"oid"`
	} `json:"mergeCommit"`
}

// ReconcileObservedEvidence queries Git and GitHub for every branch/PR
// referenced by the plan's ActiveExecution (events + Plan.Branches) and
// appends missing observed PlanExecutionEvents when facts can be proved.
//
// Repairs:
//   - pr_merged / branch_landed when a referenced branch's PR is MERGED
//   - worktree_removed when a pushed branch has no local worktree and no local branch
//
// Idempotent via DedupKey. Best-effort: individual query failures are recorded
// on the report and do not abort the whole pass. Returns a zero report when the
// plan has no ActiveExecution.
//
// Agents never call this — it is daemon-owned completion plumbing.
func (s *PlanService) ReconcileObservedEvidence(ctx context.Context, planID string) (ReconcileReport, error) {
	var rep ReconcileReport
	if err := ctx.Err(); err != nil {
		return rep, err
	}
	p, err := s.store.Get(ctx, planID)
	if err != nil {
		return rep, err
	}
	if p.ActiveExecution == nil || p.ActiveExecution.ID == "" {
		return rep, nil
	}
	root, err := s.root(p.ProjectID)
	if err != nil {
		return rep, err
	}

	events, err := s.store.ListEvents(ctx, p.ID, p.ActiveExecution.ID)
	if err != nil {
		return rep, fmt.Errorf("planstore: list events for reconcile: %w", err)
	}
	have := map[string]bool{}
	for _, ev := range events {
		if ev.DedupKey != "" {
			have[ev.DedupKey] = true
		}
		if ev.ID != "" {
			have[ev.ID] = true
		}
	}

	branches := collectReferencedBranches(p, events)
	rep.Branches = branches
	if len(branches) == 0 {
		return rep, nil
	}

	now := s.clock()
	for _, branch := range branches {
		n, errs := s.reconcileBranch(ctx, p, root, branch, now, have)
		rep.Repaired += n
		rep.Errors = append(rep.Errors, errs...)
	}
	return rep, nil
}

func collectReferencedBranches(p *Plan, events []*PlanExecutionEvent) []string {
	seen := map[string]bool{}
	var out []string
	add := func(b string) {
		b = strings.TrimSpace(b)
		if b == "" || seen[b] {
			return
		}
		seen[b] = true
		out = append(out, b)
	}
	for _, b := range p.Branches {
		add(b)
	}
	if p.ActiveExecution != nil {
		for _, b := range p.ActiveExecution.PlanBranches {
			add(b)
		}
	}
	for _, ev := range events {
		if ev.Payload == nil {
			continue
		}
		add(ev.Payload.Branch)
	}
	return out
}

func (s *PlanService) reconcileBranch(ctx context.Context, p *Plan, root, branch string, now time.Time, have map[string]bool) (int, []string) {
	var (
		repaired int
		errs     []string
	)
	execID := p.ActiveExecution.ID

	prs, err := s.listPRsForBranch(ctx, root, branch)
	if err != nil {
		errs = append(errs, fmt.Sprintf("gh pr list %s: %v", branch, err))
	} else {
		for _, pr := range prs {
			if !isMergedPRState(pr.State) {
				continue
			}
			url := pr.URL
			if url == "" && pr.Number > 0 {
				url = fmt.Sprintf("pr/%d", pr.Number)
			}
			sha := pr.MergeCommit.Oid
			if appendIfNew(ctx, s.store, have, &PlanExecutionEvent{
				DedupKey:    fmt.Sprintf("%s:%s:pr_merged:%s", p.ID, execID, url),
				PlanID:      p.ID,
				ExecutionID: execID,
				Kind:        EventKindPRMerged,
				OccurredAt:  now,
				Payload: &EventPayload{
					Branch:     branch,
					PRURL:      url,
					PRNumber:   pr.Number,
					CommitSHA:  sha,
					ExecutorID: p.ActiveExecution.ExecutorID,
				},
			}) {
				repaired++
			}
			landKey := branch
			if sha != "" {
				landKey = branch + ":" + sha
			}
			if appendIfNew(ctx, s.store, have, &PlanExecutionEvent{
				DedupKey:    fmt.Sprintf("%s:%s:branch_landed:%s", p.ID, execID, landKey),
				PlanID:      p.ID,
				ExecutionID: execID,
				Kind:        EventKindBranchLanded,
				OccurredAt:  now,
				Payload: &EventPayload{
					Branch:     branch,
					PRURL:      url,
					PRNumber:   pr.Number,
					CommitSHA:  sha,
					ExecutorID: p.ActiveExecution.ExecutorID,
				},
			}) {
				repaired++
			}
		}
	}

	gone, gerr := s.branchResourcesGone(ctx, root, branch)
	if gerr != nil {
		errs = append(errs, fmt.Sprintf("git inspect %s: %v", branch, gerr))
		return repaired, errs
	}
	if gone {
		if appendIfNew(ctx, s.store, have, &PlanExecutionEvent{
			DedupKey:    fmt.Sprintf("%s:%s:worktree_removed:%s", p.ID, execID, branch),
			PlanID:      p.ID,
			ExecutionID: execID,
			Kind:        EventKindWorktreeRemoved,
			OccurredAt:  now,
			Payload: &EventPayload{
				Branch:     branch,
				ExecutorID: p.ActiveExecution.ExecutorID,
			},
		}) {
			repaired++
		}
	}
	return repaired, errs
}

func appendIfNew(ctx context.Context, store *Store, have map[string]bool, ev *PlanExecutionEvent) bool {
	if have[ev.DedupKey] {
		return false
	}
	if err := store.AppendEvent(ctx, ev); err != nil {
		slog.Debug("planstore: reconcile append failed", "kind", ev.Kind, "err", err)
		return false
	}
	have[ev.DedupKey] = true
	return true
}

func (s *PlanService) listPRsForBranch(ctx context.Context, root, branch string) ([]ghPRJSON, error) {
	out, err := s.run(ctx, root, "gh", "pr", "list",
		"--head", branch,
		"--state", "all",
		"--json", "number,url,state,headRefName,mergeCommit")
	if err != nil {
		return nil, err
	}
	out = strings.TrimSpace(out)
	if out == "" || out == "[]" {
		return nil, nil
	}
	var prs []ghPRJSON
	if err := json.Unmarshal([]byte(out), &prs); err != nil {
		return nil, fmt.Errorf("decode gh pr list: %w", err)
	}
	return prs, nil
}

// branchResourcesGone reports whether the branch has neither a local worktree
// checkout nor a local branch ref — evidence that disposable resources were cleaned.
func (s *PlanService) branchResourcesGone(ctx context.Context, root, branch string) (bool, error) {
	porcelain, err := s.run(ctx, root, "git", "worktree", "list", "--porcelain")
	if err != nil {
		return false, err
	}
	for _, wt := range parseWorktreeList(porcelain) {
		if wt.branch == branch {
			return false, nil
		}
	}
	// git show-ref --verify returns non-zero when the ref is absent.
	_, err = s.run(ctx, root, "git", "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	if err != nil {
		return true, nil // local branch gone and no worktree → cleaned
	}
	return false, nil
}

func isMergedPRState(state string) bool {
	return strings.EqualFold(strings.TrimSpace(state), "MERGED") ||
		strings.EqualFold(strings.TrimSpace(state), "merged")
}

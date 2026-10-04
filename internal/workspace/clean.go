package workspace

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// ApplyOptions configures Apply.
type ApplyOptions struct {
	DryRun    bool
	Force     bool // allow removing Dirty worktrees
	LocalOnly bool // skip remote deletes
}

// ApplyResult lists what was (or, for DryRun, would be) removed.
type ApplyResult struct {
	WorktreesRemoved []string
	LocalsDeleted    []string
	RemotesDeleted   []string
	Skipped          []Skip
	Errors           []string // per-item failures; the rest still run
}

// Apply executes the cleanup described by report: stale worktrees first, then
// merged local branches, then merged origin branches. Protection is re-checked
// at apply time. A per-item failure is recorded in Errors and does not abort.
func Apply(ctx context.Context, o Options, a ApplyOptions, report *Report) (*ApplyResult, error) {
	if o.Repo == "" {
		return nil, fmt.Errorf("workspace: Repo is required")
	}
	if report == nil {
		return nil, fmt.Errorf("workspace: nil report")
	}
	if o.Runner == nil {
		o.Runner = ExecRunner{}
	}
	d := &detector{o: o, rep: &Report{}}
	base := report.Base
	if base == "" {
		var err error
		if base, err = d.resolveBase(ctx); err != nil {
			return nil, err
		}
	}
	wts, err := d.listWorktrees(ctx)
	if err != nil {
		return nil, err
	}
	primary := ""
	if len(wts) > 0 {
		primary = wts[0].branch
	}
	res := &ApplyResult{}
	skip := func(name, reason string) { res.Skipped = append(res.Skipped, Skip{name, reason}) }
	fail := func(format string, args ...any) { res.Errors = append(res.Errors, fmt.Sprintf(format, args...)) }
	keepBranch := map[string]bool{} // branches whose worktree could not be removed

	for _, w := range report.StaleWorktrees {
		if w.Branch == "" || isProtected(w.Branch, base, primary) {
			skip(w.Path, "protected")
			keepBranch[w.Branch] = true
			continue
		}
		if len(wts) > 0 && filepath.Clean(w.Path) == wts[0].path {
			skip(w.Path, "primary worktree")
			keepBranch[w.Branch] = true
			continue
		}
		if w.Dirty && !a.Force {
			skip(w.Path, "dirty (use --force)")
			keepBranch[w.Branch] = true
			continue
		}
		if !a.DryRun {
			args := []string{"worktree", "remove"}
			if w.Dirty {
				args = append(args, "--force")
			}
			if _, err := d.git(ctx, "", append(args, w.Path)...); err != nil {
				fail("remove worktree %s: %v", w.Path, err)
				keepBranch[w.Branch] = true
				continue
			}
		}
		res.WorktreesRemoved = append(res.WorktreesRemoved, w.Path)
	}

	for _, b := range report.LocalBranches {
		if isProtected(b.Name, base, primary) {
			skip(b.Name, "protected")
			continue
		}
		if keepBranch[b.Name] {
			skip(b.Name, "checked out in a worktree that was kept")
			continue
		}
		flag := "-d"
		if b.Kind == KindSquashMerged {
			if !a.DryRun && !d.squashVerified(ctx, b, base) {
				skip(b.Name, "squash-merge not verified")
				continue
			}
			flag = "-D"
		}
		if !a.DryRun {
			if _, err := d.git(ctx, "", "branch", flag, b.Name); err != nil {
				fail("delete branch %s: %v", b.Name, err)
				continue
			}
		}
		res.LocalsDeleted = append(res.LocalsDeleted, b.Name)
	}

	if a.LocalOnly || o.LocalOnly {
		return res, nil
	}
	for _, b := range report.RemoteBranches {
		if isProtected(b.Name, base, primary) {
			skip("origin/"+b.Name, "protected")
			continue
		}
		if !a.DryRun {
			if _, err := d.git(ctx, "", "push", "origin", "--delete", b.Name); err != nil {
				fail("delete remote branch %s: %v", b.Name, err)
				continue
			}
		}
		res.RemotesDeleted = append(res.RemotesDeleted, b.Name)
	}
	return res, nil
}

// squashVerified re-proves a squash-merge before a force delete: a gh-reported
// merged PR, or the git cherry / trial-merge checks against the local branch.
func (d *detector) squashVerified(ctx context.Context, b Branch, base string) bool {
	if strings.HasPrefix(b.Reason, "merged PR") {
		return true
	}
	return d.cherryMerged(ctx, base, b.Name) || d.treeMerged(ctx, base, b.Name)
}

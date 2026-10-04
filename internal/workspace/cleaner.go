// Package workspace detects merged branches and stale worktrees so they can be
// cleaned up. This file is detection only: nothing here deletes, prunes or
// otherwise mutates a repository (other than the optional `git fetch --prune`).
package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Branch kinds reported in Branch.Kind / Worktree.Kind.
const (
	KindMerged       = "merged"        // ancestor of base (git branch --merged)
	KindSquashMerged = "squash-merged" // content landed on base without the commits
)

// Runner executes an external command in dir and returns stdout. Injectable so
// tests can fake gh (or git).
type Runner interface {
	Run(ctx context.Context, dir, name string, args ...string) (string, error)
}

// ExecRunner is the real Runner. Stderr is included in returned errors.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// Options configures Detect.
type Options struct {
	Repo      string // repo root (main worktree)
	Base      string // empty → resolve default branch (main or master)
	Fetch     bool   // run `git fetch --prune origin` first; failure is an error
	LocalOnly bool   // skip remote-branch detection (locals + worktrees still detected)
	Runner    Runner // nil → ExecRunner
}

// Branch is a deletion candidate. For remote branches Name has the origin/ prefix stripped.
type Branch struct {
	Name   string
	Kind   string
	Reason string
}

// Worktree is a linked worktree whose checked-out branch is merged.
type Worktree struct {
	Path   string
	Branch string
	Dirty  bool // git status --porcelain non-empty; deletion needs --force (Task2)
	Kind   string
	Reason string
}

// Skip records something deliberately not offered for cleanup.
type Skip struct {
	Name   string // branch name or worktree path
	Reason string // "protected", "detached HEAD", "gh unavailable", ...
}

// Report is the detection result.
type Report struct {
	Base           string
	LocalBranches  []Branch
	RemoteBranches []Branch
	StaleWorktrees []Worktree
	Skipped        []Skip
}

// Detect scans the repo for merged branches and stale worktrees.
//
// Squash-merge detection: if gh is available, `gh pr list --state merged`
// head names are matched first. Otherwise (or when gh fails, which is only
// recorded in Skipped) a git fallback is used: `git cherry` (all commits have
// an equivalent patch on base — rebase/cherry-pick merges) or, for squashes,
// `git merge-tree --write-tree base branch` producing exactly base's tree,
// i.e. merging the branch would change nothing.
func Detect(ctx context.Context, o Options) (*Report, error) {
	if o.Runner == nil {
		o.Runner = ExecRunner{}
	}
	d := &detector{o: o, rep: &Report{}}
	return d.run(ctx)
}

type detector struct {
	o   Options
	rep *Report
}

func (d *detector) git(ctx context.Context, dir string, args ...string) (string, error) {
	if dir == "" {
		dir = d.o.Repo
	}
	return d.o.Runner.Run(ctx, dir, "git", args...)
}

func (d *detector) run(ctx context.Context) (*Report, error) {
	if d.o.Repo == "" {
		return nil, fmt.Errorf("workspace: Repo is required")
	}
	if d.o.Fetch {
		if _, err := d.git(ctx, "", "fetch", "--prune", "origin"); err != nil {
			return nil, fmt.Errorf("workspace: fetch: %w", err)
		}
	}
	base, err := d.resolveBase(ctx)
	if err != nil {
		return nil, err
	}
	d.rep.Base = base

	// Primary worktree's checked-out branch is protected.
	wts, err := d.listWorktrees(ctx)
	if err != nil {
		return nil, err
	}
	primary := ""
	if len(wts) > 0 {
		primary = wts[0].branch
	}
	protected := func(b string) bool { return isProtected(b, base, primary) }

	ghHeads, ghOK := d.ghMerged(ctx)

	merged := map[string]Branch{} // local branch name → kind
	locals, err := d.lines(ctx, "for-each-ref", "--format=%(refname:short)", "refs/heads")
	if err != nil {
		return nil, err
	}
	mergedLocal, err := d.lines(ctx, "branch", "--format=%(refname:short)", "--merged", base)
	if err != nil {
		return nil, err
	}
	isMergedLocal := toSet(mergedLocal)
	for _, b := range locals {
		if protected(b) {
			d.rep.Skipped = append(d.rep.Skipped, Skip{b, "protected"})
			continue
		}
		if br, ok := d.classify(ctx, b, b, base, isMergedLocal[b], ghHeads, ghOK); ok {
			merged[b] = br
			d.rep.LocalBranches = append(d.rep.LocalBranches, br)
		}
	}

	if !d.o.LocalOnly {
		remotes, err := d.lines(ctx, "for-each-ref", "--format=%(refname:short)", "refs/remotes/origin")
		if err != nil {
			return nil, err
		}
		mergedRemote, err := d.lines(ctx, "branch", "-r", "--format=%(refname:short)", "--merged", base)
		if err != nil {
			return nil, err
		}
		isMergedRemote := toSet(mergedRemote)
		for _, r := range remotes {
			name := strings.TrimPrefix(r, "origin/")
			if r == "origin" || r == "origin/HEAD" || name == r {
				continue
			}
			if protected(name) {
				d.rep.Skipped = append(d.rep.Skipped, Skip{r, "protected"})
				continue
			}
			if br, ok := d.classify(ctx, name, r, base, isMergedRemote[r], ghHeads, ghOK); ok {
				d.rep.RemoteBranches = append(d.rep.RemoteBranches, br)
			}
		}
	}

	for i, w := range wts {
		if i == 0 { // primary worktree is never a cleanup target
			continue
		}
		if w.branch == "" {
			d.rep.Skipped = append(d.rep.Skipped, Skip{w.path, "detached HEAD"})
			continue
		}
		if protected(w.branch) {
			d.rep.Skipped = append(d.rep.Skipped, Skip{w.path, "protected"})
			continue
		}
		br, ok := merged[w.branch]
		if !ok {
			continue
		}
		st, err := d.git(ctx, w.path, "status", "--porcelain")
		if err != nil {
			d.rep.Skipped = append(d.rep.Skipped, Skip{w.path, "status failed: " + err.Error()})
			continue
		}
		d.rep.StaleWorktrees = append(d.rep.StaleWorktrees, Worktree{
			Path: w.path, Branch: w.branch, Dirty: strings.TrimSpace(st) != "",
			Kind: br.Kind, Reason: br.Reason,
		})
	}
	return d.rep, nil
}

// classify decides whether branch (ref is the git ref to inspect) is merged.
func (d *detector) classify(ctx context.Context, name, ref, base string, direct bool, gh map[string]string, ghOK bool) (Branch, bool) {
	if direct {
		return Branch{Name: name, Kind: KindMerged, Reason: "merged into " + base}, true
	}
	if ghOK {
		if r, ok := gh[name]; ok {
			return Branch{Name: name, Kind: KindSquashMerged, Reason: r}, true
		}
	}
	if d.cherryMerged(ctx, base, ref) {
		return Branch{Name: name, Kind: KindSquashMerged, Reason: "every commit has an equivalent patch on " + base}, true
	}
	if d.treeMerged(ctx, base, ref) {
		return Branch{Name: name, Kind: KindSquashMerged, Reason: "merging into " + base + " changes nothing (squash-merged)"}, true
	}
	return Branch{}, false
}

// cherryMerged: git cherry marks commits already on base with "-"; all "-" (and
// at least one commit) means the branch was rebased/cherry-picked in.
func (d *detector) cherryMerged(ctx context.Context, base, ref string) bool {
	out, err := d.git(ctx, "", "cherry", base, ref)
	if err != nil {
		return false
	}
	n := 0
	for _, l := range splitLines(out) {
		if !strings.HasPrefix(l, "-") {
			return false
		}
		n++
	}
	return n > 0
}

// treeMerged: a squash merge leaves base containing the branch's changes, so a
// trial merge yields base's own tree.
func (d *detector) treeMerged(ctx context.Context, base, ref string) bool {
	out, err := d.git(ctx, "", "merge-tree", "--write-tree", base, ref)
	if err != nil {
		return false // conflicts or git < 2.38: not provable
	}
	tree := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
	want, err := d.git(ctx, "", "rev-parse", base+"^{tree}")
	return err == nil && tree != "" && tree == strings.TrimSpace(want)
}

// ghMerged returns head branch name → reason for merged PRs. ok=false when gh
// is missing or fails; that is recorded in Skipped but never fatal.
func (d *detector) ghMerged(ctx context.Context) (map[string]string, bool) {
	out, err := d.o.Runner.Run(ctx, d.o.Repo, "gh", "pr", "list", "--state", "merged", "--limit", "500",
		"--json", "headRefName,number,title")
	if err != nil {
		d.rep.Skipped = append(d.rep.Skipped, Skip{"gh", "gh unavailable, using git fallback"})
		return nil, false
	}
	var prs []struct {
		Head   string `json:"headRefName"`
		Number int    `json:"number"`
		Title  string `json:"title"`
	}
	if err := json.Unmarshal([]byte(out), &prs); err != nil {
		d.rep.Skipped = append(d.rep.Skipped, Skip{"gh", "gh output unparseable, using git fallback"})
		return nil, false
	}
	m := map[string]string{}
	for _, p := range prs {
		r := "merged PR"
		if p.Number > 0 {
			r = fmt.Sprintf("merged PR #%d", p.Number)
			if p.Title != "" {
				r += ": " + p.Title
			}
		}
		m[p.Head] = r
	}
	return m, true
}

func (d *detector) resolveBase(ctx context.Context) (string, error) {
	if d.o.Base != "" {
		return d.o.Base, nil
	}
	if out, err := d.git(ctx, "", "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		b := strings.TrimPrefix(strings.TrimSpace(out), "origin/")
		if _, err := d.git(ctx, "", "rev-parse", "--verify", "--quiet", "refs/heads/"+b); err == nil {
			return b, nil
		}
	}
	for _, b := range []string{"main", "master"} {
		if _, err := d.git(ctx, "", "rev-parse", "--verify", "--quiet", "refs/heads/"+b); err == nil {
			return b, nil
		}
	}
	return "", fmt.Errorf("workspace: cannot resolve default branch (no main or master)")
}

type wtEntry struct{ path, branch string }

// listWorktrees parses `git worktree list --porcelain`; the first entry is the
// primary worktree. branch is "" for detached HEAD.
func (d *detector) listWorktrees(ctx context.Context) ([]wtEntry, error) {
	out, err := d.git(ctx, "", "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("workspace: worktree list: %w", err)
	}
	var res []wtEntry
	var cur *wtEntry
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(l, "worktree "):
			res = append(res, wtEntry{path: filepath.Clean(strings.TrimPrefix(l, "worktree "))})
			cur = &res[len(res)-1]
		case strings.HasPrefix(l, "branch ") && cur != nil:
			cur.branch = strings.TrimPrefix(strings.TrimPrefix(l, "branch "), "refs/heads/")
		}
	}
	return res, nil
}

func (d *detector) lines(ctx context.Context, args ...string) ([]string, error) {
	out, err := d.git(ctx, "", args...)
	if err != nil {
		return nil, fmt.Errorf("workspace: git %s: %w", args[0], err)
	}
	return splitLines(out), nil
}

func isProtected(b, base, primary string) bool {
	switch b {
	case "main", "master", "develop", base, primary:
		return b != ""
	}
	return strings.HasPrefix(b, "release/")
}

func splitLines(s string) []string {
	var r []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			r = append(r, l)
		}
	}
	sort.Strings(r)
	return r
}

func toSet(s []string) map[string]bool {
	m := make(map[string]bool, len(s))
	for _, v := range s {
		m[v] = true
	}
	return m
}

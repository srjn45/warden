package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/srjn45/warden/internal/store"
)

// WorktreeInfo is one parsed entry from `git worktree list --porcelain`.
type WorktreeInfo struct {
	Path     string // absolute checkout path
	Branch   string // short branch name ("" when detached or bare)
	Detached bool
	Locked   bool
}

// parseWorktreeList parses `git worktree list --porcelain` output. Entries are
// blank-line separated; each opens with `worktree <abs-path>` followed by either
// `branch refs/heads/<name>` or `detached`, plus optional `locked`/`bare` lines.
func parseWorktreeList(out string) []WorktreeInfo {
	var entries []WorktreeInfo
	var cur *WorktreeInfo
	flush := func() {
		if cur != nil {
			entries = append(entries, *cur)
			cur = nil
		}
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "worktree "):
			flush()
			cur = &WorktreeInfo{Path: strings.TrimPrefix(line, "worktree ")}
		case cur == nil:
			continue
		case line == "detached":
			cur.Detached = true
		case line == "locked" || strings.HasPrefix(line, "locked "):
			cur.Locked = true
		case strings.HasPrefix(line, "branch "):
			cur.Branch = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
		}
	}
	flush()
	return entries
}

// gitWorktrees runs `git worktree list --porcelain` in repo and parses it.
func (l *Lifecycle) gitWorktrees(ctx context.Context, repo string) ([]WorktreeInfo, error) {
	out, err := l.run.Run(ctx, repo, "git", "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("git worktree list: %w", err)
	}
	return parseWorktreeList(out), nil
}

// wardenWorktrees keeps only the entries under repo/.worktrees (never the primary
// checkout or unrelated worktrees).
func wardenWorktrees(repo string, entries []WorktreeInfo) []WorktreeInfo {
	prefix := filepath.Join(repo, ".worktrees")
	var out []WorktreeInfo
	for _, e := range entries {
		if e.Path == prefix {
			continue
		}
		if strings.HasPrefix(e.Path, prefix+string(filepath.Separator)) {
			out = append(out, e)
		}
	}
	return out
}

// mainWorktreeBranch returns the branch checked out in the primary worktree (the
// entry whose path is the repo root) — the branch prune must never `branch -D`.
func mainWorktreeBranch(repo string, entries []WorktreeInfo) string {
	for _, e := range entries {
		if e.Path == repo {
			return e.Branch
		}
	}
	return ""
}

// ownerOf resolves the owning session id + lifecycle ("live"/"archived") for an
// absolute worktree path, scanning active records first then archived ones.
// Returns ("", "", nil) for an orphan.
func ownerOf(absPath string, active, archived []*store.Session) (id, lifecycle string, sess *store.Session) {
	for _, s := range active {
		if s.Worktree != "" && filepath.Join(s.Repo, s.Worktree) == absPath {
			return s.ID, "live", s
		}
	}
	for _, s := range archived {
		if s.Worktree != "" && filepath.Join(s.Repo, s.Worktree) == absPath {
			return s.ID, "archived", s
		}
	}
	return "", "", nil
}

// worktreeState classifies a worktree by the same guard prune/remove use:
// "clean" / "dirty" / "unpushed" (and "unknown" on an unexpected git error).
func (l *Lifecycle) worktreeState(ctx context.Context, repo, rel string) string {
	switch err := l.guard(ctx, CleanupTarget{Repo: repo, Worktree: rel}); {
	case err == nil:
		return "clean"
	case errors.Is(err, ErrDirtyWorktree):
		return "dirty"
	case errors.Is(err, ErrUnpushedCommits):
		return "unpushed"
	default:
		return "unknown"
	}
}

// worktreeHasCommitsAhead reports whether the worktree at abs has commits on its
// HEAD that are not reachable from base (the repo's default branch) — i.e. real
// unmerged committed work. Used by prune to protect an orphan worktree from
// removal even when it is clean and pushed. Conservative on uncertainty: an empty
// base (default branch unknown) or any git error returns false so this never
// invents a reason to hold back — the dirty/unpushed guard is the primary net and
// the no-upstream case is already covered there (guard treats it as unpushed).
func (l *Lifecycle) worktreeHasCommitsAhead(ctx context.Context, abs, base string) bool {
	if base == "" {
		return false
	}
	out, err := l.run.Run(ctx, "", "git", "-C", abs, "rev-list", "--count", base+"..HEAD")
	if err != nil {
		return false
	}
	n := strings.TrimSpace(out)
	return n != "" && n != "0"
}

// WorktreeListing is one row of `warden worktree ls`: a git worktree under
// .worktrees joined to its owning record (if any) plus its guard state.
type WorktreeListing struct {
	Path      string `json:"path"`   // relative, e.g. .worktrees/A-1
	Branch    string `json:"branch"` // "" when detached
	Detached  bool   `json:"detached"`
	Locked    bool   `json:"locked"`
	Owner     string `json:"owner"`     // owning session id; "" => orphan
	Lifecycle string `json:"lifecycle"` // "live" / "archived" / "" (orphan)
	State     string `json:"state"`     // guard state: clean/dirty/unpushed
}

// ListWorktrees is the read-only join behind `warden worktree ls`: every git
// worktree under repo/.worktrees, labelled by its owning active/archived record
// (or orphan) and annotated with its guard state. prune is this plus an action.
func (l *Lifecycle) ListWorktrees(ctx context.Context, repo string, active, archived []*store.Session) ([]WorktreeListing, error) {
	entries, err := l.gitWorktrees(ctx, repo)
	if err != nil {
		return nil, err
	}
	var out []WorktreeListing
	for _, e := range wardenWorktrees(repo, entries) {
		rel, _ := filepath.Rel(repo, e.Path)
		owner, life, _ := ownerOf(e.Path, active, archived)
		out = append(out, WorktreeListing{
			Path:      rel,
			Branch:    e.Branch,
			Detached:  e.Detached,
			Locked:    e.Locked,
			Owner:     owner,
			Lifecycle: life,
			State:     l.worktreeState(ctx, repo, rel),
		})
	}
	return out, nil
}

// PruneAction is the per-worktree outcome of a prune sweep.
type PruneAction string

const (
	PruneKeep   PruneAction = "keep"   // owned by a live (or archived, default) record
	PruneRemove PruneAction = "remove" // orphan removed (or, on --dry-run, would be)
	PruneSkip   PruneAction = "skip"   // dirty/unpushed orphan held back (or a remove error)
)

// PruneOpts configures a PruneWorktrees sweep. Active/Archived are the records
// that legitimately own a worktree; the daemon supplies them from the store
// (lifecycle has no store of its own).
type PruneOpts struct {
	DryRun          bool
	Force           bool
	IncludeArchived bool
	Active          []*store.Session // active records — always keep their worktree
	Archived        []*store.Session // archived records — keep by default; eligible with IncludeArchived
}

// PruneResult is the per-worktree outcome reported by PruneWorktrees.
type PruneResult struct {
	Path          string      `json:"path"`   // relative, e.g. .worktrees/A-1
	Branch        string      `json:"branch"` // "" when detached
	Owner         string      `json:"owner"`  // owning session id; "" => orphan
	Lifecycle     string      `json:"lifecycle"`
	Action        PruneAction `json:"action"`
	BranchDeleted bool        `json:"branch_deleted"` // a branch was (or would be) deleted
	State         string      `json:"state"`          // guard state when evaluated
	Reason        string      `json:"reason,omitempty"`
}

// PruneWorktrees reconciles git's worktree list against warden's records and
// reclaims orphans (and, with IncludeArchived, archived-owned worktrees) under
// the same dirty/unpushed guard RemoveWorktree uses. A live record always keeps
// its worktree; an archived record keeps it unless IncludeArchived. Branch
// deletion needs positive provenance: an archived owner honors its recorded
// BranchCreated flag; an orphan (provenance unknown) only deletes its branch
// under --force, only when the branch matches the worktree id, and never the
// repo's default branch. On a real run (not --dry-run) it finishes with a single
// best-effort `git worktree prune` to clear stale admin metadata.
func (l *Lifecycle) PruneWorktrees(ctx context.Context, repo string, opts PruneOpts) ([]PruneResult, error) {
	entries, err := l.gitWorktrees(ctx, repo)
	if err != nil {
		return nil, err
	}
	defaultBranch := mainWorktreeBranch(repo, entries)

	var results []PruneResult
	for _, e := range wardenWorktrees(repo, entries) {
		rel, _ := filepath.Rel(repo, e.Path)
		id := filepath.Base(e.Path)
		owner, life, sess := ownerOf(e.Path, opts.Active, opts.Archived)
		res := PruneResult{Path: rel, Branch: e.Branch, Owner: owner, Lifecycle: life}

		// A live record always keeps its worktree; an archived record keeps it
		// unless the caller opted into reclaiming archived-owned worktrees.
		candidate := owner == "" || (life == "archived" && opts.IncludeArchived)
		if !candidate {
			res.Action = PruneKeep
			res.Reason = fmt.Sprintf("owned by %s (%s)", owner, life)
			results = append(results, res)
			continue
		}

		// Classify state with the guard; hold back dirty/unpushed unless --force.
		res.State = l.worktreeState(ctx, repo, rel)
		if res.State != "clean" && !opts.Force {
			res.Action = PruneSkip
			res.Reason = res.State + " (use --force)"
			results = append(results, res)
			continue
		}
		// Extra caution for a true orphan (no owning record): even a clean, pushed
		// worktree can carry real committed work the operator never merged. The
		// guard catches dirty/unpushed, but a clean orphan branch with commits
		// ahead of the repo's default branch would otherwise be silently removed —
		// the reported "prune flagged worktrees with real committed work" false
		// positive. Hold it back unless --force. Archived-owned worktrees are
		// exempt: reclaiming them is an explicit --include-archived opt-in with
		// known provenance.
		if sess == nil && !opts.Force && l.worktreeHasCommitsAhead(ctx, e.Path, defaultBranch) {
			res.Action = PruneSkip
			res.Reason = "orphan has unmerged commits ahead of " + defaultBranch + " (use --force)"
			results = append(results, res)
			continue
		}

		// Decide branch deletion. Never the repo's default branch.
		deleteBranch := false
		if e.Branch != "" && e.Branch != defaultBranch {
			if sess != nil {
				deleteBranch = sess.BranchCreated // archived owner: trust recorded provenance
			} else {
				deleteBranch = opts.Force && e.Branch == id // orphan: only the warden-named branch, under --force
			}
		}
		res.Action = PruneRemove
		res.BranchDeleted = deleteBranch

		if opts.DryRun {
			results = append(results, res)
			continue
		}

		// Execute the removal. A remove failure downgrades to a skip with a reason.
		removeArgs := []string{"-C", repo, "worktree", "remove", rel}
		if opts.Force {
			removeArgs = []string{"-C", repo, "worktree", "remove", "--force", rel}
		}
		if out, rerr := l.run.Run(ctx, "", "git", removeArgs...); rerr != nil {
			res.Action = PruneSkip
			res.BranchDeleted = false
			res.Reason = fmt.Sprintf("git worktree remove: %v: %s", rerr, strings.TrimSpace(out))
			results = append(results, res)
			continue
		}
		if deleteBranch {
			if out, berr := l.run.Run(ctx, "", "git", "-C", repo, "branch", "-D", e.Branch); berr != nil {
				slog.Warn("prune: git branch -D failed", "agent", id, "branch", e.Branch, "err", berr, "out", strings.TrimSpace(out))
				res.BranchDeleted = false
			}
		}
		results = append(results, res)
	}

	// Clear stale .git/worktrees admin metadata after a real run (cheap and
	// idempotent); never on --dry-run, which changes nothing. Best-effort.
	if !opts.DryRun {
		if out, perr := l.run.Run(ctx, "", "git", "-C", repo, "worktree", "prune"); perr != nil {
			slog.Warn("prune: git worktree prune failed", "err", perr, "out", strings.TrimSpace(out))
		}
	}
	return results, nil
}

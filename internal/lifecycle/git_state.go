package lifecycle

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/srjn45/warden/internal/savings"
)

// RepoState is the in-progress-operation state of a working tree.
type RepoState struct {
	Rebase   bool     // a rebase is in progress (rebase-merge or rebase-apply)
	Merge    bool     // a merge is in progress (MERGE_HEAD present)
	Unmerged []string // paths with unresolved conflicts
}

const (
	rebaseInProgressHint = "resolve the conflicts then run `wd sync --continue`, or run `wd sync --abort` to drop the rebase"
)

// gitPath resolves a path inside the git dir. `git rev-parse --git-path` is used
// (not <dir>/.git/...) because a linked worktree has its own separate git dir.
func (l *Lifecycle) gitPath(ctx context.Context, dir, name string) string {
	out, err := l.run.Run(ctx, dir, "git", "rev-parse", "--git-path", name)
	if err != nil {
		return ""
	}
	p := strings.TrimSpace(out)
	if p != "" && !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	return p
}

func pathExists(p string) bool {
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

// RepoState reports whether a rebase or merge is in progress in dir and which
// paths are unmerged.
func (l *Lifecycle) RepoState(ctx context.Context, dir string) RepoState {
	st := RepoState{
		Rebase: pathExists(l.gitPath(ctx, dir, "rebase-merge")) || pathExists(l.gitPath(ctx, dir, "rebase-apply")),
		Merge:  pathExists(l.gitPath(ctx, dir, "MERGE_HEAD")),
	}
	st.Unmerged = l.unmergedPaths(ctx, dir)
	return st
}

// unresolved narrows unmerged paths to those still carrying conflict markers. An
// unmerged path stays "unmerged" in the index until `git add`, so a file the agent
// has edited but not yet staged must count as resolved (continue/commit stage it).
func unresolved(dir string, unmerged []string) []string {
	var out []string
	for _, p := range unmerged {
		b, err := os.ReadFile(filepath.Join(dir, p))
		if err != nil { // deleted / unreadable: nothing to conflict-mark
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "<<<<<<< ") || strings.HasPrefix(line, ">>>>>>> ") || line == "=======" {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

// rebaseHeadBranch returns the short name of the branch a rebase in progress is
// rebuilding (best-effort, "" if unknown).
func (l *Lifecycle) rebaseHeadBranch(ctx context.Context, dir string) string {
	for _, d := range []string{"rebase-merge", "rebase-apply"} {
		b, err := os.ReadFile(filepath.Join(l.gitPath(ctx, dir, d), "head-name"))
		if err == nil {
			return strings.TrimPrefix(strings.TrimSpace(string(b)), "refs/heads/")
		}
	}
	return ""
}

// SyncContinue finishes a conflicted rebase left in progress by Sync: it requires
// a rebase in progress, refuses while unmerged paths remain, stages the resolved
// files and runs `git rebase --continue` non-interactively. If the next commit
// conflicts it returns the same Conflicts shape as Sync.
func (l *Lifecycle) SyncContinue(ctx context.Context, dir string) (SyncResult, error) {
	if l.GitBranch(ctx, dir) == "" {
		return SyncResult{}, fmt.Errorf("not a git repository: %s", dir)
	}
	st := l.RepoState(ctx, dir)
	if !st.Rebase {
		return SyncResult{}, fmt.Errorf("no rebase in progress — nothing to continue")
	}
	if left := unresolved(dir, st.Unmerged); len(left) > 0 {
		return SyncResult{}, fmt.Errorf("unresolved conflicts remain in: %s — resolve them, then run `wd sync --continue` again (or `wd sync --abort`)", strings.Join(left, ", "))
	}
	branch := l.rebaseHeadBranch(ctx, dir)
	addOut, err := l.run.Run(ctx, dir, "git", "add", "-A")
	if err != nil {
		return SyncResult{}, fmt.Errorf("git add: %w: %s", err, strings.TrimSpace(addOut))
	}
	out, err := l.run.Run(ctx, dir, "git", "-c", "core.editor=true", "rebase", "--continue")
	raw := len(addOut) + len(out)
	sample := savings.TruncateSample(addOut + out)
	if err != nil {
		if conflicts := l.unmergedPaths(ctx, dir); len(conflicts) > 0 {
			return SyncResult{Branch: branch, Conflicts: conflicts, Output: strings.TrimSpace(out), RawBytes: raw, RawSample: sample}, nil
		}
		return SyncResult{}, fmt.Errorf("git rebase --continue: %w: %s", err, strings.TrimSpace(out))
	}
	if branch == "" {
		branch = l.GitBranch(ctx, dir)
	}
	return SyncResult{Branch: branch, Updated: true, Output: strings.TrimSpace(out), RawBytes: raw, RawSample: sample}, nil
}

// SyncAbort drops a rebase in progress (`git rebase --abort`), restoring the
// branch to its pre-sync state.
func (l *Lifecycle) SyncAbort(ctx context.Context, dir string) (SyncResult, error) {
	if l.GitBranch(ctx, dir) == "" {
		return SyncResult{}, fmt.Errorf("not a git repository: %s", dir)
	}
	if !l.RepoState(ctx, dir).Rebase {
		return SyncResult{}, fmt.Errorf("no rebase in progress — nothing to abort")
	}
	out, err := l.run.Run(ctx, dir, "git", "rebase", "--abort")
	if err != nil {
		return SyncResult{}, fmt.Errorf("git rebase --abort: %w: %s", err, strings.TrimSpace(out))
	}
	branch := l.GitBranch(ctx, dir)
	return SyncResult{Branch: branch, Output: "rebase aborted; " + branch + " restored", RawBytes: len(out), RawSample: savings.TruncateSample(out)}, nil
}

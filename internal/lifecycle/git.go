package lifecycle

import (
	"context"
	"fmt"
	"strings"
)

// protectedBranches are the long-lived integration branches an agent must never
// commit to or push directly. An agent works on its own branch; a human (or a
// reviewed PR) integrates. Keeps the wd commit / wd push rails deterministic and
// language-agnostic.
var protectedBranches = map[string]bool{"main": true, "master": true}

// IsProtectedBranch reports whether branch is one warden refuses to mutate
// directly. Exported so the daemon redirect hooks and tests share one list.
func IsProtectedBranch(branch string) bool { return protectedBranches[branch] }

// CommitResult is the compact struct `wd commit` / `mcp__warden__commit` returns
// — one value in place of the 4-6 git tool round-trips Claude would otherwise
// read (status, diff, add, commit, rev-parse, hook output).
type CommitResult struct {
	Committed  bool     `json:"committed"`             // false = clean tree, nothing to do
	SHA        string   `json:"sha,omitempty"`         // short SHA of the new commit
	Branch     string   `json:"branch"`                // branch committed onto
	Files      []string `json:"files,omitempty"`       // paths included in the commit
	HookFailed bool     `json:"hook_failed,omitempty"` // a pre-commit hook rejected the commit
	HookOutput string   `json:"hook_output,omitempty"` // captured rejection output (only on failure)
}

// PushResult is the compact struct `wd push` / `mcp__warden__push` returns.
type PushResult struct {
	Branch string `json:"branch"`
	Remote string `json:"remote"`
	Pushed bool   `json:"pushed"`
	Output string `json:"output,omitempty"`
}

// SyncResult is the compact struct `wd sync` / `mcp__warden__sync` returns. On a
// clean rebase Updated is true and Conflicts is empty; on a conflict the rebase
// is left in progress and Conflicts names the files the agent must resolve — the
// deterministic-detect half of "conflict resolution stays Claude, handed only
// the conflicting hunks."
type SyncResult struct {
	Branch    string   `json:"branch"`
	Base      string   `json:"base"`
	Updated   bool     `json:"updated"`             // rebase completed cleanly
	Conflicts []string `json:"conflicts,omitempty"` // unresolved paths (rebase in progress)
	Output    string   `json:"output,omitempty"`
}

// Commit stages and commits every change in dir on its current branch, enforcing
// the protected-branch rail and surfacing a pre-commit hook rejection as a
// structured result (not an error) so the agent sees only the failure. A clean
// tree returns Committed=false with no error.
func (l *Lifecycle) Commit(ctx context.Context, dir, message string) (CommitResult, error) {
	branch := l.GitBranch(ctx, dir)
	if branch == "" {
		return CommitResult{}, fmt.Errorf("not a git repository: %s", dir)
	}
	if protectedBranches[branch] {
		return CommitResult{}, fmt.Errorf("refusing to commit on protected branch %q — agents commit on their own branch and a human integrates", branch)
	}
	if strings.TrimSpace(message) == "" {
		return CommitResult{}, fmt.Errorf("commit message is required")
	}
	status, err := l.run.Run(ctx, dir, "git", "status", "--porcelain")
	if err != nil {
		return CommitResult{}, fmt.Errorf("git status: %w: %s", err, status)
	}
	files := parsePorcelainPaths(status)
	if len(files) == 0 {
		return CommitResult{Committed: false, Branch: branch}, nil // clean tree
	}
	if out, err := l.run.Run(ctx, dir, "git", "add", "-A"); err != nil {
		return CommitResult{}, fmt.Errorf("git add: %w: %s", err, out)
	}
	out, err := l.run.Run(ctx, dir, "git", "commit", "-m", message)
	if err != nil {
		// A pre-commit hook (or other commit-time check) rejected it. Unstage so
		// the agent is back at its pre-commit state, and hand back only the output
		// it needs to fix the failure.
		_, _ = l.run.Run(ctx, dir, "git", "reset")
		return CommitResult{Branch: branch, Files: files, HookFailed: true, HookOutput: strings.TrimSpace(out)}, nil
	}
	sha, _ := l.run.Run(ctx, dir, "git", "rev-parse", "--short", "HEAD")
	return CommitResult{Committed: true, SHA: strings.TrimSpace(sha), Branch: branch, Files: files}, nil
}

// Push pushes dir's current branch to origin (setting upstream), enforcing the
// protected-branch rail so an agent cannot push main/master directly.
func (l *Lifecycle) Push(ctx context.Context, dir string) (PushResult, error) {
	branch := l.GitBranch(ctx, dir)
	if branch == "" {
		return PushResult{}, fmt.Errorf("not a git repository: %s", dir)
	}
	if protectedBranches[branch] {
		return PushResult{}, fmt.Errorf("refusing to push protected branch %q directly — push your agent branch and open a PR", branch)
	}
	out, err := l.run.Run(ctx, dir, "git", "push", "-u", "origin", branch)
	if err != nil {
		return PushResult{}, fmt.Errorf("git push: %w: %s", err, strings.TrimSpace(out))
	}
	return PushResult{Branch: branch, Remote: "origin", Pushed: true, Output: strings.TrimSpace(out)}, nil
}

// Sync fetches origin/base and rebases dir's branch onto it. It refuses a dirty
// tree (commit first) rather than silently stashing. On conflict it leaves the
// rebase in progress and returns the conflicting paths; on any other rebase
// failure it aborts to a clean state and returns the error.
func (l *Lifecycle) Sync(ctx context.Context, dir, base string) (SyncResult, error) {
	branch := l.GitBranch(ctx, dir)
	if branch == "" {
		return SyncResult{}, fmt.Errorf("not a git repository: %s", dir)
	}
	if base == "" {
		base = "main"
	}
	status, err := l.run.Run(ctx, dir, "git", "status", "--porcelain")
	if err != nil {
		return SyncResult{}, fmt.Errorf("git status: %w: %s", err, status)
	}
	if strings.TrimSpace(status) != "" {
		return SyncResult{}, fmt.Errorf("working tree has uncommitted changes — commit them first (wd commit) before syncing")
	}
	if out, err := l.run.Run(ctx, dir, "git", "fetch", "origin", base); err != nil {
		return SyncResult{}, fmt.Errorf("git fetch origin %s: %w: %s", base, err, strings.TrimSpace(out))
	}
	out, err := l.run.Run(ctx, dir, "git", "rebase", "origin/"+base)
	if err != nil {
		conflicts := l.unmergedPaths(ctx, dir)
		if len(conflicts) == 0 {
			// Not a conflict (e.g. missing base ref) — abort to a known-clean tree.
			_, _ = l.run.Run(ctx, dir, "git", "rebase", "--abort")
			return SyncResult{}, fmt.Errorf("git rebase onto origin/%s: %w: %s", base, err, strings.TrimSpace(out))
		}
		return SyncResult{Branch: branch, Base: base, Conflicts: conflicts, Output: strings.TrimSpace(out)}, nil
	}
	return SyncResult{Branch: branch, Base: base, Updated: true, Output: strings.TrimSpace(out)}, nil
}

// unmergedPaths lists files with merge conflicts in dir (best-effort, nil on error).
func (l *Lifecycle) unmergedPaths(ctx context.Context, dir string) []string {
	out, err := l.run.Run(ctx, dir, "git", "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil
	}
	return nonEmptyLines(out)
}

// parsePorcelainPaths extracts the changed paths from `git status --porcelain`
// output. Each line is "XY <path>" (or "XY <orig> -> <new>" for a rename — the
// post-rename path is what landed, so that is the one kept).
func parsePorcelainPaths(porcelain string) []string {
	var paths []string
	for _, line := range nonEmptyLines(porcelain) {
		if len(line) < 4 {
			continue
		}
		p := strings.TrimSpace(line[3:])
		if idx := strings.Index(p, " -> "); idx >= 0 {
			p = p[idx+len(" -> "):]
		}
		paths = append(paths, p)
	}
	return paths
}

// nonEmptyLines splits s on newlines and drops blank lines.
func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

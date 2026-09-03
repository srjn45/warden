package collab

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// gitDiffTimeout bounds each `git diff` subprocess so one wedged worktree
	// can't stall the scan.
	gitDiffTimeout = 5 * time.Second
)

// defaultGitReconcileInterval is how often git diff refreshes dirty-file state
// when fsnotify is active. Real-time detection uses in-memory paths; git is the
// backstop for commits, reverts, and missed events.
const defaultGitReconcileInterval = 2 * time.Minute

// gitReadOnlyEnv returns an environment for read-only git subprocesses. Git ≥2.36
// honours GIT_OPTIONAL_LOCKS=1 so a collab scan does not block on index.lock.
func gitReadOnlyEnv() []string {
	env := os.Environ()
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_OPTIONAL_LOCKS=") {
			return env
		}
	}
	return append(env, "GIT_OPTIONAL_LOCKS=1")
}

// gitIndexLocked reports whether the worktree's index.lock exists. When true,
// skip git reconcile this tick rather than contend with user git operations.
func gitIndexLocked(worktree string) bool {
	out, err := exec.Command("git", "-C", worktree, "rev-parse", "--git-path", "index.lock").Output()
	if err != nil {
		return false
	}
	lock := strings.TrimSpace(string(out))
	if lock == "" {
		return false
	}
	if !filepath.IsAbs(lock) {
		lock = filepath.Join(worktree, lock)
	}
	_, err = os.Stat(lock)
	return err == nil
}

// gitDiffFiles returns repo-relative paths modified in worktree (vs HEAD). Any
// error — missing worktree, timeout, lock contention — yields no files.
func gitDiffFiles(ctx context.Context, worktree string) []string {
	if gitIndexLocked(worktree) {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, gitDiffTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", "-C", worktree, "diff", "--name-only", "HEAD")
	cmd.Env = gitReadOnlyEnv()
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	return files
}

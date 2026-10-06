package planexport

// COMPATIBILITY SUITE — planexport protected-branch rail (task t1-compat-baseline).
//
// RunnerGitHost refuses to commit, push or open a PR from main/master via
// lifecycle.IsProtectedBranch. Real temporary repos (bare origin) are used so
// the refusal is proven to happen before any mutation.
//
// Later tasks may only change an assertion in this file when their task prompt
// says so, and must say why in the commit message.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func compatGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

func compatRepoWithOrigin(t *testing.T, branch string) string {
	t.Helper()
	base := t.TempDir()
	origin := filepath.Join(base, "origin.git")
	compatGit(t, base, "init", "-q", "--bare", "-b", "main", origin)
	dir := filepath.Join(base, "wt")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	compatGit(t, dir, "init", "-q", "-b", branch)
	compatGit(t, dir, "remote", "add", "origin", origin)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "seed"), []byte("s"), 0o644))
	compatGit(t, dir, "add", ".")
	compatGit(t, dir, "commit", "-q", "-m", "seed")
	return dir
}

func TestCompatRunnerGitHostRefusesProtectedBranches(t *testing.T) {
	ctx := context.Background()
	for _, branch := range []string{"main", "master"} {
		t.Run(branch, func(t *testing.T) {
			dir := compatRepoWithOrigin(t, branch)
			h := NewRunnerGitHost(nil)
			head := compatGit(t, dir, "rev-parse", "HEAD")
			require.NoError(t, os.WriteFile(filepath.Join(dir, "plan.md"), []byte("p"), 0o644))

			_, err := h.CommitPath(ctx, dir, "plan.md", "msg")
			require.Error(t, err)
			require.Contains(t, err.Error(), "refusing to commit on protected branch")
			require.Equal(t, head, compatGit(t, dir, "rev-parse", "HEAD"), "no commit made")
			require.Empty(t, compatGit(t, dir, "diff", "--cached", "--name-only"), "nothing staged: refusal precedes git add")

			err = h.PushBranch(ctx, dir, branch)
			require.Error(t, err)
			require.Contains(t, err.Error(), "refusing to push protected branch")
			require.Empty(t, compatGit(t, dir, "ls-remote", "origin"), "nothing reached origin")

			_, err = h.CreateOrReusePR(ctx, dir, PRRequest{Head: branch, Title: "t", Body: "b"})
			require.Error(t, err)
			require.Contains(t, err.Error(), "refusing to open a PR from protected branch")
		})
	}
}

func TestCompatRunnerGitHostCommitsAndPushesAgentBranch(t *testing.T) {
	ctx := context.Background()
	dir := compatRepoWithOrigin(t, "plan/export")
	h := NewRunnerGitHost(nil)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "plan.md"), []byte("p"), 0o644))

	sha, err := h.CommitPath(ctx, dir, "plan.md", "export plan")
	require.NoError(t, err)
	require.Equal(t, compatGit(t, dir, "rev-parse", "HEAD"), sha)
	require.NoError(t, h.PushBranch(ctx, dir, "plan/export"))
	require.Contains(t, compatGit(t, dir, "ls-remote", "origin"), "refs/heads/plan/export")
}

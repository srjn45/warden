package lifecycle

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// twoCommitConflict leaves dir on feature with two local commits that each
// conflict with a remote main change, and the first rebase conflict in progress.
func twoCommitConflict(t *testing.T) (compatRepo, *Lifecycle) {
	t.Helper()
	r := newCompatRepo(t)
	l := compatLife()
	compatCommitFile(t, r.dir, "g.txt", "seed\n", "seed g")
	compatGit(t, r.dir, "push", "origin", "main")
	compatGit(t, r.dir, "checkout", "-b", "feature")
	compatCommitFile(t, r.dir, "f.txt", "local1\n", "local f")
	compatCommitFile(t, r.dir, "g.txt", "local2\n", "local g")
	other := r.otherClone(t)
	compatGit(t, other, "pull", "origin", "main")
	compatCommitFile(t, other, "f.txt", "remote\n", "remote f")
	compatCommitFile(t, other, "g.txt", "remote\n", "remote g")
	compatGit(t, other, "push", "origin", "main")
	res, err := l.Sync(context.Background(), r.dir, "")
	require.NoError(t, err)
	require.Equal(t, []string{"f.txt"}, res.Conflicts)
	return r, l
}

// (a) resolved + staged files survive a second sync (previously the rebase could
// be aborted by the failure path, losing the resolution).
func TestSecondSyncMidRebaseKeepsStagedResolution(t *testing.T) {
	r, l := conflictedRebase(t)
	compatWrite(t, r.dir, "f.txt", "resolved\n")
	compatGit(t, r.dir, "add", "f.txt")
	_, err := l.Sync(context.Background(), r.dir, "")
	require.ErrorContains(t, err, "already in progress")
	require.DirExists(t, filepath.Join(r.dir, ".git", "rebase-merge"))
	b, _ := os.ReadFile(filepath.Join(r.dir, "f.txt"))
	require.Equal(t, "resolved\n", string(b))
	require.Contains(t, compatGit(t, r.dir, "diff", "--cached", "--name-only"), "f.txt")
}

// (c) continue with unresolved paths is refused.
func TestSyncContinueRefusesUnresolved(t *testing.T) {
	r, l := conflictedRebase(t)
	_, err := l.SyncContinue(context.Background(), r.dir)
	require.ErrorContains(t, err, "f.txt")
	require.DirExists(t, filepath.Join(r.dir, ".git", "rebase-merge"))
}

func TestSyncContinueWithoutRebaseRefused(t *testing.T) {
	r := newCompatRepo(t)
	_, err := compatLife().SyncContinue(context.Background(), r.dir)
	require.ErrorContains(t, err, "no rebase in progress")
	_, err = compatLife().SyncAbort(context.Background(), r.dir)
	require.ErrorContains(t, err, "no rebase in progress")
}

// (d) continue across two conflicting commits.
func TestSyncContinueAcrossTwoConflictingCommits(t *testing.T) {
	r, l := twoCommitConflict(t)
	ctx := context.Background()
	compatWrite(t, r.dir, "f.txt", "resolved1\n")
	res, err := l.SyncContinue(ctx, r.dir)
	require.NoError(t, err)
	require.False(t, res.Updated)
	require.Equal(t, []string{"g.txt"}, res.Conflicts)
	compatWrite(t, r.dir, "g.txt", "resolved2\n")
	res, err = l.SyncContinue(ctx, r.dir)
	require.NoError(t, err)
	require.True(t, res.Updated)
	require.Equal(t, "feature", res.Branch)
	require.Equal(t, "feature", compatGit(t, r.dir, "rev-parse", "--abbrev-ref", "HEAD"))
	require.Equal(t, "local g", compatGit(t, r.dir, "log", "-1", "--format=%s"))
	require.Equal(t, "resolved1", compatGit(t, r.dir, "show", "HEAD~1:f.txt"))
	require.False(t, l.RepoState(ctx, r.dir).Rebase)
}

// (e) abort restores the branch.
func TestSyncAbortRestoresBranch(t *testing.T) {
	r, l := conflictedRebase(t)
	res, err := l.SyncAbort(context.Background(), r.dir)
	require.NoError(t, err)
	require.Equal(t, "feature", res.Branch)
	require.Equal(t, "feature", compatGit(t, r.dir, "rev-parse", "--abbrev-ref", "HEAD"))
	require.Equal(t, "local", compatGit(t, r.dir, "show", "HEAD:f.txt"))
	require.Empty(t, compatGit(t, r.dir, "status", "--porcelain"))
}

// (f) a merge with conflicts resolved concludes via Commit; unresolved is refused.
func TestCommitMergeUnresolvedRefusedResolvedConcludes(t *testing.T) {
	r := newCompatRepo(t)
	l := compatLife()
	compatGit(t, r.dir, "checkout", "-b", "feature")
	compatCommitFile(t, r.dir, "f.txt", "local\n", "local")
	compatGit(t, r.dir, "checkout", "main")
	compatCommitFile(t, r.dir, "f.txt", "main\n", "main")
	compatGit(t, r.dir, "checkout", "feature")
	_ = exec.Command("git", "-C", r.dir, "merge", "main").Run() // conflicts: non-zero exit expected
	_, err := l.Commit(context.Background(), r.dir, "merge")
	require.ErrorContains(t, err, "f.txt")
	compatWrite(t, r.dir, "f.txt", "both\n")
	res, err := l.Commit(context.Background(), r.dir, "merge main")
	require.NoError(t, err)
	require.True(t, res.Committed)
	require.Equal(t, "feature", res.Branch)
	require.Len(t, compatGit(t, r.dir, "log", "-1", "--format=%P"), 81, "merge commit has two parents")
}

// A resolution identical to the new base leaves a clean tree mid-rebase — the one
// state where the old Sync passed its dirty-tree check, failed `git rebase`
// ("already in progress"), saw no unmerged paths and ran `git rebase --abort`.
func TestSecondSyncMidRebaseCleanTreeDoesNotAbort(t *testing.T) {
	r, l := conflictedRebase(t)
	compatWrite(t, r.dir, "f.txt", "remote\n")
	compatGit(t, r.dir, "add", "f.txt")
	require.Empty(t, compatGit(t, r.dir, "status", "--porcelain"))
	_, err := l.Sync(context.Background(), r.dir, "")
	require.ErrorContains(t, err, "already in progress")
	require.DirExists(t, filepath.Join(r.dir, ".git", "rebase-merge"))
}

package lifecycle

// COMPATIBILITY SUITE — behaviour during an in-progress rebase (task t1-compat-baseline).
//
// Pins what Commit/Sync actually do today when a conflicted `wd sync` has left a
// rebase in progress, so a later hardening task changes it knowingly.
//
// Later tasks may only change an assertion in this file when their task prompt
// says so, and must say why in the commit message.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// conflictedRebase leaves r.dir on feature with a conflicted rebase in progress.
func conflictedRebase(t *testing.T) (compatRepo, *Lifecycle) {
	t.Helper()
	r := newCompatRepo(t)
	l := compatLife()
	compatGit(t, r.dir, "checkout", "-b", "feature")
	compatCommitFile(t, r.dir, "f.txt", "local\n", "local edit")
	other := r.otherClone(t)
	compatCommitFile(t, other, "f.txt", "remote\n", "remote f")
	compatGit(t, other, "push", "origin", "main")
	res, err := l.Sync(context.Background(), r.dir, "")
	require.NoError(t, err)
	require.Equal(t, []string{"f.txt"}, res.Conflicts)
	return r, l
}

// t2-rebase-merge-safety INTENTIONALLY changed both assertions below: a Sync or
// Commit while a rebase is in progress is now refused with a message naming
// `wd sync --continue` / `--abort` (previously: "uncommitted changes" dirty-tree
// error, and a commit landing on the detached HEAD as branch "HEAD").

// A second Sync while the rebase is in progress is REFUSED (naming the next step)
// and does NOT abort the rebase.
func TestCompatSecondSyncDuringConflictedRebaseRefusesWithoutAborting(t *testing.T) {
	r, l := conflictedRebase(t)
	_, err := l.Sync(context.Background(), r.dir, "")
	require.ErrorContains(t, err, "already in progress")
	require.ErrorContains(t, err, "wd sync --continue")
	require.DirExists(t, filepath.Join(r.dir, ".git", "rebase-merge"), "rebase still in progress")
	require.Equal(t, []string{"f.txt"}, l.unmergedPaths(context.Background(), r.dir))
}

// Commit mid-rebase is refused and commits nothing.
func TestCompatCommitMidRebaseRefused(t *testing.T) {
	r, l := conflictedRebase(t)
	compatWrite(t, r.dir, "f.txt", "resolved\n")
	head := compatGit(t, r.dir, "rev-parse", "HEAD")
	_, err := l.Commit(context.Background(), r.dir, "resolve")
	require.ErrorContains(t, err, "wd sync --continue")
	require.Equal(t, head, compatGit(t, r.dir, "rev-parse", "HEAD"))
}

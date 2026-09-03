package collab

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func TestGitReadOnlyEnvSetsOptionalLocks(t *testing.T) {
	env := gitReadOnlyEnv()
	found := false
	for _, kv := range env {
		if kv == "GIT_OPTIONAL_LOCKS=1" {
			found = true
			break
		}
	}
	require.True(t, found, "expected GIT_OPTIONAL_LOCKS=1 in git env")
}

func TestGitIndexLockedAbsent(t *testing.T) {
	root := t.TempDir()
	require.False(t, gitIndexLocked(root))
}

func TestWorktreeForLongestPrefix(t *testing.T) {
	w := newWatcherWith(newFakeFSWatch(), 64)
	w.reconcile([]string{"/wt/a", "/wt/ab"})
	require.Equal(t, "/wt/ab", w.worktreeFor("/wt/ab/internal/foo.go"))
	require.Equal(t, "/wt/a", w.worktreeFor("/wt/a/foo.go"))
	require.Equal(t, "", w.worktreeFor("/elsewhere/foo.go"))
}

func TestNoteFileChangeRecordsRepoRelativePath(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "internal")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	file := filepath.Join(sub, "auth.go")

	m := NewMonitor(fakeLister{}, nil)
	fake := newFakeFSWatch()
	m.watch = newWatcherWith(fake, 64)
	m.watch.reconcile([]string{root})

	m.noteFileChange(file)
	require.Equal(t, []string{"internal/auth.go"}, m.dirtyFiles(root))
}

func TestConflictsUsesCacheWithoutCallingDiffAgain(t *testing.T) {
	calls := 0
	m := NewMonitor(fakeLister{sessions: []*store.Session{
		{ID: "a", Worktree: "/wt/a", Status: store.StatusWorking},
		{ID: "b", Worktree: "/wt/b", Status: store.StatusWorking},
	}}, nil)
	m.diff = func(_ context.Context, worktree string) []string {
		calls++
		if worktree == "/wt/a" || worktree == "/wt/b" {
			return []string{"shared.go"}
		}
		return nil
	}
	require.NoError(t, m.refreshConflicts(context.Background()))
	before := calls
	_, err := m.Conflicts(context.Background())
	require.NoError(t, err)
	require.Equal(t, before, calls, "cached Conflicts must not invoke git/diff again")
}

func TestGitReconcileReplacesDirtyState(t *testing.T) {
	m := NewMonitor(fakeLister{sessions: []*store.Session{
		{ID: "a", Worktree: "/wt/a", Status: store.StatusWorking},
	}}, nil)
	m.addDirty("/wt/a", "stale.go")
	m.diff = func(_ context.Context, worktree string) []string {
		if worktree == "/wt/a" {
			return []string{"fresh.go"}
		}
		return nil
	}
	m.gitReconcile(context.Background())
	require.Equal(t, []string{"fresh.go"}, m.dirtyFiles("/wt/a"))
}

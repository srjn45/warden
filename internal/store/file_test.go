package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// sample returns a representative session. (Moved here from the deleted
// mongo_test.go; file_test.go is now the home for store test fixtures.)
func sample() *Session {
	return &Session{
		ID: "PROJ-350", Ticket: "PROJ-350", TmuxSession: "PROJ-350",
		Repo: "/repo", Worktree: ".worktrees/PROJ-350", Branch: "PROJ-350",
		Status: StatusSpawning,
	}
}

func newFileStore(t *testing.T) *FileStore {
	t.Helper()
	st, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	return st
}

func TestFileInsertGet(t *testing.T) {
	ctx := context.Background()
	st := newFileStore(t)
	require.NoError(t, st.Insert(ctx, sample()))

	got, err := st.Get(ctx, "PROJ-350")
	require.NoError(t, err)
	require.Equal(t, StatusSpawning, got.Status)
	require.False(t, got.CreatedAt.IsZero(), "Insert must stamp CreatedAt")
	require.False(t, got.UpdatedAt.IsZero(), "Insert must stamp UpdatedAt")
	require.NotNil(t, got.Events, "Insert must init Events to non-nil")
}

func TestFileInsertDuplicate(t *testing.T) {
	ctx := context.Background()
	st := newFileStore(t)
	require.NoError(t, st.Insert(ctx, sample()))
	require.ErrorIs(t, st.Insert(ctx, sample()), ErrExists)
}

func TestFileGetNotFound(t *testing.T) {
	ctx := context.Background()
	st := newFileStore(t)
	_, err := st.Get(ctx, "nope")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestFileBadID(t *testing.T) {
	ctx := context.Background()
	st := newFileStore(t)
	bad := sample()
	bad.ID = "../escape"
	require.ErrorIs(t, st.Insert(ctx, bad), ErrBadID)
	_, err := st.Get(ctx, "a/b")
	require.ErrorIs(t, err, ErrBadID)
}

func TestFileListSortedByUpdatedDesc(t *testing.T) {
	ctx := context.Background()
	st := newFileStore(t)
	a := sample()
	a.ID, a.TmuxSession, a.Ticket = "agent-aaaa", "agent-aaaa", ""
	b := sample()
	b.ID, b.TmuxSession, b.Ticket = "agent-bbbb", "agent-bbbb", ""
	require.NoError(t, st.Insert(ctx, a))
	require.NoError(t, st.Insert(ctx, b))
	// Touch a so its updated_at is newest.
	require.NoError(t, st.UpdateStatus(ctx, "agent-aaaa", StatusWorking))

	list, err := st.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, "agent-aaaa", list[0].ID, "most recently updated first")
}

func TestFileListSkipsCorruptFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := NewFileStore(dir)
	require.NoError(t, err)
	require.NoError(t, st.Insert(ctx, sample()))
	// Drop a junk .json file into sessions/.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sessions", "broken.json"), []byte("{not json"), 0o644))

	list, err := st.List(ctx)
	require.NoError(t, err, "one corrupt file must not fail List")
	require.Len(t, list, 1)
	require.Equal(t, "PROJ-350", list[0].ID)
}

func TestFileArchiveRemovesFromActive(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := NewFileStore(dir)
	require.NoError(t, err)
	require.NoError(t, st.Insert(ctx, sample()))

	require.NoError(t, st.Archive(ctx, "PROJ-350"))
	_, err = st.Get(ctx, "PROJ-350")
	require.ErrorIs(t, err, ErrNotFound, "archived session gone from active")
	require.FileExists(t, filepath.Join(dir, "closed", "PROJ-350.json"))
}

func TestFileDelete(t *testing.T) {
	ctx := context.Background()
	st := newFileStore(t)
	require.NoError(t, st.Insert(ctx, sample()))
	require.NoError(t, st.Delete(ctx, "PROJ-350"))
	_, err := st.Get(ctx, "PROJ-350")
	require.ErrorIs(t, err, ErrNotFound)
	require.ErrorIs(t, st.Delete(ctx, "PROJ-350"), ErrNotFound, "deleting missing is ErrNotFound")
}

package store

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

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

func TestFileInsertAfterArchive(t *testing.T) {
	ctx := context.Background()
	st := newFileStore(t)
	require.NoError(t, st.Insert(ctx, sample()))
	require.NoError(t, st.Archive(ctx, "PROJ-350"))
	// The active file is gone, so the same id is insertable again.
	require.NoError(t, st.Insert(ctx, sample()), "id reusable once archived")
	got, err := st.Get(ctx, "PROJ-350")
	require.NoError(t, err)
	require.Equal(t, StatusSpawning, got.Status)
}

func TestFileArchiveOverwritesExistingClosed(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := NewFileStore(dir)
	require.NoError(t, err)

	// First lifecycle: insert (subject A) then archive.
	first := sample()
	first.Subject = "first run"
	require.NoError(t, st.Insert(ctx, first))
	require.NoError(t, st.Archive(ctx, "PROJ-350"))

	// Second lifecycle reuses the id with a different subject, then archives —
	// the closed record is overwritten with the latest (documented behavior).
	second := sample()
	second.Subject = "second run"
	require.NoError(t, st.Insert(ctx, second))
	require.NoError(t, st.Archive(ctx, "PROJ-350"))

	data, err := os.ReadFile(filepath.Join(dir, "closed", "PROJ-350.json"))
	require.NoError(t, err)
	require.Contains(t, string(data), "second run")
	require.NotContains(t, string(data), "first run")
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

func TestFileUpdateStatusBumpsUpdatedAt(t *testing.T) {
	ctx := context.Background()
	st := newFileStore(t)
	require.NoError(t, st.Insert(ctx, sample()))
	before, err := st.Get(ctx, "PROJ-350")
	require.NoError(t, err)

	time.Sleep(2 * time.Millisecond)
	require.NoError(t, st.UpdateStatus(ctx, "PROJ-350", StatusWorking))

	after, err := st.Get(ctx, "PROJ-350")
	require.NoError(t, err)
	require.Equal(t, StatusWorking, after.Status)
	require.True(t, after.UpdatedAt.After(before.UpdatedAt), "UpdateStatus must bump updated_at")
}

func TestFileUpdateStatusNotFound(t *testing.T) {
	require.ErrorIs(t, newFileStore(t).UpdateStatus(context.Background(), "nope", StatusWorking), ErrNotFound)
}

func TestFileUpdateStatusIf(t *testing.T) {
	ctx := context.Background()
	st := newFileStore(t)
	require.NoError(t, st.Insert(ctx, sample())) // status = spawning

	// No-op when expected doesn't match.
	ok, err := st.UpdateStatusIf(ctx, "PROJ-350", StatusWorking, StatusIdle)
	require.NoError(t, err)
	require.False(t, ok)
	got, _ := st.Get(ctx, "PROJ-350")
	require.Equal(t, StatusSpawning, got.Status, "non-matching CAS leaves status unchanged")

	// Swaps when expected matches.
	ok, err = st.UpdateStatusIf(ctx, "PROJ-350", StatusSpawning, StatusWorking)
	require.NoError(t, err)
	require.True(t, ok)
	got, _ = st.Get(ctx, "PROJ-350")
	require.Equal(t, StatusWorking, got.Status)

	// Missing doc is (false, nil), not an error — matches Mongo's filtered update.
	ok, err = st.UpdateStatusIf(ctx, "ghost", StatusSpawning, StatusWorking)
	require.NoError(t, err)
	require.False(t, ok)
}

func TestFileUpdateTypeAndSubjectAndPane(t *testing.T) {
	ctx := context.Background()
	st := newFileStore(t)
	require.NoError(t, st.Insert(ctx, sample()))
	require.NoError(t, st.UpdateType(ctx, "PROJ-350", TypeDevelopment))
	require.NoError(t, st.UpdateSubject(ctx, "PROJ-350", "review auth module"))
	require.NoError(t, st.UpdatePane(ctx, "PROJ-350", "esc to interrupt"))

	got, err := st.Get(ctx, "PROJ-350")
	require.NoError(t, err)
	require.Equal(t, TypeDevelopment, got.Type)
	require.Equal(t, "review auth module", got.Subject)
	require.Equal(t, "esc to interrupt", got.LastPaneExcerpt)
}

func TestFileAppendEvent(t *testing.T) {
	ctx := context.Background()
	st := newFileStore(t)
	require.NoError(t, st.Insert(ctx, sample()))
	require.NoError(t, st.AppendEvent(ctx, "PROJ-350", Event{Type: "Notification", Detail: "needs input"}))

	got, err := st.Get(ctx, "PROJ-350")
	require.NoError(t, err)
	require.Len(t, got.Events, 1)
	require.Equal(t, "Notification", got.Events[0].Type)
	require.False(t, got.Events[0].TS.IsZero(), "AppendEvent must stamp ts")
}

func TestFileAppendEventStatus(t *testing.T) {
	ctx := context.Background()
	st := newFileStore(t)
	require.NoError(t, st.Insert(ctx, sample()))
	require.NoError(t, st.AppendEventStatus(ctx, "PROJ-350",
		Event{Type: "Stop"}, StatusIdle))

	got, err := st.Get(ctx, "PROJ-350")
	require.NoError(t, err)
	require.Len(t, got.Events, 1)
	require.Equal(t, StatusIdle, got.Status, "AppendEventStatus sets status atomically with the event")

	// Empty status only appends.
	require.NoError(t, st.AppendEventStatus(ctx, "PROJ-350", Event{Type: "Note"}, ""))
	got, _ = st.Get(ctx, "PROJ-350")
	require.Len(t, got.Events, 2)
	require.Equal(t, StatusIdle, got.Status, "empty status leaves status unchanged")
}

func TestFileClearWorktree(t *testing.T) {
	ctx := context.Background()
	st := newFileStore(t)
	s := sample() // has Worktree + Branch set
	require.NoError(t, st.Insert(ctx, s))
	require.NoError(t, st.ClearWorktree(ctx, s.ID))
	got, err := st.Get(ctx, s.ID)
	require.NoError(t, err)
	require.Empty(t, got.Worktree, "worktree cleared")
	require.Empty(t, got.Branch, "branch cleared")
	require.ErrorIs(t, st.ClearWorktree(ctx, "nope"), ErrNotFound)
}

func TestSupervisedRoundTrips(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, fs.Insert(context.Background(), &Session{ID: "a1", Supervised: true}))
	got, err := fs.Get(context.Background(), "a1")
	require.NoError(t, err)
	require.True(t, got.Supervised)
}

func TestSafeID(t *testing.T) {
	require.NoError(t, SafeID("agent-1234"))
	require.NoError(t, SafeID("work"))
	require.Error(t, SafeID(""))
	require.Error(t, SafeID("a/b"))
	require.Error(t, SafeID("../etc"))
	// ":" is a tmux target separator (session:window); an id containing it
	// silently breaks `tmux -t <id>` targeting, so it is not a safe id.
	require.Error(t, SafeID("work:1"))
}

func TestFileConcurrentAccess(t *testing.T) {
	ctx := context.Background()
	st := newFileStore(t)
	require.NoError(t, st.Insert(ctx, sample()))

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = st.UpdateStatus(ctx, "PROJ-350", StatusWorking)
			_ = st.AppendEvent(ctx, "PROJ-350", Event{Type: "tick"})
			_, _ = st.List(ctx)
		}()
	}
	wg.Wait()

	got, err := st.Get(ctx, "PROJ-350")
	require.NoError(t, err)
	require.Len(t, got.Events, 20, "every concurrent AppendEvent must be durable (no lost writes)")
}

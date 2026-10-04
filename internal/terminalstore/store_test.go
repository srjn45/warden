package terminalstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func TestCRUD(t *testing.T) {
	s, err := New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	terminal := &Terminal{
		ID:          "terminal-1",
		ProjectID:   "project-1",
		TmuxSession: "term-pane",
		Status:      StatusRunning,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	require.NoError(t, s.Insert(ctx, terminal))
	got, err := s.Get(ctx, terminal.ID)
	require.NoError(t, err)
	require.Equal(t, terminal, got)
	require.NoError(t, s.Update(ctx, terminal.ID, func(terminal *Terminal) error {
		terminal.TmuxSession = "replacement-pane"
		return nil
	}))
	got, err = s.Get(ctx, terminal.ID)
	require.NoError(t, err)
	require.Equal(t, "replacement-pane", got.TmuxSession)
	list, err := s.List(ctx)
	require.NoError(t, err)
	require.Equal(t, []*Terminal{got}, list)
	require.NoError(t, s.Delete(ctx, terminal.ID))
	_, err = s.Get(ctx, terminal.ID)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestSpawnCreatesAndInitializesTerminal(t *testing.T) {
	s, err := New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	require.NoError(t, s.Spawn(context.Background(), &Terminal{ID: "terminal-spawn", ProjectID: "project"}, "tmux-terminal"))
	got, err := s.Get(context.Background(), "terminal-spawn")
	require.NoError(t, err)
	require.Equal(t, "tmux-terminal", got.TmuxSession)
	require.Equal(t, "project", got.ProjectID)
}

func TestMigratesOnlyActiveTerminalSessions(t *testing.T) {
	dir := t.TempDir()
	legacy, err := store.NewFileStore(dir)
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, legacy.Insert(ctx, &store.Session{ID: "agent-live", Name: "n-agent-live", Status: store.StatusWorking, ProjectID: "agent-project", TmuxSession: "agent-pane"}))
	require.NoError(t, legacy.Insert(ctx, &store.Session{ID: "terminal-closed", Name: "n-terminal-closed", Kind: store.KindTerminal, Status: store.StatusIdle, ProjectID: "old-project", TmuxSession: "old-pane"}))
	require.NoError(t, legacy.Archive(ctx, "terminal-closed"))
	require.NoError(t, legacy.Insert(ctx, &store.Session{ID: "terminal-current", Name: "n-terminal-current", Kind: store.KindTerminal, Status: store.StatusIdle, ProjectID: "project-1", TmuxSession: "term-pane"}))
	require.NoError(t, legacy.Close(ctx))

	terminals, err := New(dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, terminals.Close()) })
	list, err := terminals.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	got := list[0]
	require.Equal(t, "terminal-current", got.ID)
	require.Equal(t, "project-1", got.ProjectID)
	require.Equal(t, "term-pane", got.TmuxSession)
	require.Equal(t, StatusRunning, got.Status, "StatusIdle should map to StatusRunning")
}

func TestMigrationMarkerPreventsDuplicateImport(t *testing.T) {
	dir := t.TempDir()
	legacy, err := store.NewFileStore(dir)
	require.NoError(t, err)
	require.NoError(t, legacy.Insert(context.Background(), &store.Session{ID: "terminal-1", Name: "n-terminal-1", Kind: store.KindTerminal}))
	require.NoError(t, legacy.Close(context.Background()))
	s, err := New(dir)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	s, err = New(dir)
	require.NoError(t, err)
	require.NoError(t, s.Close())
}

// TestMigrationImportIdempotence verifies that re-opening the store after the
// marker is written yields exactly the same records — no duplicates appear even
// when the legacy store still contains terminal sessions.
func TestMigrationImportIdempotence(t *testing.T) {
	dir := t.TempDir()
	legacy, err := store.NewFileStore(dir)
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, legacy.Insert(ctx, &store.Session{
		ID: "terminal-idem-1", Name: "n-terminal-idem-1",
		Kind:        store.KindTerminal,
		Status:      store.StatusIdle,
		ProjectID:   "proj-idem",
		TmuxSession: "pane-idem",
	}))
	require.NoError(t, legacy.Insert(ctx, &store.Session{
		ID: "terminal-idem-2", Name: "n-terminal-idem-2",
		Kind:        store.KindTerminal,
		Status:      store.StatusIdle,
		ProjectID:   "proj-idem",
		TmuxSession: "pane-idem-2",
	}))
	require.NoError(t, legacy.Close(ctx))

	// First open: import runs, marker written.
	s1, err := New(dir)
	require.NoError(t, err)
	list1, err := s1.List(ctx)
	require.NoError(t, err)
	require.Len(t, list1, 2)
	require.NoError(t, s1.Close())

	// Second open: marker exists, import must NOT run again.
	s2, err := New(dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s2.Close()) })
	list2, err := s2.List(ctx)
	require.NoError(t, err)
	require.Len(t, list2, 2, "re-opening must not duplicate imported terminals")
	require.Equal(t, list1[0].ID, list2[0].ID)
	require.Equal(t, list1[1].ID, list2[1].ID)
}

// TestMigrationRestartSafety verifies that a partially-written terminals-db
// (marker absent) is fully discarded and cleanly re-imported on the next open.
func TestMigrationRestartSafety(t *testing.T) {
	dir := t.TempDir()
	legacy, err := store.NewFileStore(dir)
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, legacy.Insert(ctx, &store.Session{
		ID: "terminal-rs", Name: "n-terminal-rs",
		Kind:        store.KindTerminal,
		Status:      store.StatusIdle,
		ProjectID:   "proj-rs",
		TmuxSession: "pane-rs",
	}))
	require.NoError(t, legacy.Close(ctx))

	// Simulate a partially-written state: create the dbDir with junk, but
	// leave the marker absent so New() must redo the import.
	dbDir := filepath.Join(dir, "terminals-db")
	require.NoError(t, os.MkdirAll(dbDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dbDir, "junk.bin"), []byte("corrupt"), 0o600))
	// Marker deliberately absent.

	s, err := New(dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	list, err := s.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1, "import must complete cleanly after corrupt partial state is wiped")
	require.Equal(t, "terminal-rs", list[0].ID)
	require.Equal(t, StatusRunning, list[0].Status)

	// Marker must now exist to prevent another re-import.
	_, err = os.Stat(filepath.Join(dir, importedMarker))
	require.NoError(t, err, "marker must be written after successful import")
}

// TestTerminalNameCollision verifies that two terminals with the same Name but
// different IDs coexist without error; the Name field is not a unique key.
func TestTerminalNameCollision(t *testing.T) {
	s, err := New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	a := &Terminal{ID: "terminal-name-a", Name: "shared-name", ProjectID: "proj", TmuxSession: "pane-a", Status: StatusRunning, CreatedAt: now, UpdatedAt: now}
	b := &Terminal{ID: "terminal-name-b", Name: "shared-name", ProjectID: "proj", TmuxSession: "pane-b", Status: StatusRunning, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, s.Insert(ctx, a))
	require.NoError(t, s.Insert(ctx, b), "two terminals with the same Name must not collide (Name is not a unique key)")

	list, err := s.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 2)

	// Duplicate ID must still be rejected.
	dup := &Terminal{ID: "terminal-name-a", Name: "other-name", TmuxSession: "pane-dup"}
	require.ErrorIs(t, s.Insert(ctx, dup), ErrExists, "duplicate ID must be rejected")
}

// TestMigrationStatusMapping verifies that each legacy store.Status value maps
// to the correct terminal-specific Status.
func TestMigrationStatusMapping(t *testing.T) {
	cases := []struct {
		input store.Status
		want  Status
	}{
		{store.StatusWorking, StatusRunning},
		{store.StatusWaitingForInput, StatusRunning},
		{store.StatusIdle, StatusRunning},
		{store.StatusSpawning, StatusRunning},
		{store.StatusRateLimited, StatusRunning},
		{store.StatusDone, StatusExited},
		{store.StatusErrored, StatusExited},
		{store.StatusOrphaned, StatusOrphaned},
	}
	for _, tc := range cases {
		got := mapSessionStatus(tc.input)
		require.Equal(t, tc.want, got, "mapSessionStatus(%q)", tc.input)
	}
}

// TestMigrationFullFields verifies that all populated store.Session fields are
// carried into the Terminal record during import.
func TestMigrationFullFields(t *testing.T) {
	dir := t.TempDir()
	legacy, err := store.NewFileStore(dir)
	require.NoError(t, err)
	ctx := context.Background()
	exitCode := 0
	require.NoError(t, legacy.Insert(ctx, &store.Session{
		ID:          "terminal-full",
		Kind:        store.KindTerminal,
		Name:        "my-shell",
		Status:      store.StatusDone,
		ProjectID:   "proj-full",
		TmuxSession: "pane-full",
		Workdir:     "/home/user",
		PID:         12345,
		ExitCode:    &exitCode,
	}))
	require.NoError(t, legacy.Close(ctx))

	ts, err := New(dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ts.Close()) })
	got, err := ts.Get(ctx, "terminal-full")
	require.NoError(t, err)

	require.Equal(t, "terminal-full", got.ID)
	require.Equal(t, "proj-full", got.ProjectID)
	require.Equal(t, "my-shell", got.Name)
	require.Equal(t, "pane-full", got.TmuxSession)
	require.Equal(t, "/home/user", got.Workdir)
	require.Equal(t, 12345, got.PID)
	require.Equal(t, StatusExited, got.Status)
	require.NotNil(t, got.ExitCode)
	require.Equal(t, 0, *got.ExitCode)
	// The legacy store sets timestamps on Insert; we only verify they were carried
	// through (non-zero) rather than comparing exact wall-clock values.
	require.False(t, got.CreatedAt.IsZero(), "CreatedAt must be set by migration")
	require.False(t, got.UpdatedAt.IsZero(), "UpdatedAt must be set by migration")
}

package terminalstore

import (
	"context"
	"testing"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func TestCRUD(t *testing.T) {
	s, err := New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	ctx := context.Background()
	terminal := &Terminal{ID: "terminal-1", ProjectID: "project-1", TmuxSession: "term-pane"}
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

func TestMigratesOnlyActiveTerminalSessions(t *testing.T) {
	dir := t.TempDir()
	legacy, err := store.NewFileStore(dir)
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, legacy.Insert(ctx, &store.Session{ID: "agent-live", Status: store.StatusWorking, ProjectID: "agent-project", TmuxSession: "agent-pane"}))
	require.NoError(t, legacy.Insert(ctx, &store.Session{ID: "terminal-closed", Kind: store.KindTerminal, Status: store.StatusIdle, ProjectID: "old-project", TmuxSession: "old-pane"}))
	require.NoError(t, legacy.Archive(ctx, "terminal-closed"))
	require.NoError(t, legacy.Insert(ctx, &store.Session{ID: "terminal-current", Kind: store.KindTerminal, Status: store.StatusIdle, ProjectID: "project-1", TmuxSession: "term-pane"}))
	require.NoError(t, legacy.Close(ctx))

	terminals, err := New(dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, terminals.Close()) })
	list, err := terminals.List(ctx)
	require.NoError(t, err)
	require.Equal(t, []*Terminal{{ID: "terminal-current", ProjectID: "project-1", TmuxSession: "term-pane"}}, list)
}

func TestMigrationMarkerPreventsDuplicateImport(t *testing.T) {
	dir := t.TempDir()
	legacy, err := store.NewFileStore(dir)
	require.NoError(t, err)
	require.NoError(t, legacy.Insert(context.Background(), &store.Session{ID: "terminal-1", Kind: store.KindTerminal}))
	require.NoError(t, legacy.Close(context.Background()))
	s, err := New(dir)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	s, err = New(dir)
	require.NoError(t, err)
	require.NoError(t, s.Close())
}

package agentstore

import (
	"context"
	"testing"

	"github.com/srjn45/scriva"
	"github.com/srjn45/warden/internal/store"
	"github.com/srjn45/warden/internal/wardendb"
	"github.com/stretchr/testify/require"
)

func newSchema3Store(t *testing.T) *Store {
	t.Helper()
	db, err := wardendb.Open(t.TempDir())
	require.NoError(t, err)
	c, err := wardendb.Register[map[string]any](db, wardendb.Spec{Name: "projects", Version: 3, Options: []scriva.CollectionOption{scriva.WithUniqueIndex("id")}})
	require.NoError(t, err)
	projects, err := c.Engine()
	require.NoError(t, err)
	_, _, err = projects.InsertWithKey("p", map[string]any{"id": "p", "status": "open"})
	require.NoError(t, err)
	s, err := NewSchema3StoreFromDB(db)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()); require.NoError(t, db.Close()) })
	return s
}

func TestSchema3PurgeRemovesArchivedRow(t *testing.T) {
	ctx := context.Background()
	s := newSchema3Store(t)
	require.NoError(t, s.Insert(ctx, &Agent{ID: "a", Name: "a", ProjectID: "p", Status: store.StatusDone}))

	require.ErrorIs(t, s.Purge(ctx, "a"), ErrNotArchived, "a live agent is never purged")

	require.NoError(t, s.Archive(ctx, "a"))
	require.NoError(t, s.Purge(ctx, "a"))
	closed, err := s.ListClosed(ctx)
	require.NoError(t, err)
	require.Empty(t, closed)
	require.ErrorIs(t, s.Purge(ctx, "a"), ErrNotFound)
	// The identity is free again once its archive is gone.
	require.NoError(t, s.Insert(ctx, &Agent{ID: "a", Name: "a", ProjectID: "p", Status: store.StatusIdle}))
}

func TestSchema3PurgeKeepsReferencedParent(t *testing.T) {
	ctx := context.Background()
	s := newSchema3Store(t)
	require.NoError(t, s.Insert(ctx, &Agent{ID: "parent", Name: "parent", ProjectID: "p", Status: store.StatusDone}))
	require.NoError(t, s.Insert(ctx, &Agent{ID: "child", Name: "child", ProjectID: "p", ParentID: "parent", Status: store.StatusDone}))
	require.NoError(t, s.Archive(ctx, "parent"))
	require.NoError(t, s.Archive(ctx, "child"))

	require.ErrorIs(t, s.Purge(ctx, "parent"), ErrStillReferenced)
	require.NoError(t, s.Purge(ctx, "child"))
	require.NoError(t, s.Purge(ctx, "parent"), "the parent goes once its last child is purged")
}

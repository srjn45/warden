package agentstore

import (
	"context"
	"testing"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/query"
	"github.com/srjn45/warden/internal/store"
	"github.com/srjn45/warden/internal/wardendb"
	"github.com/stretchr/testify/require"
)

func TestSchema3AgentsArchiveInPlaceAndKeepImmutableOwners(t *testing.T) {
	ctx := context.Background()
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
	parent := &Agent{ID: "parent", Name: "parent", ProjectID: "p", Prompt: "full parent payload", Status: store.StatusIdle}
	require.NoError(t, s.Insert(ctx, parent))
	child := &Agent{ID: "child", Name: "child", ProjectID: "p", ParentID: "parent", Prompt: "full child payload", Role: "worker", Status: store.StatusWorking}
	require.NoError(t, s.Insert(ctx, child))
	for _, mutate := range []func(*Agent){func(a *Agent) { a.ProjectID = "other" }, func(a *Agent) { a.ParentID = "" }, func(a *Agent) { a.PlanID = "later-plan" }} {
		require.Error(t, s.Update(ctx, "child", func(a *Agent) error { mutate(a); return nil }))
	}
	require.NoError(t, s.Archive(ctx, "parent"))
	got, err := s.Get(ctx, "child")
	require.NoError(t, err)
	require.Equal(t, "parent", got.ParentID)
	closed, err := s.ListClosed(ctx)
	require.NoError(t, err)
	require.Len(t, closed, 1)
	require.Equal(t, "archived", closed[0].Lifecycle)
	require.NotNil(t, closed[0].ArchivedAt)
	require.Equal(t, "p", closed[0].ProjectID)
	require.Error(t, s.Insert(ctx, &Agent{ID: "parent", Name: "parent", ProjectID: "p", Status: store.StatusIdle}), "replacement cannot reuse an archived identity")
	col, err := wardendb.Lookup[agentRecord](db, "agents")
	require.NoError(t, err)
	eng, err := col.Engine()
	require.NoError(t, err)
	rows, err := eng.Scan(query.MatchAll)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	for _, row := range rows {
		for _, field := range []string{"parent_id", "child_agents", "child_pipelines", "child_autopilots"} {
			require.NotContains(t, row.Data, field)
		}
	}
	registered, err := db.Registered()
	require.NoError(t, err)
	for _, info := range registered {
		require.NotEqual(t, "archived_agents", info.Name)
	}
	require.Error(t, s.Insert(ctx, &Agent{ID: "missing-project", Name: "missing-project", Status: store.StatusIdle}))
	require.NoError(t, s.Insert(ctx, &Agent{ID: "explicit-global", Name: "explicit-global", Scope: "global", Status: store.StatusIdle}))
}

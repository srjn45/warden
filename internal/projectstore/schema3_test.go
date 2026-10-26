package projectstore

import (
	"testing"

	"github.com/srjn45/scriva"
	"github.com/srjn45/warden/internal/wardendb"
	"github.com/stretchr/testify/require"
)

func TestSchema3ProjectAndGroupStoreOnlyOwnFields(t *testing.T) {
	db, err := wardendb.Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	s, err := NewSchema3StoreFromDB(db)
	require.NoError(t, err)
	_, err = s.CreateGroup(ProjectGroup{ID: "g", Name: "group"})
	require.NoError(t, err)
	order := 0
	require.NoError(t, s.Upsert(Project{ID: "p", Name: "remote", GroupID: "g", GroupOrder: &order}))
	require.NoError(t, s.Upsert(Project{ID: "other", Name: "second remote"}), "empty paths must not collide")
	c, err := wardendb.Register[map[string]any](db, wardendb.Spec{Name: "agents", Version: 3, Options: []scriva.CollectionOption{scriva.WithUniqueIndex("id")}})
	require.NoError(t, err)
	agents, err := c.Engine()
	require.NoError(t, err)
	_, _, err = agents.InsertWithKey("child", map[string]any{"id": "child", "project_id": "p"})
	require.NoError(t, err)
	project, err := s.Get("p")
	require.NoError(t, err)
	require.Equal(t, []string{"child"}, project.Agents)
	group, err := s.GetGroup("g")
	require.NoError(t, err)
	require.Equal(t, []string{"p"}, group.ProjectIDs)
	projects, err := wardendb.Lookup[projectHeader](db, projectsCollection)
	require.NoError(t, err)
	projectEng, err := projects.Engine()
	require.NoError(t, err)
	raw, err := projectEng.GetByKey("p")
	require.NoError(t, err)
	for _, field := range []string{"agents", "pipelines", "terminals", "plans", "autopilots"} {
		require.NotContains(t, raw.Data, field)
	}
	groups, err := wardendb.Lookup[groupHeader](db, groupsCollection)
	require.NoError(t, err)
	groupEng, err := groups.Engine()
	require.NoError(t, err)
	raw, err = groupEng.GetByKey("g")
	require.NoError(t, err)
	require.NotContains(t, raw.Data, "project_ids")
	project.GroupID, project.GroupOrder = "", nil
	require.Error(t, s.Upsert(project))
	require.ErrorIs(t, s.Delete("p"), wardendb.ErrReferenced)
	require.Error(t, s.DeleteGroup("g"))
}

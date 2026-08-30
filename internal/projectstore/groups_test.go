package projectstore

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCreateGetListGroup(t *testing.T) {
	s := newTestStore(t)

	_, err := s.GetGroup("missing")
	require.ErrorIs(t, err, ErrGroupNotFound)

	// A blank id is minted; name is required.
	g, err := s.CreateGroup(ProjectGroup{Name: "Backend", ProjectIDs: []string{"/repos/a", "/repos/b"}})
	require.NoError(t, err)
	require.NotEmpty(t, g.ID)
	require.Equal(t, "Backend", g.Name)
	require.Equal(t, []string{"/repos/a", "/repos/b"}, g.ProjectIDs)
	require.False(t, g.CreatedAt.IsZero())
	require.False(t, g.UpdatedAt.IsZero())

	got, err := s.GetGroup(g.ID)
	require.NoError(t, err)
	require.Equal(t, g.Name, got.Name)
	require.Equal(t, g.ProjectIDs, got.ProjectIDs)

	// Second group; List is sorted by name.
	_, err = s.CreateGroup(ProjectGroup{Name: "Apps"})
	require.NoError(t, err)
	list, err := s.ListGroups()
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, "Apps", list[0].Name)
	require.Equal(t, "Backend", list[1].Name)
}

func TestCreateGroupValidation(t *testing.T) {
	s := newTestStore(t)
	_, err := s.CreateGroup(ProjectGroup{Name: ""})
	require.ErrorIs(t, err, ErrInvalidGroupName)

	// A supplied id that already exists is rejected.
	_, err = s.CreateGroup(ProjectGroup{ID: "g1", Name: "One"})
	require.NoError(t, err)
	_, err = s.CreateGroup(ProjectGroup{ID: "g1", Name: "Dup"})
	require.ErrorIs(t, err, ErrGroupExists)
}

func TestCreateGroupDedupesProjectIDs(t *testing.T) {
	s := newTestStore(t)
	g, err := s.CreateGroup(ProjectGroup{Name: "G", ProjectIDs: []string{"a", "", "a", "b", "b"}})
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b"}, g.ProjectIDs)
}

func TestUpdateGroup(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return base }
	g, err := s.CreateGroup(ProjectGroup{Name: "Old", ProjectIDs: []string{"a"}})
	require.NoError(t, err)

	s.now = func() time.Time { return base.Add(time.Hour) }
	updated, err := s.UpdateGroup(ProjectGroup{ID: g.ID, Name: "New", ProjectIDs: []string{"a", "a", "c"}})
	require.NoError(t, err)
	require.Equal(t, "New", updated.Name)
	require.Equal(t, []string{"a", "c"}, updated.ProjectIDs)
	require.True(t, updated.CreatedAt.Equal(g.CreatedAt)) // preserved
	require.True(t, updated.UpdatedAt.After(g.UpdatedAt)) // advanced

	// Persisted.
	got, err := s.GetGroup(g.ID)
	require.NoError(t, err)
	require.Equal(t, "New", got.Name)
}

func TestUpdateGroupErrors(t *testing.T) {
	s := newTestStore(t)
	_, err := s.UpdateGroup(ProjectGroup{ID: "nope", Name: "X"})
	require.ErrorIs(t, err, ErrGroupNotFound)

	g, err := s.CreateGroup(ProjectGroup{Name: "G"})
	require.NoError(t, err)
	_, err = s.UpdateGroup(ProjectGroup{ID: g.ID, Name: ""})
	require.ErrorIs(t, err, ErrInvalidGroupName)
}

func TestDeleteGroupIdempotent(t *testing.T) {
	s := newTestStore(t)
	g, err := s.CreateGroup(ProjectGroup{Name: "G"})
	require.NoError(t, err)
	require.NoError(t, s.DeleteGroup(g.ID))
	_, err = s.GetGroup(g.ID)
	require.ErrorIs(t, err, ErrGroupNotFound)
	// Deleting an absent group is not an error.
	require.NoError(t, s.DeleteGroup(g.ID))
}

func TestAddRemoveProjectFromGroup(t *testing.T) {
	s := newTestStore(t)
	g, err := s.CreateGroup(ProjectGroup{Name: "G", ProjectIDs: []string{"a"}})
	require.NoError(t, err)

	// Add a new member, then a duplicate (no-op).
	g2, err := s.AddProjectToGroup(g.ID, "b")
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b"}, g2.ProjectIDs)
	g3, err := s.AddProjectToGroup(g.ID, "a")
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b"}, g3.ProjectIDs)

	// Remove a member, and removing an absent member is a no-op.
	g4, err := s.RemoveProjectFromGroup(g.ID, "a")
	require.NoError(t, err)
	require.Equal(t, []string{"b"}, g4.ProjectIDs)
	g5, err := s.RemoveProjectFromGroup(g.ID, "zzz")
	require.NoError(t, err)
	require.Equal(t, []string{"b"}, g5.ProjectIDs)

	// Persisted across a fresh Get.
	got, err := s.GetGroup(g.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"b"}, got.ProjectIDs)
}

func TestAddRemoveProjectGroupErrors(t *testing.T) {
	s := newTestStore(t)
	_, err := s.AddProjectToGroup("nope", "a")
	require.ErrorIs(t, err, ErrGroupNotFound)
	_, err = s.RemoveProjectFromGroup("nope", "a")
	require.ErrorIs(t, err, ErrGroupNotFound)

	g, err := s.CreateGroup(ProjectGroup{Name: "G"})
	require.NoError(t, err)
	_, err = s.AddProjectToGroup(g.ID, "")
	require.ErrorIs(t, err, ErrInvalidID)
}

func TestGroupsPersistAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	require.NoError(t, err)
	g, err := s.CreateGroup(ProjectGroup{Name: "Persisted", ProjectIDs: []string{"p1"}})
	require.NoError(t, err)
	require.NoError(t, s.Close())

	s2, err := NewStore(dir)
	require.NoError(t, err)
	t.Cleanup(func() { s2.Close() })
	got, err := s2.GetGroup(g.ID)
	require.NoError(t, err)
	require.Equal(t, "Persisted", got.Name)
	require.Equal(t, []string{"p1"}, got.ProjectIDs)
}

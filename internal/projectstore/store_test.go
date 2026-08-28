package projectstore

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

func TestUpsertGetList(t *testing.T) {
	s := newTestStore(t)

	_, err := s.Get("/repos/alpha")
	require.ErrorIs(t, err, ErrNotFound)

	require.NoError(t, s.Upsert(Project{ID: "/repos/alpha", Name: "Alpha", Path: "/repos/alpha"}))
	require.NoError(t, s.Upsert(Project{ID: "https://github.com/x/beta", Name: "Beta"}))

	got, err := s.Get("/repos/alpha")
	require.NoError(t, err)
	require.Equal(t, "Alpha", got.Name)
	require.Equal(t, "/repos/alpha", got.Path)
	// Empty status normalizes to open.
	require.Equal(t, StatusOpen, got.Status)
	require.False(t, got.CreatedAt.IsZero())
	require.False(t, got.UpdatedAt.IsZero())

	// Sorted by name: Alpha before Beta.
	list, err := s.List()
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, "Alpha", list[0].Name)
	require.Equal(t, "Beta", list[1].Name)
}

func TestUpsertEmptyID(t *testing.T) {
	s := newTestStore(t)
	require.ErrorIs(t, s.Upsert(Project{Name: "no id"}), ErrInvalidID)
}

func TestUpsertUpdatesInPlacePreservingCreatedAt(t *testing.T) {
	s := newTestStore(t)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return base }
	require.NoError(t, s.Upsert(Project{ID: "p1", Name: "First"}))
	first, err := s.Get("p1")
	require.NoError(t, err)

	// Advance the clock and update in place.
	s.now = func() time.Time { return base.Add(time.Hour) }
	require.NoError(t, s.Upsert(Project{ID: "p1", Name: "Renamed"}))

	got, err := s.Get("p1")
	require.NoError(t, err)
	require.Equal(t, "Renamed", got.Name)
	// CreatedAt preserved, UpdatedAt advanced.
	require.True(t, got.CreatedAt.Equal(first.CreatedAt))
	require.True(t, got.UpdatedAt.After(first.UpdatedAt))

	// No duplicate row.
	list, err := s.List()
	require.NoError(t, err)
	require.Len(t, list, 1)
}

func TestOpenProjectInsertsAndReopens(t *testing.T) {
	s := newTestStore(t)

	// First open inserts an open row.
	p, err := s.OpenProject("/repos/gamma", "Gamma", "/repos/gamma")
	require.NoError(t, err)
	require.Equal(t, StatusOpen, p.Status)
	require.Equal(t, "Gamma", p.Name)

	// Close it (hibernate).
	closed, err := s.CloseProject("/repos/gamma")
	require.NoError(t, err)
	require.Equal(t, StatusClosed, closed.Status)

	// Reopen by id alone must NOT blank the stored name/path, and flips to open.
	reopened, err := s.OpenProject("/repos/gamma", "", "")
	require.NoError(t, err)
	require.Equal(t, StatusOpen, reopened.Status)
	require.Equal(t, "Gamma", reopened.Name)
	require.Equal(t, "/repos/gamma", reopened.Path)

	// Reopen with a new name overwrites it.
	renamed, err := s.OpenProject("/repos/gamma", "Gamma2", "")
	require.NoError(t, err)
	require.Equal(t, "Gamma2", renamed.Name)
	require.Equal(t, "/repos/gamma", renamed.Path) // path untouched by empty arg
}

func TestOpenProjectEmptyID(t *testing.T) {
	s := newTestStore(t)
	_, err := s.OpenProject("", "x", "y")
	require.ErrorIs(t, err, ErrInvalidID)
}

func TestCloseProjectNotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.CloseProject("nope")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestDeleteIdempotent(t *testing.T) {
	s := newTestStore(t)
	require.NoError(t, s.Upsert(Project{ID: "p1", Name: "One"}))
	require.NoError(t, s.Delete("p1"))
	_, err := s.Get("p1")
	require.ErrorIs(t, err, ErrNotFound)
	// Deleting an absent row is not an error.
	require.NoError(t, s.Delete("p1"))
}

func TestPersistenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	require.NoError(t, err)
	_, err = s.OpenProject("p1", "Persisted", "/p1")
	require.NoError(t, err)
	require.NoError(t, s.Close())

	// Reopen the DB and confirm the row survived.
	s2, err := NewStore(dir)
	require.NoError(t, err)
	t.Cleanup(func() { s2.Close() })
	got, err := s2.Get("p1")
	require.NoError(t, err)
	require.Equal(t, "Persisted", got.Name)
	require.Equal(t, StatusOpen, got.Status)
}

func TestStatusValidAndNormalize(t *testing.T) {
	require.True(t, StatusOpen.Valid())
	require.True(t, StatusClosed.Valid())
	require.False(t, Status("bogus").Valid())

	require.Equal(t, StatusOpen, NormalizeStatus(""))
	require.Equal(t, StatusOpen, NormalizeStatus("bogus"))
	require.Equal(t, StatusClosed, NormalizeStatus(StatusClosed))
}

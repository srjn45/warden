package projectstore

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTouchAndListOrdersMostRecentFirst(t *testing.T) {
	s, err := NewStore(t.TempDir())
	require.NoError(t, err)
	defer s.Close()

	require.NoError(t, s.Touch(Recent{Key: "github.com/org/a", Name: "a", Path: "/work/a"}))
	time.Sleep(2 * time.Millisecond)
	require.NoError(t, s.Touch(Recent{Key: "github.com/org/b", Name: "b", Path: "/work/b"}))

	list, err := s.List()
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, "github.com/org/b", list[0].Key, "most-recently-touched sorts first")
	require.Equal(t, "github.com/org/a", list[1].Key)
	require.False(t, list[0].LastOpened.IsZero(), "Touch stamps LastOpened")
}

func TestTouchUpsertsInPlace(t *testing.T) {
	s, err := NewStore(t.TempDir())
	require.NoError(t, err)
	defer s.Close()

	require.NoError(t, s.Touch(Recent{Key: "local:/work/x", Name: "x", Path: "/work/x"}))
	require.NoError(t, s.Touch(Recent{Key: "local:/work/x", Name: "x-renamed", Path: "/work/x2"}))

	list, err := s.List()
	require.NoError(t, err)
	require.Len(t, list, 1, "re-touching the same key updates in place, not appends")
	require.Equal(t, "x-renamed", list[0].Name, "display name is refreshed on re-touch")
	require.Equal(t, "/work/x2", list[0].Path, "path is refreshed on re-touch")
}

func TestTouchRejectsEmptyKey(t *testing.T) {
	s, err := NewStore(t.TempDir())
	require.NoError(t, err)
	defer s.Close()
	require.Error(t, s.Touch(Recent{Name: "no-key", Path: "/work/x"}))
}

func TestListPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	require.NoError(t, err)
	require.NoError(t, s.Touch(Recent{Key: "github.com/org/a", Name: "a", Path: "/work/a", Remote: "git@github.com:org/a.git"}))
	require.NoError(t, s.Close())

	s2, err := NewStore(dir)
	require.NoError(t, err)
	defer s2.Close()
	list, err := s2.List()
	require.NoError(t, err)
	require.Len(t, list, 1, "recent list survives a store reopen")
	require.Equal(t, "git@github.com:org/a.git", list[0].Remote)
}

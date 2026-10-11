package projectstore

import (
	"os"
	"testing"

	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/warden/internal/wardendb"
	"github.com/stretchr/testify/require"
)

// useShared switches openTestStore to the shared-handle implementation. TestMain
// runs the whole behavioural suite once per implementation, so the shared port
// is held to exactly the legacy store's tests.
var useShared bool

func TestMain(m *testing.M) {
	if code := m.Run(); code != 0 {
		os.Exit(code)
	}
	useShared = true
	os.Exit(m.Run())
}

// openTestStore opens the store under test at dir; the returned store's Close
// also releases the shared handle in shared mode so tests can reopen dir.
func openTestStore(t *testing.T, dir string) (*Store, error) {
	t.Helper()
	if !useShared {
		return NewStore(dir)
	}
	db, err := wardendb.Open(dir)
	if err != nil {
		return nil, err
	}
	s, err := NewStoreFromDB(db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	s.release = db.Close
	return s, nil
}

func TestSharedCollectionConfigParity(t *testing.T) {
	db, err := wardendb.Open(t.TempDir())
	require.NoError(t, err)
	defer db.Close()
	s, err := NewStoreFromDB(db)
	require.NoError(t, err)

	// Same durability as the legacy store, enforced by the strict policy.
	legacy := newLegacy(t)
	for name, shared := range map[string]docs{projectsCollection: s.col, groupsCollection: s.groups} {
		cfg := shared.Config()
		require.Equal(t, engine.SyncModeAlways, cfg.SyncMode, name)
		require.Equal(t, legacy.col.Config().SyncMode, cfg.SyncMode, name)
	}

	// Collections are registered on the handle with contract names/versions.
	infos, err := db.Registered()
	require.NoError(t, err)
	require.Len(t, infos, 2)
	for i, want := range CollectionSpecs() {
		require.Equal(t, want.Name, infos[i].Name)
		require.Equal(t, want.Version, infos[i].Version)
	}

	// Limits match the legacy store (unlimited) and every declared index can
	// be (re-)ensured on the registered collections.
	pc, err := wardendb.Lookup[Project](db, projectsCollection)
	require.NoError(t, err)
	gc, err := wardendb.Lookup[ProjectGroup](db, groupsCollection)
	require.NoError(t, err)
	for name, get := range map[string]func() (*engine.Collection, error){projectsCollection: pc.Engine, groupsCollection: gc.Engine} {
		eng, err := get()
		require.NoError(t, err)
		require.Zero(t, eng.Config().MaxRecords, name)
		require.Zero(t, eng.Config().MaxBytes, name)
		for _, f := range CollectionIndexes()[name] {
			require.NoError(t, eng.EnsureIndex(f), name+"."+f)
		}
	}
	// A second registration of the same collections is refused, not shadowed.
	_, err = NewStoreFromDB(db)
	require.ErrorIs(t, err, wardendb.ErrDuplicateCollection)
}

func TestSharedStoreDoesNotCloseHandle(t *testing.T) {
	db, err := wardendb.Open(t.TempDir())
	require.NoError(t, err)
	s, err := NewStoreFromDB(db)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	_, err = s.OpenProject("p", "P", "/p") // handle still usable
	require.NoError(t, err)
	require.NoError(t, db.Close())
	_, err = s.Get("p")
	require.ErrorIs(t, err, wardendb.ErrClosed)
}

func newLegacy(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

package agentstore

import (
	"os"
	"testing"

	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/warden/internal/wardendb"
	"github.com/stretchr/testify/require"
)

// useShared switches newTestStore to the shared-handle implementation. TestMain
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

// newTestStore opens the store under test at dir; in shared mode the returned
// store's Close also releases the handle so tests can reopen dir.
func newTestStore(dir string) (*Store, error) {
	if !useShared {
		return New(dir)
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

// skipShared skips tests of the legacy on-disk path (agents-db layout, ownership
// lock, legacy sessions import, offline verify/repair), which the shared
// constructor deliberately does not own.
func skipShared(t *testing.T) {
	t.Helper()
	if useShared {
		t.Skip("legacy on-disk path; not part of the shared constructor")
	}
}

func TestSharedCollectionConfigParity(t *testing.T) {
	db, err := wardendb.Open(t.TempDir())
	require.NoError(t, err)
	defer db.Close()
	s, err := NewStoreFromDB(db)
	require.NoError(t, err)
	defer s.Close()

	legacy, err := New(t.TempDir())
	require.NoError(t, err)
	defer legacy.Close()

	for name, pair := range map[string][2]docs{
		agentsCollection:         {s.col, legacy.col},
		archivedAgentsCollection: {s.closed, legacy.closed},
	} {
		sc, lc := pair[0].Config(), pair[1].Config()
		require.Equal(t, engine.SyncModeAlways, sc.SyncMode, name)
		require.Equal(t, lc.SyncMode, sc.SyncMode, name)
		require.Equal(t, lc.MaxRecords, sc.MaxRecords, name)
		require.Equal(t, lc.MaxBytes, sc.MaxBytes, name)
	}

	infos, err := db.Registered()
	require.NoError(t, err)
	require.Len(t, infos, 2)
	for i, want := range CollectionSpecs() {
		require.Equal(t, want.Name, infos[i].Name)
		require.Equal(t, want.Version, infos[i].Version)
	}

	for name := range CollectionIndexes() {
		c, err := wardendb.Lookup[Agent](db, name)
		require.NoError(t, err)
		eng, err := c.Engine()
		require.NoError(t, err)
		for _, f := range CollectionIndexes()[name] {
			require.NoError(t, eng.EnsureIndex(f), name+"."+f)
		}
	}

	_, err = NewStoreFromDB(db)
	require.ErrorIs(t, err, wardendb.ErrDuplicateCollection)
}

func TestSharedStoreDoesNotCloseHandle(t *testing.T) {
	db, err := wardendb.Open(t.TempDir())
	require.NoError(t, err)
	s, err := NewStoreFromDB(db)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	_, err = s.col.Count(nil) // handle still usable
	require.NoError(t, err)
	require.NoError(t, db.Close())
	_, err = s.col.Count(nil)
	require.ErrorIs(t, err, wardendb.ErrClosed)
}

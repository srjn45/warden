package schedule

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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

// openTestStore opens the store under test at path; in shared mode the returned
// store's Close also releases the handle so tests can reopen the same dir.
func openTestStore(t *testing.T, path string) (*Store, error) {
	t.Helper()
	if !useShared {
		return NewStore(path)
	}
	db, err := wardendb.Open(filepath.Dir(path))
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

	// The strict policy forces SyncModeAlways on every shared collection.
	for name, shared := range map[string]docs{collectionName: s.col} {
		cfg := shared.Config()
		require.Equal(t, engine.SyncModeAlways, cfg.SyncMode, name)
		require.Zero(t, cfg.MaxRecords, name)
		require.Zero(t, cfg.MaxBytes, name)
	}

	infos, err := db.Registered()
	require.NoError(t, err)
	require.Len(t, infos, len(CollectionSpecs()))
	for i, want := range CollectionSpecs() {
		require.Equal(t, want.Name, infos[i].Name)
		require.Equal(t, want.Version, infos[i].Version)
	}

	// Every declared index can be (re-)ensured on the registered collection.
	c, err := wardendb.Lookup[Schedule](db, collectionName)
	require.NoError(t, err)
	eng, err := c.Engine()
	require.NoError(t, err)
	for _, f := range CollectionIndexes()[collectionName] {
		require.NoError(t, eng.EnsureIndex(f), f)
	}

	// A second registration of the same collection is refused, not shadowed.
	_, err = NewStoreFromDB(db)
	require.ErrorIs(t, err, wardendb.ErrDuplicateCollection)
}

func TestSharedStoreDoesNotCloseHandle(t *testing.T) {
	db, err := wardendb.Open(t.TempDir())
	require.NoError(t, err)
	s, err := NewStoreFromDB(db)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	sc, err := New(Params{Name: "n", Cron: "0 9 * * *", Prompt: "go"}, time.Now())
	require.NoError(t, err)
	require.NoError(t, s.Create(sc))
	require.NoError(t, db.Close())
}

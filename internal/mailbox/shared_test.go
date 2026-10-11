package mailbox

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

// openTestStore opens the store under test at dir; in shared mode the returned
// store's Close also releases the handle so tests can reopen the same dir.
func openTestStore(t *testing.T, dir string) (*Store, error) {
	t.Helper()
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
	s.dir = dir // test-only: lets TestAllIgnoresTempFiles drop a stray file
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
	c, err := wardendb.Lookup[Message](db, collectionName)
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
	_, err = s.Append(Message{To: "a", From: "me", Body: "hi"})
	require.NoError(t, err)
	require.NoError(t, db.Close())
}

package terminalstore

import (
	"context"
	"os"
	"testing"

	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/warden/internal/wardendb"
	"github.com/stretchr/testify/require"
)

// useShared switches newTestStore to the shared-handle implementation. TestMain
// runs the whole behavioural suite once per implementation.
var useShared bool

func TestMain(m *testing.M) {
	if code := m.Run(); code != 0 {
		os.Exit(code)
	}
	useShared = true
	os.Exit(m.Run())
}

// newTestStore opens the store under test at dir; in shared mode Close also
// releases the handle so tests can reopen dir.
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

func TestSharedCollectionConfigParity(t *testing.T) {
	db, err := wardendb.Open(t.TempDir())
	require.NoError(t, err)
	defer db.Close()
	s, err := NewStoreFromDB(db)
	require.NoError(t, err)

	cfg := s.col.Config()
	require.Equal(t, engine.SyncModeAlways, cfg.SyncMode)
	require.Zero(t, cfg.MaxRecords)
	require.Zero(t, cfg.MaxBytes)

	infos, err := db.Registered()
	require.NoError(t, err)
	require.Len(t, infos, 1)
	require.Equal(t, terminalsCollection, infos[0].Name)
	require.Equal(t, recordSchemaVersion, infos[0].Version)

	tc, err := wardendb.Lookup[Terminal](db, terminalsCollection)
	require.NoError(t, err)
	eng, err := tc.Engine()
	require.NoError(t, err)
	for _, f := range CollectionIndexes()[terminalsCollection] {
		require.NoError(t, eng.EnsureIndex(f), f)
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
	_, err = s.List(context.Background())
	require.NoError(t, err) // handle still usable
	require.NoError(t, db.Close())
	_, err = s.List(context.Background())
	require.ErrorIs(t, err, wardendb.ErrClosed)
}

// skipShared skips legacy-directory migration tests: the shared constructor has
// no import step by design (migration readers stay on the legacy path).
func skipShared(t *testing.T) {
	t.Helper()
	if useShared {
		t.Skip("legacy-directory import is not part of the shared constructor")
	}
}

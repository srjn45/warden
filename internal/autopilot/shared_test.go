package autopilot

import (
	"testing"

	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/warden/internal/wardendb"
	"github.com/stretchr/testify/require"
)

// useShared switches openTestRunStore to the shared-handle implementation.
// TestMain (limits_test.go) runs the whole behavioural suite once per
// implementation.
var useShared bool

// openTestRunStore opens the RunStore under test at dir; in shared mode its
// Close also releases the handle so tests can reopen dir.
func openTestRunStore(dir string) (*RunStore, error) {
	if !useShared {
		return NewRunStore(dir)
	}
	db, err := wardendb.Open(dir)
	if err != nil {
		return nil, err
	}
	s, err := NewRunStoreFromDB(db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	s.release = db.Close
	return s, nil
}

func TestSharedRunCollectionConfigParity(t *testing.T) {
	db, err := wardendb.Open(t.TempDir())
	require.NoError(t, err)
	defer db.Close()
	s, err := NewRunStoreFromDB(db)
	require.NoError(t, err)

	legacy, err := NewRunStore(t.TempDir())
	require.NoError(t, err)
	defer legacy.Close()
	cfg := s.col.Config()
	require.Equal(t, engine.SyncModeAlways, cfg.SyncMode)
	require.Equal(t, legacy.col.Config().SyncMode, cfg.SyncMode)

	infos, err := db.Registered()
	require.NoError(t, err)
	require.Len(t, infos, len(RunCollectionSpecs()))
	for i, want := range RunCollectionSpecs() {
		require.Equal(t, want.Name, infos[i].Name)
		require.Equal(t, want.Version, infos[i].Version)
	}

	col, err := wardendb.Lookup[RunRecord](db, runsCollection)
	require.NoError(t, err)
	eng, err := col.Engine()
	require.NoError(t, err)
	require.Zero(t, eng.Config().MaxRecords)
	require.Zero(t, eng.Config().MaxBytes)
	for _, f := range RunCollectionIndexes()[runsCollection] {
		require.NoError(t, eng.EnsureIndex(f), f)
	}
	_, err = NewRunStoreFromDB(db)
	require.ErrorIs(t, err, wardendb.ErrDuplicateCollection)
}

func TestSharedRunStoreDoesNotCloseHandle(t *testing.T) {
	db, err := wardendb.Open(t.TempDir())
	require.NoError(t, err)
	s, err := NewRunStoreFromDB(db)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	require.NoError(t, s.Create(RunRecord{RunID: "ap-x", Name: "x"}))
	require.NoError(t, db.Close())
	_, err = s.Get("ap-x")
	require.ErrorIs(t, err, wardendb.ErrClosed)
}

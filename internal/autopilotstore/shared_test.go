package autopilotstore

import (
	"context"
	"os"
	"testing"

	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/warden/internal/wardendb"
	"github.com/stretchr/testify/require"
)

// useShared switches openTestStore to the shared-handle implementation.
// TestMain runs the whole behavioural suite once per implementation.
var useShared bool

func TestMain(m *testing.M) {
	if code := m.Run(); code != 0 {
		os.Exit(code)
	}
	useShared = true
	os.Exit(m.Run())
}

// openTestStore opens the RunStore under test at dir; in shared mode its
// Close also releases the handle so tests can reopen dir.
func openTestStore(dir string) (*Store, error) {
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

	legacy, err := New(t.TempDir())
	require.NoError(t, err)
	defer legacy.Close()
	cfg := s.col.Config()
	require.Equal(t, engine.SyncModeAlways, cfg.SyncMode)
	require.Equal(t, legacy.col.Config().SyncMode, cfg.SyncMode)

	infos, err := db.Registered()
	require.NoError(t, err)
	require.Len(t, infos, len(CollectionSpecs()))
	for i, want := range CollectionSpecs() {
		require.Equal(t, want.Name, infos[i].Name)
		require.Equal(t, want.Version, infos[i].Version)
	}

	col, err := wardendb.Lookup[Autopilot](db, autopilotsCollection)
	require.NoError(t, err)
	eng, err := col.Engine()
	require.NoError(t, err)
	require.Zero(t, eng.Config().MaxRecords)
	require.Zero(t, eng.Config().MaxBytes)
	for _, f := range CollectionIndexes()[autopilotsCollection] {
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
	require.NoError(t, s.Create(context.Background(), &Autopilot{ID: "ap-x", PlanID: "p"}))
	require.NoError(t, db.Close())
	_, err = s.Get(context.Background(), "ap-x")
	require.ErrorIs(t, err, wardendb.ErrClosed)
}

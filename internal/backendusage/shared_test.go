package backendusage

import (
	"os"
	"sync"
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

var (
	openMu  sync.Mutex
	openDBs = map[string]*wardendb.DB{}
)

// openTestStore opens the store under test at dir. In shared mode a second open
// of the same dir stands in for a daemon restart: the previous handle is closed
// first and the new store sees the data the old one persisted.
func openTestStore(t *testing.T, dir string) (*SnapshotStore, error) {
	t.Helper()
	if !useShared {
		return NewSnapshotStore(dir)
	}
	openMu.Lock()
	defer openMu.Unlock()
	if old := openDBs[dir]; old != nil {
		_ = old.Close()
		delete(openDBs, dir)
	}
	require.NoError(t, os.MkdirAll(dir, 0o700))
	db, err := wardendb.Open(dir)
	if err != nil {
		return nil, err
	}
	s, err := NewSnapshotStoreFromDB(db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	openDBs[dir] = db
	t.Cleanup(func() {
		openMu.Lock()
		defer openMu.Unlock()
		if openDBs[dir] == db {
			_ = db.Close()
			delete(openDBs, dir)
		}
	})
	return s, nil
}

func TestSharedCollectionConfigParity(t *testing.T) {
	db, err := wardendb.Open(t.TempDir())
	require.NoError(t, err)
	defer db.Close()
	_, err = NewSnapshotStoreFromDB(db)
	require.NoError(t, err)

	// The strict policy forces SyncModeAlways on every shared collection.
	infos, err := db.Registered()
	require.NoError(t, err)
	require.Len(t, infos, len(CollectionSpecs()))
	for i, want := range CollectionSpecs() {
		require.Equal(t, want.Name, infos[i].Name)
		require.Equal(t, want.Version, infos[i].Version)
		c, err := wardendb.Lookup[UsageSnapshot](db, want.Name)
		require.NoError(t, err)
		eng, err := c.Engine()
		require.NoError(t, err)
		cfg := eng.Config()
		require.Equal(t, engine.SyncModeAlways, cfg.SyncMode, want.Name)
		require.Zero(t, cfg.MaxRecords, want.Name)
		require.Zero(t, cfg.MaxBytes, want.Name)
	}

	// A second registration of the same collections is refused, not shadowed.
	_, err = NewSnapshotStoreFromDB(db)
	require.ErrorIs(t, err, wardendb.ErrDuplicateCollection)
}

func TestSharedStoreDoesNotCloseHandle(t *testing.T) {
	db, err := wardendb.Open(t.TempDir())
	require.NoError(t, err)
	_, err = NewSnapshotStoreFromDB(db)
	require.NoError(t, err)
	require.NoError(t, db.Close())
}

func TestSnapshotStoreRetentionPrunesOldObservations(t *testing.T) {
	store, err := openTestStore(t, t.TempDir())
	require.NoError(t, err)
	d := CapacityDomain{Provider: "codex", ProfileFingerprint: "p"}
	old := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	_, err = store.Record(UsageSnapshot{Domain: d, ObservedAt: old, RecordedAt: old, Authoritative: true})
	require.NoError(t, err)
	now := old.Add(SnapshotRetention + time.Hour)
	_, err = store.Record(UsageSnapshot{Domain: d, ObservedAt: now, RecordedAt: now, Authoritative: true})
	require.NoError(t, err)
	all, err := store.readLocked()
	require.NoError(t, err)
	require.Len(t, all, 1)
	require.EqualValues(t, 2, all[0].Revision)
}

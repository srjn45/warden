package planstore

import (
	"context"
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

	// Same durability as the legacy plans store; the strict policy forces
	// SyncModeAlways on every shared collection.
	legacy := newLegacy(t)
	pairs := map[string][2]docs{
		plansCollection:  {s.col, legacy.col},
		eventsCollection: {s.events, legacy.events},
		notesCollection:  {s.notes, legacy.notes},
	}
	for name, p := range pairs {
		cfg, lcfg := p[0].Config(), p[1].Config()
		require.Equal(t, engine.SyncModeAlways, cfg.SyncMode, name)
		require.Equal(t, lcfg.SyncMode, cfg.SyncMode, name)
		// WithSegmentMaxSize is expressible per collection. The legacy 2m
		// WithCompactInterval is NOT: the strict policy owns the cadence.
		require.Equal(t, lcfg.SegmentMaxSize, cfg.SegmentMaxSize, name)
		require.Zero(t, cfg.MaxRecords, name)
		require.Zero(t, cfg.MaxBytes, name)
	}

	// Collections are registered on the handle with contract names/versions.
	infos, err := db.Registered()
	require.NoError(t, err)
	require.Len(t, infos, len(CollectionSpecs()))
	for i, want := range CollectionSpecs() {
		require.Equal(t, want.Name, infos[i].Name)
		require.Equal(t, want.Version, infos[i].Version)
	}

	// Every declared index can be (re-)ensured on the registered collections.
	ensureIndexes[Plan](t, db, plansCollection)
	ensureIndexes[PlanExecutionEvent](t, db, eventsCollection)
	ensureIndexes[ExecutionNote](t, db, notesCollection)

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
	require.NoError(t, s.Create(context.Background(), &Plan{ID: "plan-1", Name: "p", Status: PlanStatusPending})) // handle still usable
	require.NoError(t, db.Close())
	_, err = s.Get(context.Background(), "plan-1")
	require.ErrorIs(t, err, wardendb.ErrClosed)
}

func newLegacy(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

func ensureIndexes[T any](t *testing.T, db *wardendb.DB, name string) {
	t.Helper()
	c, err := wardendb.Lookup[T](db, name)
	require.NoError(t, err)
	eng, err := c.Engine()
	require.NoError(t, err)
	for _, f := range CollectionIndexes()[name] {
		require.NoError(t, eng.EnsureIndex(f), name+"."+f)
	}
}

package backendstore

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

// NewTestStore is the exported form of newTestStore for the external
// (backendstore_test) test files.
func NewTestStore(dir string) (*Store, error) { return openTestStore(dir) }

// newTestStore opens the store under test at dir; in shared mode the returned
// store's Close also releases the handle so tests can reopen dir.
func openTestStore(dir string) (*Store, error) {
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

// skipShared skips tests of the legacy on-disk path (standalone dir layout,
// package file lock, offline verify/repair, legacy migrations), which the
// shared constructor deliberately does not own.
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

	legacy, err := NewStore(t.TempDir())
	require.NoError(t, err)
	defer legacy.Close()

	for name, pair := range map[string][2]docs{
		backendsCollection:    {s.col, legacy.col},
		modelsCollectionName:  {s.modelsCol, legacy.modelsCol},
		roleTiersCollection:   {s.rolesCol, legacy.rolesCol},
		handoverCollection:    {s.handoverCol, legacy.handoverCol},
		quotasCollection:      {s.quotasCol, legacy.quotasCol},
		rlCooldownsCollection: {s.rlCooldownsCol, legacy.rlCooldownsCol},
	} {
		sc, lc := pair[0].Config(), pair[1].Config()
		require.Equal(t, engine.SyncModeAlways, sc.SyncMode, name)
		require.Equal(t, lc.SyncMode, sc.SyncMode, name)
		require.Equal(t, lc.MaxRecords, sc.MaxRecords, name)
		require.Equal(t, lc.MaxBytes, sc.MaxBytes, name)
	}

	infos, err := db.Registered()
	require.NoError(t, err)
	require.Len(t, infos, len(CollectionSpecs()))
	for i, want := range CollectionSpecs() {
		require.Equal(t, want.Name, infos[i].Name)
		require.Equal(t, want.Version, infos[i].Version)
	}

	// Seeding ran on the shared store exactly as on the legacy one.
	sb, err := s.List()
	require.NoError(t, err)
	lb, err := legacy.List()
	require.NoError(t, err)
	require.Equal(t, len(lb), len(sb))

	// Every declared index can be re-ensured on the registered collections.
	mc, err := wardendb.Lookup[ModelEntry](db, modelsCollectionName)
	require.NoError(t, err)
	eng, err := mc.Engine()
	require.NoError(t, err)
	for _, f := range CollectionIndexes()[modelsCollectionName] {
		require.NoError(t, eng.EnsureIndex(f), f)
	}

	// A second registration of the same collections is refused, not shadowed.
	_, err = NewStoreFromDB(db)
	require.Error(t, err)
}

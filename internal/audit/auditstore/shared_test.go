package auditstore

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/wardendb"
	"github.com/stretchr/testify/require"
)

// trails runs fn once against the file-backed audit.Writer and once against the
// shared-handle Store, so both implementations meet the same behavioural suite.
func trails(t *testing.T, fn func(t *testing.T, tr audit.Trail)) {
	t.Helper()
	t.Run("legacy", func(t *testing.T) {
		fn(t, audit.NewWriter(filepath.Join(t.TempDir(), "audit.jsonl")))
	})
	t.Run("shared", func(t *testing.T) {
		db, err := wardendb.Open(t.TempDir())
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		s, err := NewStoreFromDB(db)
		require.NoError(t, err)
		fn(t, s)
	})
}

func TestTrailAppendReadAndFilters(t *testing.T) {
	trails(t, func(t *testing.T, tr audit.Trail) {
		base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
		tr.Log(audit.Event{Time: base, Action: audit.ActionSpawn, Actor: "a", Target: "agent-1", Detail: map[string]string{"name": "x"}})
		tr.Log(audit.Event{Time: base.Add(time.Hour), Action: audit.ActionTerminate, Target: "agent-1"})
		tr.Log(audit.Event{Time: base.Add(2 * time.Hour), Action: audit.ActionSpawn, Target: "agent-2"})

		all, err := tr.Read(audit.Filter{})
		require.NoError(t, err)
		require.Len(t, all, 3)
		require.Equal(t, "agent-1", all[0].Target)
		require.Equal(t, "x", all[0].Detail["name"])
		require.Equal(t, "agent-2", all[2].Target, "append order, oldest first")

		got, _ := tr.Read(audit.Filter{Action: audit.ActionSpawn})
		require.Len(t, got, 2)
		got, _ = tr.Read(audit.Filter{Target: "agent-1"})
		require.Len(t, got, 2)
		got, _ = tr.Read(audit.Filter{Since: base.Add(time.Hour), Until: base.Add(time.Hour)})
		require.Len(t, got, 1)
		got, _ = tr.Read(audit.Filter{Limit: 1})
		require.Len(t, got, 1)
		require.Equal(t, "agent-2", got[0].Target, "limit keeps the most recent")
	})
}

func TestTrailStampsTimeAndEmptyRead(t *testing.T) {
	trails(t, func(t *testing.T, tr audit.Trail) {
		got, err := tr.Read(audit.Filter{})
		require.NoError(t, err)
		require.NotNil(t, got)
		require.Empty(t, got)

		tr.Log(audit.Event{Action: audit.ActionApprove})
		got, err = tr.Read(audit.Filter{})
		require.NoError(t, err)
		require.Len(t, got, 1)
		require.False(t, got[0].Time.IsZero())
	})
}

func TestTrailConcurrentLog(t *testing.T) {
	trails(t, func(t *testing.T, tr audit.Trail) {
		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); tr.Log(audit.Event{Action: audit.ActionSpawn, Target: "t"}) }()
		}
		wg.Wait()
		got, err := tr.Read(audit.Filter{})
		require.NoError(t, err)
		require.Len(t, got, 20)
	})
}

func TestNilStoreIsNoop(t *testing.T) {
	var s *Store
	require.NotPanics(t, func() { s.Log(audit.Event{Action: audit.ActionSpawn}) })
	got, err := s.Read(audit.Filter{})
	require.NoError(t, err)
	require.Empty(t, got)
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
	require.Len(t, infos, len(CollectionSpecs()))
	for i, want := range CollectionSpecs() {
		require.Equal(t, want.Name, infos[i].Name)
		require.Equal(t, want.Version, infos[i].Version)
	}

	c, err := wardendb.Lookup[audit.Event](db, collectionName)
	require.NoError(t, err)
	eng, err := c.Engine()
	require.NoError(t, err)
	for _, f := range CollectionIndexes()[collectionName] {
		require.NoError(t, eng.EnsureIndex(f), f)
	}

	_, err = NewStoreFromDB(db)
	require.ErrorIs(t, err, wardendb.ErrDuplicateCollection)
}

func TestSharedStoreLeavesHandleOpen(t *testing.T) {
	db, err := wardendb.Open(t.TempDir())
	require.NoError(t, err)
	s, err := NewStoreFromDB(db)
	require.NoError(t, err)
	s.Log(audit.Event{Action: audit.ActionSpawn})
	require.NoError(t, db.Close())
}

package metrics

import (
	"testing"
	"time"

	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/warden/internal/wardendb"
	"github.com/stretchr/testify/require"
)

// sampleStores runs fn once against the day-file Recorder and once against the
// shared-handle Store, so both implementations meet the same behavioural suite.
func sampleStores(t *testing.T, fn func(t *testing.T, st SampleStore)) {
	t.Helper()
	t.Run("legacy", func(t *testing.T) {
		r, err := NewRecorder(t.TempDir())
		require.NoError(t, err)
		fn(t, r)
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

func TestSampleStoreRecordAndHistory(t *testing.T) {
	sampleStores(t, func(t *testing.T, st SampleStore) {
		require.NoError(t, st.Record(Sample{TakenAt: ts("2026-06-08T10:00:00Z"), System: SystemStats{AgentCount: 1}}))
		require.NoError(t, st.Record(Sample{TakenAt: ts("2026-06-08T10:00:15Z"), System: SystemStats{AgentCount: 2},
			Agents: []AgentStat{{ID: "a1", RSSBytes: 1 << 30, CPUPercent: 12.5, ContextTokens: 900}},
			Daemon: DaemonStat{Goroutines: 7}}))
		require.NoError(t, st.Record(Sample{TakenAt: ts("2026-06-09T09:00:00Z"), System: SystemStats{AgentCount: 3}}))

		got, err := st.History(ts("2026-06-08T10:00:15Z"), 100)
		require.NoError(t, err)
		require.Len(t, got, 2)
		require.Equal(t, 3, got[0].System.AgentCount)
		require.Equal(t, 2, got[1].System.AgentCount)
		require.Equal(t, []AgentStat{{ID: "a1", RSSBytes: 1 << 30, CPUPercent: 12.5, ContextTokens: 900}}, got[1].Agents)
		require.Equal(t, 7, got[1].Daemon.Goroutines)

		got, err = st.History(time.Time{}, 1)
		require.NoError(t, err)
		require.Len(t, got, 1)
		require.Equal(t, 3, got[0].System.AgentCount)
	})
}

func TestSampleStorePrune(t *testing.T) {
	sampleStores(t, func(t *testing.T, st SampleStore) {
		require.NoError(t, st.Record(Sample{TakenAt: ts("2026-05-01T10:00:00Z")}))
		require.NoError(t, st.Record(Sample{TakenAt: ts("2026-06-09T10:00:00Z")}))
		require.NoError(t, st.Prune(ts("2026-06-09T12:00:00Z"), 7))
		got, err := st.History(time.Time{}, 100)
		require.NoError(t, err)
		require.Len(t, got, 1)
		require.Equal(t, 9, got[0].TakenAt.Day())
	})
}

func TestSampleStoreEmptyAndSameInstant(t *testing.T) {
	sampleStores(t, func(t *testing.T, st SampleStore) {
		got, err := st.History(time.Time{}, 100)
		require.NoError(t, err)
		require.Empty(t, got)

		at := ts("2026-06-09T10:00:00Z")
		require.NoError(t, st.Record(Sample{TakenAt: at}))
		require.NoError(t, st.Record(Sample{TakenAt: at}))
		got, err = st.History(time.Time{}, 100)
		require.NoError(t, err)
		require.Len(t, got, 2)
	})
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

	c, err := wardendb.Lookup[Sample](db, collectionName)
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
	require.NoError(t, s.Record(Sample{TakenAt: time.Now()}))
	require.NoError(t, db.Close())
}

package backendusage

import (
	"context"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/stretchr/testify/require"
)

func TestSnapshotStoreAdapterContracts(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	store, err := NewSnapshotStore(t.TempDir())
	require.NoError(t, err)
	rolling, weekly := 20.0, 100.0
	reset := now.Add(5 * time.Hour)
	modelUsed := 12.0
	adapter := fakeAdapter{id: "claude", fetch: func(context.Context, backendstore.Backend) Result {
		return Result{Status: StatusOK, ObservedAt: now, Account: &Account{ProfileFingerprint: "profile-a"}, Usage: []Limit{
			{ID: "rolling", Scope: "all", Label: "rolling", UsedPercent: &rolling, ResetsAt: &reset},
			{ID: "weekly", Scope: "all", Label: "weekly", UsedPercent: &weekly}, // missing reset is valid
		}}
	}}
	s := NewService(fakeRegistry{rows: []backendstore.Backend{{ID: "claude", Tier: backendstore.TierSubscription, Installed: true}}}, adapter)
	s.now = func() time.Time { return now }
	s.SetSnapshotStore(store)
	_, err = s.Snapshot(context.Background(), true)
	require.NoError(t, err)
	got, ok, err := store.Latest(CapacityDomain{Provider: "claude", ProfileFingerprint: "profile-a"}, now, FreshTTL)
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, got.Authoritative)
	require.Equal(t, FreshnessFresh, got.Freshness)
	require.Equal(t, BucketAvailable, got.Buckets[0].State)
	require.Equal(t, BucketExhausted, got.Buckets[1].State)
	require.Nil(t, got.Buckets[1].ResetsAt)

	// A model-specific provider is isolated by provider/profile domain rather than
	// being flattened into Claude's account-wide windows.
	_, err = store.Record(UsageSnapshot{Domain: CapacityDomain{Provider: "codex", ProfileFingerprint: "profile-b", Route: "gpt-5"}, ObservedAt: now, RecordedAt: now, SourceStatus: StatusOK, Authoritative: true, Freshness: FreshnessFresh, Buckets: []CapacityBucket{{Key: "gpt-5:primary", State: BucketAvailable, UsedPercent: &modelUsed}}})
	require.NoError(t, err)
	_, ok, err = store.Latest(CapacityDomain{Provider: "codex", ProfileFingerprint: "profile-b", Route: "gpt-5"}, now, FreshTTL)
	require.NoError(t, err)
	require.True(t, ok)
	_, ok, err = store.Latest(CapacityDomain{Provider: "claude", ProfileFingerprint: "profile-b"}, now, FreshTTL)
	require.NoError(t, err)
	require.False(t, ok, "profiles must not share capacity observations")
}

func TestSnapshotStoreFailedUnsupportedPartialAndStaleAreNotExhaustion(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	store, err := NewSnapshotStore(t.TempDir())
	require.NoError(t, err)
	d := CapacityDomain{Provider: "cursor", ProfileFingerprint: "p"}
	used := 35.0
	first, err := store.Record(UsageSnapshot{Domain: d, ObservedAt: now, RecordedAt: now, SourceStatus: StatusOK, Authoritative: true, Freshness: FreshnessFresh, Buckets: []CapacityBucket{{Key: "api", State: BucketAvailable, UsedPercent: &used}}})
	require.NoError(t, err)
	failed, err := store.Record(UsageSnapshot{Domain: d, ObservedAt: now.Add(time.Minute), RecordedAt: now.Add(time.Minute), SourceStatus: StatusUnsupported, Freshness: FreshnessUnknown})
	require.NoError(t, err)
	require.Greater(t, failed.Revision, first.Revision)
	require.Equal(t, BucketUnknown, failed.Buckets[0].State)
	require.Equal(t, 35.0, *failed.Buckets[0].UsedPercent)
	partial, err := store.Record(UsageSnapshot{Domain: d, ObservedAt: now.Add(2 * time.Minute), RecordedAt: now.Add(2 * time.Minute), SourceStatus: StatusOK, Authoritative: true, Freshness: FreshnessFresh, Buckets: []CapacityBucket{{Key: "api", State: BucketUnknown}}})
	require.NoError(t, err)
	require.Equal(t, BucketUnknown, partial.Buckets[0].State)
	require.Equal(t, 35.0, *partial.Buckets[0].UsedPercent)
	stale, ok, err := store.Latest(d, now.Add(4*FreshTTL), FreshTTL)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, FreshnessStale, stale.Freshness)
}

func TestSnapshotStoreConflictingReadingFailsClosedAndSurvivesRestart(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	store, err := NewSnapshotStore(dir)
	require.NoError(t, err)
	d := CapacityDomain{Provider: "codex", ProfileFingerprint: "p"}
	used, remaining := 1.0, 99.0
	reached := "reached"
	// Explicit provider hard-limit state wins over contradictory percentages.
	b := bucketFromLimit(Limit{ID: "primary", Scope: "gpt", Label: "primary", UsedPercent: &used, RemainingPercent: &remaining, LimitState: &reached}, true)
	require.Equal(t, BucketExhausted, b.State)
	_, err = store.Record(UsageSnapshot{Domain: d, ObservedAt: now, RecordedAt: now, SourceStatus: StatusRateLimited, Authoritative: true, Freshness: FreshnessFresh, Buckets: []CapacityBucket{b}})
	require.NoError(t, err)
	store, err = NewSnapshotStore(dir)
	require.NoError(t, err)
	got, ok, err := store.Latest(d, now, FreshTTL)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, BucketExhausted, got.Buckets[0].State)
}

func TestSnapshotStoreLatestAllMarksFreshness(t *testing.T) {
	store, err := NewSnapshotStore(t.TempDir())
	require.NoError(t, err)
	now := time.Now().UTC()
	used := 100.0
	_, err = store.Record(UsageSnapshot{Domain: CapacityDomain{Provider: "claude", ProfileFingerprint: "a"}, ObservedAt: now, RecordedAt: now, SourceStatus: StatusOK, Authoritative: true, Freshness: FreshnessFresh, Buckets: []CapacityBucket{{Key: "weekly", State: BucketExhausted, UsedPercent: &used}}})
	require.NoError(t, err)
	_, err = store.Record(UsageSnapshot{Domain: CapacityDomain{Provider: "claude", ProfileFingerprint: "b"}, ObservedAt: now.Add(-time.Hour), RecordedAt: now.Add(-time.Hour), SourceStatus: StatusOK, Authoritative: true, Freshness: FreshnessFresh, Buckets: []CapacityBucket{{Key: "weekly", State: BucketAvailable}}})
	require.NoError(t, err)
	all, err := store.LatestAll(now, 15*time.Minute)
	require.NoError(t, err)
	require.Len(t, all, 2)
	byFP := map[string]UsageSnapshot{}
	for _, s := range all {
		byFP[s.Domain.ProfileFingerprint] = s
	}
	require.Equal(t, FreshnessFresh, byFP["a"].Freshness)
	require.Equal(t, FreshnessStale, byFP["b"].Freshness)
}

func TestSnapshotStorePrunesExpiredHistoryButKeepsRecentObservations(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	store, err := NewSnapshotStore(t.TempDir())
	require.NoError(t, err)
	store.retention = time.Hour
	d := CapacityDomain{Provider: "claude"}
	_, err = store.Record(UsageSnapshot{Domain: d, ObservedAt: now.Add(-2 * time.Hour), RecordedAt: now.Add(-2 * time.Hour), SourceStatus: StatusOK, Authoritative: true})
	require.NoError(t, err)
	_, err = store.Record(UsageSnapshot{Domain: d, ObservedAt: now, RecordedAt: now, SourceStatus: StatusOK, Authoritative: true})
	require.NoError(t, err)
	store.mu.Lock()
	all, err := store.readLocked()
	store.mu.Unlock()
	require.NoError(t, err)
	require.Len(t, all, 1, "history older than retention is pruned on write")
}

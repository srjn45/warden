package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// insertUndecodable writes a raw record straight into the active collection whose
// body decodes at the engine layer but NOT into a Session (Events typed as a
// string), simulating on-disk schema drift / partial corruption without needing
// to mangle segment bytes.
func insertUndecodable(t *testing.T, fs *FileStore, key string) {
	t.Helper()
	_, _, err := fs.active.InsertWithKey(key, map[string]any{
		"id":     key,
		"events": "not-an-array", // []Event can't unmarshal from a string
	})
	require.NoError(t, err)
}

// TestListDegradesOnCorruptRecord is the Phase-3 complete-or-error invariant: a
// single undecodable active record makes List return a typed DegradedScanError
// with NO partial slice — never a silently short fleet.
func TestListDegradesOnCorruptRecord(t *testing.T) {
	ctx := context.Background()
	fs := newFileStore(t)

	require.NoError(t, fs.Insert(ctx, sample()))
	insertUndecodable(t, fs, "corrupt-1")

	sessions, err := fs.List(ctx)
	require.Error(t, err)
	require.Nil(t, sessions, "a degraded scan must not return a partial list")

	d, ok := IsDegraded(err)
	require.True(t, ok, "error must be a DegradedScanError")
	require.Len(t, d.Failures, 1)
	require.Equal(t, "corrupt-1", d.Failures[0].Key)
	require.Equal(t, DegradeDecode, d.Failures[0].Class)
	require.Equal(t, "active", d.Failures[0].Collection)
}

// TestListDegradedReportsEveryFailure verifies every bad record is surfaced (so
// operator tooling / store-health sees the full extent), not just the first.
func TestListDegradedReportsEveryFailure(t *testing.T) {
	ctx := context.Background()
	fs := newFileStore(t)

	require.NoError(t, fs.Insert(ctx, sample()))
	insertUndecodable(t, fs, "corrupt-1")
	insertUndecodable(t, fs, "corrupt-2")

	_, err := fs.List(ctx)
	d, ok := IsDegraded(err)
	require.True(t, ok)
	require.Len(t, d.Failures, 2)
}

// TestListCleanIsNotDegraded is the negative control: a healthy fleet returns the
// complete list with a nil error.
func TestListCleanIsNotDegraded(t *testing.T) {
	ctx := context.Background()
	fs := newFileStore(t)
	require.NoError(t, fs.Insert(ctx, sample()))

	sessions, err := fs.List(ctx)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
}

// TestInsertUniquenessScanDegrades verifies the corruption fails closed on the
// write path too: Insert's name-uniqueness scan surfaces the DegradedScanError
// rather than silently skipping the unreadable record (which could let a
// duplicate name slip in).
func TestInsertUniquenessScanDegrades(t *testing.T) {
	ctx := context.Background()
	fs := newFileStore(t)
	insertUndecodable(t, fs, "corrupt-1")

	named := sample()
	named.Name = "worker"
	err := fs.Insert(ctx, named)
	_, ok := IsDegraded(err)
	require.True(t, ok, "Insert uniqueness scan must fail closed on a degraded active scan")
}

// TestListClosedTolerantSkipsCorrupt verifies the ARCHIVE policy differs: a
// corrupt historical record is skipped (with a count), so one bad closed record
// never makes the whole history unlistable.
func TestListClosedTolerantSkipsCorrupt(t *testing.T) {
	ctx := context.Background()
	fs := newFileStore(t)

	// One good archived record, one undecodable.
	good := sample()
	rec, err := toRecord(good)
	require.NoError(t, err)
	_, err = fs.closed.Upsert(good.ID, rec)
	require.NoError(t, err)
	_, _, err = fs.closed.InsertWithKey("corrupt-closed", map[string]any{
		"id": "corrupt-closed", "events": "not-an-array",
	})
	require.NoError(t, err)

	sessions, skipped, err := fs.ListClosedDegraded(ctx)
	require.NoError(t, err, "closed scan is tolerant, not complete-or-error")
	require.Len(t, sessions, 1)
	require.Equal(t, 1, skipped)
}

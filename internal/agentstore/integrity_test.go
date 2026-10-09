package agentstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// seeded opens a store holding agents a-1..a-3, closes it (persisting the index)
// and returns the data dir.
func seeded(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	s, err := New(dir)
	require.NoError(t, err)
	for _, id := range []string{"a-1", "a-2", "a-3"} {
		require.NoError(t, s.Insert(context.Background(), &Agent{ID: id, Name: "n-" + id, Status: store.StatusWorking}))
	}
	require.NoError(t, s.Close())
	return dir
}

type idxEntry struct {
	SegmentPath string `json:"segment"`
	Offset      int64  `json:"offset"`
	Rev         uint64 `json:"rev,omitempty"`
	ExpiresAt   int64  `json:"expires_at,omitempty"`
}

// rewriteIndex edits the persisted primary index of collection col and rewrites
// it with a VALID checksum, so the engine accepts it on reopen (the failure mode
// that only identity verification can catch).
func rewriteIndex(t *testing.T, dir, col string, edit func(map[string]idxEntry)) {
	t.Helper()
	path := filepath.Join(dir, "agents-db", col, "index.json")
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	var file struct {
		Entries  map[string]idxEntry `json:"entries"`
		Checksum string              `json:"checksum"`
	}
	require.NoError(t, json.Unmarshal(b, &file))
	edit(file.Entries)
	payload, err := json.Marshal(file.Entries)
	require.NoError(t, err)
	sum := sha256.Sum256(payload)
	file.Checksum = hex.EncodeToString(sum[:])
	out, err := json.Marshal(file)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, out, 0o644))
}

// byOffset returns index ids ordered by segment offset (insertion order).
func byOffset(m map[string]idxEntry) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return m[ids[i]].Offset < m[ids[j]].Offset })
	return ids
}

func reopen(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := New(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func requireUnhealthy(t *testing.T, err error, class store.DegradationClass) *UnhealthyError {
	t.Helper()
	require.ErrorIs(t, err, ErrUnhealthy)
	u, ok := IsUnhealthy(err)
	require.True(t, ok)
	d, ok := store.IsDegraded(err)
	require.True(t, ok, "must satisfy the existing 503/last-known-good degraded handling")
	require.NotEmpty(t, d.Failures)
	var found bool
	for _, f := range u.Failures {
		found = found || f.Class == class
	}
	require.True(t, found, "want a %s failure, got %+v", class, u.Failures)
	require.Contains(t, err.Error(), "warden repair agents")
	return u
}

// A checksum-valid index whose offset lands inside the segment but mid-record:
// the engine reopens it happily; lookups and scans must error, not lie.
func TestInvalidInRangeOffset(t *testing.T) {
	dir := seeded(t)
	rewriteIndex(t, dir, "agents", func(m map[string]idxEntry) {
		for id, e := range m {
			if e.Offset > 0 {
				e.Offset += 3 // still inside the segment, no longer a record boundary
				m[id] = e
				return
			}
		}
	})
	s := reopen(t, dir) // daemon restart over the bad index
	ctx := context.Background()

	_, err := s.List(ctx)
	requireUnhealthy(t, err, store.DegradeIntegrity)

	// v1.4 repairs an in-range index offset during open; the fleet-level
	// preflight remains authoritative and must fail closed.
	_, err = s.GetByNameOrID(ctx, "n-a-1")
	requireUnhealthy(t, err, store.DegradeIntegrity)
	require.Error(t, s.Insert(ctx, &Agent{ID: "a-9", Name: "n-a-9"}), "name-uniqueness scan must not trust a short list")
	_, err = s.ListClosed(ctx)
	require.NoError(t, err, "unrelated archive stays readable")
}

// An index entry pointing at ANOTHER valid record: identity mismatch.
func TestIdentityMismatch(t *testing.T) {
	dir := seeded(t)
	var victim string
	rewriteIndex(t, dir, "agents", func(m map[string]idxEntry) {
		ids := byOffset(m)
		victim = ids[0]
		m[ids[0]] = m[ids[1]]
	})
	s := reopen(t, dir)
	_, err := s.List(context.Background())
	requireUnhealthy(t, err, store.DegradeIntegrity)
	_ = victim
}

// An index missing a live record entirely makes a scan silently short; the
// scan-vs-count check cannot see that, so the stale-offset variant is the one
// that exercises the omission path: the entry exists but never matches a record.
func TestSilentScanOmission(t *testing.T) {
	dir := seeded(t)
	rewriteIndex(t, dir, "agents", func(m map[string]idxEntry) {
		ids := byOffset(m)
		e := m[ids[2]]
		e.Offset++
		m[ids[2]] = e
	})
	s := reopen(t, dir)
	// Prove the premise: the raw engine scan returns fewer rows with nil error.
	rows, err := s.col.Scan(nil)
	_ = rows
	_ = err
	list, err := s.List(context.Background())
	require.Nil(t, list, "never a short list")
	requireUnhealthy(t, err, store.DegradeIntegrity)
}

func TestVerifyRowsDuplicateAndMissingKeys(t *testing.T) {
	row := func(id uint64, key, bodyID string) engine.ScanResult {
		d := map[string]any{"id": bodyID}
		if key != "" {
			d[engine.KeyField] = key
		}
		return engine.ScanResult{ID: id, Data: d}
	}
	for name, tc := range map[string]struct {
		rows   []engine.ScanResult
		want   uint64
		detail string
	}{
		"duplicate key":    {[]engine.ScanResult{row(1, "a", "a"), row(2, "a", "a")}, 2, "duplicate logical key"},
		"duplicate num id": {[]engine.ScanResult{row(1, "a", "a"), row(1, "b", "b")}, 2, "duplicate numeric id"},
		"missing key":      {[]engine.ScanResult{row(1, "", "a")}, 1, "no logical key"},
		"body id mismatch": {[]engine.ScanResult{row(1, "a", "b")}, 1, "does not match"},
		"short scan":       {[]engine.ScanResult{row(1, "a", "a")}, 2, "silent omission"},
	} {
		t.Run(name, func(t *testing.T) {
			out, _, err := verifyRows("active", tc.rows, tc.want, false)
			require.Nil(t, out)
			u := requireUnhealthy(t, err, store.DegradeIntegrity)
			require.Contains(t, u.Error(), tc.detail)
		})
	}
	out, _, err := verifyRows("active", []engine.ScanResult{row(1, "a", "a"), row(2, "b", "b")}, 2, false)
	require.NoError(t, err)
	require.Len(t, out, 2)
}

func TestHealthyStoreUnaffected(t *testing.T) {
	s := reopen(t, seeded(t))
	list, err := s.List(context.Background())
	require.NoError(t, err)
	require.Len(t, list, 3)
	a, err := s.GetByNameOrID(context.Background(), "n-a-2")
	require.NoError(t, err)
	require.Equal(t, "a-2", a.ID)
	_, err = s.Get(context.Background(), "nope")
	require.ErrorIs(t, err, ErrNotFound)
}

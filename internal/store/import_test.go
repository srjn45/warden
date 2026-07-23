package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// legacyCorpus returns a representative mix of legacy records: a no-name agent,
// a tagged one, one carrying ExitCode + rate-limit pointers, and an archived one.
// The active slice is written to sessions/, the closed slice to closed/.
func legacyCorpus() (active, closed []*Session) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	code := 137
	restore := now.Add(time.Hour)
	noName := &Session{
		ID: "agent-noname", TmuxSession: "agent-noname", Type: TypeDevelopment,
		Repo: "/repo", Status: StatusWorking, CreatedAt: now, UpdatedAt: now,
		Events: []Event{{TS: now, Type: "spawn", Detail: "started"}},
	}
	tagged := &Session{
		ID: "agent-tagged", Name: "builder", TmuxSession: "agent-tagged", Type: TypeCode,
		Repo: "/repo", Status: StatusIdle, Tags: []string{"backend", "urgent"},
		CreatedAt: now, UpdatedAt: now.Add(time.Minute), Events: []Event{},
	}
	exited := &Session{
		ID: "agent-exited", TmuxSession: "agent-exited", Type: TypeTests,
		Repo: "/repo", Status: StatusRateLimited, ExitCode: &code,
		RateLimitedAt: &now, RateLimitRestoreAt: &restore, RateLimitRetryCount: 2,
		CreatedAt: now, UpdatedAt: now.Add(2 * time.Minute), Events: []Event{},
	}
	archived := &Session{
		ID: "agent-archived", Name: "done-one", TmuxSession: "agent-archived", Type: TypeDocs,
		Repo: "/repo", Status: StatusDone, ExitCode: intPtr(0),
		CreatedAt: now, UpdatedAt: now.Add(3 * time.Minute), Events: []Event{},
	}
	return []*Session{noName, tagged, exited}, []*Session{archived}
}

func intPtr(v int) *int { return &v }

// TestImportFidelity seeds legacy JSON and asserts every record round-trips
// byte-identically through the import into the ScrivaDB collections.
func TestImportFidelity(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	active, closed := legacyCorpus()
	// Mark provenance already-migrated so the import imports flags verbatim and
	// this test checks pure round-trip fidelity (provenance fold has its own test).
	require.NoError(t, os.WriteFile(filepath.Join(dir, provenanceMarker), []byte("done\n"), 0o600))
	for _, s := range active {
		writeLegacy(t, dir, "sessions", s)
	}
	for _, s := range closed {
		writeLegacy(t, dir, "closed", s)
	}

	st, err := NewFileStore(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close(ctx) })

	for _, want := range active {
		got, err := st.Get(ctx, want.ID)
		require.NoError(t, err, "active id %s", want.ID)
		require.Equal(t, want, got, "active record %s must round-trip identically", want.ID)
	}
	list, err := st.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, len(active))

	closedList, err := st.ListClosed(ctx)
	require.NoError(t, err)
	require.Len(t, closedList, len(closed))
	require.Equal(t, closed[0], closedList[0], "closed record must round-trip identically")

	// An id living only in closed is not visible via Get (active-only) — the
	// pre-existing quirk import relies on.
	_, err = st.Get(ctx, "agent-archived")
	require.ErrorIs(t, err, ErrNotFound)
}

// TestImportProvenanceVerbatimWhenMarked verifies that when the old
// .provenance-migrated marker is present, the importer trusts the explicit flags
// in the legacy JSON — an adopted record (WorktreeCreated=false with a worktree
// on disk) is NOT re-inferred to true.
func TestImportProvenanceVerbatimWhenMarked(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, provenanceMarker), []byte("done\n"), 0o600))

	// Adopted: has a worktree + branch==id (which backfill WOULD infer as created)
	// but explicit flags say adopted. Verbatim import must keep them false.
	adopted := &Session{
		ID: "agent-adopted", TmuxSession: "agent-adopted", Repo: "/repo",
		Worktree: ".worktrees/agent-adopted", Branch: "agent-adopted", Status: StatusWorking,
		WorktreeCreated: false, BranchCreated: false,
	}
	writeLegacy(t, dir, "sessions", adopted)

	st, err := NewFileStore(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close(ctx) })

	got, err := st.Get(ctx, "agent-adopted")
	require.NoError(t, err)
	require.False(t, got.WorktreeCreated, "explicit adopted flag must not be clobbered")
	require.False(t, got.BranchCreated, "explicit adopted flag must not be clobbered")
}

// TestImportSkipsUnsafeID verifies a legacy record whose id fails safeID is
// skipped with a warning while the rest import, and the sentinel is still written.
func TestImportSkipsUnsafeID(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	good := sample()
	writeLegacy(t, dir, "sessions", good)
	// A file whose decoded id contains a path separator — unsafe, must be skipped.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sessions"), 0o700))
	require.NoError(t, atomicWriteJSON(filepath.Join(dir, "sessions", "evil.json"),
		&Session{ID: "a/b", Status: StatusWorking}))

	st, err := NewFileStore(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close(ctx) })

	list, err := st.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1, "only the safe-id record imports")
	require.Equal(t, good.ID, list[0].ID)
	require.FileExists(t, filepath.Join(dir, importedMarker))
}

// TestImportIdempotent verifies a second open of an imported tree neither
// re-imports nor duplicates records, and that post-upgrade writes stay in ScrivaDB
// (a legacy file added after import is ignored on the next open).
func TestImportIdempotent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	writeLegacy(t, dir, "sessions", sample())

	st, err := NewFileStore(dir)
	require.NoError(t, err)
	require.NoError(t, st.Insert(ctx, &Session{ID: "post-upgrade", Status: StatusWorking}))
	require.NoError(t, st.Close(ctx))

	// Drop another legacy file AFTER the import completed; the sentinel exists, so
	// the next open must NOT re-import it.
	writeLegacy(t, dir, "sessions", &Session{ID: "ignored-legacy", Status: StatusWorking})

	st2, err := NewFileStore(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st2.Close(ctx) })

	list, err := st2.List(ctx)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, s := range list {
		ids[s.ID] = true
	}
	require.Len(t, list, 2, "imported record + post-upgrade insert, no duplicates, no re-import")
	require.True(t, ids["PROJ-350"], "originally imported record present")
	require.True(t, ids["post-upgrade"], "post-upgrade insert survived")
	require.False(t, ids["ignored-legacy"], "a legacy file added after import must be ignored")
}

// TestImportRollback simulates a LoadJSONL failure on the closed collection (two
// legacy files sharing the same id → duplicate-key abort): NewFileStore errors,
// the sentinel is absent, and a subsequent open (after fixing the legacy data)
// wipes the half-built db and re-imports to a correct state.
func TestImportRollback(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// One good active record.
	writeLegacy(t, dir, "sessions", sample())
	// Two closed files carrying the SAME "id" — LoadJSONL aborts on the duplicate
	// key, failing the closed collection's atomic load.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "closed"), 0o700))
	require.NoError(t, atomicWriteJSON(filepath.Join(dir, "closed", "one.json"),
		&Session{ID: "dup", Subject: "first", Status: StatusDone}))
	require.NoError(t, atomicWriteJSON(filepath.Join(dir, "closed", "two.json"),
		&Session{ID: "dup", Subject: "second", Status: StatusDone}))

	_, err := NewFileStore(dir)
	require.Error(t, err, "duplicate key in the closed batch must fail the import")
	require.NoFileExists(t, filepath.Join(dir, importedMarker), "sentinel not written on failed import")

	// Fix the legacy data (remove the duplicate) and re-open: the half-built db is
	// wiped and re-imported cleanly.
	require.NoError(t, os.Remove(filepath.Join(dir, "closed", "two.json")))

	st, err := NewFileStore(dir)
	require.NoError(t, err, "re-open wipes the partial db and re-imports from intact legacy JSON")
	t.Cleanup(func() { _ = st.Close(ctx) })

	active, err := st.List(ctx)
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.Equal(t, "PROJ-350", active[0].ID)

	closed, err := st.ListClosed(ctx)
	require.NoError(t, err)
	require.Len(t, closed, 1)
	require.Equal(t, "dup", closed[0].ID)
	require.FileExists(t, filepath.Join(dir, importedMarker))
}

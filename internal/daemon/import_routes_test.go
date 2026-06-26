package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// sampleExport builds a small envelope and round-trips it through JSON so the
// tests exercise the real serialized shape `warden export` writes, not just the
// in-memory structs.
func sampleExport(t *testing.T) *store.Export {
	t.Helper()
	env := store.Export{
		Version:    store.ExportVersion,
		ExportedAt: time.Now().UTC(),
		Sessions: []*store.Session{
			{ID: "A-1", Type: store.TypeDevelopment, Subject: "first", Worktree: "/wt/a-1", Branch: "A-1"},
			{ID: "A-2", Type: store.TypeAnalysis, Subject: "second"},
		},
	}
	blob, err := json.Marshal(env)
	require.NoError(t, err)
	var got store.Export
	require.NoError(t, json.Unmarshal(blob, &got))
	return &got
}

func TestImportRoundTrip(t *testing.T) {
	st := newFakeStore()
	env := sampleExport(t)

	res, err := importSessions(context.Background(), st, env, false)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"A-1", "A-2"}, res.Imported)
	require.Empty(t, res.Skipped)
	require.Empty(t, res.Merged)

	// Records landed with their metadata intact, worktree pointer included.
	got, err := st.Get(context.Background(), "A-1")
	require.NoError(t, err)
	require.Equal(t, "first", got.Subject)
	require.Equal(t, "/wt/a-1", got.Worktree)
}

func TestImportSkipsCollisionByDefault(t *testing.T) {
	st := newFakeStore()
	env := sampleExport(t)

	_, err := importSessions(context.Background(), st, env, false)
	require.NoError(t, err)

	// Re-importing the same dump is a no-op: both ids already exist, so they are
	// skipped rather than re-inserted or errored (idempotency).
	res, err := importSessions(context.Background(), st, sampleExport(t), false)
	require.NoError(t, err)
	require.Empty(t, res.Imported)
	require.ElementsMatch(t, []string{"A-1", "A-2"}, res.Skipped)
}

func TestImportMergeOverwritesCollision(t *testing.T) {
	st := newFakeStore()
	_, err := importSessions(context.Background(), st, sampleExport(t), false)
	require.NoError(t, err)

	// A second dump for the same id but with changed metadata, imported with merge.
	updated := &store.Export{
		Version:  store.ExportVersion,
		Sessions: []*store.Session{{ID: "A-1", Type: store.TypeDevelopment, Subject: "rewritten"}},
	}
	res, err := importSessions(context.Background(), st, updated, true)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"A-1"}, res.Merged)
	require.Empty(t, res.Skipped)

	got, err := st.Get(context.Background(), "A-1")
	require.NoError(t, err)
	require.Equal(t, "rewritten", got.Subject)
}

func TestImportRejectsRecordWithoutID(t *testing.T) {
	st := newFakeStore()
	env := &store.Export{Sessions: []*store.Session{{ID: ""}}}
	_, err := importSessions(context.Background(), st, env, false)
	require.Error(t, err)
}

// TestImportAgainstFileStore exercises the production FileStore, whose Insert
// enforces name uniqueness (unlike fakeStore). It pins two behaviors the id-keyed
// idempotency must survive there: a same-id+same-name re-import is skipped (not
// mis-reported as a name clash), and a new id whose name collides with a
// different record is imported with the alias dropped.
func TestImportAgainstFileStore(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFileStore(t.TempDir())
	require.NoError(t, err)

	env := &store.Export{
		Version:  store.ExportVersion,
		Sessions: []*store.Session{{ID: "A-1", Name: "alpha", Subject: "first"}},
	}
	res, err := importSessions(ctx, st, env, false)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"A-1"}, res.Imported)

	// Same id and name again: idempotent skip, not an ErrNameExists failure.
	res, err = importSessions(ctx, st, env, false)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"A-1"}, res.Skipped)
	require.Empty(t, res.Imported)

	// New id, but the "alpha" name is already taken: imported with name dropped.
	clash := &store.Export{
		Version:  store.ExportVersion,
		Sessions: []*store.Session{{ID: "B-2", Name: "alpha", Subject: "clash"}},
	}
	res, err = importSessions(ctx, st, clash, false)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"B-2"}, res.Imported)
	require.ElementsMatch(t, []string{"B-2"}, res.Renamed)

	got, err := st.Get(ctx, "B-2")
	require.NoError(t, err)
	require.Equal(t, "", got.Name, "colliding name should be dropped on import")
	require.Equal(t, "clash", got.Subject)
}

func TestHandleImportHTTP(t *testing.T) {
	fs := newFakeStore()
	ts := testServer(t, fs)
	defer ts.Close()

	post := func(env *store.Export, merge bool) store.ImportResult {
		t.Helper()
		blob, err := json.Marshal(env)
		require.NoError(t, err)
		url := ts.URL + "/api/v1/import"
		if merge {
			url += "?merge=true"
		}
		resp, err := http.Post(url, "application/json", bytes.NewReader(blob))
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var res store.ImportResult
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&res))
		return res
	}

	env := &store.Export{Version: store.ExportVersion, Sessions: []*store.Session{{ID: "X-1", Subject: "v1"}}}
	require.ElementsMatch(t, []string{"X-1"}, post(env, false).Imported)

	// Without merge the second POST skips the existing id.
	require.ElementsMatch(t, []string{"X-1"}, post(env, false).Skipped)

	// With ?merge=true it overwrites.
	env2 := &store.Export{Version: store.ExportVersion, Sessions: []*store.Session{{ID: "X-1", Subject: "v2"}}}
	require.ElementsMatch(t, []string{"X-1"}, post(env2, true).Merged)
	got, err := fs.Get(context.Background(), "X-1")
	require.NoError(t, err)
	require.Equal(t, "v2", got.Subject)

	// A malformed body is a 400.
	resp, err := http.Post(ts.URL+"/api/v1/import", "application/json", bytes.NewReader([]byte("{bad")))
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

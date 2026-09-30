package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/stretchr/testify/require"
)

// Phase 11: old HTTP scan callers keep working; response carries deprecation
// notice; scan cannot reseed Status after ImportLegacy.

func TestScanProjectPlans_compatCallerNoticeAndCanonicalSkip(t *testing.T) {
	root := t.TempDir()
	writePlanYAML(t, root, "plans/pending/compat-ship.yaml", "compat-ship")
	// Enrich YAML so ImportLegacy can load a real definition.
	abs := filepath.Join(root, "plans/pending/compat-ship.yaml")
	require.NoError(t, os.WriteFile(abs, []byte(
		"version: 1\nname: compat-ship\ngoal: ship it\ntasks:\n  - id: t1\n    prompt: do work\n",
	), 0o644))

	ps, err := planstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = ps.Close() })
	projects, err := projectstore.NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = projects.Close() })
	_, err = projects.OpenProject(root, "test", root)
	require.NoError(t, err)

	srv := &Server{store: newFakeStore(), life: &fakeLife{}, plans: ps, projects: projects}
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	// Old caller: scan creates a stub first.
	resp := postJSON(t, planURL(ts.URL, root, "/scan"), map[string]any{})
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var scan1 struct {
		Upserted int    `json:"upserted"`
		Notice   string `json:"notice"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&scan1))
	require.Equal(t, 1, scan1.Upserted)
	require.Contains(t, scan1.Notice, "deprecated")

	// Explicit import fills the canonical definition.
	imp := postJSON(t, planURL(ts.URL, root, "/import-legacy"), map[string]any{})
	defer imp.Body.Close()
	require.Equal(t, http.StatusOK, imp.StatusCode)

	id := planstore.PlanID(root, "compat-ship")
	before, err := ps.Get(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, "ship it", before.Goal)
	require.Equal(t, planstore.PlanStatusPending, before.Status)

	// Move replica into in_progress/ — scan must not flip Status.
	require.NoError(t, os.Remove(abs))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "plans/in_progress"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "plans/in_progress/compat-ship.yaml"), []byte(
		"version: 1\nname: compat-ship\ngoal: hijacked\ntasks:\n  - id: t1\n    prompt: do work\n",
	), 0o644))

	resp2 := postJSON(t, planURL(ts.URL, root, "/scan"), map[string]any{})
	defer resp2.Body.Close()
	require.Equal(t, http.StatusOK, resp2.StatusCode)
	var scan2 struct {
		Upserted         int    `json:"upserted"`
		SkippedCanonical int    `json:"skipped_canonical"`
		Notice           string `json:"notice"`
	}
	require.NoError(t, json.NewDecoder(resp2.Body).Decode(&scan2))
	require.Equal(t, 0, scan2.Upserted)
	require.Equal(t, 1, scan2.SkippedCanonical)
	require.Contains(t, scan2.Notice, "cannot affect canonical")

	after, err := ps.Get(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, planstore.PlanStatusPending, after.Status)
	require.Equal(t, "ship it", after.Goal)
}

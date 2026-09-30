package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/projectstore"
)

func writeFullPlanYAML(t *testing.T, root, rel, name, goal, tasks string) {
	t.Helper()
	abs := filepath.Join(root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
	body := "version: 1\nname: " + name + "\ngoal: " + goal + "\ntasks:\n" + tasks
	require.NoError(t, os.WriteFile(abs, []byte(body), 0o644))
}

func TestImportLegacyPlans_route(t *testing.T) {
	root := t.TempDir()
	writeFullPlanYAML(t, root, "plans/pending/p1.yaml", "p1", "g1",
		"  - id: t1\n    prompt: do it\n")
	writeFullPlanYAML(t, root, "plans/in_progress/p2.yaml", "p2", "g2",
		"  - id: a\n    prompt: first\n  - id: b\n    prompt: second\n    after: [a]\n")
	writeFullPlanYAML(t, root, "plans/completed/p3.yaml", "p3", "g3",
		"  - id: t1\n    prompt: done\n")
	writeFullPlanYAML(t, root, "plans/archived/p4.yaml", "p4", "g4",
		"  - id: t1\n    prompt: old\n")
	writeFullPlanYAML(t, root, "plans/pending/broken.yaml", "broken", "g",
		"  - prompt: missing id\n")

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
	t.Cleanup(ts.Close)

	// Report-only first.
	resp := postJSON(t, planURL(ts.URL, root, "/import-legacy"), map[string]any{"report_only": true})
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var report oapi.ImportLegacyPlansResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&report))
	require.True(t, report.ReportOnly)
	require.Len(t, report.Imported, 4)
	require.Len(t, report.Errors, 1)
	_, err = ps.Get(context.Background(), planstore.PlanID(root, "p1"))
	require.ErrorIs(t, err, planstore.ErrNotFound)

	// Real import.
	resp2 := postJSON(t, planURL(ts.URL, root, "/import-legacy"), map[string]any{})
	defer resp2.Body.Close()
	require.Equal(t, http.StatusOK, resp2.StatusCode)
	require.NoError(t, json.NewDecoder(resp2.Body).Decode(&report))
	require.False(t, report.ReportOnly)
	require.Len(t, report.Imported, 4)
	require.Len(t, report.Errors, 1)

	p2, err := ps.Get(context.Background(), planstore.PlanID(root, "p2"))
	require.NoError(t, err)
	require.Equal(t, planstore.PlanStatusInProgress, p2.Status)
	require.Equal(t, []string{"a"}, p2.Tasks[1].After)
	require.NotNil(t, p2.StartedAt)

	// Idempotent re-import.
	resp3 := postJSON(t, planURL(ts.URL, root, "/import-legacy"), map[string]any{})
	defer resp3.Body.Close()
	require.Equal(t, http.StatusOK, resp3.StatusCode)
	require.NoError(t, json.NewDecoder(resp3.Body).Decode(&report))
	require.Len(t, report.Skipped, 4)
	require.Empty(t, report.Imported)

	// Conflict when YAML diverges.
	writeFullPlanYAML(t, root, "plans/pending/p1.yaml", "p1", "CHANGED",
		"  - id: t1\n    prompt: do it\n")
	resp4 := postJSON(t, planURL(ts.URL, root, "/import-legacy"), map[string]any{})
	defer resp4.Body.Close()
	require.Equal(t, http.StatusOK, resp4.StatusCode)
	require.NoError(t, json.NewDecoder(resp4.Body).Decode(&report))
	require.NotEmpty(t, report.Conflicted)
	p1, err := ps.Get(context.Background(), planstore.PlanID(root, "p1"))
	require.NoError(t, err)
	require.Equal(t, "g1", p1.Goal)

	// Source files untouched (including broken).
	_, err = os.Stat(filepath.Join(root, "plans/pending/broken.yaml"))
	require.NoError(t, err)
}

func TestStartupPlanScanDisabled_twoConsecutiveStarts(t *testing.T) {
	root := t.TempDir()
	writeFullPlanYAML(t, root, "plans/pending/auto.yaml", "auto", "should not auto-import",
		"  - id: t1\n    prompt: p\n")

	ps, err := planstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = ps.Close() })
	projects, err := projectstore.NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = projects.Close() })
	_, err = projects.OpenProject(root, "test", root)
	require.NoError(t, err)

	srv := &Server{store: newFakeStore(), life: &fakeLife{}, plans: ps, projects: projects}

	// Two consecutive "daemon starts" invoke the retired startup scan hook.
	srv.runStartupPlanScan(context.Background())
	srv.runStartupPlanScan(context.Background())

	list, err := ps.ListByProject(context.Background(), root)
	require.NoError(t, err)
	require.Empty(t, list, "startup must not import legacy YAML into ScrivaDB")
}

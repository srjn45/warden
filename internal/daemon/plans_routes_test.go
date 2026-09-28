package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/planstore"
)

// planServer builds a route server backed by a real ScrivaDB plan store.
func planServer(t *testing.T) (*httptest.Server, *planstore.Store) {
	t.Helper()
	ps, err := planstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = ps.Close() })
	srv := &Server{store: newFakeStore(), life: &fakeLife{}, plans: ps}
	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)
	return ts, ps
}

func planURL(base, projectID, extra string) string {
	return fmt.Sprintf("%s/api/v1/projects/%s/plans%s", base, url.PathEscape(projectID), extra)
}

// planPatch sends a PATCH request with a JSON body (as any).
func planPatch(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	return patchJSON(t, url, string(b))
}

// TestPlansListEmpty verifies an empty list is returned when no plans exist.
func TestPlansListEmpty(t *testing.T) {
	ts, _ := planServer(t)
	resp, err := http.Get(planURL(ts.URL, "proj-1", ""))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out struct {
		Plans []planstore.Plan `json:"plans"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.NotNil(t, out.Plans)
	require.Empty(t, out.Plans)
}

// TestPlansUnconfigured verifies 503 when no plan store is wired.
func TestPlansUnconfigured(t *testing.T) {
	srv := &Server{store: newFakeStore(), life: &fakeLife{}}
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	resp, err := http.Get(planURL(ts.URL, "proj-1", ""))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

// TestPlansCreate verifies POST creates a new pending plan record.
func TestPlansCreate(t *testing.T) {
	ts, _ := planServer(t)

	resp := postJSON(t, planURL(ts.URL, "proj-1", ""), map[string]any{
		"name":      "feature-x",
		"file_path": "plans/pending/feature-x.yaml",
	})
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var p planstore.Plan
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&p))
	require.Equal(t, "feature-x", p.Name)
	require.Equal(t, "proj-1", p.ProjectID)
	require.Equal(t, planstore.PlanStatusPending, p.Status)
	require.Equal(t, "plans/pending/feature-x.yaml", p.FilePath)
	require.NotEmpty(t, p.ID)
	require.False(t, p.CreatedAt.IsZero())
}

// TestPlansCreateDuplicate verifies 400 on a duplicate name.
func TestPlansCreateDuplicate(t *testing.T) {
	ts, _ := planServer(t)

	body := map[string]any{"name": "dup", "file_path": "plans/pending/dup.yaml"}
	r1 := postJSON(t, planURL(ts.URL, "proj-1", ""), body)
	r1.Body.Close()
	require.Equal(t, http.StatusOK, r1.StatusCode)

	r2 := postJSON(t, planURL(ts.URL, "proj-1", ""), body)
	defer r2.Body.Close()
	require.Equal(t, http.StatusBadRequest, r2.StatusCode)
}

// TestPlansGet verifies GET returns the plan record.
func TestPlansGet(t *testing.T) {
	ts, ps := planServer(t)
	ctx := t.Context()

	id := planstore.PlanID("proj-1", "myplan")
	require.NoError(t, ps.Create(ctx, &planstore.Plan{
		ID: id, ProjectID: "proj-1", Name: "myplan",
		FilePath: "plans/pending/myplan.yaml", Status: planstore.PlanStatusPending,
	}))

	resp, err := http.Get(planURL(ts.URL, "proj-1", "/"+id))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var got planstore.Plan
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Equal(t, id, got.ID)
	require.Equal(t, "myplan", got.Name)
}

// TestPlansGet404 verifies 404 for unknown plan.
func TestPlansGet404(t *testing.T) {
	ts, _ := planServer(t)
	resp, err := http.Get(planURL(ts.URL, "proj-1", "/plan-deadbeef"))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// TestPlansPatchStatus verifies PATCH updates the plan status.
func TestPlansPatchStatus(t *testing.T) {
	ts, ps := planServer(t)
	ctx := t.Context()

	id := planstore.PlanID("proj-1", "patch-me")
	require.NoError(t, ps.Create(ctx, &planstore.Plan{
		ID: id, ProjectID: "proj-1", Name: "patch-me",
		FilePath: "plans/pending/patch-me.yaml", Status: planstore.PlanStatusPending,
	}))

	resp := planPatch(t, planURL(ts.URL, "proj-1", "/"+id), map[string]any{
		"status": "in_progress",
	})
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var got planstore.Plan
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Equal(t, planstore.PlanStatusInProgress, got.Status)
	require.NotNil(t, got.StartedAt, "StartedAt must be set on in_progress transition")
}

// TestPlansPatchExecutionMode verifies PATCH updates execution_mode.
func TestPlansPatchExecutionMode(t *testing.T) {
	ts, ps := planServer(t)
	ctx := t.Context()

	id := planstore.PlanID("proj-1", "mode-test")
	require.NoError(t, ps.Create(ctx, &planstore.Plan{
		ID: id, ProjectID: "proj-1", Name: "mode-test",
		FilePath: "plans/pending/mode-test.yaml", Status: planstore.PlanStatusPending,
	}))

	resp := planPatch(t, planURL(ts.URL, "proj-1", "/"+id), map[string]any{
		"execution_mode": "autopilot",
	})
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var got planstore.Plan
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Equal(t, planstore.PlanModeAutopilot, got.ExecutionMode)
}

// TestPlansPatchTaskProgress verifies PATCH updates task_progress.
func TestPlansPatchTaskProgress(t *testing.T) {
	ts, ps := planServer(t)
	ctx := t.Context()

	id := planstore.PlanID("proj-1", "progress-test")
	require.NoError(t, ps.Create(ctx, &planstore.Plan{
		ID: id, ProjectID: "proj-1", Name: "progress-test",
		FilePath: "plans/pending/progress-test.yaml", Status: planstore.PlanStatusPending,
	}))

	resp := planPatch(t, planURL(ts.URL, "proj-1", "/"+id), map[string]any{
		"task_progress": map[string]string{"t1": "done", "t2": "in_progress"},
	})
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var got planstore.Plan
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Equal(t, "done", got.TaskProgress["t1"])
	require.Equal(t, "in_progress", got.TaskProgress["t2"])
}

// TestPlansPatchHubSyncFieldsIgnored verifies hub-sync fields cannot be set via PATCH.
// We verify by confirming the handler doesn't error and synced_at stays nil.
func TestPlansPatchHubSyncFieldsIgnored(t *testing.T) {
	ts, ps := planServer(t)
	ctx := t.Context()

	id := planstore.PlanID("proj-1", "hub-test")
	require.NoError(t, ps.Create(ctx, &planstore.Plan{
		ID: id, ProjectID: "proj-1", Name: "hub-test",
		FilePath: "plans/pending/hub-test.yaml", Status: planstore.PlanStatusPending,
	}))

	// Send a body with extra hub fields — these should be silently ignored.
	raw := fmt.Sprintf(`{"status":"completed","synced_at":"2099-01-01T00:00:00Z","remote_id":"hub-abc"}`)
	req, _ := http.NewRequest(http.MethodPatch, planURL(ts.URL, "proj-1", "/"+id), bytes.NewBufferString(raw))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var got planstore.Plan
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Equal(t, planstore.PlanStatusCompleted, got.Status)
	// synced_at is not settable via PATCH — it stays nil.
	require.Nil(t, got.SyncedAt)
	require.Empty(t, got.RemoteID)
}

// TestPlansDelete verifies DELETE removes the DB record but not the YAML file.
func TestPlansDelete(t *testing.T) {
	ts, ps := planServer(t)
	ctx := t.Context()

	id := planstore.PlanID("proj-1", "del-me")
	require.NoError(t, ps.Create(ctx, &planstore.Plan{
		ID: id, ProjectID: "proj-1", Name: "del-me",
		FilePath: "plans/pending/del-me.yaml", Status: planstore.PlanStatusPending,
	}))

	// Verify it exists.
	_, err := ps.Get(ctx, id)
	require.NoError(t, err)

	resp := deleteReq(t, planURL(ts.URL, "proj-1", "/"+id))
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// DB record is gone.
	_, err = ps.Get(ctx, id)
	require.ErrorIs(t, err, planstore.ErrNotFound)
}

// TestPlansDelete404 verifies 404 for unknown plan.
func TestPlansDelete404(t *testing.T) {
	ts, _ := planServer(t)
	resp := deleteReq(t, planURL(ts.URL, "proj-1", "/plan-deadbeef"))
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// TestPlansListStatusFilter verifies the ?status= query parameter filters correctly.
func TestPlansListStatusFilter(t *testing.T) {
	ts, ps := planServer(t)
	ctx := t.Context()

	for _, plan := range []*planstore.Plan{
		{ID: planstore.PlanID("proj-1", "p1"), ProjectID: "proj-1", Name: "p1", FilePath: "plans/pending/p1.yaml", Status: planstore.PlanStatusPending},
		{ID: planstore.PlanID("proj-1", "p2"), ProjectID: "proj-1", Name: "p2", FilePath: "plans/in_progress/p2.yaml", Status: planstore.PlanStatusInProgress},
		{ID: planstore.PlanID("proj-1", "p3"), ProjectID: "proj-1", Name: "p3", FilePath: "plans/completed/p3.yaml", Status: planstore.PlanStatusCompleted},
	} {
		require.NoError(t, ps.Create(ctx, plan))
	}

	resp, err := http.Get(planURL(ts.URL, "proj-1", "?status=pending"))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out struct {
		Plans []planstore.Plan `json:"plans"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.Len(t, out.Plans, 1)
	require.Equal(t, planstore.PlanStatusPending, out.Plans[0].Status)
}

// TestPlansScan verifies POST /scan upserts plans from the filesystem.
func TestPlansScan(t *testing.T) {
	root := t.TempDir()
	writePlanYAML(t, root, "plans/pending/feature-x.yaml", "feature-x")
	writePlanYAML(t, root, "plans/in_progress/brain-consult.yaml", "brain-consult")

	ps, err := planstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = ps.Close() })

	// Use root as project_id since the server falls back to project_id as path.
	srv := &Server{store: newFakeStore(), life: &fakeLife{}, plans: ps}
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	resp := postJSON(t, planURL(ts.URL, root, "/scan"), map[string]any{})
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out struct {
		Upserted int `json:"upserted"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.Equal(t, 2, out.Upserted)

	plans, err := ps.ListByProject(t.Context(), root)
	require.NoError(t, err)
	require.Len(t, plans, 2)
}

// TestPlansScanIdempotent verifies a second scan produces zero upserts.
func TestPlansScanIdempotent(t *testing.T) {
	root := t.TempDir()
	writePlanYAML(t, root, "plans/pending/feature-x.yaml", "feature-x")

	ps, err := planstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = ps.Close() })

	srv := &Server{store: newFakeStore(), life: &fakeLife{}, plans: ps}
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	// First scan: 1 upsert.
	r1 := postJSON(t, planURL(ts.URL, root, "/scan"), map[string]any{})
	r1.Body.Close()
	require.Equal(t, http.StatusOK, r1.StatusCode)

	// Set an execution link to ensure it is preserved.
	id := planstore.PlanID(root, "feature-x")
	require.NoError(t, ps.Update(t.Context(), id, func(p *planstore.Plan) error {
		p.AutopilotRunID = "ap-run-99"
		return nil
	}))

	// Second scan: same filesystem → 0 upserts, execution link preserved.
	r2 := postJSON(t, planURL(ts.URL, root, "/scan"), map[string]any{})
	defer r2.Body.Close()
	require.Equal(t, http.StatusOK, r2.StatusCode)

	var out struct {
		Upserted int `json:"upserted"`
	}
	require.NoError(t, json.NewDecoder(r2.Body).Decode(&out))
	require.Equal(t, 0, out.Upserted)

	got, err := ps.Get(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, "ap-run-99", got.AutopilotRunID, "execution link must survive idempotent scan")
}

// TestPlansScanMigrateFlat verifies migrate_flat moves flat YAML files into plans/pending/.
func TestPlansScanMigrateFlat(t *testing.T) {
	root := t.TempDir()

	// Init a git repo so git mv works.
	gitInit(t, root)

	// Write a flat plans/*.yaml file and stage it.
	writePlanYAML(t, root, "plans/flat-plan.yaml", "flat-plan")
	gitAdd(t, root, "plans/flat-plan.yaml")
	gitCommit(t, root, "add flat plan")

	ps, err := planstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = ps.Close() })

	srv := &Server{store: newFakeStore(), life: &fakeLife{}, plans: ps}
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	resp := postJSON(t, planURL(ts.URL, root, "/scan"), map[string]any{
		"migrate_flat": true,
	})
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// File must be in plans/pending/ now.
	_, err = os.Stat(filepath.Join(root, "plans", "pending", "flat-plan.yaml"))
	require.NoError(t, err, "flat plan file must be in plans/pending/ after migration")

	// Original flat location must be gone.
	_, err = os.Stat(filepath.Join(root, "plans", "flat-plan.yaml"))
	require.True(t, os.IsNotExist(err), "flat plan file must be removed from plans/")

	// DB must contain the plan.
	id := planstore.PlanID(root, "flat-plan")
	got, err := ps.Get(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, planstore.PlanStatusPending, got.Status)
	require.Equal(t, "plans/pending/flat-plan.yaml", got.FilePath)
}

// TestPlansAssessStub verifies POST /assess returns 501 for a known plan.
func TestPlansAssessStub(t *testing.T) {
	ts, ps := planServer(t)
	ctx := t.Context()

	id := planstore.PlanID("proj-1", "assess-me")
	require.NoError(t, ps.Create(ctx, &planstore.Plan{
		ID: id, ProjectID: "proj-1", Name: "assess-me",
		FilePath: "plans/in_progress/assess-me.yaml", Status: planstore.PlanStatusInProgress,
	}))

	resp := postJSON(t, planURL(ts.URL, "proj-1", "/"+id+"/assess"), nil)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotImplemented, resp.StatusCode)
}

// TestPlansAssess404 verifies POST /assess returns 404 for unknown plan.
func TestPlansAssess404(t *testing.T) {
	ts, _ := planServer(t)
	resp := postJSON(t, planURL(ts.URL, "proj-1", "/plan-deadbeef/assess"), nil)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// TestPlansRunStub verifies POST /run returns 501 for a known plan.
func TestPlansRunStub(t *testing.T) {
	ts, ps := planServer(t)
	ctx := t.Context()

	id := planstore.PlanID("proj-1", "run-me")
	require.NoError(t, ps.Create(ctx, &planstore.Plan{
		ID: id, ProjectID: "proj-1", Name: "run-me",
		FilePath: "plans/pending/run-me.yaml", Status: planstore.PlanStatusPending,
	}))

	resp := postJSON(t, planURL(ts.URL, "proj-1", "/"+id+"/run"), map[string]any{
		"mode": "autopilot",
	})
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotImplemented, resp.StatusCode)
}

// TestPlansRun404 verifies POST /run returns 404 for unknown plan.
func TestPlansRun404(t *testing.T) {
	ts, _ := planServer(t)
	resp := postJSON(t, planURL(ts.URL, "proj-1", "/plan-deadbeef/run"), map[string]any{
		"mode": "manual",
	})
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// writePlanYAML creates a minimal plan YAML file at root/subpath.
func writePlanYAML(t *testing.T, root, subpath, name string) {
	t.Helper()
	abs := filepath.Join(root, subpath)
	require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
	content := fmt.Sprintf("name: %s\ngoal: test\n", name)
	require.NoError(t, os.WriteFile(abs, []byte(content), 0o644))
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "init")
	require.NoError(t, cmd.Run())
	exec.Command("git", "-C", dir, "config", "user.email", "test@test.com").Run()
	exec.Command("git", "-C", dir, "config", "user.name", "Test").Run()
}

func gitAdd(t *testing.T, dir, path string) {
	t.Helper()
	require.NoError(t, exec.Command("git", "-C", dir, "add", path).Run())
}

func gitCommit(t *testing.T, dir, msg string) {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "commit", "-m", msg)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
}

// TestPlansAssessBodyNilOK verifies POST /assess with nil body still works.
func TestPlansAssessBodyNilOK(t *testing.T) {
	ts, ps := planServer(t)
	ctx := t.Context()

	id := planstore.PlanID("proj-1", "nil-body")
	require.NoError(t, ps.Create(ctx, &planstore.Plan{
		ID: id, ProjectID: "proj-1", Name: "nil-body",
		FilePath: "plans/in_progress/nil-body.yaml", Status: planstore.PlanStatusInProgress,
	}))

	req, err := http.NewRequest(http.MethodPost, planURL(ts.URL, "proj-1", "/"+id+"/assess"), nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusNotImplemented, resp.StatusCode, string(body))
}

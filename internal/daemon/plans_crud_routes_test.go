package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/planstore"
)

// crudPlanServer builds a route server for the /api/v1/plans CRUD surface.
// project_id is a real temp directory so PlanService can write YAML.
func crudPlanServer(t *testing.T) (*httptest.Server, *planstore.Store, string) {
	t.Helper()
	root := t.TempDir()
	ps, err := planstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = ps.Close() })
	srv := &Server{store: newFakeStore(), life: &fakeLife{}, plans: ps}
	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)
	return ts, ps, root
}

func crudPlansURL(base, extra string, query url.Values) string {
	u := base + "/api/v1/plans" + extra
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return u
}

func sampleCreateBody(projectID, name string) map[string]any {
	return map[string]any{
		"project_id":  projectID,
		"name":        name,
		"goal":        "ship the feature",
		"tasks":       []map[string]any{{"id": "t1", "prompt": "do the work"}},
		"constraints": []string{"stay in lane"},
		"done_when":   []string{"tests pass"},
	}
}

func decodePlan(t *testing.T, resp *http.Response) oapi.Plan {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Truef(t, resp.StatusCode >= 200 && resp.StatusCode < 300, "status %d: %s", resp.StatusCode, body)
	var p oapi.Plan
	require.NoError(t, json.Unmarshal(body, &p), string(body))
	return p
}

func TestPlanCRUDCreateThenGet(t *testing.T) {
	ts, _, root := crudPlanServer(t)

	resp := postJSON(t, crudPlansURL(ts.URL, "", nil), sampleCreateBody(root, "feature-x"))
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	created := decodePlan(t, resp)
	require.Equal(t, "feature-x", created.Name)
	require.Equal(t, root, created.ProjectId)
	require.Equal(t, oapi.PlanStatusPending, created.Status)
	require.Equal(t, "ship the feature", created.Goal)
	require.Len(t, created.Tasks, 1)
	require.Equal(t, "t1", created.Tasks[0].Id)
	require.Equal(t, oapi.TaskStatus("pending"), created.TaskProgress["t1"])
	require.Equal(t, filepath.Join("plans", "pending", "feature-x.yaml"), created.FilePath)
	require.FileExists(t, filepath.Join(root, created.FilePath))

	get, err := http.Get(ts.URL + "/api/v1/plans/" + created.Id)
	require.NoError(t, err)
	defer get.Body.Close()
	require.Equal(t, http.StatusOK, get.StatusCode)
	got := decodePlan(t, get)
	require.Equal(t, created.Id, got.Id)
	require.Equal(t, created.Name, got.Name)
	require.Equal(t, created.Goal, got.Goal)
	require.Equal(t, created.Status, got.Status)
}

func TestPlanCRUDCreateThenUpdatePendingOnly(t *testing.T) {
	ts, _, root := crudPlanServer(t)

	createdResp := postJSON(t, crudPlansURL(ts.URL, "", nil), sampleCreateBody(root, "editable"))
	defer createdResp.Body.Close()
	created := decodePlan(t, createdResp)

	patch := planPatch(t, ts.URL+"/api/v1/plans/"+created.Id, map[string]any{
		"goal": "updated goal",
		"tasks": []map[string]any{
			{"id": "t1", "prompt": "do the work"},
			{"id": "t2", "prompt": "review it", "after": []string{"t1"}},
		},
	})
	defer patch.Body.Close()
	require.Equal(t, http.StatusOK, patch.StatusCode)
	updated := decodePlan(t, patch)
	require.Equal(t, "updated goal", updated.Goal)
	require.Len(t, updated.Tasks, 2)

	run := postJSON(t, ts.URL+"/api/v1/plans/"+created.Id+"/run", map[string]any{
		"execution_mode": "manual",
	})
	defer run.Body.Close()
	require.Equal(t, http.StatusOK, run.StatusCode)

	blocked := planPatch(t, ts.URL+"/api/v1/plans/"+created.Id, map[string]any{
		"goal": "should fail",
	})
	defer blocked.Body.Close()
	require.Equal(t, http.StatusConflict, blocked.StatusCode)
}

func TestPlanCRUDRunTaskStatusCompleteHappyPath(t *testing.T) {
	ts, _, root := crudPlanServer(t)

	createdResp := postJSON(t, crudPlansURL(ts.URL, "", nil), sampleCreateBody(root, "happy-path"))
	defer createdResp.Body.Close()
	created := decodePlan(t, createdResp)

	run := postJSON(t, ts.URL+"/api/v1/plans/"+created.Id+"/run", map[string]any{
		"execution_mode": "manual",
	})
	defer run.Body.Close()
	require.Equal(t, http.StatusOK, run.StatusCode)
	running := decodePlan(t, run)
	require.Equal(t, oapi.PlanStatusInProgress, running.Status)
	require.Equal(t, oapi.PlanExecutionModeManual, running.ExecutionMode)
	require.Equal(t, filepath.Join("plans", "in_progress", "happy-path.yaml"), running.FilePath)
	require.FileExists(t, filepath.Join(root, running.FilePath))
	_, err := os.Stat(filepath.Join(root, "plans", "pending", "happy-path.yaml"))
	require.True(t, os.IsNotExist(err))

	status := postJSON(t, ts.URL+"/api/v1/plans/"+created.Id+"/tasks/t1/status", map[string]any{
		"status": "done",
	})
	defer status.Body.Close()
	require.Equal(t, http.StatusOK, status.StatusCode)
	progressed := decodePlan(t, status)
	require.Equal(t, oapi.TaskStatus("done"), progressed.TaskProgress["t1"])

	complete := postJSON(t, ts.URL+"/api/v1/plans/"+created.Id+"/complete", nil)
	defer complete.Body.Close()
	require.Equal(t, http.StatusOK, complete.StatusCode)
	done := decodePlan(t, complete)
	require.Equal(t, oapi.PlanStatusCompleted, done.Status)
	require.Equal(t, filepath.Join("plans", "completed", "happy-path.yaml"), done.FilePath)
	require.FileExists(t, filepath.Join(root, done.FilePath))
	require.False(t, done.CompletedAt.IsZero())
}

func TestPlanCRUDCompleteBlockedByIncompleteTasks(t *testing.T) {
	ts, _, root := crudPlanServer(t)

	createdResp := postJSON(t, crudPlansURL(ts.URL, "", nil), sampleCreateBody(root, "sad-path"))
	defer createdResp.Body.Close()
	created := decodePlan(t, createdResp)

	run := postJSON(t, ts.URL+"/api/v1/plans/"+created.Id+"/run", map[string]any{
		"execution_mode": "manual",
	})
	defer run.Body.Close()
	require.Equal(t, http.StatusOK, run.StatusCode)

	complete := postJSON(t, ts.URL+"/api/v1/plans/"+created.Id+"/complete", nil)
	defer complete.Body.Close()
	require.Equal(t, http.StatusUnprocessableEntity, complete.StatusCode)

	var body oapi.PlanCompletionError
	require.NoError(t, json.NewDecoder(complete.Body).Decode(&body))
	require.Contains(t, body.IncompleteTasks, "t1")
	require.NotEmpty(t, body.Error)
}

func TestPlanCRUDListAndArchive(t *testing.T) {
	ts, _, root := crudPlanServer(t)

	createdResp := postJSON(t, crudPlansURL(ts.URL, "", nil), sampleCreateBody(root, "list-me"))
	defer createdResp.Body.Close()
	created := decodePlan(t, createdResp)

	list, err := http.Get(crudPlansURL(ts.URL, "", url.Values{
		"project_id": {root},
		"status":     {"pending"},
	}))
	require.NoError(t, err)
	defer list.Body.Close()
	require.Equal(t, http.StatusOK, list.StatusCode)
	var plans []oapi.Plan
	require.NoError(t, json.NewDecoder(list.Body).Decode(&plans))
	require.Len(t, plans, 1)
	require.Equal(t, created.Id, plans[0].Id)

	arch := postJSON(t, ts.URL+"/api/v1/plans/"+created.Id+"/archive", nil)
	defer arch.Body.Close()
	require.Equal(t, http.StatusOK, arch.StatusCode)
	archived := decodePlan(t, arch)
	require.Equal(t, oapi.PlanStatusArchived, archived.Status)
	require.FileExists(t, filepath.Join(root, "plans", "archived", "list-me.yaml"))
}

func TestPlanCRUDCreateValidation(t *testing.T) {
	ts, _, root := crudPlanServer(t)

	resp := postJSON(t, crudPlansURL(ts.URL, "", nil), map[string]any{
		"project_id":  root,
		"name":        "no-tasks",
		"goal":        "missing tasks",
		"tasks":       []map[string]any{},
		"constraints": []string{},
		"done_when":   []string{},
	})
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	var body struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.Contains(t, body.Error, "tasks")
}

func TestPlanCRUDGet404(t *testing.T) {
	ts, _, _ := crudPlanServer(t)
	resp, err := http.Get(ts.URL + "/api/v1/plans/plan-deadbeef")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestPlanCRUDCreateDuplicate(t *testing.T) {
	ts, _, root := crudPlanServer(t)
	body := sampleCreateBody(root, "dup-plan")

	r1 := postJSON(t, crudPlansURL(ts.URL, "", nil), body)
	r1.Body.Close()
	require.Equal(t, http.StatusCreated, r1.StatusCode)

	r2 := postJSON(t, crudPlansURL(ts.URL, "", nil), body)
	defer r2.Body.Close()
	require.Equal(t, http.StatusBadRequest, r2.StatusCode)
}

func TestPlanCRUDUnconfigured(t *testing.T) {
	srv := &Server{store: newFakeStore(), life: &fakeLife{}}
	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)

	resp, err := http.Get(fmt.Sprintf("%s/api/v1/plans?project_id=proj-1", ts.URL))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

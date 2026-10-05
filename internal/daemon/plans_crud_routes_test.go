package daemon

import (
	"context"
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
// project_id is a temp directory path (local-project convention); no plans/
// directory is required for DB-native PlanService operations.
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
	require.Equal(t, "", created.FilePath)
	require.Equal(t, int64(1), created.Revision)
	require.NotEmpty(t, created.ContentHash)
	_, err := os.Stat(filepath.Join(root, "plans"))
	require.True(t, os.IsNotExist(err))

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
	require.Equal(t, int64(2), updated.Revision)
	require.Equal(t, created.Revision, int64(1))

	stale := planPatch(t, ts.URL+"/api/v1/plans/"+created.Id, map[string]any{
		"goal":              "stale",
		"expected_revision": 1,
	})
	defer stale.Body.Close()
	require.Equal(t, http.StatusConflict, stale.StatusCode)
	var conflict oapi.PlanMutationConflict
	require.NoError(t, json.NewDecoder(stale.Body).Decode(&conflict))
	require.Equal(t, created.Id, conflict.PlanId)
	require.Equal(t, int64(1), conflict.Expected)
	require.Equal(t, int64(2), conflict.Actual)

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
	require.Equal(t, "", running.FilePath)
	_, err := os.Stat(filepath.Join(root, "plans"))
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
	require.Equal(t, "", done.FilePath)
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
	require.False(t, archived.ArchivedAt.IsZero())
	_, err = os.Stat(filepath.Join(root, "plans"))
	require.True(t, os.IsNotExist(err))
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

func createTwoTaskPendingPlan(t *testing.T, ts *httptest.Server, root, name string) oapi.Plan {
	t.Helper()
	resp := postJSON(t, crudPlansURL(ts.URL, "", nil), map[string]any{
		"project_id": root,
		"name":       name,
		"goal":       "mutate tasks",
		"tasks": []map[string]any{
			{"id": "t1", "prompt": "first"},
			{"id": "t2", "prompt": "second", "after": []string{"t1"}},
		},
		"constraints": []string{},
		"done_when":   []string{},
	})
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	return decodePlan(t, resp)
}

func planDelete(t *testing.T, rawURL string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, rawURL, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

func TestPlanCRUDTaskAddUpdateDelete(t *testing.T) {
	ts, _, root := crudPlanServer(t)
	created := createTwoTaskPendingPlan(t, ts, root, "task-mutations")

	add := postJSON(t, ts.URL+"/api/v1/plans/"+created.Id+"/tasks", map[string]any{
		"id":     "t3",
		"prompt": "third",
		"after":  []string{"t2"},
	})
	defer add.Body.Close()
	require.Equal(t, http.StatusOK, add.StatusCode)
	afterAdd := decodePlan(t, add)
	require.Len(t, afterAdd.Tasks, 3)
	require.Equal(t, "t3", afterAdd.Tasks[2].Id)
	require.Equal(t, oapi.TaskStatus("pending"), afterAdd.TaskProgress["t3"])
	require.Equal(t, created.Revision+1, afterAdd.Revision)

	prompt := "refined second"
	patch := planPatch(t, ts.URL+"/api/v1/plans/"+created.Id+"/tasks/t2/definition", map[string]any{
		"prompt":            prompt,
		"after":             []string{"t1"},
		"expected_revision": afterAdd.Revision,
	})
	defer patch.Body.Close()
	require.Equal(t, http.StatusOK, patch.StatusCode)
	afterPatch := decodePlan(t, patch)
	require.Equal(t, prompt, afterPatch.Tasks[1].Prompt)
	require.Equal(t, afterAdd.Revision+1, afterPatch.Revision)

	del := planDelete(t, fmt.Sprintf("%s/api/v1/plans/%s/tasks/t3?expected_revision=%d",
		ts.URL, created.Id, afterPatch.Revision))
	defer del.Body.Close()
	require.Equal(t, http.StatusOK, del.StatusCode)
	afterDel := decodePlan(t, del)
	require.Len(t, afterDel.Tasks, 2)
	_, ok := afterDel.TaskProgress["t3"]
	require.False(t, ok)
	require.Equal(t, afterPatch.Revision+1, afterDel.Revision)
}

func TestPlanCRUDTaskAddValidationAndConflicts(t *testing.T) {
	ts, _, root := crudPlanServer(t)
	created := createTwoTaskPendingPlan(t, ts, root, "task-add-errors")

	cycle := postJSON(t, ts.URL+"/api/v1/plans/"+created.Id+"/tasks", map[string]any{
		"id":     "t0",
		"prompt": "cycle seed",
		"after":  []string{"t2"},
	})
	defer cycle.Body.Close()
	require.Equal(t, http.StatusOK, cycle.StatusCode)
	withT0 := decodePlan(t, cycle)

	// Make t2 depend on t0 while t0 already depends on t2 → cycle.
	bad := planPatch(t, ts.URL+"/api/v1/plans/"+created.Id+"/tasks/t2/definition", map[string]any{
		"after": []string{"t0"},
	})
	defer bad.Body.Close()
	require.Equal(t, http.StatusBadRequest, bad.StatusCode)

	missing := postJSON(t, ts.URL+"/api/v1/plans/"+created.Id+"/tasks", map[string]any{
		"id":     "t9",
		"prompt": "missing dep",
		"after":  []string{"nope"},
	})
	defer missing.Body.Close()
	require.Equal(t, http.StatusBadRequest, missing.StatusCode)

	stale := postJSON(t, ts.URL+"/api/v1/plans/"+created.Id+"/tasks", map[string]any{
		"id":                "t4",
		"prompt":            "stale",
		"expected_revision": 1,
	})
	defer stale.Body.Close()
	require.Equal(t, http.StatusConflict, stale.StatusCode)
	var conflict oapi.PlanMutationConflict
	require.NoError(t, json.NewDecoder(stale.Body).Decode(&conflict))
	require.Equal(t, created.Id, conflict.PlanId)
	require.Equal(t, int64(1), conflict.Expected)
	require.Equal(t, withT0.Revision, conflict.Actual)

	run := postJSON(t, ts.URL+"/api/v1/plans/"+created.Id+"/run", map[string]any{
		"execution_mode": "manual",
	})
	defer run.Body.Close()
	require.Equal(t, http.StatusOK, run.StatusCode)

	blocked := postJSON(t, ts.URL+"/api/v1/plans/"+created.Id+"/tasks", map[string]any{
		"id":     "extra",
		"prompt": "should fail",
	})
	defer blocked.Body.Close()
	require.Equal(t, http.StatusConflict, blocked.StatusCode)
}

func TestPlanCRUDTaskDeleteBlockedByDependent(t *testing.T) {
	ts, _, root := crudPlanServer(t)
	created := createTwoTaskPendingPlan(t, ts, root, "task-rm-blocked")

	del := planDelete(t, ts.URL+"/api/v1/plans/"+created.Id+"/tasks/t1")
	defer del.Body.Close()
	require.Equal(t, http.StatusBadRequest, del.StatusCode)

	var body struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.NewDecoder(del.Body).Decode(&body))
	require.Contains(t, body.Error, "depends")
}

func TestPlanCRUDTaskUpdateNotPendingAndNotFound(t *testing.T) {
	ts, _, root := crudPlanServer(t)
	created := createTwoTaskPendingPlan(t, ts, root, "task-update-gate")

	run := postJSON(t, ts.URL+"/api/v1/plans/"+created.Id+"/run", map[string]any{
		"execution_mode": "manual",
	})
	defer run.Body.Close()
	require.Equal(t, http.StatusOK, run.StatusCode)

	blocked := planPatch(t, ts.URL+"/api/v1/plans/"+created.Id+"/tasks/t1/definition", map[string]any{
		"prompt": "nope",
	})
	defer blocked.Body.Close()
	require.Equal(t, http.StatusConflict, blocked.StatusCode)

	missing := planPatch(t, ts.URL+"/api/v1/plans/plan-deadbeef/tasks/t1/definition", map[string]any{
		"prompt": "nope",
	})
	defer missing.Body.Close()
	require.Equal(t, http.StatusNotFound, missing.StatusCode)

	delMissing := planDelete(t, ts.URL+"/api/v1/plans/plan-deadbeef/tasks/t1")
	defer delMissing.Body.Close()
	require.Equal(t, http.StatusNotFound, delMissing.StatusCode)
}

func TestPlanCRUDDelete(t *testing.T) {
	ts, _, root := crudPlanServer(t)
	createdResp := postJSON(t, crudPlansURL(ts.URL, "", nil), sampleCreateBody(root, "delete-me"))
	defer createdResp.Body.Close()
	created := decodePlan(t, createdResp)

	del := planDelete(t, ts.URL+"/api/v1/plans/"+created.Id)
	defer del.Body.Close()
	require.Equal(t, http.StatusOK, del.StatusCode)

	get, err := http.Get(ts.URL + "/api/v1/plans/" + created.Id)
	require.NoError(t, err)
	defer get.Body.Close()
	require.Equal(t, http.StatusNotFound, get.StatusCode)

	again := planDelete(t, ts.URL+"/api/v1/plans/"+created.Id)
	defer again.Body.Close()
	require.Equal(t, http.StatusNotFound, again.StatusCode)
}

func TestPlanCRUDDeleteStatusGate(t *testing.T) {
	cases := []struct {
		status planstore.PlanStatus
		code   int
		msg    string
	}{
		{planstore.PlanStatusInProgress, http.StatusConflict, "plan is in progress; stop and archive it before deleting"},
		{planstore.PlanStatusCompleted, http.StatusConflict, "plan is completed; archive it before deleting"},
		{planstore.PlanStatusArchived, http.StatusOK, ""},
	}
	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			ts, ps, root := crudPlanServer(t)
			createdResp := postJSON(t, crudPlansURL(ts.URL, "", nil), sampleCreateBody(root, "gate-"+string(tc.status)))
			defer createdResp.Body.Close()
			created := decodePlan(t, createdResp)
			require.NoError(t, ps.Update(context.Background(), created.Id, func(p *planstore.Plan) error {
				p.Status = tc.status
				return nil
			}))
			del := planDelete(t, ts.URL+"/api/v1/plans/"+created.Id)
			defer del.Body.Close()
			require.Equal(t, tc.code, del.StatusCode)
			if tc.msg != "" {
				body, _ := io.ReadAll(del.Body)
				require.Contains(t, string(body), tc.msg)
			}
		})
	}
}

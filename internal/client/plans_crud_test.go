package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPlansCRUDClientRoundTrip(t *testing.T) {
	const planJSON = `{"id":"plan-ab12cd34","project_id":"/tmp/proj","name":"feature-x",
		"goal":"ship","file_path":"plans/pending/feature-x.yaml","status":"pending",
		"constraints":["stay in lane"],"done_when":["tests pass"],
		"tasks":[{"id":"t1","prompt":"do the work"}],
		"task_progress":{"t1":"pending"},
		"created_at":"2026-09-28T00:00:00Z","updated_at":"2026-09-28T00:00:00Z"}`

	var last struct {
		method, path, body string
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		last.method, last.path, last.body = r.Method, r.URL.Path, string(b)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/plans":
			require.Equal(t, "/tmp/proj", r.URL.Query().Get("project_id"))
			require.Equal(t, "pending", r.URL.Query().Get("status"))
			_, _ = w.Write([]byte("[" + planJSON + "]"))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/plans":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(planJSON))
		case r.URL.Path == "/api/v1/plans/plan-ab12cd34" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(planJSON))
		case r.URL.Path == "/api/v1/plans/plan-ab12cd34" && r.Method == http.MethodPatch:
			_, _ = w.Write([]byte(planJSON))
		case r.URL.Path == "/api/v1/plans/plan-ab12cd34/run":
			_, _ = w.Write([]byte(planJSON))
		case r.URL.Path == "/api/v1/plans/plan-ab12cd34/tasks/t1/status":
			_, _ = w.Write([]byte(planJSON))
		case r.URL.Path == "/api/v1/plans/plan-ab12cd34/complete":
			_, _ = w.Write([]byte(planJSON))
		case r.URL.Path == "/api/v1/plans/plan-ab12cd34/archive":
			_, _ = w.Write([]byte(planJSON))
		case r.URL.Path == "/api/v1/plans/plan-ab12cd34/sync_to_repo":
			_, _ = w.Write([]byte(`{"plan_id":"plan-ab12cd34","revision":1,"content_hash":"sha256:x",
				"repository":"github.com/example/repo","target_ref":"main",
				"output_path":"plans/pending/feature-x.yaml","branch":"warden/plan-sync/plan-ab12cd34/1",
				"commit_sha":"abc","pr_url":"https://example.test/pull/1","outcome":"success","reused":false}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	c := New(ts.URL)
	ctx := context.Background()

	listed, err := c.PlansList(ctx, "/tmp/proj", "pending")
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, "plan-ab12cd34", listed[0].ID)

	got, err := c.PlansGet(ctx, "plan-ab12cd34")
	require.NoError(t, err)
	require.Equal(t, "feature-x", got.Name)
	require.Equal(t, "ship", got.Goal)

	created, err := c.PlansCreate(ctx, PlansCreateRequest{
		ProjectID: "/tmp/proj",
		Name:      "feature-x",
		Goal:      "ship",
		Tasks:     []PlanTaskSpec{{ID: "t1", Prompt: "do the work"}},
	})
	require.NoError(t, err)
	require.Equal(t, "plan-ab12cd34", created.ID)
	require.Equal(t, http.MethodPost, last.method)
	require.Contains(t, last.body, `"goal":"ship"`)

	updated, err := c.PlansUpdate(ctx, "plan-ab12cd34", PlansUpdateRequest{Goal: "ship better"})
	require.NoError(t, err)
	require.Equal(t, "feature-x", updated.Name)
	require.Equal(t, http.MethodPatch, last.method)

	ran, err := c.PlansRun(ctx, "plan-ab12cd34", "manual")
	require.NoError(t, err)
	require.NotNil(t, ran)
	require.Equal(t, "/api/v1/plans/plan-ab12cd34/run", last.path)
	require.Contains(t, last.body, `"execution_mode":"manual"`)

	st, err := c.PlansUpdateTaskStatus(ctx, "plan-ab12cd34", "t1", "done")
	require.NoError(t, err)
	require.NotNil(t, st)
	require.Equal(t, "/api/v1/plans/plan-ab12cd34/tasks/t1/status", last.path)

	done, err := c.PlansComplete(ctx, "plan-ab12cd34")
	require.NoError(t, err)
	require.NotNil(t, done)
	require.Equal(t, "/api/v1/plans/plan-ab12cd34/complete", last.path)

	arch, err := c.PlansArchive(ctx, "plan-ab12cd34")
	require.NoError(t, err)
	require.NotNil(t, arch)
	require.Equal(t, "/api/v1/plans/plan-ab12cd34/archive", last.path)

	syncRes, err := c.PlansSyncToRepo(ctx, "plan-ab12cd34", PlansSyncToRepoRequest{TargetRef: "main"})
	require.NoError(t, err)
	require.Equal(t, "success", syncRes.Outcome)
	require.Equal(t, "/api/v1/plans/plan-ab12cd34/sync_to_repo", last.path)
	require.Contains(t, last.body, `"target_ref":"main"`)
}

func TestPlansCompleteSurfacesStructuredError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":            "incomplete tasks: t1, t2",
			"incomplete_tasks": []string{"t1", "t2"},
		})
	}))
	defer ts.Close()

	_, err := New(ts.URL).PlansComplete(context.Background(), "plan-ab12cd34")
	require.Error(t, err)
	var se *StatusError
	require.ErrorAs(t, err, &se)
	require.Equal(t, http.StatusUnprocessableEntity, se.Code)
	require.Contains(t, se.Msg, "incomplete tasks: t1, t2")
	require.Contains(t, string(se.Body), `"t1"`)
}

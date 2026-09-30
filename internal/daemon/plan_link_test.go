package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/ctxstore"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/store"
)

// planLinkServer wires projects + plans + pipelines + spawn for plan-link tests.
func planLinkServer(t *testing.T) (*httptest.Server, *planstore.Store, *projectstore.Store, *pipeline.Store) {
	t.Helper()
	plans, err := planstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = plans.Close() })
	projs, err := projectstore.NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { projs.Close() })
	ps, err := pipeline.NewStore(t.TempDir())
	require.NoError(t, err)
	cs, err := ctxstore.New(t.TempDir())
	require.NoError(t, err)
	ss := newFakeStore()
	life := &fakeLife{}
	exec := NewExecutor(ps, ss, life, cs, func() {})
	srv := &Server{
		store:    ss,
		life:     life,
		exec:     exec,
		projects: projs,
		plans:    plans,
		hub:      newHub(),
		done:     make(chan struct{}),
	}
	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)
	return ts, plans, projs, ps
}

func seedPlan(t *testing.T, plans *planstore.Store, projectID, name string) *planstore.Plan {
	t.Helper()
	p := &planstore.Plan{
		ID:        planstore.PlanID(projectID, name),
		ProjectID: projectID,
		Name:      name,
		FilePath:  "plans/pending/" + name + ".yaml",
		Status:    planstore.PlanStatusPending,
	}
	require.NoError(t, plans.Create(context.Background(), p))
	return p
}

func TestValidatePlanLinkEmptyAlwaysOK(t *testing.T) {
	srv := &Server{}
	code, msg := srv.validatePlanLink(context.Background(), "", "proj")
	require.Equal(t, 0, code)
	require.Empty(t, msg)
}

func TestSpawnPlanlessAnalysisAgent(t *testing.T) {
	ts, _, projs, _ := planLinkServer(t)
	projDir := t.TempDir()
	proj, err := projs.OpenProject(projDir, "analysis-proj", projDir)
	require.NoError(t, err)

	body := map[string]any{
		"type": "analysis", "ticket": "analysis-1", "repo": projDir,
		"project_id": proj.ID, "cwd": projDir, "prompt": "look around",
	}
	b, _ := json.Marshal(body)
	resp, err := http.Post(ts.URL+"/api/v1/spawn", "application/json", bytes.NewReader(b))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var sess store.Session
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&sess))
	require.Equal(t, proj.ID, sess.ProjectID)
	require.Empty(t, sess.PlanID)
}

func TestSpawnPlanBoundWorkerAgent(t *testing.T) {
	ts, plans, projs, _ := planLinkServer(t)
	projDir := t.TempDir()
	proj, err := projs.OpenProject(projDir, "worker-proj", projDir)
	require.NoError(t, err)
	plan := seedPlan(t, plans, proj.ID, "feature-x")

	body := map[string]any{
		"type": "development", "ticket": "worker-1", "repo": projDir,
		"project_id": proj.ID, "plan_id": plan.ID, "role": "worker",
		"cwd": projDir, "prompt": "implement task",
	}
	b, _ := json.Marshal(body)
	resp, err := http.Post(ts.URL+"/api/v1/spawn", "application/json", bytes.NewReader(b))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var sess store.Session
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&sess))
	require.Equal(t, proj.ID, sess.ProjectID)
	require.Equal(t, plan.ID, sess.PlanID)
}

func TestSpawnRejectsUnknownPlanID(t *testing.T) {
	ts, _, projs, _ := planLinkServer(t)
	projDir := t.TempDir()
	proj, err := projs.OpenProject(projDir, "bad-plan-proj", projDir)
	require.NoError(t, err)

	body := map[string]any{
		"prompt": "x", "cwd": projDir, "project_id": proj.ID, "plan_id": "plan-deadbeef",
	}
	b, _ := json.Marshal(body)
	resp, err := http.Post(ts.URL+"/api/v1/spawn", "application/json", bytes.NewReader(b))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestSpawnRejectsPlanFromOtherProject(t *testing.T) {
	ts, plans, projs, _ := planLinkServer(t)
	projA := t.TempDir()
	projB := t.TempDir()
	a, err := projs.OpenProject(projA, "proj-a", projA)
	require.NoError(t, err)
	bProj, err := projs.OpenProject(projB, "proj-b", projB)
	require.NoError(t, err)
	plan := seedPlan(t, plans, a.ID, "owned-by-a")

	body := map[string]any{
		"prompt": "x", "cwd": projB, "project_id": bProj.ID, "plan_id": plan.ID,
	}
	raw, _ := json.Marshal(body)
	resp, err := http.Post(ts.URL+"/api/v1/spawn", "application/json", bytes.NewReader(raw))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestPipelineCreatePlanlessArbitrary(t *testing.T) {
	ts, _, projs, _ := planLinkServer(t)
	projDir := t.TempDir()
	proj, err := projs.OpenProject(projDir, "pipe-proj", projDir)
	require.NoError(t, err)

	p := createPipeline(t, ts.URL, `{"project_id":"`+proj.ID+`","spec":"name: arbitrary\nrepo: `+projDir+`\njobs:\n  - id: a\n    prompt: go\n    worktree: none\n"}`)
	require.Equal(t, proj.ID, p.ProjectID)
	require.Empty(t, p.PlanID)
}

func TestPipelineCreatePlanBound(t *testing.T) {
	ts, plans, projs, _ := planLinkServer(t)
	projDir := t.TempDir()
	proj, err := projs.OpenProject(projDir, "bound-pipe", projDir)
	require.NoError(t, err)
	plan := seedPlan(t, plans, proj.ID, "pipe-plan")

	p := createPipeline(t, ts.URL, `{"project_id":"`+proj.ID+`","plan_id":"`+plan.ID+`","spec":"name: bound\nrepo: `+projDir+`\njobs:\n  - id: a\n    prompt: go\n    worktree: none\n"}`)
	require.Equal(t, proj.ID, p.ProjectID)
	require.Equal(t, plan.ID, p.PlanID)
}

func TestPipelineCreateRejectsUnknownPlanID(t *testing.T) {
	ts, _, projs, _ := planLinkServer(t)
	projDir := t.TempDir()
	proj, err := projs.OpenProject(projDir, "pipe-bad", projDir)
	require.NoError(t, err)

	resp, err := http.Post(ts.URL+"/api/v1/pipelines", "application/json", bytes.NewBufferString(
		`{"project_id":"`+proj.ID+`","plan_id":"plan-missing","spec":"name: bad\nrepo: /r\njobs:\n  - id: a\n    prompt: go\n    worktree: none\n"}`,
	))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestPipelineCreateRejectsPlanFromOtherProject(t *testing.T) {
	ts, plans, projs, _ := planLinkServer(t)
	projA := t.TempDir()
	projB := t.TempDir()
	a, err := projs.OpenProject(projA, "pipe-a", projA)
	require.NoError(t, err)
	bProj, err := projs.OpenProject(projB, "pipe-b", projB)
	require.NoError(t, err)
	plan := seedPlan(t, plans, a.ID, "a-only")

	resp, err := http.Post(ts.URL+"/api/v1/pipelines", "application/json", bytes.NewBufferString(
		`{"project_id":"`+bProj.ID+`","plan_id":"`+plan.ID+`","spec":"name: cross\nrepo: /r\njobs:\n  - id: a\n    prompt: go\n    worktree: none\n"}`,
	))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

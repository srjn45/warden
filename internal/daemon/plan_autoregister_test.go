package daemon

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/projectstore"
)

func newPlanAutoRegServer(t *testing.T) (*httptest.Server, *planstore.Store, *projectstore.Store, *fakeLife) {
	t.Helper()
	plans, err := planstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = plans.Close() })
	projects, err := projectstore.NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = projects.Close() })
	life := &fakeLife{}
	srv := &Server{store: newFakeStore(), life: life, plans: plans, projects: projects}
	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)
	return ts, plans, projects, life
}

func TestCreatePlanRegistersUnknownProject(t *testing.T) {
	ts, _, projects, _ := newPlanAutoRegServer(t)
	dir := t.TempDir()

	resp := postJSON(t, crudPlansURL(ts.URL, "", nil), sampleCreateBody(dir, "feat"))
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	created := decodePlan(t, resp)

	p, err := projects.Get(dir)
	require.NoError(t, err)
	require.Equal(t, projectstore.StatusOpen, projectstore.NormalizeStatus(p.Status))
	require.Equal(t, dir, p.Path)
	require.Equal(t, filepath.Base(dir), p.Name)
	require.Contains(t, p.Plans, created.Id)
}

func TestCreateProjectPlanRegistersUnknownProject(t *testing.T) {
	ts, _, projects, _ := newPlanAutoRegServer(t)
	dir := t.TempDir()

	resp := postJSON(t, planURL(ts.URL, dir, ""), map[string]any{"name": "legacy", "file_path": "plans/pending/legacy.yaml"})
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	p, err := projects.Get(dir)
	require.NoError(t, err)
	require.Equal(t, projectstore.StatusOpen, projectstore.NormalizeStatus(p.Status))
	require.Equal(t, []string{planstore.PlanID(dir, "legacy")}, p.Plans)
}

func TestCreatePlanReopensClosedProject(t *testing.T) {
	ts, _, projects, _ := newPlanAutoRegServer(t)
	dir := t.TempDir()
	closedProjectFixture(t, projects, dir)

	resp := postJSON(t, crudPlansURL(ts.URL, "", nil), sampleCreateBody(dir, "feat"))
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	got, err := projects.Get(dir)
	require.NoError(t, err)
	require.Equal(t, projectstore.StatusOpen, projectstore.NormalizeStatus(got.Status))
	require.Equal(t, "keep-me", got.Name)
	require.Contains(t, got.Plans, "plan-1")
	require.Len(t, got.Plans, 2)
}

func TestRunPlanReopensClosedProject(t *testing.T) {
	ts, plans, projects, life := newPlanAutoRegServer(t)
	root := t.TempDir()
	gitInit(t, root)
	_, err := projects.OpenProject(root, "keep-me", root)
	require.NoError(t, err)
	_, err = projects.CloseProject(root)
	require.NoError(t, err)

	id := planstore.PlanID(root, "manual-plan")
	require.NoError(t, plans.Create(t.Context(), &planstore.Plan{
		ID: id, ProjectID: root, Name: "manual-plan", Goal: "test",
		Tasks:        []planstore.PlanTask{{ID: "t1", Prompt: "task 1"}},
		Status:       planstore.PlanStatusPending,
		TaskProgress: map[string]string{"t1": "pending"},
	}))

	resp := postJSON(t, planURL(ts.URL, root, "/"+id+"/run"), map[string]any{"mode": "manual"})
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NotNil(t, life.spawned)

	p, err := projects.Get(root)
	require.NoError(t, err)
	require.Equal(t, projectstore.StatusOpen, projectstore.NormalizeStatus(p.Status))
	require.Equal(t, "keep-me", p.Name)
	stored, err := plans.Get(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, root, stored.ProjectID, "plan ProjectID is never rewritten")
}

func TestCreatePlanUnknownRelativeProjectStill404s(t *testing.T) {
	ts, _, projects, _ := newPlanAutoRegServer(t)

	resp := postJSON(t, crudPlansURL(ts.URL, "", nil), sampleCreateBody("not-a-path", "feat"))
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	projs, err := projects.List()
	require.NoError(t, err)
	require.Empty(t, projs)
}

func TestListPlansDoesNotRegisterProject(t *testing.T) {
	ts, _, projects, _ := newPlanAutoRegServer(t)
	dir := t.TempDir()

	resp, err := http.Get(crudPlansURL(ts.URL, "", map[string][]string{"project_id": {dir}}))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp2, err := http.Get(planURL(ts.URL, dir, ""))
	require.NoError(t, err)
	defer resp2.Body.Close()

	projs, err := projects.List()
	require.NoError(t, err)
	require.Empty(t, projs, "read-only plan listing registers nothing")
}

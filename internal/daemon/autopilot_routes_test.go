package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/auth"
	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/router"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

type autopilotTestResolver struct{}

func (autopilotTestResolver) Resolve(context.Context, router.ResolveOptions) (*router.Resolution, error) {
	return &router.Resolution{BackendID: "claude", Tier: backendstore.Tier1}, nil
}

// apFakeEnv is a minimal autopilot.Env for route tests: any dir resolves to a
// fixed repo, gh is OK, and the integration branch is auto-created.
type apFakeEnv struct {
	repo    string
	ghErr   error
	created bool
}

func (e *apFakeEnv) GitToplevel(context.Context, string) (string, error) { return e.repo, nil }
func (e *apFakeEnv) DefaultBranch(context.Context, string) (string, error) {
	return "main", nil
}
func (e *apFakeEnv) BranchExists(context.Context, string, string) (bool, error) {
	return e.created, nil
}
func (e *apFakeEnv) CreateBranch(context.Context, string, string, string) error {
	e.created = true
	return nil
}
func (e *apFakeEnv) GHAuthOK(context.Context) error { return e.ghErr }
func (e *apFakeEnv) BackendKnown(string) error      { return nil }
func (e *apFakeEnv) WorkflowsCoverPRs(context.Context, string, string) (bool, error) {
	return false, nil
}

func newAutopilotServer(t *testing.T, env autopilot.Env, plans []string) *httptest.Server {
	t.Helper()
	// A real (fake) lifecycle so enabling spawns a brain through the daemon runtime
	// wired by SetAutopilotController (S3).
	srv := &Server{store: newFakeStore(), life: &fakeLife{}, hub: newHub(), done: make(chan struct{})}
	srv.SetAutopilotController(autopilot.NewController(autopilot.ControllerConfig{
		Plans:             plans,
		IntegrationBranch: "autopilot/integration",
		Gate:              "auto",
		Resolver:          autopilotTestResolver{},
	}, env))
	return httptest.NewServer(srv.router())
}

func TestGuardianVisibilityAndRunStopCleanup(t *testing.T) {
	dir := t.TempDir()
	plan := filepath.Join(dir, "guardian.yaml")
	require.NoError(t, os.WriteFile(plan, []byte("version: 1\ngoal: ship\n"), 0o644))
	st := newFakeStore()
	life := &fakeLife{}
	srv := &Server{store: st, life: life, hub: newHub(), done: make(chan struct{})}
	c := autopilot.NewController(autopilot.ControllerConfig{Plans: []string{plan}, BaseDir: dir,
		IntegrationBranch: "autopilot/integration", Resolver: autopilotTestResolver{}}, &apFakeEnv{repo: dir})
	srv.SetAutopilotController(c)
	status, err := c.Enable(context.Background(), "")
	require.NoError(t, err)
	runID := status.Runs[0].RunID
	guardianID := "guardian-" + strings.TrimPrefix(runID, "ap-")

	listed, err := srv.ListSessions(context.Background(), oapi.ListSessionsRequestObject{})
	require.NoError(t, err)
	visible := listed.(oapi.ListSessions200JSONResponse)
	require.Len(t, visible.Sessions, 1)
	require.Equal(t, "agent-test", visible.Sessions[0].ID)

	listed, err = srv.ListSessions(context.Background(), oapi.ListSessionsRequestObject{Params: oapi.ListSessionsParams{All: true}})
	require.NoError(t, err)
	all := listed.(oapi.ListSessions200JSONResponse)
	require.Len(t, all.Sessions, 2)
	guardian := all.Sessions[0]
	if guardian.ID != guardianID {
		guardian = all.Sessions[1]
	}
	require.Equal(t, guardianID, guardian.ID)
	require.Contains(t, guardian.Tags, "system:true")
	require.Contains(t, guardian.Tags, "autopilot-run:"+runID)

	_, err = c.StopRun(context.Background(), runID)
	require.NoError(t, err)
	guardianSession, err := st.Get(context.Background(), guardianID)
	require.NoError(t, err)
	require.Equal(t, store.StatusDone, guardianSession.Status)
}

func TestGuardianBootReconcileTerminatesOrphans(t *testing.T) {
	st := newFakeStore()
	for _, sess := range []*store.Session{
		{ID: "guardian-live", Status: store.StatusWorking, Tags: []string{"system:true", "autopilot-run:ap-live"}},
		{ID: "guardian-dead", Status: store.StatusDone, Tags: []string{"system:true", "autopilot-run:ap-dead"}},
		{ID: "guardian-orphan", Status: store.StatusWorking, Tags: []string{"system:true", "autopilot-run:ap-gone"}},
		{ID: "system-other", Status: store.StatusWorking, Tags: []string{"system:true"}},
		{ID: "ordinary", Status: store.StatusWorking},
	} {
		require.NoError(t, st.Insert(context.Background(), sess))
	}
	rt := autopilotRuntime{s: &Server{store: st, life: &fakeLife{}, hub: newHub()}}
	missing, err := rt.ReconcileGuardians(context.Background(), map[string]string{"ap-live": "guardian-live", "ap-dead": "guardian-dead", "ap-missing": "guardian-missing"})
	require.NoError(t, err)
	require.Equal(t, []string{"ap-dead", "ap-missing"}, missing)
	live, _ := st.Get(context.Background(), "guardian-live")
	orphan, _ := st.Get(context.Background(), "guardian-orphan")
	ordinary, _ := st.Get(context.Background(), "ordinary")
	other, _ := st.Get(context.Background(), "system-other")
	require.Equal(t, store.StatusWorking, live.Status)
	require.Equal(t, store.StatusDone, orphan.Status)
	require.Equal(t, store.StatusWorking, ordinary.Status)
	require.Equal(t, store.StatusWorking, other.Status, "non-guardian system session is not reconciled")
}

func TestAutopilotEnableStatusDisable(t *testing.T) {
	dir := t.TempDir()
	plan := filepath.Join(dir, "plan.yaml")
	require.NoError(t, os.WriteFile(plan, []byte("version: 1\ngoal: ship\n"), 0o644))

	ts := newAutopilotServer(t, &apFakeEnv{repo: dir}, []string{plan})
	defer ts.Close()

	// GET before enabling → disabled, no runs.
	var st autopilot.Status
	apGetJSON(t, ts.URL+"/api/v1/autopilot", &st)
	require.False(t, st.Enabled)
	require.Empty(t, st.Runs)

	// Enable → active, one run, a headless brain spawned on the first free backend.
	code := apPostJSON(t, ts.URL+"/api/v1/autopilot", `{"enabled":true}`, &st)
	require.Equal(t, http.StatusOK, code)
	require.True(t, st.Enabled)
	require.Len(t, st.Runs, 1)
	require.Equal(t, autopilot.StateActive, st.Runs[0].State)
	require.Equal(t, "autopilot/plan", st.Runs[0].IntegrationBranch)
	require.Contains(t, st.Runs[0].GateWarning, "gate auto downgraded to local")
	require.NotNil(t, st.Runs[0].Brain)
	require.NotEmpty(t, st.Runs[0].Brain.AgentID)

	raw, err := json.Marshal(st.Runs[0])
	require.NoError(t, err)
	require.Contains(t, string(raw), `"integration_branch":"autopilot/plan"`)

	// Disable → kill switch.
	code = apPostJSON(t, ts.URL+"/api/v1/autopilot", `{"enabled":false}`, &st)
	require.Equal(t, http.StatusOK, code)
	require.False(t, st.Enabled)
	require.Len(t, st.Runs, 1)
	require.Equal(t, autopilot.StateStopped, st.Runs[0].State)
}

func TestSpawnAnnotatesWorkerPromptWithIntegrationBranch(t *testing.T) {
	dir := t.TempDir()
	plan := filepath.Join(dir, "ship.yaml")
	require.NoError(t, os.WriteFile(plan, []byte("version: 1\ngoal: ship\n"), 0o644))
	life := &fakeLife{}
	srv := &Server{store: newFakeStore(), life: life, hub: newHub(), done: make(chan struct{})}
	c := autopilot.NewController(autopilot.ControllerConfig{
		Plans: []string{plan}, BaseDir: dir, IntegrationBranch: autopilot.DefaultIntegrationBranch,
		Resolver: autopilotTestResolver{},
	}, &apFakeEnv{repo: dir})
	srv.SetAutopilotController(c)
	st, err := c.Enable(context.Background(), dir)
	require.NoError(t, err)
	require.Equal(t, "autopilot/ship", st.Runs[0].IntegrationBranch)
	brainID := st.Runs[0].Brain.AgentID
	require.NotEmpty(t, brainID)

	ts := httptest.NewServer(srv.router())
	defer ts.Close()
	body, _ := json.Marshal(SpawnRequest{Prompt: "Implement the API", Role: "worker", Ticket: "worker-1", Cwd: t.TempDir()})
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/spawn", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(auth.ActorHeader, brainID)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.NotNil(t, life.spawned)
	require.Contains(t, life.spawned.Prompt, "Implement the API")
	require.Contains(t, life.spawned.Prompt, autopilot.WorkerSpawnBranchPrompt("autopilot/ship"))
}

func TestAutopilotEnable409ListsFailures(t *testing.T) {
	dir := t.TempDir() // plan file intentionally absent
	ts := newAutopilotServer(t, &apFakeEnv{repo: dir}, []string{filepath.Join(dir, "missing.yaml")})
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/v1/autopilot", "application/json", strings.NewReader(`{"enabled":true}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusConflict, resp.StatusCode)

	var body struct {
		Error    string   `json:"error"`
		Failures []string `json:"failures"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.NotEmpty(t, body.Failures)
	require.Contains(t, strings.Join(body.Failures, " "), "plan file not found")
}

func TestAutopilotRunRegistryLifecycleRoutes(t *testing.T) {
	dir := t.TempDir()
	plan := filepath.Join(dir, "named.yaml")
	require.NoError(t, os.WriteFile(plan, []byte("version: 1\ngoal: ship\n"), 0o644))
	ts := newAutopilotServer(t, &apFakeEnv{repo: dir}, nil)
	defer ts.Close()

	var run autopilot.RunStatus
	code := apPostJSON(t, ts.URL+"/api/v1/autopilot/runs", `{"name":"release","repo":"`+dir+`","plan_file":"`+plan+`"}`, &run)
	require.Equal(t, http.StatusCreated, code)
	require.Equal(t, "release", run.Name)
	require.Equal(t, autopilot.StateRegistered, run.State)

	for _, step := range []struct {
		action string
		state  autopilot.RunState
	}{
		{"start", autopilot.StateActive}, {"pause", autopilot.StatePaused},
		{"resume", autopilot.StateActive}, {"stop", autopilot.StateStopped},
	} {
		code = apPostJSON(t, ts.URL+"/api/v1/autopilot/runs/"+run.RunID+"/"+step.action, `{}`, &run)
		require.Equal(t, http.StatusOK, code, step.action)
		require.Equal(t, step.state, run.State, step.action)
	}

	var runs []autopilot.RunStatus
	apGetJSON(t, ts.URL+"/api/v1/autopilot/runs", &runs)
	require.Len(t, runs, 1)
	require.Equal(t, autopilot.StateStopped, runs[0].State)

	code = apPostJSON(t, ts.URL+"/api/v1/autopilot/runs/"+run.RunID+"/unregister", `{}`, &run)
	require.Equal(t, http.StatusOK, code)
	apGetJSON(t, ts.URL+"/api/v1/autopilot/runs", &runs)
	require.Empty(t, runs)
}

func TestAutopilotUnconfigured(t *testing.T) {
	// No Controller wired: GET reports disabled, POST is 403.
	srv := &Server{store: newFakeStore(), hub: newHub(), done: make(chan struct{})}
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	var st autopilot.Status
	apGetJSON(t, ts.URL+"/api/v1/autopilot", &st)
	require.False(t, st.Enabled)

	resp, err := http.Post(ts.URL+"/api/v1/autopilot", "application/json", strings.NewReader(`{"enabled":true}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// TestCompleteAutopilotHandler exercises the brain's completion signal: only the
// run's own brain may complete it (403 otherwise), completion writes the in-place
// marker (comment preserved) and transitions the run to complete, and it is
// idempotent.
func TestCompleteAutopilotHandler(t *testing.T) {
	dir := t.TempDir()
	plan := filepath.Join(dir, "plan.yaml")
	require.NoError(t, os.WriteFile(plan, []byte("# owner comment — keep me\nversion: 1\ngoal: ship\n"), 0o644))

	srv := &Server{store: newFakeStore(), life: &fakeLife{}, hub: newHub(), done: make(chan struct{})}
	srv.SetAutopilotController(autopilot.NewController(autopilot.ControllerConfig{
		Plans:             []string{plan},
		IntegrationBranch: "autopilot/integration",
		Gate:              "auto",
		Resolver:          autopilotTestResolver{},
	}, &apFakeEnv{repo: dir}))

	st, err := srv.autopilot.Enable(context.Background(), "")
	require.NoError(t, err)
	require.Len(t, st.Runs, 1)
	runID := st.Runs[0].RunID

	// A non-brain caller (no actor header) is refused and nothing changes.
	resp, err := srv.CompleteAutopilot(ctxWithActor(""), oapi.CompleteAutopilotRequestObject{})
	require.NoError(t, err)
	_, forbidden := resp.(oapi.CompleteAutopilot403JSONResponse)
	require.True(t, forbidden, "a non-brain caller gets 403")
	require.Equal(t, autopilot.StateActive, srv.autopilot.Status().Runs[0].State)

	// A stale brain with the right role/tag cannot complete the run.
	brain := &store.Session{ID: "brain-caller", Role: autopilotBrainRole, Tags: []string{"autopilot", "run:" + runID}}
	require.NoError(t, srv.store.Insert(context.Background(), brain))
	resp, err = srv.CompleteAutopilot(ctxWithActor("brain-caller"), oapi.CompleteAutopilotRequestObject{})
	require.NoError(t, err)
	_, forbidden = resp.(oapi.CompleteAutopilot403JSONResponse)
	require.True(t, forbidden, "a stale brain cannot complete the run")

	// The current brain completes the run.
	activeBrainID := st.Runs[0].Brain.AgentID

	resp, err = srv.CompleteAutopilot(ctxWithActor(activeBrainID), oapi.CompleteAutopilotRequestObject{})
	require.NoError(t, err)
	ok200, isOK := resp.(oapi.CompleteAutopilot200JSONResponse)
	require.True(t, isOK, "the brain completes its run (200)")
	require.Len(t, ok200.Runs, 1)
	require.Equal(t, autopilot.StateComplete, ok200.Runs[0].State)

	// The plan file gained a durable, re-parseable marker with the comment intact.
	raw, err := os.ReadFile(plan)
	require.NoError(t, err)
	require.Contains(t, string(raw), "# owner comment — keep me")
	p, err := autopilot.LoadPlan(plan)
	require.NoError(t, err)
	require.True(t, p.IsComplete())

	// Idempotent: completing again is still a 200 no-op.
	resp, err = srv.CompleteAutopilot(ctxWithActor("brain-caller"), oapi.CompleteAutopilotRequestObject{})
	require.NoError(t, err)
	_, isOK = resp.(oapi.CompleteAutopilot200JSONResponse)
	require.True(t, isOK)
}

func TestUpdateTaskStatusRejectsStaleBrain(t *testing.T) {
	dir := t.TempDir()
	plan := filepath.Join(dir, "plan.yaml")
	require.NoError(t, os.WriteFile(plan, []byte("version: 1\ngoal: ship\ntasks:\n  - id: build\n    prompt: build it\n"), 0o644))

	srv := &Server{store: newFakeStore(), life: &fakeLife{}, hub: newHub(), done: make(chan struct{})}
	srv.SetAutopilotController(autopilot.NewController(autopilot.ControllerConfig{
		Plans: []string{plan}, IntegrationBranch: "autopilot/integration", Resolver: autopilotTestResolver{},
	}, &apFakeEnv{repo: dir}))
	st, err := srv.autopilot.Enable(context.Background(), "")
	require.NoError(t, err)
	require.Len(t, st.Runs, 1)
	require.NotNil(t, st.Runs[0].Brain)
	runID, activeBrainID := st.Runs[0].RunID, st.Runs[0].Brain.AgentID

	stale := &store.Session{ID: "stale-brain", Role: autopilotBrainRole, Tags: []string{"autopilot", "run:" + runID}}
	require.NoError(t, srv.store.Insert(context.Background(), stale))
	req := oapi.UpdateAutopilotTaskStatusRequestObject{Body: &oapi.AutopilotTaskStatusRequest{
		RunId: runID, TaskId: "build", Status: oapi.AutopilotTaskStatusRequestStatusActive,
	}}

	resp, err := srv.UpdateAutopilotTaskStatus(ctxWithActor("stale-brain"), req)
	require.NoError(t, err)
	_, forbidden := resp.(oapi.UpdateAutopilotTaskStatus403JSONResponse)
	require.True(t, forbidden, "a superseded brain must not rewrite the task ledger")

	resp, err = srv.UpdateAutopilotTaskStatus(ctxWithActor(activeBrainID), req)
	require.NoError(t, err)
	_, ok := resp.(oapi.UpdateAutopilotTaskStatus200JSONResponse)
	require.True(t, ok, "the current active brain may update its task")
}

// --- small JSON helpers ---

func apGetJSON(t *testing.T, url string, out any) {
	t.Helper()
	resp, err := http.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.NoError(t, json.NewDecoder(resp.Body).Decode(out))
}

func apPostJSON(t *testing.T, url, body string, out any) int {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.NoError(t, json.NewDecoder(resp.Body).Decode(out))
	return resp.StatusCode
}

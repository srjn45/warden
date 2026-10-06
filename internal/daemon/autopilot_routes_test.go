package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/agentstore"
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

func TestManagerVisibilityAndRunStopCleanup(t *testing.T) {
	dir := t.TempDir()
	plan := filepath.Join(dir, "guardian.yaml")
	require.NoError(t, os.WriteFile(plan, []byte("version: 1\ngoal: ship\n"), 0o644))
	st := newFakeStore()
	life := &fakeLife{}
	srv := &Server{store: st, life: life, hub: newHub(), done: make(chan struct{})}
	c := autopilot.NewController(autopilot.ControllerConfig{Plans: []string{plan}, BaseDir: dir,
		IntegrationBranch: "autopilot/integration", Resolver: autopilotTestResolver{}}, &apFakeEnv{repo: dir})
	srv.SetAutopilotController(c)
	status, err := c.ReconcileConfiguredPlans(context.Background(), "")
	require.NoError(t, err)
	runID := status.Runs[0].RunID
	managerID := autopilot.ManagerSlotID("guardian")

	listed, err := srv.ListSessions(context.Background(), oapi.ListSessionsRequestObject{})
	require.NoError(t, err)
	visible := listed.(oapi.ListSessions200JSONResponse)
	require.Len(t, visible.Sessions, 1)
	require.Equal(t, managerID, visible.Sessions[0].ID)

	listed, err = srv.ListSessions(context.Background(), oapi.ListSessionsRequestObject{Params: oapi.ListSessionsParams{All: true}})
	require.NoError(t, err)
	all := listed.(oapi.ListSessions200JSONResponse)
	require.Len(t, all.Sessions, 1)
	require.Equal(t, managerID, all.Sessions[0].ID)

	_, err = c.StopRun(context.Background(), runID)
	require.NoError(t, err)
	require.Equal(t, managerID, life.terminated)
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
	st, err := c.ReconcileConfiguredPlans(context.Background(), dir)
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

func TestAutopilotUnconfigured(t *testing.T) {
	// No Controller wired: GET reports no runs.
	srv := &Server{store: newFakeStore(), hub: newHub(), done: make(chan struct{})}
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	var st autopilot.Status
	apGetJSON(t, ts.URL+"/api/v1/autopilot", &st)
	require.Empty(t, st.Runs)
}

// TestCompleteAutopilotHandler exercises the brain's completion signal under
// managed completion (run-to-final-pr §E): only the run's own brain may call it,
// and the call only confirms done_when — the run stays active until the daemon
// opens the final PR and it goes green. Non-brain / stale-brain callers get 403.
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
	require.True(t, srv.autopilot.CompletionManaged(), "daemon runtime owns the completion phase")

	st, err := srv.autopilot.ReconcileConfiguredPlans(context.Background(), "")
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
	brain := &agentstore.Agent{ID: "brain-caller", Role: autopilotBrainRole, Tags: []string{"autopilot", "run:" + runID}}
	require.NoError(t, srv.store.Insert(context.Background(), brain))
	resp, err = srv.CompleteAutopilot(ctxWithActor("brain-caller"), oapi.CompleteAutopilotRequestObject{})
	require.NoError(t, err)
	_, forbidden = resp.(oapi.CompleteAutopilot403JSONResponse)
	require.True(t, forbidden, "a stale brain cannot complete the run")

	// The current brain verifies done_when; the run does not complete yet.
	activeBrainID := st.Runs[0].Brain.AgentID

	resp, err = srv.CompleteAutopilot(ctxWithActor(activeBrainID), oapi.CompleteAutopilotRequestObject{})
	require.NoError(t, err)
	ok200, isOK := resp.(oapi.CompleteAutopilot200JSONResponse)
	require.True(t, isOK, "the brain's verify signal is accepted (200)")
	require.Len(t, ok200.Runs, 1)
	require.Equal(t, autopilot.StateActive, ok200.Runs[0].State, "managed completion defers StateComplete until the final PR is green")

	// The plan file is untouched — the complete marker is written only when the
	// final PR goes green (or nothing-to-merge / human merge).
	raw, err := os.ReadFile(plan)
	require.NoError(t, err)
	require.Contains(t, string(raw), "# owner comment — keep me")
	require.NotContains(t, string(raw), "status: complete")
	p, err := autopilot.LoadPlan(plan)
	require.NoError(t, err)
	require.False(t, p.IsComplete())

	// Idempotent: verifying again is still a 200 no-op.
	resp, err = srv.CompleteAutopilot(ctxWithActor(activeBrainID), oapi.CompleteAutopilotRequestObject{})
	require.NoError(t, err)
	_, isOK = resp.(oapi.CompleteAutopilot200JSONResponse)
	require.True(t, isOK)
	require.Equal(t, autopilot.StateActive, srv.autopilot.Status().Runs[0].State)
}

func TestUpdateTaskStatusRejectsStaleBrain(t *testing.T) {
	dir := t.TempDir()
	plan := filepath.Join(dir, "plan.yaml")
	require.NoError(t, os.WriteFile(plan, []byte("version: 1\ngoal: ship\ntasks:\n  - id: build\n    prompt: build it\n"), 0o644))

	srv := &Server{store: newFakeStore(), life: &fakeLife{}, hub: newHub(), done: make(chan struct{})}
	srv.SetAutopilotController(autopilot.NewController(autopilot.ControllerConfig{
		Plans: []string{plan}, IntegrationBranch: "autopilot/integration", Resolver: autopilotTestResolver{},
	}, &apFakeEnv{repo: dir}))
	st, err := srv.autopilot.ReconcileConfiguredPlans(context.Background(), "")
	require.NoError(t, err)
	require.Len(t, st.Runs, 1)
	require.NotNil(t, st.Runs[0].Brain)
	runID, activeBrainID := st.Runs[0].RunID, st.Runs[0].Brain.AgentID

	stale := &agentstore.Agent{ID: "stale-brain", Role: autopilotBrainRole, Tags: []string{"autopilot", "run:" + runID}}
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

func TestAutopilotSessionBackRefsRoundTripREST(t *testing.T) {
	st := newFakeStore()
	now := time.Now().UTC().Truncate(time.Second)
	sess := &agentstore.Agent{
		ID: "default-autopilot", Type: store.TypeDevelopment, Status: store.StatusWorking,
		AutopilotRunID: "ap-abc123def456", AutopilotSlot: store.AutopilotSlotManager,
		CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, st.Insert(context.Background(), sess))
	srv := &Server{store: st, life: &fakeLife{}, hub: newHub(), done: make(chan struct{})}

	resp, err := srv.GetSession(context.Background(), oapi.GetSessionRequestObject{Id: sess.ID})
	require.NoError(t, err)
	got := resp.(oapi.GetSession200JSONResponse)
	require.Equal(t, "ap-abc123def456", got.AutopilotRunID)
	require.Equal(t, store.AutopilotSlotManager, got.AutopilotSlot)
	require.Empty(t, got.AutopilotTaskID)
}

func TestAutopilotRunStatusSlotFieldsREST(t *testing.T) {
	dir := t.TempDir()
	plan := filepath.Join(dir, "plan.yaml")
	require.NoError(t, os.WriteFile(plan, []byte("version: 1\ngoal: ship\n"), 0o644))
	c := autopilot.NewController(autopilot.ControllerConfig{
		Plans: []string{plan}, BaseDir: dir, IntegrationBranch: autopilot.DefaultIntegrationBranch,
		Resolver: autopilotTestResolver{},
	}, &apFakeEnv{repo: dir})
	srv := &Server{store: newFakeStore(), life: &fakeLife{}, hub: newHub(), done: make(chan struct{})}
	srv.SetAutopilotController(c)
	st, err := c.ReconcileConfiguredPlans(context.Background(), dir)
	require.NoError(t, err)
	require.Len(t, st.Runs, 1)
	run := st.Runs[0]
	require.Equal(t, "plan-autopilot", run.ManagerSlotID)
	require.Equal(t, "plan-guardian", run.GuardianSlotID)
	require.Equal(t, "plan", run.SlotScope)
	require.NotEmpty(t, run.IntegrationBranch)

	ts := httptest.NewServer(srv.router())
	defer ts.Close()
	apGetJSON(t, ts.URL+"/api/v1/autopilot", &st)
	require.Len(t, st.Runs, 1)
	require.Equal(t, run.ManagerSlotID, st.Runs[0].ManagerSlotID)
}

// --- small JSON helpers ---

func apGetJSON(t *testing.T, url string, out any) {
	t.Helper()
	resp, err := http.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.NoError(t, json.NewDecoder(resp.Body).Decode(out))
}

package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/autopilotstore"
	"github.com/srjn45/warden/internal/ctxstore"
	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/store"
)

func newPlanRestartServer(t *testing.T) (ts *httptest.Server, srv *Server, root, planID string) {
	t.Helper()
	root = t.TempDir()
	gitInit(t, root)
	require.NoError(t, exec.Command("git", "-C", root, "commit", "--allow-empty", "-m", "init").Run())

	plans, err := planstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = plans.Close() })
	projects, err := projectstore.NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = projects.Close() })
	_, err = projects.OpenProject(root, "test", root)
	require.NoError(t, err)

	cs, err := ctxstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })

	data := t.TempDir()
	runs, err := autopilot.NewRunStore(data)
	require.NoError(t, err)
	live, err := autopilotstore.New(data)
	require.NoError(t, err)
	t.Cleanup(func() { _ = live.Close() })

	controller := autopilot.NewController(autopilot.ControllerConfig{
		BaseDir: root, RunStore: runs, LiveStore: live, PlanSource: plans, Gate: "local",
		Resolver: autopilotTestResolver{},
	}, &apFakeEnv{repo: root})
	t.Cleanup(func() { require.NoError(t, controller.Close()) })

	srv = &Server{
		store: newFakeStore(), life: &fakeLife{}, hub: newHub(), done: make(chan struct{}),
		plans: plans, projects: projects, cstore: cs,
	}
	srv.SetAutopilotController(controller)

	seedPlanYAML(t, root, "plans/pending/restart-plan.yaml", "restart-plan")
	planID = planstore.PlanID(root, "restart-plan")
	require.NoError(t, plans.Create(t.Context(), &planstore.Plan{
		ID: planID, ProjectID: root, Name: "restart-plan", Goal: "restart me",
		Tasks:    []planstore.PlanTask{{ID: "t1", Prompt: "do"}, {ID: "t2", Prompt: "more"}},
		FilePath: "plans/pending/restart-plan.yaml", Status: planstore.PlanStatusPending,
		TaskProgress: map[string]string{"t1": "pending", "t2": "pending"},
	}))

	ts = httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)
	return ts, srv, root, planID
}

func runPlanAutopilot(t *testing.T, ts *httptest.Server, planID string) string {
	t.Helper()
	resp := postJSON(t, ts.URL+"/api/v1/plans/"+planID+"/run", map[string]any{"execution_mode": "autopilot"})
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var got planstore.Plan
	require.NoError(t, json.Unmarshal(body, &got))
	require.NotEmpty(t, got.AutopilotRunID)
	return got.AutopilotRunID
}

func postRestart(t *testing.T, url, planID string, body any) (int, []byte) {
	t.Helper()
	resp := postJSON(t, url+"/api/v1/plans/"+planID+"/restart", body)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func TestRestartPlanFromStopped(t *testing.T) {
	ts, srv, root, planID := newPlanRestartServer(t)
	runID := runPlanAutopilot(t, ts, planID)

	stop, err := http.Post(ts.URL+"/api/v1/plans/"+planID+"/stop", "application/json", http.NoBody)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, stop.Body)
	_ = stop.Body.Close()
	require.Equal(t, http.StatusOK, stop.StatusCode)

	// Old worker still on disk (stop only tears the manager brain down).
	require.NoError(t, srv.store.Insert(context.Background(), &agentstore.Agent{
		ID: "worker-old", AutopilotRunID: runID, Role: "worker",
		Branch: "w/kept", Worktree: t.TempDir(), TmuxSession: "tmux-worker-old",
		Repo: root, Status: store.StatusWorking,
		Tags: []string{"autopilot", "run:" + runID},
	}))

	code, body := postRestart(t, ts.URL, planID, nil)
	require.Equal(t, http.StatusOK, code, string(body))
	var got oapi.Plan
	require.NoError(t, json.Unmarshal(body, &got))
	require.Equal(t, oapi.PlanStatusInProgress, got.Status)
	require.Equal(t, runID, got.AutopilotRunId)

	// Old worker archived; a fresh manager occupies the slot.
	_, err = srv.store.Get(context.Background(), "worker-old")
	require.ErrorIs(t, err, agentstore.ErrNotFound)
	closed, err := srv.store.ListClosed(context.Background())
	require.NoError(t, err)
	var archivedWorker bool
	for _, s := range closed {
		if s.ID == "worker-old" {
			archivedWorker = true
		}
	}
	require.True(t, archivedWorker)

	st := srv.autopilot.Status()
	require.Len(t, st.Runs, 1)
	require.Equal(t, autopilot.StateActive, st.Runs[0].State)
	require.NotNil(t, st.Runs[0].Brain)
}

func TestRestartPlanRefusesActiveWithoutForce(t *testing.T) {
	ts, _, _, planID := newPlanRestartServer(t)
	_ = runPlanAutopilot(t, ts, planID)

	code, body := postRestart(t, ts.URL, planID, nil)
	require.Equal(t, http.StatusConflict, code, string(body))
	require.Contains(t, string(body), "pass --force")
}

func TestRestartPlanForceFromActive(t *testing.T) {
	ts, srv, _, planID := newPlanRestartServer(t)
	_ = runPlanAutopilot(t, ts, planID)
	before := srv.autopilot.Status().Runs[0].Brain.AgentID

	code, body := postRestart(t, ts.URL, planID, map[string]any{"force": true})
	require.Equal(t, http.StatusOK, code, string(body))
	after := srv.autopilot.Status().Runs[0].Brain.AgentID
	require.NotEmpty(t, after)
	// Slot id is stable; HotSwap would keep the same id — restart re-uses the
	// slot after teardown, so the id matches but the session was replaced.
	require.Equal(t, before, after, "manager slot id is stable across restart")
	require.Equal(t, autopilot.StateActive, srv.autopilot.Status().Runs[0].State)
}

func TestRestartPlanNotFoundAndWrongStatus(t *testing.T) {
	ts, _, _, planID := newPlanRestartServer(t)

	code, body := postRestart(t, ts.URL, "plan-deadbeef", nil)
	require.Equal(t, http.StatusNotFound, code, string(body))

	code, body = postRestart(t, ts.URL, planID, nil)
	require.Equal(t, http.StatusConflict, code, string(body))
	require.Contains(t, string(body), "not in_progress")
}

func TestRestartPlanUnsupportedMode(t *testing.T) {
	ts, srv, root, _ := newPlanRestartServer(t)
	id := planstore.PlanID(root, "manual-plan")
	require.NoError(t, srv.plans.Create(t.Context(), &planstore.Plan{
		ID: id, ProjectID: root, Name: "manual-plan", Goal: "manual",
		Tasks:    []planstore.PlanTask{{ID: "t1", Prompt: "do"}},
		FilePath: "plans/in_progress/manual-plan.yaml", Status: planstore.PlanStatusInProgress,
		ExecutionMode:   planstore.PlanModeManual,
		ActiveExecution: &planstore.PlanExecution{ExecutionMode: planstore.PlanModeManual, ExecutorID: "manual-1"},
	}))

	code, body := postRestart(t, ts.URL, id, nil)
	require.Equal(t, http.StatusConflict, code, string(body))
	require.Contains(t, string(body), "not supported for manual")
}

func TestTeardownRunAgentsBranchPolicy(t *testing.T) {
	root := t.TempDir()
	gitInit(t, root)
	require.NoError(t, exec.Command("git", "-C", root, "commit", "--allow-empty", "-m", "init").Run())
	integ := "autopilot/integ"
	require.NoError(t, exec.Command("git", "-C", root, "branch", integ).Run())

	// Branch with unmerged commits → keep.
	kept := "w/kept"
	require.NoError(t, exec.Command("git", "-C", root, "checkout", "-b", kept).Run())
	require.NoError(t, exec.Command("git", "-C", root, "commit", "--allow-empty", "-m", "work").Run())
	require.NoError(t, exec.Command("git", "-C", root, "checkout", integ).Run())

	// Branch pointing at integ tip → delete.
	empty := "w/empty"
	require.NoError(t, exec.Command("git", "-C", root, "branch", empty, integ).Run())

	st := newFakeStore()
	life := &fakeLife{}
	srv := &Server{store: st, life: life, hub: newHub(), done: make(chan struct{})}
	runID := "ap-branchpol"
	for _, a := range []*agentstore.Agent{
		{
			ID: "mgr", AutopilotRunID: runID, Role: "autopilot",
			TmuxSession: "tmux-mgr", Status: store.StatusWorking,
			Tags: []string{"autopilot", "run:" + runID},
		},
		{
			ID: "w-kept", AutopilotRunID: runID, Role: "worker", Branch: kept,
			Worktree: filepath.Join(root, "wt-kept"), TmuxSession: "tmux-kept",
			Repo: root, Status: store.StatusWorking, BranchCreated: true,
			Tags: []string{"autopilot", "run:" + runID},
		},
		{
			ID: "w-empty", AutopilotRunID: runID, Role: "worker", Branch: empty,
			Worktree: filepath.Join(root, "wt-empty"), TmuxSession: "tmux-empty",
			Repo: root, Status: store.StatusWorking, BranchCreated: true,
			Tags: []string{"autopilot", "run:" + runID},
		},
	} {
		if a.Worktree != "" {
			require.NoError(t, os.MkdirAll(a.Worktree, 0o755))
		}
		require.NoError(t, st.Insert(context.Background(), a))
	}

	td, err := (autopilotRuntime{s: srv}).TeardownRunAgents(context.Background(), runID, root, integ)
	require.NoError(t, err)
	require.Equal(t, 3, td.AgentsRemoved)
	require.Contains(t, td.BranchesKept, kept)
	require.Contains(t, td.BranchesDeleted, empty)

	out, err := exec.Command("git", "-C", root, "branch", "--list", kept).CombinedOutput()
	require.NoError(t, err)
	require.Contains(t, string(out), kept)
	out, err = exec.Command("git", "-C", root, "branch", "--list", empty).CombinedOutput()
	require.NoError(t, err)
	require.Empty(t, strings.TrimSpace(string(out)), "empty branch must be deleted")
}

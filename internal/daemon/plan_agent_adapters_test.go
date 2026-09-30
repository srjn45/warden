package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/store"
)

func TestOrchestratorDisplayName(t *testing.T) {
	require.Equal(t, "O:my-plan", orchestratorDisplayName("my-plan"))
	require.Equal(t, "O:my-plan", orchestratorDisplayName("O:my-plan"))
	require.Equal(t, "O:unnamed", orchestratorDisplayName(""))
	require.Equal(t, "O:unnamed", orchestratorDisplayName("  "))
}

func TestManualDisplayName(t *testing.T) {
	require.Equal(t, "M:my-plan", manualDisplayName("my-plan"))
	require.Equal(t, "M:my-plan", manualDisplayName("M:my-plan"))
	require.Equal(t, "M:unnamed", manualDisplayName(""))
}

// TestSpawnPlanlessOrchestratorStillWorks verifies an independent orchestrator
// Agent (no PlanID) still spawns successfully — plan adapters must not break
// planless creation.
func TestSpawnPlanlessOrchestratorStillWorks(t *testing.T) {
	ts, _, projs, _ := planLinkServer(t)
	projDir := t.TempDir()
	proj, err := projs.OpenProject(projDir, "orch-proj", projDir)
	require.NoError(t, err)

	body := map[string]any{
		"role": "orchestrator", "prompt": "coordinate the fleet",
		"project_id": proj.ID, "cwd": projDir,
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
	require.Equal(t, "orchestrator", sess.Role)
}

// TestSpawnPlanlessGeneralStillWorks verifies an independent general Agent
// without PlanID still works.
func TestSpawnPlanlessGeneralStillWorks(t *testing.T) {
	ts, _, projs, _ := planLinkServer(t)
	projDir := t.TempDir()
	proj, err := projs.OpenProject(projDir, "gen-proj", projDir)
	require.NoError(t, err)

	body := map[string]any{
		"role": "general", "prompt": "explore the repo",
		"project_id": proj.ID, "cwd": projDir,
	}
	b, _ := json.Marshal(body)
	resp, err := http.Post(ts.URL+"/api/v1/spawn", "application/json", bytes.NewReader(b))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var sess store.Session
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&sess))
	require.Empty(t, sess.PlanID)
	require.Equal(t, "general", sess.Role)
}

// TestStampPlanSpawnBackRefs inherits PlanID from a plan-bound parent.
func TestStampPlanSpawnBackRefs(t *testing.T) {
	fs := newFakeStore()
	fs.data["orch-1"] = &agentstore.Agent{
		ID: "orch-1", PlanID: "plan-abc", Role: "orchestrator",
	}
	srv := &Server{store: fs}

	sr := &SpawnRequest{Role: "worker", Prompt: "do task"}
	srv.stampPlanSpawnBackRefs(ctxWithActor("orch-1"), sr)
	require.Equal(t, "plan-abc", sr.PlanID)

	// Explicit PlanID wins.
	sr2 := &SpawnRequest{Role: "worker", PlanID: "plan-other"}
	srv.stampPlanSpawnBackRefs(ctxWithActor("orch-1"), sr2)
	require.Equal(t, "plan-other", sr2.PlanID)

	// Planless parent leaves PlanID empty.
	fs.data["plain"] = &agentstore.Agent{ID: "plain", Role: "general"}
	sr3 := &SpawnRequest{Role: "worker"}
	srv.stampPlanSpawnBackRefs(ctxWithActor("plain"), sr3)
	require.Empty(t, sr3.PlanID)
}

// TestPlanBoundGitEventsEmitPlanExecutionEvents verifies commit/push/check/PR
// from a plan-bound agent append typed PlanExecutionEvents.
func TestPlanBoundGitEventsEmitPlanExecutionEvents(t *testing.T) {
	root := t.TempDir()
	plans, err := planstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = plans.Close() })
	projects, err := projectstore.NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = projects.Close() })
	_, err = projects.OpenProject(root, "test", root)
	require.NoError(t, err)

	plan := &planstore.Plan{
		ID: "plan-git01", ProjectID: root, Name: "git-plan",
		FilePath: "plans/in_progress/git-plan.yaml", Status: planstore.PlanStatusInProgress,
		ExecutionMode: planstore.PlanModeManual,
		ActiveExecution: &planstore.PlanExecution{
			ID: "pe-git01", PlanID: "plan-git01",
			ExecutionMode: planstore.PlanModeManual, ExecutorID: "agent-test",
		},
	}
	require.NoError(t, plans.Create(context.Background(), plan))

	life := &fakeLife{
		gitCommitResult: lifecycle.CommitResult{Committed: true, SHA: "abc1234", Branch: "feat"},
		gitPushResult:   lifecycle.PushResult{Branch: "feat", Remote: "origin", Pushed: true},
		checkResult:     lifecycle.CheckResult{Passed: true},
		prResult:        lifecycle.PRResult{URL: "https://example.com/pr/1", Branch: "feat", Created: true},
	}
	fs := newFakeStore()
	fs.data["agent-test"] = &agentstore.Agent{
		ID: "agent-test", PlanID: plan.ID, Workdir: root, Status: store.StatusWorking,
	}
	srv := &Server{store: fs, life: life, plans: plans, projects: projects}
	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)

	post := func(path string, body map[string]any) {
		t.Helper()
		b, _ := json.Marshal(body)
		resp, err := http.Post(ts.URL+path, "application/json", bytes.NewReader(b))
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
	}

	post("/api/v1/git/commit", map[string]any{"session": "agent-test", "message": "x"})
	post("/api/v1/git/push", map[string]any{"session": "agent-test"})
	post("/api/v1/check", map[string]any{"session": "agent-test", "name": "test"})
	post("/api/v1/sessions/agent-test/create-pr", map[string]any{"base": "main"})

	events, err := plans.ListEvents(context.Background(), plan.ID, "pe-git01")
	require.NoError(t, err)
	kinds := map[planstore.EventKind]bool{}
	for _, ev := range events {
		kinds[ev.Kind] = true
	}
	require.True(t, kinds[planstore.EventKindCommitCreated], "commit_created")
	require.True(t, kinds[planstore.EventKindBranchPushed], "branch_pushed")
	require.True(t, kinds[planstore.EventKindCheckCompleted], "check_completed")
	require.True(t, kinds[planstore.EventKindPROpened], "pr_opened")
}

// TestPlanlessGitEventsDoNotTouchPlanStore verifies planless agents still
// commit/check without requiring a Plan or writing PlanExecutionEvents.
func TestPlanlessGitEventsDoNotTouchPlanStore(t *testing.T) {
	root := t.TempDir()
	plans, err := planstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = plans.Close() })

	life := &fakeLife{
		gitCommitResult: lifecycle.CommitResult{Committed: true, SHA: "deadbee", Branch: "x"},
	}
	fs := newFakeStore()
	fs.data["agent-plain"] = &agentstore.Agent{
		ID: "agent-plain", Workdir: root, Status: store.StatusWorking,
	}
	srv := &Server{store: fs, life: life, plans: plans}
	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)

	b, _ := json.Marshal(map[string]any{"session": "agent-plain", "message": "x"})
	resp, err := http.Post(ts.URL+"/api/v1/git/commit", "application/json", bytes.NewReader(b))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

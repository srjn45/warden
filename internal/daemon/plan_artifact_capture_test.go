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
	"github.com/srjn45/warden/internal/handoff"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/store"
)

func setupArtifactPlan(t *testing.T) (*Server, *planstore.Store, *agentstore.Agent, string) {
	t.Helper()
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
		ID: "plan-art01", ProjectID: root, Name: "artifact",
		FilePath: "plans/in_progress/artifact.yaml", Status: planstore.PlanStatusInProgress,
		ExecutionMode: planstore.PlanModeManual,
		ActiveExecution: &planstore.PlanExecution{
			ID: "pe-art01", PlanID: "plan-art01",
			ExecutionMode: planstore.PlanModeManual, ExecutorID: "agent-art",
		},
	}
	require.NoError(t, plans.Create(context.Background(), plan))

	fs := newFakeStore()
	agent := &agentstore.Agent{
		ID: "agent-art", PlanID: plan.ID, Workdir: root, Worktree: root + "/wt",
		Branch: "feat/art", BranchCreated: true, Status: store.StatusWorking,
		TmuxSession: "tmux-art",
	}
	fs.data[agent.ID] = agent
	life := &fakeLife{}
	srv := &Server{store: fs, life: life, plans: plans, projects: projects}
	return srv, plans, agent, plan.ActiveExecution.ID
}

func TestArtifactCapture_TerminateEmitsAgentFinished(t *testing.T) {
	srv, plans, agent, execID := setupArtifactPlan(t)
	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/api/v1/sessions/"+agent.ID+"/terminate", "application/json", nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	events, err := plans.ListEvents(context.Background(), agent.PlanID, execID)
	require.NoError(t, err)
	require.True(t, hasEventKind(events, planstore.EventKindAgentFinished))
}

func TestArtifactCapture_RemoveWorktreeEmitsWorktreeRemoved(t *testing.T) {
	srv, plans, agent, execID := setupArtifactPlan(t)
	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/api/v1/sessions/"+agent.ID+"/remove-worktree",
		"application/json", bytes.NewReader([]byte(`{"force":true}`)))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	events, err := plans.ListEvents(context.Background(), agent.PlanID, execID)
	require.NoError(t, err)
	require.True(t, hasEventKind(events, planstore.EventKindWorktreeRemoved))
}

func TestArtifactCapture_PushTracksPlanBranch(t *testing.T) {
	srv, plans, agent, _ := setupArtifactPlan(t)
	srv.life = &fakeLife{
		gitPushResult: lifecycle.PushResult{Branch: "feat/art", Remote: "origin", Pushed: true},
	}
	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)

	b, _ := json.Marshal(map[string]any{"session": agent.ID})
	resp, err := http.Post(ts.URL+"/api/v1/git/push", "application/json", bytes.NewReader(b))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	p, err := plans.Get(context.Background(), agent.PlanID)
	require.NoError(t, err)
	require.Contains(t, p.Branches, "feat/art")
	require.Contains(t, p.ActiveExecution.PlanBranches, "feat/art")
}

func TestArtifactCapture_HandoffGoesToNoteNotEvent(t *testing.T) {
	srv, plans, agent, execID := setupArtifactPlan(t)
	srv.life = &fakeLife{
		hotSwapResult: &lifecycle.SwapResult{
			Agent:       agent,
			HandoffPath: "/tmp/handoff-agent-art.md",
			Handoff: handoff.Handoff{
				SessionID: agent.ID, Goal: "finish the feature", NextStep: "open PR",
				Backend: "claude", SuccessorBackend: "codex", Reason: "manual",
			},
			FromBackend: "claude", ToBackend: "codex",
		},
	}
	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)

	body, _ := json.Marshal(map[string]any{"backend": "codex", "reason": "manual"})
	resp, err := http.Post(ts.URL+"/api/v1/sessions/"+agent.ID+"/switch",
		"application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	notes, err := plans.ListNotes(context.Background(), agent.PlanID, execID)
	require.NoError(t, err)
	require.NotEmpty(t, notes)
	require.Contains(t, notes[0].Content, "agent handoff")
	require.Equal(t, agent.ID, notes[0].AgentID)

	events, err := plans.ListEvents(context.Background(), agent.PlanID, execID)
	require.NoError(t, err)
	for _, ev := range events {
		require.NotEqual(t, "handoff", string(ev.Kind))
	}
}

func TestArtifactCapture_LandEmitsPRMergedAndBranchLanded(t *testing.T) {
	_, plans, agent, execID := setupArtifactPlan(t)
	srv := &Server{plans: plans}
	srv.recordPlanBoundLandEvents(agent, "feat/art", 7, "sha-merge", true)

	events, err := plans.ListEvents(context.Background(), agent.PlanID, execID)
	require.NoError(t, err)
	require.True(t, hasEventKind(events, planstore.EventKindPRMerged))
	require.True(t, hasEventKind(events, planstore.EventKindBranchLanded))
	require.True(t, hasEventKind(events, planstore.EventKindWorktreeRemoved))
}

func TestArtifactCapture_PlanlessAgentSkipsEvents(t *testing.T) {
	plans, err := planstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = plans.Close() })
	fs := newFakeStore()
	fs.data["plain"] = &agentstore.Agent{
		ID: "plain", Workdir: t.TempDir(), Status: store.StatusWorking, TmuxSession: "t",
	}
	srv := &Server{store: fs, life: &fakeLife{}, plans: plans}
	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/api/v1/sessions/plain/terminate", "application/json", nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func hasEventKind(events []*planstore.PlanExecutionEvent, kind planstore.EventKind) bool {
	for _, ev := range events {
		if ev.Kind == kind {
			return true
		}
	}
	return false
}

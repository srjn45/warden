package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/srjn45/warden/internal/agentbackend/backends"
	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/backendusage"
	"github.com/srjn45/warden/internal/handoff"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

type slotSpawnStore struct {
	*fakeStore
}

func TestSpawnBrainCreatesManagerSlot(t *testing.T) {
	repo := t.TempDir()
	fs := &slotSpawnStore{fakeStore: newFakeStore()}
	fl := &fakeLife{}
	srv := &Server{store: fs, life: fl}
	rt := autopilotRuntime{s: srv}

	spec := autopilot.BrainSpec{
		SlotScope: "voyage",
		Repo:      repo,
		Prompt:    "brief",
		Backend:   "claude",
		Tags:      []string{"autopilot", "run:ap-abc"},
	}
	handle, err := rt.SpawnBrain(context.Background(), spec)
	require.NoError(t, err)
	require.Equal(t, "voyage-autopilot", handle.AgentID)
	require.NotNil(t, fl.spawned)
	require.Equal(t, "voyage-autopilot", fl.spawned.Ticket)
}

func TestSpawnBrainAdoptsLiveSlot(t *testing.T) {
	repo := t.TempDir()
	fs := &slotSpawnStore{fakeStore: newFakeStore()}
	now := time.Now().UTC()
	fs.data["voyage-autopilot"] = &agentstore.Agent{
		ID: "voyage-autopilot", Name: "n-voyage-autopilot", TmuxSession: "voyage-autopilot",
		Status: store.StatusWorking, AiCli: "claude", UpdatedAt: now, CreatedAt: now,
	}
	fl := &fakeLife{}
	srv := &Server{store: fs, life: fl}
	rt := autopilotRuntime{s: srv}

	handle, err := rt.SpawnBrain(context.Background(), autopilot.BrainSpec{
		SlotScope: "voyage",
		Repo:      repo,
		Prompt:    "brief",
		Tags:      []string{"autopilot", "run:ap-abc"},
	})
	require.NoError(t, err)
	require.Equal(t, "voyage-autopilot", handle.AgentID)
	require.Nil(t, fl.spawned, "live slot is adopted without spawning")
}

func TestRotateBrainInvokesHotSwapNotRecovery(t *testing.T) {
	st := newFakeStore()
	life := &fakeLife{}
	workdir := t.TempDir()
	sess := &agentstore.Agent{
		ID: "agent-mgr-1", Name: "n-agent-mgr-1", TmuxSession: "agent-mgr-1", AiCli: "claude", Model: "opus",
		Role: "autopilot", Repo: workdir, Workdir: workdir, Status: store.StatusWorking,
		Tags: []string{"autopilot", "run:ap-rotate"},
	}
	require.NoError(t, st.Insert(context.Background(), sess))

	bs, err := backendstore.NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bs.Close()) })
	spy := &recoveryLife{failures: map[string]error{}}
	coord := NewBackendRecoveryCoordinator(st, bs, backendusage.NewService(bs), spy)

	srv := &Server{store: st, life: life, hub: newHub()}
	srv.SetBackendRecovery(coord)
	rt := autopilotRuntime{s: srv}

	handle, err := rt.RotateBrain(context.Background(), autopilot.RotateBrainSpec{
		AgentID: sess.ID, Backend: "codex", Prompt: "continue the run", Reason: autopilot.RotateReasonHeal,
	})
	require.NoError(t, err)
	require.Equal(t, sess.ID, handle.AgentID, "manager slot id is unchanged")
	require.Equal(t, "codex", handle.Backend)
	require.Equal(t, 1, life.hotSwapCalls, "guardian rotation calls Lifecycle.HotSwap")
	require.Equal(t, "codex", life.hotSwapReq.Backend)
	require.Equal(t, lifecycle.SwapReasonManual, life.hotSwapReq.Reason)
	require.Empty(t, spy.swaps, "BackendRecoveryCoordinator must not perform the swap")

	got, err := st.Get(context.Background(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, "codex", got.AiCli)
	require.Nil(t, got.BackendRecovery, "guardian rotation must not start a recovery generation")
}

func TestRotateBrainWithLiveWorkersPreservesTreeAndLand(t *testing.T) {
	dataDir := t.TempDir()
	workdir := t.TempDir()
	st, err := agentstore.New(dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	runID := "ap-liveworkers"
	mgrID := "agent-mgr-1"
	now := time.Now().UTC()
	mgr := &agentstore.Agent{
		ID: mgrID, Name: mgrID, TmuxSession: mgrID, AiCli: "claude", Model: "opus",
		Role: "autopilot", Repo: workdir, Workdir: workdir, Branch: "autopilot/manager",
		Worktree: workdir, Status: store.StatusWorking, CreatedAt: now, UpdatedAt: now,
		Tags: []string{"autopilot", "run:" + runID},
	}
	require.NoError(t, st.Insert(context.Background(), mgr))

	workers := []*agentstore.Agent{
		{
			ID: "agent-w1", Name: "agent-w1", TmuxSession: "agent-w1", ParentID: mgrID, AiCli: "claude",
			Role: "worker", Repo: workdir, Workdir: workdir, Branch: "autopilot/task-a",
			Worktree: t.TempDir(), Status: store.StatusWorking, CreatedAt: now, UpdatedAt: now,
			Tags: []string{"autopilot", "run:" + runID},
		},
		{
			ID: "agent-w2", Name: "agent-w2", TmuxSession: "agent-w2", ParentID: mgrID, AiCli: "claude",
			Role: "worker", Repo: workdir, Workdir: workdir, Branch: "autopilot/task-b",
			Worktree: t.TempDir(), Status: store.StatusWorking, CreatedAt: now, UpdatedAt: now,
			Tags: []string{"autopilot", "run:" + runID},
		},
	}
	for _, w := range workers {
		require.NoError(t, st.Insert(context.Background(), w))
	}

	fr := &lifecycle.FakeRunner{Responses: map[string]lifecycle.FakeResp{}}
	lc := lifecycle.New(fr, &lifecycle.FakeConfig{})
	lc.ProjectsDir = t.TempDir()
	lc.PromptsDir = filepath.Join(dataDir, "prompts")
	require.NoError(t, os.MkdirAll(lc.PromptsDir, 0o755))

	bs, err := backendstore.NewStore(filepath.Join(dataDir, "backends"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bs.Close()) })
	spy := &recoveryLife{failures: map[string]error{}}
	coord := NewBackendRecoveryCoordinator(st, bs, backendusage.NewService(bs), spy)

	srv := &Server{store: st, life: NewLifecycleAdapter(lc, st), hub: newHub()}
	srv.SetBackendRecovery(coord)
	rt := autopilotRuntime{s: srv}

	handle, err := rt.RotateBrain(context.Background(), autopilot.RotateBrainSpec{
		AgentID: mgrID, Backend: "codex", Prompt: "re-read the plan and continue",
		Reason: autopilot.RotateReasonHeal,
	})
	require.NoError(t, err)
	require.Equal(t, mgrID, handle.AgentID, "manager id is unchanged after HotSwap")
	require.Equal(t, "codex", handle.Backend)
	require.Empty(t, spy.swaps, "guardian rotation must not invoke BackendRecoveryCoordinator")

	gotMgr, err := st.Get(context.Background(), mgrID)
	require.NoError(t, err)
	require.Equal(t, mgrID, gotMgr.ID)
	require.Equal(t, "codex", gotMgr.AiCli)
	require.Nil(t, gotMgr.BackendRecovery)

	wantHandoff := handoff.Path(workdir, mgrID)
	require.FileExists(t, wantHandoff, "handoff is written at the stable slot path")
	body, err := os.ReadFile(wantHandoff)
	require.NoError(t, err)
	require.Contains(t, string(body), mgrID, "handoff names the unchanged slot id")

	for _, w := range workers {
		got, gerr := st.Get(context.Background(), w.ID)
		require.NoError(t, gerr)
		require.Equal(t, mgrID, got.ParentID, "worker %s parent_id must not be rewritten", w.ID)
		require.Equal(t, w.Tags, got.Tags)
		tgt := srv.resolveLandTarget(context.Background(), w.ID)
		require.True(t, tgt.owned, "worker %s stays landable by tag after rotation", w.ID)
		require.Equal(t, runID, tgt.runID)
		require.Equal(t, w.Branch, tgt.branch)
	}

	merges := 0
	host := stubLandHost{
		pr:       autopilot.PRInfo{Number: 7, BaseRef: "autopilot/integration", HeadSHA: "sha-head", Mergeable: true},
		prFound:  true,
		ci:       autopilot.GateGreen,
		mergeSHA: "sha-merge",
		merges:   &merges,
	}
	res, err := autopilot.Land(context.Background(), autopilot.LandRequest{
		RunActive:         true,
		Owned:             true,
		Branch:            workers[0].Branch,
		Worktree:          workers[0].Worktree,
		IntegrationBranch: "autopilot/integration",
		DefaultBranch:     "main",
		Gate:              "ci",
		Strategy:          "squash",
	}, host, nil)
	require.NoError(t, err)
	require.Equal(t, "sha-merge", res.SHA)
	require.Equal(t, 1, merges, "a live worker remains landable after manager rotation")
}

func TestBrainSessionLossCauses(t *testing.T) {
	fs := &slotSpawnStore{fakeStore: newFakeStore()}
	now := time.Now().UTC()
	put := func(id string, st store.Status) {
		fs.data[id] = &agentstore.Agent{ID: id, TmuxSession: id, Status: st, UpdatedAt: now, CreatedAt: now}
	}
	put("live", store.StatusWorking)
	put("done", store.StatusDone)
	put("errored", store.StatusErrored)
	put("orphan", store.StatusOrphaned)
	rt := autopilotRuntime{s: &Server{store: fs}}
	ctx := context.Background()

	require.Equal(t, autopilot.SessionPresent, rt.BrainSession(ctx, "live"))
	require.Equal(t, autopilot.SessionMissing, rt.BrainSession(ctx, "gone"))
	require.Equal(t, "missing", rt.BrainLossCause(ctx, "gone"))
	for _, id := range []string{"done", "errored", "orphan"} {
		require.Equal(t, autopilot.SessionMissing, rt.BrainSession(ctx, id), id)
		require.Equal(t, "terminal", rt.BrainLossCause(ctx, id), id)
	}
	require.Equal(t, autopilot.SessionUnknown, autopilotRuntime{s: &Server{}}.BrainSession(ctx, "x"))
}

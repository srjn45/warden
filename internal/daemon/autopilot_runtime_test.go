package daemon

import (
	"context"
	"testing"
	"time"

	_ "github.com/srjn45/warden/internal/agentbackend/backends"
	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/backendusage"
	"github.com/srjn45/warden/internal/handoff"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
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
	fs.data["voyage-autopilot"] = &store.Session{
		ID: "voyage-autopilot", TmuxSession: "voyage-autopilot",
		Status: store.StatusWorking, Backend: "claude", UpdatedAt: now, CreatedAt: now,
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

func TestSpawnGuardianCreatesAndAdoptsSlot(t *testing.T) {
	repo := t.TempDir()
	fs := &slotSpawnStore{fakeStore: newFakeStore()}
	srv := &Server{store: fs}
	rt := autopilotRuntime{s: srv}
	ctx := context.Background()

	id, err := rt.SpawnGuardian(ctx, "ap-abc", "voyage", repo)
	require.NoError(t, err)
	require.Equal(t, "voyage-guardian", id)

	id2, err := rt.SpawnGuardian(ctx, "ap-abc", "voyage", repo)
	require.NoError(t, err)
	require.Equal(t, "voyage-guardian", id2)
	require.Len(t, fs.data, 1)
}

func TestRotateBrainInvokesHotSwapNotRecovery(t *testing.T) {
	st := newFakeStore()
	life := &fakeLife{}
	workdir := t.TempDir()
	sess := &store.Session{
		ID: "agent-mgr-1", TmuxSession: "agent-mgr-1", Backend: "claude", Model: "opus",
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
	require.Equal(t, "codex", got.Backend)
	require.Nil(t, got.BackendRecovery, "guardian rotation must not start a recovery generation")
}

func TestRotateBrainWithLiveWorkersPreservesTreeAndLand(t *testing.T) {
	dataDir := t.TempDir()
	workdir := t.TempDir()
	st, err := store.NewFileStore(dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { st.Close(context.Background()) })

	runID := "ap-liveworkers"
	mgrID := "agent-mgr-1"
	now := time.Now().UTC()
	mgr := &store.Session{
		ID: mgrID, Name: mgrID, TmuxSession: mgrID, Backend: "claude", Model: "opus",
		Role: "autopilot", Repo: workdir, Workdir: workdir, Branch: "autopilot/manager",
		Worktree: workdir, Status: store.StatusWorking, CreatedAt: now, UpdatedAt: now,
		Tags: []string{"autopilot", "run:" + runID},
	}
	require.NoError(t, st.Insert(context.Background(), mgr))

	workers := []*store.Session{
		{
			ID: "agent-w1", TmuxSession: "agent-w1", ParentID: mgrID, Backend: "claude",
			Role: "worker", Repo: workdir, Workdir: workdir, Branch: "autopilot/task-a",
			Worktree: t.TempDir(), Status: store.StatusWorking, CreatedAt: now, UpdatedAt: now,
			Tags: []string{"autopilot", "run:" + runID},
		},
		{
			ID: "agent-w2", TmuxSession: "agent-w2", ParentID: mgrID, Backend: "claude",
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
	require.Equal(t, "codex", gotMgr.Backend)
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

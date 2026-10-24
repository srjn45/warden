package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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

type stalledManagerLife struct {
	managerLaunchLife
	entered, release chan struct{}
}

func (f *stalledManagerLife) Spawn(ctx context.Context, req SpawnRequest) (*agentstore.Agent, error) {
	close(f.entered)
	<-f.release // model a production dependency that ignores cancellation
	return f.managerLaunchLife.Spawn(ctx, req)
}

func TestRecoveryAutopilotAPIRespondsDuringStalledProductionSpawn(t *testing.T) {
	repo := t.TempDir()
	plan := filepath.Join(repo, "api.yaml")
	require.NoError(t, os.WriteFile(plan, []byte("version: 1\ngoal: recover\n"), 0o600))
	runs, err := autopilot.NewRunStore(t.TempDir())
	require.NoError(t, err)
	id := autopilot.RunID(repo, plan)
	require.NoError(t, runs.Create(autopilot.RunRecord{RunID: id, Repo: repo, PlanFile: plan,
		State: autopilot.StateActive, SlotScope: "api", IntegrationBranch: "autopilot/api"}))
	life := &stalledManagerLife{entered: make(chan struct{}), release: make(chan struct{})}
	srv := &Server{store: newFakeStore(), life: life, hub: newHub(), done: make(chan struct{})}
	c := autopilot.NewController(autopilot.ControllerConfig{RunStore: runs, BaseDir: repo,
		Resolver: autopilotTestResolver{}}, &apFakeEnv{repo: repo})
	srv.SetAutopilotController(c)
	t.Cleanup(func() {
		select {
		case <-life.release:
		default:
			close(life.release)
		}
		require.Eventually(t, func() bool { return c.Status().Recovery.InFlight == 0 }, 2*time.Second, time.Millisecond)
		require.NoError(t, c.Close())
	})
	select {
	case <-life.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("boot recovery did not reach production spawn")
	}
	httpServer := httptest.NewServer(srv.router())
	defer httpServer.Close()
	client := &http.Client{Timeout: time.Second}
	response, err := client.Get(httpServer.URL + "/api/v1/autopilot")
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var status autopilot.Status
	require.NoError(t, json.NewDecoder(response.Body).Decode(&status))
	require.Positive(t, status.Recovery.InFlight)
	_, err = c.PauseRun(context.Background(), id)
	require.NoError(t, err)
}

type duplicateManagerLife struct {
	fakeLife
	calls int
}

func (f *duplicateManagerLife) Spawn(context.Context, SpawnRequest) (*agentstore.Agent, error) {
	f.calls++
	return nil, errors.New("tmux new-session: exit status 1: duplicate session: voyage-autopilot")
}

func TestSpawnBrainDuplicateSessionReconcilesExactlyOnce(t *testing.T) {
	for _, scenario := range []string{"verified", "foreign", "missing", "ambiguous"} {
		t.Run(scenario, func(t *testing.T) {
			st, life := newFakeStore(), &duplicateManagerLife{}
			spec := autopilot.BrainSpec{RunID: "ap-voyage", PlanID: "plan-voyage", ProjectID: "project-voyage",
				SlotScope: "voyage", Repo: t.TempDir(), Backend: "codex"}
			checks := 0
			srv := &Server{store: st, life: life}
			srv.managerSlotInspector = managerSlotInspectorFunc(func(context.Context, string) (managerSlotObservation, error) {
				checks++
				if checks == 1 || scenario == "missing" {
					return managerSlotObservation{}, errManagerSlotMissing
				}
				obs := verifiedManagerObservation(autopilot.BootReconcileRun{RunID: spec.RunID, PlanID: spec.PlanID,
					ProjectID: spec.ProjectID, Repo: spec.Repo, ManagerSlotID: "voyage-autopilot"})
				if scenario == "foreign" {
					obs.RunID = "foreign"
				}
				if scenario == "ambiguous" {
					return managerSlotObservation{}, errors.New("metadata unavailable")
				}
				return obs, nil
			})
			handle, err := (autopilotRuntime{s: srv}).SpawnBrain(context.Background(), spec)
			if scenario == "verified" {
				require.NoError(t, err)
				require.Equal(t, "voyage-autopilot", handle.AgentID)
				require.False(t, handle.NewLaunch)
			} else {
				require.Error(t, err)
				require.Empty(t, handle.AgentID)
			}
			require.Equal(t, 1, life.calls)
			require.Equal(t, 2, checks, "one initial inspection and one race reconciliation")
			require.Empty(t, life.terminated)
			require.Empty(t, life.tornDown)
		})
	}
}

func TestSpawnBrainRefusesForeignRegisteredManagersWithoutMutation(t *testing.T) {
	for _, status := range []store.Status{store.StatusWorking, store.StatusDone} {
		for _, field := range []string{"role", "run", "plan", "project", "repo", "workdir", "slot", "tmux", "ambiguous"} {
			t.Run(string(status)+"/"+field, func(t *testing.T) {
				st, life := newFakeStore(), &fakeLife{}
				spec := autopilot.BrainSpec{RunID: "ap-voyage", PlanID: "plan-voyage", ProjectID: "project-voyage", SlotScope: "voyage", Repo: t.TempDir()}
				a := &agentstore.Agent{ID: "voyage-autopilot", TmuxSession: "voyage-autopilot", Role: autopilotBrainRole,
					AutopilotRunID: spec.RunID, AutopilotSlot: store.AutopilotSlotManager, PlanID: spec.PlanID,
					ProjectID: spec.ProjectID, Repo: spec.Repo, Workdir: spec.Repo, Status: status}
				switch field {
				case "role":
					a.Role = "worker"
				case "run":
					a.AutopilotRunID = "foreign"
				case "plan":
					a.PlanID = "foreign"
				case "project":
					a.ProjectID = "foreign"
				case "repo":
					a.Repo = "/foreign"
				case "workdir":
					a.Workdir = "/foreign"
				case "slot":
					a.AutopilotSlot = ""
				case "tmux":
					a.TmuxSession = "foreign-pane"
				case "ambiguous":
					st.data["other"] = &agentstore.Agent{ID: "other", Role: autopilotBrainRole, AutopilotRunID: spec.RunID, Status: store.StatusWorking}
				}
				st.data[a.ID] = a
				before := *a
				_, err := (autopilotRuntime{s: &Server{store: st, life: life}}).SpawnBrain(context.Background(), spec)
				require.ErrorIs(t, err, errManagerAdoptionRefused)
				require.Equal(t, before, *st.data[a.ID])
				require.Nil(t, life.spawned)
				require.Empty(t, life.terminated)
			})
		}
	}
}

func TestSpawnBrainVerifiesRegisteredPaneBeforeAdoptionOrTeardown(t *testing.T) {
	for _, field := range []string{"verified", "run", "plan", "project", "repo", "workdir", "role", "backend", "token", "unknown"} {
		t.Run(field, func(t *testing.T) {
			st, life := newFakeStore(), &fakeLife{}
			spec := autopilot.BrainSpec{RunID: "ap-voyage", PlanID: "plan-voyage", ProjectID: "project-voyage", SlotScope: "voyage", Repo: t.TempDir()}
			a := &agentstore.Agent{ID: "voyage-autopilot", TmuxSession: "voyage-autopilot", Role: autopilotBrainRole,
				AutopilotRunID: spec.RunID, AutopilotSlot: store.AutopilotSlotManager, PlanID: spec.PlanID,
				ProjectID: spec.ProjectID, Repo: spec.Repo, Workdir: spec.Repo, Status: store.StatusWorking, AiCli: "codex",
				Runtime: &store.SessionRuntime{LaunchID: "launch-1"}}
			st.data[a.ID] = a
			before := *a
			srv := &Server{store: st, life: life, managerSlotInspector: managerSlotInspectorFunc(func(context.Context, string) (managerSlotObservation, error) {
				obs := verifiedManagerObservation(autopilot.BootReconcileRun{RunID: spec.RunID, PlanID: spec.PlanID,
					ProjectID: spec.ProjectID, Repo: spec.Repo, ManagerSlotID: a.ID})
				switch field {
				case "run":
					obs.RunID = "foreign"
				case "plan":
					obs.PlanID = "foreign"
				case "project":
					obs.ProjectID = "foreign"
				case "repo":
					obs.Repo = "/foreign"
				case "workdir":
					obs.Workdir = "/foreign"
				case "role":
					obs.Role = "worker"
				case "backend":
					obs.Backend = "claude"
				case "token":
					obs.LaunchID = "replacement"
				case "unknown":
					return managerSlotObservation{}, errors.New("transport unavailable")
				}
				return obs, nil
			})}
			handle, err := (autopilotRuntime{s: srv}).SpawnBrain(context.Background(), spec)
			if field == "verified" {
				require.NoError(t, err)
				require.Equal(t, a.ID, handle.AgentID)
				require.False(t, handle.NewLaunch)
			} else {
				require.ErrorIs(t, err, errManagerAdoptionRefused)
			}
			require.Equal(t, before, *st.data[a.ID])
			require.Nil(t, life.spawned)
			require.Empty(t, life.terminated)
		})
	}
}

type canceledRotationLife struct {
	managerLaunchLife
	cancel context.CancelFunc
}

type rotationContextStore struct {
	*fakeStore
	observedDeadline bool
}

func (s *rotationContextStore) Update(ctx context.Context, id string, fn func(*agentstore.Agent) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline, ok := ctx.Deadline()
	s.observedDeadline = ok && time.Until(deadline) <= brainTeardownTimeout
	return s.fakeStore.Update(ctx, id, fn)
}

func (f *canceledRotationLife) HotSwap(ctx context.Context, a *agentstore.Agent, req lifecycle.SwapRequest) (*lifecycle.SwapResult, error) {
	res, err := f.managerLaunchLife.HotSwap(ctx, a, req)
	f.cancel()
	return res, err
}

func TestRotateBrainCanceledCompletionRecordsOwnedReplacement(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := &rotationContextStore{fakeStore: newFakeStore()}
	a := &agentstore.Agent{ID: "voyage-autopilot", TmuxSession: "voyage-autopilot", Role: autopilotBrainRole,
		AutopilotRunID: "ap-voyage", AutopilotSlot: store.AutopilotSlotManager, AiCli: "claude",
		Runtime: &store.SessionRuntime{LaunchID: "retired-token"}}
	require.NoError(t, st.Insert(ctx, a))
	life := &canceledRotationLife{cancel: cancel}
	rt := autopilotRuntime{s: &Server{store: st, life: life}}
	handle, err := rt.RotateBrain(ctx, autopilot.RotateBrainSpec{AgentID: a.ID, Backend: "codex"})
	require.NoError(t, err)
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	require.True(t, handle.NewLaunch)
	require.Equal(t, "successor-token", handle.LaunchID)
	require.True(t, st.observedDeadline)
	require.NoError(t, rt.CleanupRecoveryBrain(context.Background(), "ap-voyage", handle))
	require.Equal(t, a.TmuxSession, life.terminated)
}

func TestRecoveryCleanupRequiresExactLaunchEvidence(t *testing.T) {
	st, life := newFakeStore(), &fakeLife{}
	a := &agentstore.Agent{ID: "voyage-autopilot", TmuxSession: "voyage-autopilot", AutopilotRunID: "ap-voyage",
		AutopilotSlot: store.AutopilotSlotManager, Runtime: &store.SessionRuntime{LaunchID: "current"}}
	require.NoError(t, st.Insert(context.Background(), a))
	rt := autopilotRuntime{s: &Server{store: st, life: life}}
	handle := autopilot.BrainHandle{AgentID: a.ID, NewLaunch: true}
	require.ErrorContains(t, rt.CleanupRecoveryBrain(context.Background(), "ap-voyage", handle), "identity")
	handle.LaunchID = "old"
	require.NoError(t, rt.CleanupRecoveryBrain(context.Background(), "ap-voyage", handle))
	require.Empty(t, life.terminated, "recycled slot survives cleanup")
	_, err := newManagerLaunchHandle(&agentstore.Agent{ID: a.ID}, "codex")
	require.ErrorContains(t, err, "token")
	// A stale store record alone must not authorize killing a recycled pane.
	handle.LaunchID = "current"
	rt.s.managerSlotInspector = managerSlotInspectorFunc(func(context.Context, string) (managerSlotObservation, error) {
		return managerSlotObservation{TmuxSession: a.TmuxSession, RunID: a.AutopilotRunID,
			Role: autopilotBrainRole, LaunchID: "replacement"}, nil
	})
	require.NoError(t, rt.CleanupRecoveryBrain(context.Background(), "ap-voyage", handle))
	require.Empty(t, life.terminated)
}

func TestSpawnBrainProductionLaunchHasOwnershipToken(t *testing.T) {
	st := newFakeStore()
	lc := lifecycle.New(&lifecycle.FakeRunner{}, &lifecycle.FakeConfig{})
	lc.PromptsDir = t.TempDir()
	rt := autopilotRuntime{s: &Server{store: st, life: NewLifecycleAdapter(lc, st)}}
	handle, err := rt.SpawnBrain(context.Background(), autopilot.BrainSpec{RunID: "ap-production", PlanID: "plan-production",
		ProjectID: "project-production", Repo: t.TempDir(), SlotScope: "production", Backend: "codex", Prompt: "recover"})
	require.NoError(t, err)
	require.True(t, handle.NewLaunch)
	require.NotEmpty(t, handle.LaunchID)
	a, err := st.Get(context.Background(), handle.AgentID)
	require.NoError(t, err)
	require.Equal(t, a.Runtime.LaunchID, handle.LaunchID)
	require.Equal(t, "ap-production", a.AutopilotRunID)
}

type managerLaunchLife struct{ fakeLife }

func (f *managerLaunchLife) Spawn(ctx context.Context, req SpawnRequest) (*agentstore.Agent, error) {
	a, err := f.fakeLife.Spawn(ctx, req)
	if err == nil {
		a.AutopilotRunID, a.AutopilotSlot = req.AutopilotRunID, req.AutopilotSlot
		a.Runtime = &store.SessionRuntime{Kind: "tmux", LaunchID: "spawn-token"}
	}
	return a, err
}

func (f *managerLaunchLife) HotSwap(ctx context.Context, a *agentstore.Agent, req lifecycle.SwapRequest) (*lifecycle.SwapResult, error) {
	res, err := f.fakeLife.HotSwap(ctx, a, req)
	if err == nil {
		res.Agent.Runtime = &store.SessionRuntime{Kind: "tmux", LaunchID: "successor-token"}
	}
	return res, err
}

func TestSpawnBrainCreatesManagerSlot(t *testing.T) {
	repo := t.TempDir()
	fs := &slotSpawnStore{fakeStore: newFakeStore()}
	fl := &managerLaunchLife{}
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
		Role: autopilotBrainRole, AutopilotRunID: "ap-abc", AutopilotSlot: store.AutopilotSlotManager,
		Repo: repo, Workdir: repo,
	}
	fl := &fakeLife{}
	srv := &Server{store: fs, life: fl}
	rt := autopilotRuntime{s: srv}

	handle, err := rt.SpawnBrain(context.Background(), autopilot.BrainSpec{
		RunID:     "ap-abc",
		SlotScope: "voyage",
		Repo:      repo,
		Prompt:    "brief",
		Tags:      []string{"autopilot", "run:ap-abc"},
	})
	require.NoError(t, err)
	require.Equal(t, "voyage-autopilot", handle.AgentID)
	require.Nil(t, fl.spawned, "live slot is adopted without spawning")
}

func TestSpawnBrainRefusesUnverifiedLiveSlotWithoutLaunchingRival(t *testing.T) {
	fs := &slotSpawnStore{fakeStore: newFakeStore()}
	fl := &fakeLife{}
	spec := autopilot.BrainSpec{RunID: "ap-voyage", SlotScope: "voyage", Repo: "/repo/voyage", PlanID: "plan-voyage", ProjectID: "project-voyage", Backend: "claude"}
	srv := &Server{store: fs, life: fl, managerSlotInspector: managerSlotInspectorFunc(func(_ context.Context, slot string) (managerSlotObservation, error) {
		return managerSlotObservation{TmuxSession: slot, RunID: "ap-foreign", PlanID: spec.PlanID, ProjectID: spec.ProjectID,
			Repo: spec.Repo, Workdir: spec.Repo, Role: autopilotBrainRole, Backend: "claude", AICLISessionID: "session-foreign", LaunchID: "launch-foreign"}, nil
	})}
	_, err := (autopilotRuntime{s: srv}).SpawnBrain(context.Background(), spec)
	require.ErrorIs(t, err, errManagerAdoptionRefused)
	require.Nil(t, fl.spawned, "unverified live slot must not trigger a rival launch")
}

func TestRotateBrainInvokesHotSwapNotRecovery(t *testing.T) {
	st := newFakeStore()
	life := &managerLaunchLife{}
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

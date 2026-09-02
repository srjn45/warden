package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/auth"
	"github.com/srjn45/warden/internal/ctxstore"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/poller"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// ctxWithActor returns a context carrying an HTTP request whose actor header names
// the calling agent (empty ⇒ no header, i.e. a human/web caller), matching what
// stashRequest installs in production.
func ctxWithActor(actor string) context.Context {
	r := &http.Request{Header: http.Header{}}
	if actor != "" {
		r.Header.Set(auth.ActorHeader, actor)
	}
	return context.WithValue(context.Background(), requestCtxKey{}, r)
}

// requireForbidden asserts err is a 403 not_owned refusal.
func requireForbidden(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	var ae apiError
	require.True(t, errors.As(err, &ae), "expected an apiError")
	require.Equal(t, http.StatusForbidden, ae.code)
	require.Contains(t, ae.msg, "not_owned")
}

func TestGuardOwnership(t *testing.T) {
	fs := newFakeStore()
	// The run's brain (role autopilot, owning run:ap-1).
	brain := &store.Session{ID: "brain-1", Role: autopilotBrainRole, Tags: []string{"autopilot", "run:ap-1"}}
	// A worker of this run.
	owned := &store.Session{ID: "worker-1", Tags: []string{"autopilot", "run:ap-1"}}
	// A worker of a different run.
	foreign := &store.Session{ID: "worker-2", Tags: []string{"autopilot", "run:ap-2"}}
	// A hand-launched agent with no autopilot tags.
	manual := &store.Session{ID: "manual-1"}
	// An ordinary (non-brain) agent making a call.
	human := &store.Session{ID: "dev-1"}
	for _, s := range []*store.Session{brain, owned, foreign, manual, human} {
		require.NoError(t, fs.Insert(context.Background(), s))
	}
	srv := &Server{store: fs}

	t.Run("brain may act on its own run's worker", func(t *testing.T) {
		require.NoError(t, srv.guardOwnership(ctxWithActor("brain-1"), owned))
	})
	t.Run("brain may act on itself", func(t *testing.T) {
		require.NoError(t, srv.guardOwnership(ctxWithActor("brain-1"), brain))
	})
	t.Run("brain is refused a foreign run's worker", func(t *testing.T) {
		requireForbidden(t, srv.guardOwnership(ctxWithActor("brain-1"), foreign))
	})
	t.Run("brain is refused a manual agent", func(t *testing.T) {
		requireForbidden(t, srv.guardOwnership(ctxWithActor("brain-1"), manual))
	})
	t.Run("non-brain agent caller is unaffected", func(t *testing.T) {
		require.NoError(t, srv.guardOwnership(ctxWithActor("dev-1"), manual))
	})
	t.Run("human caller (no actor header) is unaffected", func(t *testing.T) {
		require.NoError(t, srv.guardOwnership(ctxWithActor(""), manual))
	})
	t.Run("unknown actor id is unaffected", func(t *testing.T) {
		require.NoError(t, srv.guardOwnership(ctxWithActor("ghost"), manual))
	})
}

// TestGuardOwnershipBrainWithoutRunTagOwnsNothing proves a brain that somehow
// carries no run tag is denied every foreign target (the safe default).
func TestGuardOwnershipBrainWithoutRunTag(t *testing.T) {
	fs := newFakeStore()
	brain := &store.Session{ID: "brain-x", Role: autopilotBrainRole, Tags: []string{"autopilot"}}
	target := &store.Session{ID: "worker-x", Tags: []string{"autopilot", "run:ap-1"}}
	require.NoError(t, fs.Insert(context.Background(), brain))
	require.NoError(t, fs.Insert(context.Background(), target))
	srv := &Server{store: fs}
	requireForbidden(t, srv.guardOwnership(ctxWithActor("brain-x"), target))
}

// TestInheritOwnershipTags proves autopilot's ownership tags flow mechanically
// from the calling agent to the agents (and pipelines) it creates, and that
// every other caller's tags pass through untouched.
func TestInheritOwnershipTags(t *testing.T) {
	fs := newFakeStore()
	manager := &store.Session{ID: "mgr-1", Role: autopilotBrainRole, Tags: []string{"autopilot", "run:ap-1"}}
	worker := &store.Session{ID: "wrk-1", Tags: []string{"autopilot", "run:ap-1"}}
	untagged := &store.Session{ID: "plain-1"}
	noRun := &store.Session{ID: "odd-1", Tags: []string{"autopilot"}}
	for _, s := range []*store.Session{manager, worker, untagged, noRun} {
		require.NoError(t, fs.Insert(context.Background(), s))
	}
	srv := &Server{store: fs}

	t.Run("manager's spawn inherits its run tags", func(t *testing.T) {
		got := srv.inheritOwnershipTags(ctxWithActor("mgr-1"), nil)
		require.ElementsMatch(t, []string{"autopilot", "run:ap-1"}, got)
	})
	t.Run("worker's spawn inherits too (transitive fence)", func(t *testing.T) {
		got := srv.inheritOwnershipTags(ctxWithActor("wrk-1"), []string{"custom"})
		require.ElementsMatch(t, []string{"custom", "autopilot", "run:ap-1"}, got)
	})
	t.Run("already-present tags are not duplicated", func(t *testing.T) {
		got := srv.inheritOwnershipTags(ctxWithActor("mgr-1"), []string{"autopilot", "run:ap-1"})
		require.ElementsMatch(t, []string{"autopilot", "run:ap-1"}, got)
	})
	t.Run("ordinary agent caller passes through", func(t *testing.T) {
		got := srv.inheritOwnershipTags(ctxWithActor("plain-1"), []string{"x"})
		require.Equal(t, []string{"x"}, got)
	})
	t.Run("human caller (no actor header) passes through", func(t *testing.T) {
		require.Nil(t, srv.inheritOwnershipTags(ctxWithActor(""), nil))
	})
	t.Run("autopilot tag without a run tag inherits nothing", func(t *testing.T) {
		require.Nil(t, srv.inheritOwnershipTags(ctxWithActor("odd-1"), nil))
	})
}

// TestSpawnRouteInheritsAutopilotTags proves POST /spawn stamps the caller's
// ownership tags through the real handler: a manager that forgets to pass tags
// still gets its worker fenced into the run.
func TestSpawnRouteInheritsAutopilotTags(t *testing.T) {
	fs := newFakeStore()
	mgr := &store.Session{ID: "mgr-1", Role: autopilotBrainRole, Tags: []string{"autopilot", "run:ap-9"}}
	require.NoError(t, fs.Insert(context.Background(), mgr))
	s := &Server{store: fs, life: &fakeLife{}}
	ts := httptest.NewServer(s.router())
	defer ts.Close()

	b, _ := json.Marshal(SpawnRequest{Prompt: "do x", Cwd: t.TempDir()})
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/spawn", bytes.NewReader(b))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(auth.ActorHeader, "mgr-1")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var sess store.Session
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&sess))
	require.ElementsMatch(t, []string{"autopilot", "run:ap-9"}, sess.Tags)
}

// TestPipelineRouteInheritsAutopilotTags proves the pipeline escalation path
// stays inside the fence: tags are captured at POST /pipelines (where the actor
// identity still exists) and stamped onto each job session the executor spawns
// later.
func TestPipelineRouteInheritsAutopilotTags(t *testing.T) {
	ps, err := pipeline.NewStore(t.TempDir())
	require.NoError(t, err)
	cs, err := ctxstore.New(t.TempDir())
	require.NoError(t, err)
	fs := newFakeStore()
	exec := NewExecutor(ps, fs, &fakeLife{}, cs, func() {})
	srv := &Server{store: fs, life: &fakeLife{}, exec: exec, hub: newHub(), done: make(chan struct{})}
	mgr := &store.Session{ID: "mgr-1", Role: autopilotBrainRole, Tags: []string{"autopilot", "run:ap-9"}}
	require.NoError(t, fs.Insert(context.Background(), mgr))
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/pipelines", strings.NewReader(yamlBody))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(auth.ActorHeader, "mgr-1")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	p, err := ps.Get("demo")
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"autopilot", "run:ap-9"}, p.Tags)

	resp2, err := http.Post(ts.URL+"/api/v1/pipelines/demo/start", "application/json", nil)
	require.NoError(t, err)
	resp2.Body.Close()
	require.Equal(t, http.StatusOK, resp2.StatusCode)
	// The injected root-span-out job spawns the first real job ("a") on a
	// follow-up async Reconcile, so poll until the job session appears.
	var job *store.Session
	require.Eventually(t, func() bool {
		s, gerr := fs.Get(context.Background(), "demo-a")
		if gerr != nil {
			return false
		}
		job = s
		return true
	}, 2*time.Second, 5*time.Millisecond, "job session demo-a should be spawned")
	require.ElementsMatch(t, []string{"autopilot", "run:ap-9"}, job.Tags)
}

// TestInstallDefaultAutoApprovePolicy proves the §10 seam installs a generous
// default only when the owner has configured no rules, and never clobbers an
// existing policy.
func TestInstallDefaultAutoApprovePolicy(t *testing.T) {
	t.Run("installs when owner has no rules", func(t *testing.T) {
		p := poller.New(nil, 0)
		rt := autopilotRuntime{s: &Server{poller: p}}
		rt.InstallDefaultAutoApprovePolicy()
		got := p.AutoApprovePolicySnapshot()
		require.True(t, got.Enabled, "the default policy enables auto-approve")
		require.True(t, got.HasRules(), "the default policy carries an allow rule")
	})

	t.Run("no-op when owner already configured rules", func(t *testing.T) {
		p := poller.New(nil, 0)
		owner := approval.Policy{Rules: approval.Rules{Deny: []approval.Rule{{Tool: "Bash"}}}}
		p.SetAutoApprovePolicy(owner)
		rt := autopilotRuntime{s: &Server{poller: p}}
		rt.InstallDefaultAutoApprovePolicy()
		got := p.AutoApprovePolicySnapshot()
		require.False(t, got.Enabled, "an owner-configured policy is left untouched")
		require.Len(t, got.Rules.Deny, 1)
		require.Empty(t, got.Rules.Allow)
	})
}

func TestStampAutopilotSpawnBackRefsClearsParentID(t *testing.T) {
	fs := newFakeStore()
	manager := &store.Session{
		ID:     "agent-brain",
		Role:   autopilotBrainRole,
		Tags:   []string{"autopilot", "run:ap-deadbeef1234"},
		Status: store.StatusWorking,
	}
	require.NoError(t, fs.Insert(context.Background(), manager))

	srv := &Server{store: fs}
	ctx := ctxWithActor("agent-brain")
	sr := &SpawnRequest{Role: "worker", Task: "build-feature", ParentID: "agent-brain"}
	srv.stampAutopilotSpawnBackRefs(ctx, sr)

	require.Empty(t, sr.ParentID)
	require.Equal(t, "ap-deadbeef1234", sr.AutopilotRunID)
	require.Equal(t, store.AutopilotSlotWorker, sr.AutopilotSlot)
	require.Equal(t, "build-feature", sr.AutopilotTaskID)
}

func TestStampAutopilotSpawnBackRefsLeavesNonWorkerAlone(t *testing.T) {
	fs := newFakeStore()
	require.NoError(t, fs.Insert(context.Background(), &store.Session{
		ID: "agent-brain", Role: autopilotBrainRole,
		Tags: []string{"autopilot", "run:ap-deadbeef1234"}, Status: store.StatusWorking,
	}))

	srv := &Server{store: fs}
	ctx := ctxWithActor("agent-brain")
	sr := &SpawnRequest{Role: "planner", ParentID: "agent-brain"}
	srv.stampAutopilotSpawnBackRefs(ctx, sr)

	require.Equal(t, "agent-brain", sr.ParentID)
	require.Empty(t, sr.AutopilotRunID)
}

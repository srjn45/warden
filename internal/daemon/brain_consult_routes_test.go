package daemon

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/brainconsult"
	"github.com/srjn45/warden/internal/config"
	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/stretchr/testify/require"
)

// routeMockConsultor is a test double for the autopilot brain-consult entry point.
type routeMockConsultor struct {
	result  brainconsult.Result
	err     error
	called  atomic.Int32
	lastReq brainconsult.Request
}

func (m *routeMockConsultor) Consult(_ context.Context, req brainconsult.Request) (brainconsult.Result, error) {
	m.called.Add(1)
	m.lastReq = req
	return m.result, m.err
}

func enableAutopilotWithBrain(t *testing.T, srv *Server, plan string) (runID, brainID string) {
	t.Helper()
	srv.SetAutopilotController(autopilot.NewController(autopilot.ControllerConfig{
		Plans:             []string{plan},
		IntegrationBranch: "autopilot/integration",
		Gate:              "auto",
		Resolver:          autopilotTestResolver{},
	}, &apFakeEnv{repo: filepath.Dir(plan)}))
	st, err := srv.autopilot.Enable(context.Background(), "")
	require.NoError(t, err)
	require.Len(t, st.Runs, 1)
	require.NotNil(t, st.Runs[0].Brain)
	return st.Runs[0].RunID, st.Runs[0].Brain.AgentID
}

func TestConsultBrainHandlerHappyPath(t *testing.T) {
	dir := t.TempDir()
	plan := filepath.Join(dir, "plan.yaml")
	require.NoError(t, os.WriteFile(plan, []byte("version: 1\ngoal: ship\n"), 0o644))

	mc := &routeMockConsultor{result: brainconsult.Result{
		Action: brainconsult.ActionNudgeAgent, Reason: "worker idle", BrainID: "consult-1",
	}}
	srv := &Server{store: newFakeStore(), life: &fakeLife{}, hub: newHub(), done: make(chan struct{})}
	srv.SetBaselineConfig(config.Config{BrainConsult: config.BrainConsultConfig{Enabled: true, Timeout: "10m", MaxConcurrent: 1}})
	srv.SetBrainConsultor(mc, 1)
	runID, brainID := enableAutopilotWithBrain(t, srv, plan)

	intent := "unblock stuck worker"
	resp, err := srv.ConsultBrain(ctxWithActor(brainID), oapi.ConsultBrainRequestObject{
		Body: &oapi.BrainConsultRequest{
			Intent:    intent,
			Situation: "worker-1 waiting on design call",
			Goal:      "pick a direction so the worker can continue",
			TaskId:    "task-a",
		},
	})
	require.NoError(t, err)
	ok, isOK := resp.(oapi.ConsultBrain200JSONResponse)
	require.True(t, isOK, "active manager gets 200, got %T", resp)
	require.Equal(t, oapi.BrainConsultResultActionNudgeAgent, ok.Action)
	require.Equal(t, "worker idle", ok.Reason)
	require.Equal(t, "consult-1", ok.BrainId)

	require.Equal(t, int32(1), mc.called.Load())
	require.Equal(t, intent, mc.lastReq.Intent)
	require.Equal(t, runID, mc.lastReq.RunID)
	require.Equal(t, "task-a", mc.lastReq.TaskID)
	require.Equal(t, defaultAutopilotAllowed, mc.lastReq.Allowed)
}

func TestConsultBrainHandlerRejectsNonManager(t *testing.T) {
	dir := t.TempDir()
	plan := filepath.Join(dir, "plan.yaml")
	require.NoError(t, os.WriteFile(plan, []byte("version: 1\ngoal: ship\n"), 0o644))

	mc := &routeMockConsultor{result: brainconsult.Result{Action: brainconsult.ActionNoop}}
	srv := &Server{store: newFakeStore(), life: &fakeLife{}, hub: newHub(), done: make(chan struct{})}
	srv.SetBaselineConfig(config.Config{BrainConsult: config.BrainConsultConfig{Enabled: true}})
	srv.SetBrainConsultor(mc, 1)
	runID, _ := enableAutopilotWithBrain(t, srv, plan)

	resp, err := srv.ConsultBrain(ctxWithActor(""), oapi.ConsultBrainRequestObject{
		Body: &oapi.BrainConsultRequest{Intent: "x"},
	})
	require.NoError(t, err)
	_, forbidden := resp.(oapi.ConsultBrain403JSONResponse)
	require.True(t, forbidden, "anonymous caller gets 403")
	require.Equal(t, int32(0), mc.called.Load())

	stale := &agentstore.Agent{ID: "stale-mgr", Role: autopilotBrainRole, Tags: []string{"autopilot", "run:" + runID}}
	require.NoError(t, srv.store.Insert(context.Background(), stale))
	resp, err = srv.ConsultBrain(ctxWithActor("stale-mgr"), oapi.ConsultBrainRequestObject{
		Body: &oapi.BrainConsultRequest{Intent: "x"},
	})
	require.NoError(t, err)
	_, forbidden = resp.(oapi.ConsultBrain403JSONResponse)
	require.True(t, forbidden, "stale manager gets 403")
	require.Equal(t, int32(0), mc.called.Load())
}

func TestConsultBrainHandlerDisabled(t *testing.T) {
	dir := t.TempDir()
	plan := filepath.Join(dir, "plan.yaml")
	require.NoError(t, os.WriteFile(plan, []byte("version: 1\ngoal: ship\n"), 0o644))

	mc := &routeMockConsultor{result: brainconsult.Result{Action: brainconsult.ActionNoop}}
	srv := &Server{store: newFakeStore(), life: &fakeLife{}, hub: newHub(), done: make(chan struct{})}
	srv.SetBaselineConfig(config.Config{BrainConsult: config.BrainConsultConfig{Enabled: false}})
	srv.SetBrainConsultor(mc, 1)
	_, brainID := enableAutopilotWithBrain(t, srv, plan)

	resp, err := srv.ConsultBrain(ctxWithActor(brainID), oapi.ConsultBrainRequestObject{
		Body: &oapi.BrainConsultRequest{Intent: "x"},
	})
	require.NoError(t, err)
	_, forbidden := resp.(oapi.ConsultBrain403JSONResponse)
	require.True(t, forbidden, "disabled consult returns 403")
	require.Equal(t, int32(0), mc.called.Load())
}

func TestConsultBrainHandlerNoReply(t *testing.T) {
	dir := t.TempDir()
	plan := filepath.Join(dir, "plan.yaml")
	require.NoError(t, os.WriteFile(plan, []byte("version: 1\ngoal: ship\n"), 0o644))

	mc := &routeMockConsultor{
		result: brainconsult.Result{BrainID: "consult-timeout"},
		err:    brainconsult.ErrNoBrainReply,
	}
	srv := &Server{store: newFakeStore(), life: &fakeLife{}, hub: newHub(), done: make(chan struct{})}
	srv.SetBaselineConfig(config.Config{BrainConsult: config.BrainConsultConfig{Enabled: true}})
	srv.SetBrainConsultor(mc, 1)
	_, brainID := enableAutopilotWithBrain(t, srv, plan)

	resp, err := srv.ConsultBrain(ctxWithActor(brainID), oapi.ConsultBrainRequestObject{
		Body: &oapi.BrainConsultRequest{Intent: "x"},
	})
	require.NoError(t, err)
	_, timedOut := resp.(oapi.ConsultBrain504JSONResponse)
	require.True(t, timedOut, "no-reply maps to 504, got %T", resp)
}

func TestConsultBrainHandlerRequiresIntent(t *testing.T) {
	dir := t.TempDir()
	plan := filepath.Join(dir, "plan.yaml")
	require.NoError(t, os.WriteFile(plan, []byte("version: 1\ngoal: ship\n"), 0o644))

	mc := &routeMockConsultor{result: brainconsult.Result{Action: brainconsult.ActionNoop}}
	srv := &Server{store: newFakeStore(), life: &fakeLife{}, hub: newHub(), done: make(chan struct{})}
	srv.SetBaselineConfig(config.Config{BrainConsult: config.BrainConsultConfig{Enabled: true}})
	srv.SetBrainConsultor(mc, 1)
	_, brainID := enableAutopilotWithBrain(t, srv, plan)

	_, err := srv.ConsultBrain(ctxWithActor(brainID), oapi.ConsultBrainRequestObject{
		Body: &oapi.BrainConsultRequest{Intent: "  "},
	})
	require.Error(t, err)
	require.Equal(t, int32(0), mc.called.Load())
}

func TestConsultBrainDoesNotTouchManagerSlot(t *testing.T) {
	// Regression guard for D5: a successful consult must leave the long-lived
	// manager slot and Guardian heal ladder untouched.
	dir := t.TempDir()
	plan := filepath.Join(dir, "plan.yaml")
	require.NoError(t, os.WriteFile(plan, []byte("version: 1\ngoal: ship\n"), 0o644))

	mc := &routeMockConsultor{result: brainconsult.Result{
		Action: brainconsult.ActionWait, Reason: "re-check next tick", BrainID: "c-1",
	}}
	srv := &Server{store: newFakeStore(), life: &fakeLife{}, hub: newHub(), done: make(chan struct{})}
	srv.SetBaselineConfig(config.Config{BrainConsult: config.BrainConsultConfig{Enabled: true}})
	srv.SetBrainConsultor(mc, 1)
	runID, brainID := enableAutopilotWithBrain(t, srv, plan)

	before := srv.autopilot.Status()
	require.Equal(t, autopilot.StateActive, before.Runs[0].State)
	require.Equal(t, brainID, before.Runs[0].Brain.AgentID)

	resp, err := srv.ConsultBrain(ctxWithActor(brainID), oapi.ConsultBrainRequestObject{
		Body: &oapi.BrainConsultRequest{Intent: "design call"},
	})
	require.NoError(t, err)
	_, isOK := resp.(oapi.ConsultBrain200JSONResponse)
	require.True(t, isOK)

	after := srv.autopilot.Status()
	require.Equal(t, autopilot.StateActive, after.Runs[0].State)
	require.Equal(t, brainID, after.Runs[0].Brain.AgentID)
	require.Equal(t, runID, after.Runs[0].RunID)
	require.Equal(t, before.Runs[0].Brain.AgentID, after.Runs[0].Brain.AgentID)
}

func TestBrainConsultSpawnerInsertAndTeardown(t *testing.T) {
	// Spawner adapter: Spawn persists the session; Teardown reaches lifecycle.
	st := newFakeStore()
	life := &fakeLife{}
	sp := brainConsultSpawner{life: life, store: st}

	sess, err := sp.Spawn(context.Background(), brainconsult.BrainSpawnArgs{
		Cwd: t.TempDir(), Prompt: "decide", Role: autopilotBrainRole,
	})
	require.NoError(t, err)
	require.NotEmpty(t, sess.ID)
	got, err := st.Get(context.Background(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, sess.ID, got.ID)

	require.NoError(t, sp.Teardown(context.Background(), sess))
	require.Equal(t, sess.ID, life.tornDown)
}

func TestConfigureBrainConsultDisabledLeavesNil(t *testing.T) {
	srv := &Server{store: newFakeStore(), life: &fakeLife{}, hub: newHub(), done: make(chan struct{})}
	srv.ConfigureBrainConsult(config.BrainConsultConfig{Enabled: false})
	require.Nil(t, srv.brainConsultor)

	srv.ConfigureBrainConsult(config.BrainConsultConfig{Enabled: true, Timeout: "1m", MaxConcurrent: 2})
	require.NotNil(t, srv.brainConsultor)
}

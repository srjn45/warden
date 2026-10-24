package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

type managerSlotInspectorFunc func(context.Context, string) (managerSlotObservation, error)

func (f managerSlotInspectorFunc) InspectManagerSlot(ctx context.Context, slot string) (managerSlotObservation, error) {
	return f(ctx, slot)
}

func verifiedManagerObservation(spec autopilot.BootReconcileRun) managerSlotObservation {
	return managerSlotObservation{
		TmuxSession: spec.ManagerSlotID, RunID: spec.RunID, PlanID: spec.PlanID,
		ProjectID: spec.ProjectID, Repo: spec.Repo, Workdir: spec.Repo,
		Role: autopilotBrainRole, Backend: "codex", AICLISessionID: "backend-session-1",
	}
}

func TestReconcileSessionsMigratesLegacyManagerToSlot(t *testing.T) {
	st := newFakeStore()
	runID := "ap-abc123def456"
	legacyID := "agent-8bed0ec5"
	slotID := "default-autopilot"
	require.NoError(t, st.Insert(context.Background(), &agentstore.Agent{
		ID: legacyID, TmuxSession: legacyID, Role: autopilotBrainRole,
		Status: store.StatusWorking, Tags: []string{"autopilot", "run:" + runID},
	}))
	rt := autopilotRuntime{s: &Server{store: st, life: &fakeLife{}, hub: newHub()}}
	spec := autopilot.BootReconcileRun{
		RunID: runID, SlotScope: "default", ManagerSlotID: slotID,
		LegacyBrainID: legacyID,
	}
	require.NoError(t, rt.ReconcileSessions(context.Background(), []autopilot.BootReconcileRun{spec}))

	_, err := st.Get(context.Background(), legacyID)
	require.Error(t, err)
	got, err := st.Get(context.Background(), slotID)
	require.NoError(t, err)
	require.Equal(t, slotID, got.ID)
	require.Equal(t, legacyID, got.TmuxSession, "live tmux pane name is preserved")
	require.Equal(t, runID, got.AutopilotRunID)
	require.Equal(t, store.AutopilotSlotManager, got.AutopilotSlot)
}

func TestReconcileSessionsStampsWorkerBackRefs(t *testing.T) {
	st := newFakeStore()
	runID := "ap-workers"
	require.NoError(t, st.Insert(context.Background(), &agentstore.Agent{
		ID: "worker-a", Role: "worker", Status: store.StatusWorking,
		ParentID: "agent-dead-manager", Task: "docs",
		Tags: []string{"autopilot", "run:" + runID},
	}))
	rt := autopilotRuntime{s: &Server{store: st, life: &fakeLife{}, hub: newHub()}}
	spec := autopilot.BootReconcileRun{
		RunID: runID, SlotScope: "default", ManagerSlotID: "default-autopilot",
	}
	require.NoError(t, rt.ReconcileSessions(context.Background(), []autopilot.BootReconcileRun{spec}))
	got, err := st.Get(context.Background(), "worker-a")
	require.NoError(t, err)
	require.Equal(t, runID, got.AutopilotRunID)
	require.Equal(t, store.AutopilotSlotWorker, got.AutopilotSlot)
	require.Equal(t, "docs", got.AutopilotTaskID)
	require.Empty(t, got.ParentID)
}

func TestReconcileSessionsIdempotent(t *testing.T) {
	st := newFakeStore()
	runID := "ap-idem"
	slotID := "plan-autopilot"
	require.NoError(t, st.Insert(context.Background(), &agentstore.Agent{
		ID: slotID, TmuxSession: slotID, Role: autopilotBrainRole, Status: store.StatusWorking,
		AutopilotRunID: runID, AutopilotSlot: store.AutopilotSlotManager,
		Tags: []string{"autopilot", "run:" + runID},
	}))
	rt := autopilotRuntime{s: &Server{store: st, life: &fakeLife{}, hub: newHub()}}
	spec := autopilot.BootReconcileRun{
		RunID: runID, SlotScope: "plan", ManagerSlotID: slotID,
	}
	require.NoError(t, rt.ReconcileSessions(context.Background(), []autopilot.BootReconcileRun{spec}))
	require.NoError(t, rt.ReconcileSessions(context.Background(), []autopilot.BootReconcileRun{spec}))
	got, err := st.Get(context.Background(), slotID)
	require.NoError(t, err)
	require.Equal(t, runID, got.AutopilotRunID)
}

func TestReconcileSessionsAdoptsOnlyVerifiedMissingManager(t *testing.T) {
	st := newFakeStore()
	spec := autopilot.BootReconcileRun{
		RunID: "ap-payments", SlotScope: "payments", ManagerSlotID: "payments-autopilot",
		PlanID: "plan-payments", ProjectID: "project-payments", Repo: "/repo/payments",
	}
	rt := autopilotRuntime{s: &Server{store: st, life: &fakeLife{}, hub: newHub(),
		managerSlotInspector: managerSlotInspectorFunc(func(_ context.Context, slot string) (managerSlotObservation, error) {
			require.Equal(t, spec.ManagerSlotID, slot)
			return verifiedManagerObservation(spec), nil
		}),
	}}
	require.NoError(t, rt.ReconcileSessions(context.Background(), []autopilot.BootReconcileRun{spec}))
	got, err := st.Get(context.Background(), spec.ManagerSlotID)
	require.NoError(t, err)
	require.Equal(t, spec.ManagerSlotID, got.Name)
	require.Equal(t, autopilotBrainRole, got.Role)
	require.Equal(t, spec.RunID, got.AutopilotRunID)
	require.Equal(t, spec.PlanID, got.PlanID)
	require.Equal(t, spec.ProjectID, got.ProjectID)
	require.Equal(t, spec.Repo, got.Workdir)
	require.Equal(t, store.AutopilotSlotManager, got.AutopilotSlot)
	require.Equal(t, []string{"autopilot", "run:" + spec.RunID}, got.Tags)
}

func TestReconcileSessionsRefusesForeignOrAmbiguousMissingManager(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*managerSlotObservation)
	}{
		{name: "foreign run", mutate: func(o *managerSlotObservation) { o.RunID = "ap-other" }},
		{name: "ambiguous project", mutate: func(o *managerSlotObservation) { o.ProjectID = "project-other" }},
		{name: "missing continuity", mutate: func(o *managerSlotObservation) { o.AICLISessionID = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeStore()
			spec := autopilot.BootReconcileRun{RunID: "ap-payments", SlotScope: "payments", ManagerSlotID: "payments-autopilot", PlanID: "plan-payments", ProjectID: "project-payments", Repo: "/repo/payments"}
			rt := autopilotRuntime{s: &Server{store: st, life: &fakeLife{}, hub: newHub(), managerSlotInspector: managerSlotInspectorFunc(func(_ context.Context, _ string) (managerSlotObservation, error) {
				o := verifiedManagerObservation(spec)
				tc.mutate(&o)
				return o, nil
			})}}
			require.NoError(t, rt.ReconcileSessions(context.Background(), []autopilot.BootReconcileRun{spec}))
			_, err := st.Get(context.Background(), spec.ManagerSlotID)
			require.ErrorIs(t, err, agentstore.ErrNotFound)
		})
	}
}

func TestReconcileSessionsRefusesUnavailableManagerMetadata(t *testing.T) {
	st := newFakeStore()
	spec := autopilot.BootReconcileRun{RunID: "ap-payments", SlotScope: "payments", ManagerSlotID: "payments-autopilot", PlanID: "plan-payments", ProjectID: "project-payments", Repo: "/repo/payments"}
	rt := autopilotRuntime{s: &Server{store: st, life: &fakeLife{}, hub: newHub(), managerSlotInspector: managerSlotInspectorFunc(func(context.Context, string) (managerSlotObservation, error) {
		return managerSlotObservation{}, errors.New("tmux unavailable")
	})}}
	require.NoError(t, rt.ReconcileSessions(context.Background(), []autopilot.BootReconcileRun{spec}))
	_, err := st.Get(context.Background(), spec.ManagerSlotID)
	require.ErrorIs(t, err, agentstore.ErrNotFound)
}

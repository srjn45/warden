package daemon

import (
	"context"
	"testing"

	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func TestReconcileSessionsMigratesLegacyManagerToSlot(t *testing.T) {
	st := newFakeStore()
	runID := "ap-abc123def456"
	legacyID := "agent-8bed0ec5"
	slotID := "default-autopilot"
	require.NoError(t, st.Insert(context.Background(), &store.Session{
		ID: legacyID, TmuxSession: legacyID, Role: autopilotBrainRole,
		Status: store.StatusWorking, Tags: []string{"autopilot", "run:" + runID},
	}))
	rt := autopilotRuntime{s: &Server{store: st, life: &fakeLife{}, hub: newHub()}}
	spec := autopilot.BootReconcileRun{
		RunID: runID, SlotScope: "default", ManagerSlotID: slotID,
		LegacyBrainID: legacyID, GuardianSlotID: "default-guardian",
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

func TestReconcileSessionsRetiresLegacyGuardian(t *testing.T) {
	st := newFakeStore()
	runID := "ap-live"
	require.NoError(t, st.Insert(context.Background(), &store.Session{
		ID: "guardian-deadbeef", Status: store.StatusWorking,
		Tags: []string{"system:true", "autopilot-run:" + runID},
	}))
	rt := autopilotRuntime{s: &Server{store: st, life: &fakeLife{}, hub: newHub()}}
	spec := autopilot.BootReconcileRun{
		RunID: runID, SlotScope: "default", ManagerSlotID: "default-autopilot",
		GuardianSlotID: "default-guardian",
	}
	require.NoError(t, rt.ReconcileSessions(context.Background(), []autopilot.BootReconcileRun{spec}))
	got, err := st.Get(context.Background(), "guardian-deadbeef")
	require.NoError(t, err)
	require.Equal(t, store.StatusDone, got.Status)
}

func TestReconcileSessionsStampsWorkerBackRefs(t *testing.T) {
	st := newFakeStore()
	runID := "ap-workers"
	require.NoError(t, st.Insert(context.Background(), &store.Session{
		ID: "worker-a", Role: "worker", Status: store.StatusWorking,
		ParentID: "agent-dead-manager", Task: "docs",
		Tags: []string{"autopilot", "run:" + runID},
	}))
	rt := autopilotRuntime{s: &Server{store: st, life: &fakeLife{}, hub: newHub()}}
	spec := autopilot.BootReconcileRun{
		RunID: runID, SlotScope: "default", ManagerSlotID: "default-autopilot",
		GuardianSlotID: "default-guardian",
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
	require.NoError(t, st.Insert(context.Background(), &store.Session{
		ID: slotID, TmuxSession: slotID, Role: autopilotBrainRole, Status: store.StatusWorking,
		AutopilotRunID: runID, AutopilotSlot: store.AutopilotSlotManager,
		Tags: []string{"autopilot", "run:" + runID},
	}))
	rt := autopilotRuntime{s: &Server{store: st, life: &fakeLife{}, hub: newHub()}}
	spec := autopilot.BootReconcileRun{
		RunID: runID, SlotScope: "plan", ManagerSlotID: slotID, GuardianSlotID: "plan-guardian",
	}
	require.NoError(t, rt.ReconcileSessions(context.Background(), []autopilot.BootReconcileRun{spec}))
	require.NoError(t, rt.ReconcileSessions(context.Background(), []autopilot.BootReconcileRun{spec}))
	got, err := st.Get(context.Background(), slotID)
	require.NoError(t, err)
	require.Equal(t, runID, got.AutopilotRunID)
}

func TestGuardianBootReconcileTerminatesLegacyGuardianForSlot(t *testing.T) {
	st := newFakeStore()
	runID := "ap-live"
	for _, sess := range []*store.Session{
		{ID: "default-guardian", Status: store.StatusWorking, Tags: []string{"system:true", "autopilot-run:" + runID}},
		{ID: "guardian-deadbeef", Status: store.StatusWorking, Tags: []string{"system:true", "autopilot-run:" + runID}},
	} {
		require.NoError(t, st.Insert(context.Background(), sess))
	}
	rt := autopilotRuntime{s: &Server{store: st, life: &fakeLife{}, hub: newHub()}}
	missing, err := rt.ReconcileGuardians(context.Background(), map[string]string{runID: "default-guardian"})
	require.NoError(t, err)
	require.Empty(t, missing)
	live, _ := st.Get(context.Background(), "default-guardian")
	legacy, _ := st.Get(context.Background(), "guardian-deadbeef")
	require.Equal(t, store.StatusWorking, live.Status)
	require.Equal(t, store.StatusDone, legacy.Status)
}

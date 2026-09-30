package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/autopilotstore"
	"github.com/srjn45/warden/internal/planstore"
)

func TestPlanCompletionWatcherReapsAutopilotForCompletedPlan(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	plans, err := planstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = plans.Close() })

	completedID := planstore.PlanID(root, "done")
	activeID := planstore.PlanID(root, "active")
	require.NoError(t, plans.Create(ctx, &planstore.Plan{
		ID: completedID, ProjectID: root, Name: "done", Status: planstore.PlanStatusCompleted,
	}))
	require.NoError(t, plans.Create(ctx, &planstore.Plan{
		ID: activeID, ProjectID: root, Name: "active", Status: planstore.PlanStatusInProgress,
	}))

	data := t.TempDir()
	runs, err := autopilot.NewRunStore(data)
	require.NoError(t, err)
	live, err := autopilotstore.New(data)
	require.NoError(t, err)
	t.Cleanup(func() { _ = live.Close() })
	controller := autopilot.NewController(autopilot.ControllerConfig{
		BaseDir: root, RunStore: runs, LiveStore: live, PlanSource: plans, Gate: "local",
	}, autopilot.NewExecEnv())
	t.Cleanup(func() { require.NoError(t, controller.Close()) })

	for _, a := range []*autopilotstore.Autopilot{
		{ID: "ap-old", PlanID: completedID, ProjectID: root, Name: "AP:done"},
		{ID: "ap-current", PlanID: completedID, ProjectID: root, Name: "AP:done"},
		{ID: "ap-active", PlanID: activeID, ProjectID: root, Name: "AP:active"},
	} {
		require.NoError(t, live.Create(ctx, a))
	}

	srv := &Server{plans: plans, autopilot: controller}
	srv.tickPlanCompletion(ctx)
	srv.tickPlanCompletion(ctx) // reconciliation is idempotent

	_, err = live.Get(ctx, "ap-old")
	require.ErrorIs(t, err, autopilotstore.ErrNotFound)
	_, err = live.Get(ctx, "ap-current")
	require.ErrorIs(t, err, autopilotstore.ErrNotFound)
	_, err = live.Get(ctx, "ap-active")
	require.NoError(t, err, "uncompleted plans must retain their executors")
}

// TestPlanCompletionWatcherTearsDownCompleteAutopilotWhileFinalizeBlocked covers
// the UI bug: CompleteRun leaves StateComplete in Status, Finalize may still be
// blocked (e.g. incomplete tasks), but the disposable Autopilot must leave the tree.
func TestPlanCompletionWatcherTearsDownCompleteAutopilotWhileFinalizeBlocked(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	plans, err := planstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = plans.Close() })

	planID := planstore.PlanID(root, "blocked")
	apID := "ap-complete-blocked"
	now := time.Now().UTC()
	require.NoError(t, plans.Create(ctx, &planstore.Plan{
		ID: planID, ProjectID: root, Name: "blocked",
		Status: planstore.PlanStatusInProgress, ExecutionMode: planstore.PlanModeAutopilot,
		AutopilotRunID: apID,
		TaskProgress:   map[string]string{"t1": "pending"},
		Tasks:          []planstore.PlanTask{{ID: "t1", Prompt: "do it"}},
		ActiveExecution: &planstore.PlanExecution{
			ID: "pe-blocked", PlanID: planID, ExecutionMode: planstore.PlanModeAutopilot,
			ExecutorID: apID, StartedAt: now,
		},
	}))

	data := t.TempDir()
	runs, err := autopilot.NewRunStore(data)
	require.NoError(t, err)
	live, err := autopilotstore.New(data)
	require.NoError(t, err)
	t.Cleanup(func() { _ = live.Close() })
	controller := autopilot.NewController(autopilot.ControllerConfig{
		BaseDir: root, RunStore: runs, LiveStore: live, PlanSource: plans, Gate: "local",
	}, autopilot.NewExecEnv())
	t.Cleanup(func() { require.NoError(t, controller.Close()) })

	require.NoError(t, live.Create(ctx, &autopilotstore.Autopilot{
		ID: apID, PlanID: planID, ProjectID: root, Name: "AP:blocked",
		Diagnostics: autopilotstore.Diagnostics{State: string(autopilot.StateComplete), Repo: root},
	}))
	require.NoError(t, controller.RecoverLiveAutopilots(ctx))

	var state autopilot.RunState
	for _, r := range controller.Status().Runs {
		if r.RunID == apID {
			state = r.State
		}
	}
	require.Equal(t, autopilot.StateComplete, state)

	srv := &Server{plans: plans, autopilot: controller}
	srv.tickPlanCompletion(ctx)

	_, err = live.Get(ctx, apID)
	require.ErrorIs(t, err, autopilotstore.ErrNotFound, "complete autopilot must be removed from live store")
	for _, r := range controller.Status().Runs {
		require.NotEqual(t, apID, r.RunID, "complete autopilot must leave Status")
	}
	got, err := plans.Get(ctx, planID)
	require.NoError(t, err)
	require.Equal(t, planstore.PlanStatusInProgress, got.Status, "plan stays in_progress when Finalize is blocked")
}

func TestPlanCompletionWatcherReapsOrphanCompleteAutopilot(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	plans, err := planstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = plans.Close() })

	planID := planstore.PlanID(root, "orphan")
	currentID := "ap-current-healing"
	orphanID := "ap-orphan-complete"
	require.NoError(t, plans.Create(ctx, &planstore.Plan{
		ID: planID, ProjectID: root, Name: "orphan",
		Status: planstore.PlanStatusInProgress, ExecutionMode: planstore.PlanModeAutopilot,
		AutopilotRunID: currentID,
	}))

	data := t.TempDir()
	runs, err := autopilot.NewRunStore(data)
	require.NoError(t, err)
	live, err := autopilotstore.New(data)
	require.NoError(t, err)
	t.Cleanup(func() { _ = live.Close() })
	controller := autopilot.NewController(autopilot.ControllerConfig{
		BaseDir: root, RunStore: runs, LiveStore: live, PlanSource: plans, Gate: "local",
	}, autopilot.NewExecEnv())
	t.Cleanup(func() { require.NoError(t, controller.Close()) })

	require.NoError(t, live.Create(ctx, &autopilotstore.Autopilot{
		ID: currentID, PlanID: planID, ProjectID: root, Name: "AP:orphan",
		Diagnostics: autopilotstore.Diagnostics{State: string(autopilot.StateHealing), Repo: root},
	}))
	require.NoError(t, live.Create(ctx, &autopilotstore.Autopilot{
		ID: orphanID, PlanID: planID, ProjectID: root, Name: "AP:orphan",
		Diagnostics: autopilotstore.Diagnostics{State: string(autopilot.StateComplete), Repo: root},
	}))
	require.NoError(t, controller.RecoverLiveAutopilots(ctx))

	srv := &Server{plans: plans, autopilot: controller}
	srv.tickPlanCompletion(ctx)

	_, err = live.Get(ctx, orphanID)
	require.ErrorIs(t, err, autopilotstore.ErrNotFound, "orphan complete executor must be reaped")
	_, err = live.Get(ctx, currentID)
	require.NoError(t, err, "current non-complete executor must remain")
}

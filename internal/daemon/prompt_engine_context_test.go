package daemon

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/planstore"
)

func TestCanonicalStrategyContextUsesPlanStoreAndMatchingRun(t *testing.T) {
	plans, err := planstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = plans.Close() })
	require.NoError(t, plans.Create(context.Background(), &planstore.Plan{
		ID: "plan-context", Goal: "ship the bounded change", Constraints: []string{"no shell"},
		AutopilotRunID: "ap-context", Tasks: []planstore.PlanTask{{ID: "t1", Prompt: "wire direct prompts"}},
		TaskProgress: map[string]string{"t1": "in_progress"},
	}))
	s := &Server{plans: plans}
	ag := &agentstore.Agent{PlanID: "plan-context", AutopilotRunID: "ap-context", AutopilotTaskID: "t1"}
	got := s.canonicalStrategyContext(context.Background(), ag, ag.AutopilotRunID)
	require.Equal(t, "ship the bounded change", got.PlanGoal)
	require.Equal(t, "wire direct prompts", got.ActiveTask)
	require.Equal(t, "no shell", got.Constraints)

	// A manager has no worker task id; it is bounded only when canonical
	// progress identifies one active task.
	ag.AutopilotTaskID = ""
	require.Equal(t, "wire direct prompts", s.canonicalStrategyContext(context.Background(), ag, ag.AutopilotRunID).ActiveTask)
	ag.AutopilotTaskID = "t1"

	ag.AutopilotRunID = "wrong-run"
	require.Equal(t, "", s.canonicalStrategyContext(context.Background(), ag, ag.AutopilotRunID).PlanGoal)
	ag.AutopilotRunID = "ap-context"
	ag.AutopilotTaskID = "missing"
	require.Equal(t, "", s.canonicalStrategyContext(context.Background(), ag, ag.AutopilotRunID).ActiveTask)
}

package autopilot

import (
	"testing"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func TestWorkerSpawnRole(t *testing.T) {
	require.True(t, WorkerSpawnRole("worker"))
	require.True(t, WorkerSpawnRole("implementer"))
	require.False(t, WorkerSpawnRole("planner"))
}

func TestSessionRunIDBackRefAndLegacyTag(t *testing.T) {
	require.Equal(t, "ap-abc", SessionRunID(&agentstore.Agent{AutopilotRunID: "ap-abc"}))
	require.Equal(t, "ap-legacy", SessionRunID(&agentstore.Agent{Tags: []string{"run:ap-legacy"}}))
}

func TestIsManagerAndWorkerRecord(t *testing.T) {
	require.True(t, IsManagerRecord(&agentstore.Agent{
		AutopilotSlot: store.AutopilotSlotManager, AutopilotRunID: "ap-x",
	}))
	require.True(t, IsWorkerRecord(&agentstore.Agent{
		AutopilotSlot: store.AutopilotSlotWorker, AutopilotRunID: "ap-x", Tags: []string{"autopilot"},
	}))
}

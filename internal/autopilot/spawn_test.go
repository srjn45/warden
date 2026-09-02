package autopilot

import (
	"testing"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func TestWorkerSpawnRole(t *testing.T) {
	require.True(t, WorkerSpawnRole("worker"))
	require.True(t, WorkerSpawnRole("implementer"))
	require.False(t, WorkerSpawnRole("planner"))
}

func TestSessionRunIDBackRefAndLegacyTag(t *testing.T) {
	require.Equal(t, "ap-abc", SessionRunID(&store.Session{AutopilotRunID: "ap-abc"}))
	require.Equal(t, "ap-legacy", SessionRunID(&store.Session{Tags: []string{"run:ap-legacy"}}))
}

func TestIsManagerAndWorkerRecord(t *testing.T) {
	require.True(t, IsManagerRecord(&store.Session{
		AutopilotSlot: store.AutopilotSlotManager, AutopilotRunID: "ap-x",
	}))
	require.True(t, IsWorkerRecord(&store.Session{
		AutopilotSlot: store.AutopilotSlotWorker, AutopilotRunID: "ap-x", Tags: []string{"autopilot"},
	}))
}

package autopilot

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWorkerSpawnBranchPrompt(t *testing.T) {
	require.Empty(t, WorkerSpawnBranchPrompt(" "))
	got := WorkerSpawnBranchPrompt("autopilot/ship")
	require.Contains(t, got, "wd sync onto autopilot/ship first")
	require.Contains(t, got, "wd check before PR")
	require.Contains(t, got, "wd job done when green")
	require.Contains(t, got, "Open PRs against autopilot/ship")
}

func TestAppendWorkerSpawnBranch(t *testing.T) {
	require.Equal(t, "do x", AppendWorkerSpawnBranch("do x", ""))
	require.Equal(t, WorkerSpawnBranchPrompt("autopilot/ship"), AppendWorkerSpawnBranch("", "autopilot/ship"))
	got := AppendWorkerSpawnBranch("Implement the API", "autopilot/ship")
	require.Contains(t, got, "Implement the API")
	require.Contains(t, got, WorkerSpawnBranchPrompt("autopilot/ship"))
	// already mentions the branch — do not duplicate
	require.Equal(t, got, AppendWorkerSpawnBranch(got, "autopilot/ship"))
}

package planstore

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestApplyDefaultTaskDAG_ChainsWhenNoEdges(t *testing.T) {
	tasks := []TaskSpec{
		{ID: "a", Prompt: "first"},
		{ID: "b", Prompt: "second"},
		{ID: "c", Prompt: "third"},
	}
	got := ApplyDefaultTaskDAG(tasks)
	require.Empty(t, got[0].After)
	require.Equal(t, []string{"a"}, got[1].After)
	require.Equal(t, []string{"b"}, got[2].After)
}

func TestApplyDefaultTaskDAG_PreservesExplicitEdges(t *testing.T) {
	tasks := []TaskSpec{
		{ID: "a", Prompt: "first"},
		{ID: "b", Prompt: "second"}, // intentional parallel root
		{ID: "c", Prompt: "join", After: []string{"a", "b"}},
	}
	got := ApplyDefaultTaskDAG(tasks)
	require.Empty(t, got[0].After)
	require.Empty(t, got[1].After)
	require.Equal(t, []string{"a", "b"}, got[2].After)
}

func TestValidateTaskDAG_RejectsCycle(t *testing.T) {
	err := ValidateTaskDAG([]TaskSpec{
		{ID: "a", Prompt: "x", After: []string{"b"}},
		{ID: "b", Prompt: "y", After: []string{"a"}},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "cycle")
}

func TestValidateTaskDAG_RejectsSelfEdge(t *testing.T) {
	err := ValidateTaskDAG([]TaskSpec{
		{ID: "a", Prompt: "x", After: []string{"a"}},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "itself")
}

func TestReadyAndBlockedTaskIDs(t *testing.T) {
	tasks := []PlanTask{
		{ID: "a", Prompt: "first"},
		{ID: "b", Prompt: "second", After: []string{"a"}},
		{ID: "c", Prompt: "third", After: []string{"b"}},
	}
	require.Equal(t, []string{"a"}, ReadyTaskIDs(tasks, nil))
	require.Equal(t, []string{"b", "c"}, BlockedTaskIDs(tasks, nil))

	progress := map[string]string{"a": "done"}
	require.Equal(t, []string{"b"}, ReadyTaskIDs(tasks, progress))
	require.Equal(t, []string{"c"}, BlockedTaskIDs(tasks, progress))
	require.True(t, TaskDepsSatisfied(tasks, progress, "b"))
	require.False(t, TaskDepsSatisfied(tasks, progress, "c"))
}

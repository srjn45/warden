package cli

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srajanpathak/agentctl/internal/store"
)

func TestBuildSuccessorParams(t *testing.T) {
	old := &store.Session{Workdir: "/repo/.worktrees/CRD-1", Supervised: true, Repo: "/repo", Worktree: "/repo/.worktrees/CRD-1"}
	p := buildSuccessorParams(old, "do the thing")
	require.Equal(t, "do the thing", p.Prompt)
	require.Equal(t, "/repo/.worktrees/CRD-1", p.Cwd, "successor must launch in the old agent's workdir (the worktree)")
	require.True(t, p.Supervised, "successor inherits supervised mode")
	// Prompt-mode spawn: no Type/Repo/Worktree, so the existing worktree is reused by cwd, not recreated.
	require.Empty(t, p.Type)
	require.Empty(t, p.Repo)
	require.False(t, p.Worktree)
}

func TestComposeSuccessorPrompt(t *testing.T) {
	got := composeSuccessorPrompt("Finish the migration.", "/repo/.agentctl/rotate-handoff.md")
	require.Contains(t, got, "/repo/.agentctl/rotate-handoff.md", "must point successor at the handoff file")
	require.Contains(t, got, "Finish the migration.", "must include the human-reviewed resume prompt")
}

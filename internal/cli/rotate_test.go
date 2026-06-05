package cli

import (
	"os"
	"path/filepath"
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

func TestSelfSessionID(t *testing.T) {
	t.Setenv("AGENTCTL_SESSION_ID", "agent-abc123")
	id, err := selfSessionID()
	require.NoError(t, err)
	require.Equal(t, "agent-abc123", id)

	t.Setenv("AGENTCTL_SESSION_ID", "")
	_, err = selfSessionID()
	require.Error(t, err, "must error when not run inside an agent session")
}

func TestValidateHandoff(t *testing.T) {
	dir := t.TempDir()

	missing := filepath.Join(dir, "nope.md")
	require.Error(t, validateHandoff(missing), "missing file is an error")

	empty := filepath.Join(dir, "empty.md")
	require.NoError(t, os.WriteFile(empty, nil, 0o644))
	require.Error(t, validateHandoff(empty), "empty file is an error")

	good := filepath.Join(dir, "good.md")
	require.NoError(t, os.WriteFile(good, []byte("notes"), 0o644))
	require.NoError(t, validateHandoff(good))
}

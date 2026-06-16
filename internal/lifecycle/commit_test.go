package lifecycle

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCommitWorktreeCommitsDirtyTree(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git status --porcelain": {Out: " M internal/foo.go\n"},
	}}
	committed, err := New(fr, &FakeConfig{}).CommitWorktree(context.Background(), "/wt", "pipeline p/a: done")
	require.NoError(t, err)
	require.True(t, committed)
	require.Contains(t, fr.calledArgs(), []string{"git", "add", "-A"})
	require.Contains(t, fr.calledArgs(), []string{"git", "commit", "-m", "pipeline p/a: done"})
}

func TestCommitWorktreeSkipsCleanTree(t *testing.T) {
	fr := &FakeRunner{} // unmatched "git status --porcelain" -> "" -> clean tree
	committed, err := New(fr, &FakeConfig{}).CommitWorktree(context.Background(), "/wt", "msg")
	require.NoError(t, err)
	require.False(t, committed)
	require.NotContains(t, fr.calledArgs(), []string{"git", "commit", "-m", "msg"})
}

package lifecycle

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProtectedBranchesDefaultAndCustomConfig(t *testing.T) {
	ctx := context.Background()
	defaultBranch := "git symbolic-ref --quiet --short refs/remotes/origin/HEAD"

	defaultLife := New(&FakeRunner{Responses: map[string]FakeResp{
		defaultBranch: {Out: "origin/develop\n"},
	}}, &FakeConfig{})
	require.True(t, defaultLife.IsProtectedBranchInRepo(ctx, "/repo", "main"))
	require.True(t, defaultLife.IsProtectedBranchInRepo(ctx, "/repo", "master"))
	require.True(t, defaultLife.IsProtectedBranchInRepo(ctx, "/repo", "develop"))
	require.False(t, defaultLife.IsProtectedBranchInRepo(ctx, "/repo", "autopilot/plan"))

	customLife := New(&FakeRunner{Responses: map[string]FakeResp{
		defaultBranch: {Out: "origin/develop\n"},
	}}, &FakeConfig{ProtectedBranches: []string{"release", "production"}})
	require.False(t, customLife.IsProtectedBranchInRepo(ctx, "/repo", "main"), "the configured list replaces main/master")
	require.True(t, customLife.IsProtectedBranchInRepo(ctx, "/repo", "release"))
	require.True(t, customLife.IsProtectedBranchInRepo(ctx, "/repo", "production"))
	require.True(t, customLife.IsProtectedBranchInRepo(ctx, "/repo", "develop"), "the repository default remains protected")
}

func TestProtectedBranchesCanAllowRepositoryDefault(t *testing.T) {
	l := New(&FakeRunner{Responses: map[string]FakeResp{
		"git symbolic-ref --quiet --short refs/remotes/origin/HEAD": {Out: "origin/develop\n"},
	}}, &FakeConfig{ProtectedBranches: []string{"release"}, ProtectDefaultBranchOff: true})
	require.False(t, l.IsProtectedBranchInRepo(context.Background(), "/repo", "develop"))
	require.True(t, l.IsProtectedBranchInRepo(context.Background(), "/repo", "release"))
}

func TestProtectedBranchesApplyToCommitPushAndPR(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git rev-parse --abbrev-ref HEAD":                           {Out: "develop\n"},
		"git symbolic-ref --quiet --short refs/remotes/origin/HEAD": {Out: "origin/develop\n"},
	}}
	l := New(fr, &FakeConfig{})
	ctx := context.Background()

	_, err := l.Commit(ctx, "/repo", "nope")
	require.ErrorContains(t, err, "protected branch")
	_, err = l.Push(ctx, "/repo", false)
	require.ErrorContains(t, err, "protected branch")
	_, err = l.CreatePR(ctx, "/repo", "title", "body", "main")
	require.ErrorContains(t, err, "protected branch")
}

func TestAutopilotBranchIsCommittableAndPushableByDefault(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git rev-parse --abbrev-ref HEAD": {Out: "autopilot/example\n"},
		"git status --porcelain":          {Out: " M change.go\n"},
		"git rev-parse --short HEAD":      {Out: "abc1234\n"},
	}}
	l := New(fr, &FakeConfig{})
	ctx := context.Background()

	_, err := l.Commit(ctx, "/repo", "allow autopilot integration branch")
	require.NoError(t, err)
	_, err = l.Push(ctx, "/repo", false)
	require.NoError(t, err)
	require.Contains(t, fr.calledArgs(), []string{"git", "commit", "-m", "allow autopilot integration branch"})
	require.Contains(t, fr.calledArgs(), []string{"git", "push", "-u", "origin", "autopilot/example"})
}

func TestProtectedBranchesHotReload(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git symbolic-ref --quiet --short refs/remotes/origin/HEAD": {Out: "origin/develop\n"},
	}}
	l := New(fr, &FakeConfig{})
	require.False(t, l.IsProtectedBranchInRepo(context.Background(), "/repo", "release"))
	l.SetConfig(&FakeConfig{ProtectedBranches: []string{"release"}})
	require.True(t, l.IsProtectedBranchInRepo(context.Background(), "/repo", "release"))
}

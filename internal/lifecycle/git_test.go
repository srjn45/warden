package lifecycle

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCommitDirtyTreeReturnsResult(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git rev-parse --abbrev-ref HEAD": {Out: "feature-x\n"},
		"git status --porcelain":          {Out: " M internal/foo.go\n?? new.go\n"},
		"git rev-parse --short HEAD":      {Out: "abc1234\n"},
	}}
	res, err := New(fr, &FakeConfig{}).Commit(context.Background(), "/wt", "do the thing")
	require.NoError(t, err)
	require.True(t, res.Committed)
	require.Equal(t, "abc1234", res.SHA)
	require.Equal(t, "feature-x", res.Branch)
	require.ElementsMatch(t, []string{"internal/foo.go", "new.go"}, res.Files)
	require.Contains(t, fr.calledArgs(), []string{"git", "add", "-A"})
	require.Contains(t, fr.calledArgs(), []string{"git", "commit", "-m", "do the thing"})
}

func TestCommitCleanTreeIsNoOp(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git rev-parse --abbrev-ref HEAD": {Out: "feature-x\n"},
		// status unmatched → "" → clean
	}}
	res, err := New(fr, &FakeConfig{}).Commit(context.Background(), "/wt", "msg")
	require.NoError(t, err)
	require.False(t, res.Committed)
	require.Equal(t, "feature-x", res.Branch)
	require.NotContains(t, fr.calledArgs(), []string{"git", "commit", "-m", "msg"})
}

func TestCommitRefusesProtectedBranch(t *testing.T) {
	for _, b := range []string{"main", "master"} {
		fr := &FakeRunner{Responses: map[string]FakeResp{
			"git rev-parse --abbrev-ref HEAD": {Out: b + "\n"},
		}}
		_, err := New(fr, &FakeConfig{}).Commit(context.Background(), "/wt", "msg")
		require.Error(t, err, "must refuse to commit on %s", b)
		require.NotContains(t, fr.calledArgs(), []string{"git", "add", "-A"}, "no staging when refused")
	}
}

func TestCommitEmptyMessageRejected(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git rev-parse --abbrev-ref HEAD": {Out: "feature-x\n"},
	}}
	_, err := New(fr, &FakeConfig{}).Commit(context.Background(), "/wt", "  ")
	require.Error(t, err)
}

func TestCommitHookFailureIsStructuredNotError(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git rev-parse --abbrev-ref HEAD": {Out: "feature-x\n"},
		"git status --porcelain":          {Out: " M foo.go\n"},
		"git commit -m msg":               {Out: "gofmt found issues\n", Err: errStub("hook exit 1")},
	}}
	res, err := New(fr, &FakeConfig{}).Commit(context.Background(), "/wt", "msg")
	require.NoError(t, err, "a hook rejection is a result, not a transport error")
	require.True(t, res.HookFailed)
	require.False(t, res.Committed)
	require.Contains(t, res.HookOutput, "gofmt")
	require.Contains(t, fr.calledArgs(), []string{"git", "reset"}, "staging is undone after a hook failure")
}

func TestPushRefusesProtectedBranch(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git rev-parse --abbrev-ref HEAD": {Out: "main\n"},
	}}
	_, err := New(fr, &FakeConfig{}).Push(context.Background(), "/wt")
	require.Error(t, err)
	require.NotContains(t, fr.calledArgs(), []string{"git", "push", "-u", "origin", "main"})
}

func TestPushHappyPath(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git rev-parse --abbrev-ref HEAD": {Out: "feature-x\n"},
	}}
	res, err := New(fr, &FakeConfig{}).Push(context.Background(), "/wt")
	require.NoError(t, err)
	require.True(t, res.Pushed)
	require.Equal(t, "feature-x", res.Branch)
	require.Equal(t, "origin", res.Remote)
	require.Contains(t, fr.calledArgs(), []string{"git", "push", "-u", "origin", "feature-x"})
}

func TestSyncRefusesDirtyTree(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git rev-parse --abbrev-ref HEAD": {Out: "feature-x\n"},
		"git status --porcelain":          {Out: " M foo.go\n"},
	}}
	_, err := New(fr, &FakeConfig{}).Sync(context.Background(), "/wt", "main")
	require.Error(t, err)
	require.NotContains(t, fr.calledArgs(), []string{"git", "rebase", "origin/main"})
}

func TestSyncCleanRebase(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git rev-parse --abbrev-ref HEAD": {Out: "feature-x\n"},
		// clean tree, fetch + rebase succeed (unmatched → "")
	}}
	res, err := New(fr, &FakeConfig{}).Sync(context.Background(), "/wt", "")
	require.NoError(t, err)
	require.True(t, res.Updated)
	require.Equal(t, "main", res.Base, "empty base defaults to main")
	require.Empty(t, res.Conflicts)
	require.Contains(t, fr.calledArgs(), []string{"git", "fetch", "origin", "main"})
	require.Contains(t, fr.calledArgs(), []string{"git", "rebase", "origin/main"})
}

func TestSyncConflictLeavesRebaseInProgress(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git rev-parse --abbrev-ref HEAD":      {Out: "feature-x\n"},
		"git rebase origin/main":               {Out: "CONFLICT in foo.go\n", Err: errStub("rebase exit 1")},
		"git diff --name-only --diff-filter=U": {Out: "foo.go\nbar.go\n"},
	}}
	res, err := New(fr, &FakeConfig{}).Sync(context.Background(), "/wt", "main")
	require.NoError(t, err)
	require.False(t, res.Updated)
	require.ElementsMatch(t, []string{"foo.go", "bar.go"}, res.Conflicts)
	require.NotContains(t, fr.calledArgs(), []string{"git", "rebase", "--abort"}, "a conflict leaves the rebase in progress for resolution")
}

func TestSyncNonConflictFailureAborts(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git rev-parse --abbrev-ref HEAD": {Out: "feature-x\n"},
		"git rebase origin/main":          {Out: "fatal: invalid upstream\n", Err: errStub("rebase exit 128")},
		// diff-filter=U unmatched → "" → no conflicts → abort path
	}}
	_, err := New(fr, &FakeConfig{}).Sync(context.Background(), "/wt", "main")
	require.Error(t, err)
	require.Contains(t, fr.calledArgs(), []string{"git", "rebase", "--abort"}, "a non-conflict failure aborts to a clean tree")
}

func TestParsePorcelainPaths(t *testing.T) {
	in := " M a.go\n?? b.go\nR  old.go -> new.go\n\n"
	require.Equal(t, []string{"a.go", "b.go", "new.go"}, parsePorcelainPaths(in))
}

func TestGitConventionsHint(t *testing.T) {
	t.Run("on by default", func(t *testing.T) {
		got := New(&FakeRunner{}, &FakeConfig{}).gitConventionsHint()
		require.Contains(t, got, "--append-system-prompt")
		require.Contains(t, got, "wd commit")
		require.Contains(t, got, "wd check")
		require.True(t, len(got) > 0 && got[0] == ' ', "leading space so it concatenates onto claudeLaunch output")
		require.NotContains(t, got, "\n", "must stay a single typed line")
		require.NotContains(t, gitConventionsGuidance, "'", "guidance must stay apostrophe-free (keeps the single-quoted shell form clean)")
	})
	t.Run("opt-out via config", func(t *testing.T) {
		got := New(&FakeRunner{}, &FakeConfig{GitConventionsOff: true}).gitConventionsHint()
		require.Equal(t, "", got)
	})
}

package lifecycle

import (
	"context"
	"strings"
	"testing"

	"github.com/srajanpathak/agentctl/internal/store"
	"github.com/stretchr/testify/require"
)

const noOtherWorktrees = "worktree /repo\nHEAD abc\nbranch refs/heads/main\n"

func TestSpawnDevelopmentCreatesWorktreeTmuxAndDoc(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git worktree list --porcelain": {Out: noOtherWorktrees},
	}}
	lc := New(fr)
	s, err := lc.Spawn(context.Background(), SpawnRequest{
		Type: store.TypeDevelopment, Ticket: "PROJ-350", Repo: "/repo",
	})
	require.NoError(t, err)
	require.Equal(t, "PROJ-350", s.ID)
	require.Equal(t, store.TypeDevelopment, s.Type)
	require.Equal(t, store.StatusSpawning, s.Status)
	require.Equal(t, ".worktrees/PROJ-350", s.Worktree)
	require.Equal(t, "PROJ-350", s.Branch)

	// Worktree on a new branch.
	require.Contains(t, fr.calledArgs(), []string{"git", "worktree", "add", ".worktrees/PROJ-350", "-b", "PROJ-350"})
	// Detached tmux session in the worktree.
	require.Contains(t, fr.calledArgs(), []string{"tmux", "new-session", "-d", "-s", "PROJ-350", "-c", "/repo/.worktrees/PROJ-350"})
	// Launch claude UNATTENDED.
	require.Contains(t, fr.calledArgs(), []string{"tmux", "send-keys", "-t", "PROJ-350", "claude --dangerously-skip-permissions", "Enter"})
}

func TestSpawnAdoptsExistingWorktree(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git worktree list --porcelain": {Out: noOtherWorktrees + "\nworktree /repo/.worktrees/PROJ-350\nHEAD def\nbranch refs/heads/PROJ-350\n"},
	}}
	lc := New(fr)
	_, err := lc.Spawn(context.Background(), SpawnRequest{
		Type: store.TypeDevelopment, Ticket: "PROJ-350", Repo: "/repo",
	})
	require.NoError(t, err)
	// Adopt: must NOT call `git worktree add` again.
	require.NotContains(t, fr.calledArgs(), []string{"git", "worktree", "add", ".worktrees/PROJ-350", "-b", "PROJ-350"})
	require.Contains(t, fr.calledArgs(), []string{"tmux", "new-session", "-d", "-s", "PROJ-350", "-c", "/repo/.worktrees/PROJ-350"})
}

func TestSpawnNoWorktreeTypeRunsInRepoWithAutoID(t *testing.T) {
	fr := &FakeRunner{}
	lc := New(fr)
	s, err := lc.Spawn(context.Background(), SpawnRequest{Type: store.TypeBuildkiteDebug, Repo: "/repo"})
	require.NoError(t, err)
	require.Empty(t, s.Worktree)
	require.Empty(t, s.Branch)
	require.Empty(t, s.Ticket)
	require.True(t, strings.HasPrefix(s.ID, "buildkitedebug-"), "auto id for no-ticket session, got %q", s.ID)
	// No git calls for a no-worktree type.
	for _, argv := range fr.calledArgs() {
		require.NotEqual(t, "git", argv[0], "no-worktree type must not call git")
	}
	require.Contains(t, fr.calledArgs(), []string{"tmux", "new-session", "-d", "-s", s.ID, "-c", "/repo"})
	require.Contains(t, fr.calledArgs(), []string{"tmux", "send-keys", "-t", s.ID, "claude --dangerously-skip-permissions", "Enter"})
}

func TestSpawnPRReviewChecksOutPR(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git worktree list --porcelain": {Out: noOtherWorktrees},
	}}
	lc := New(fr)
	s, err := lc.Spawn(context.Background(), SpawnRequest{Type: store.TypePRReview, Repo: "/repo", PR: "12345"})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(s.ID, "prreview-"), "got %q", s.ID)
	require.Equal(t, ".worktrees/"+s.ID, s.Worktree)
	require.Equal(t, "12345", s.PR)
	// Detached worktree, then `gh pr checkout` inside it.
	require.Contains(t, fr.calledArgs(), []string{"git", "worktree", "add", "--detach", s.Worktree})
	require.Contains(t, fr.calledArgs(), []string{"gh", "pr", "checkout", "12345"})
}

func TestSpawnSpikeWorktreeIsOptIn(t *testing.T) {
	// Default: no worktree.
	fr := &FakeRunner{}
	s1, err := New(fr).Spawn(context.Background(), SpawnRequest{Type: store.TypeSpike, Repo: "/repo"})
	require.NoError(t, err)
	require.Empty(t, s1.Worktree)

	// --worktree: new-branch worktree like development.
	fr2 := &FakeRunner{Responses: map[string]FakeResp{"git worktree list --porcelain": {Out: noOtherWorktrees}}}
	s2, err := New(fr2).Spawn(context.Background(), SpawnRequest{Type: store.TypeSpike, Repo: "/repo", Worktree: true})
	require.NoError(t, err)
	require.Equal(t, ".worktrees/"+s2.ID, s2.Worktree)
	require.Contains(t, fr2.calledArgs(), []string{"git", "worktree", "add", s2.Worktree, "-b", s2.ID})
}

// calledArgs is a test helper.
func (f *FakeRunner) calledArgs() [][]string {
	out := make([][]string, 0, len(f.Calls))
	for _, c := range f.Calls {
		out = append(out, c.Argv)
	}
	return out
}

func TestCleanupGuardAbortsOnUncommitted(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git -C /repo/.worktrees/A-1 status --porcelain": {Out: " M file.go\n"},
	}}
	lc := New(fr)
	err := lc.Cleanup(context.Background(), cleanupInput("A-1"), false)
	require.ErrorIs(t, err, ErrDirtyWorktree)
	// Guard must run BEFORE worktree removal.
	require.NotContains(t, fr.calledArgs(), []string{"git", "-C", "/repo", "worktree", "remove", ".worktrees/A-1"})
}

func TestCleanupGuardAbortsOnUnpushed(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git -C /repo/.worktrees/A-1 status --porcelain":   {Out: ""},
		"git -C /repo/.worktrees/A-1 log @{u}.. --oneline": {Out: "abc123 wip\n"},
	}}
	lc := New(fr)
	err := lc.Cleanup(context.Background(), cleanupInput("A-1"), false)
	require.ErrorIs(t, err, ErrUnpushedCommits)
}

func TestCleanupForceProceedsAndKillsTmuxFirst(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git -C /repo/.worktrees/A-1 status --porcelain": {Out: " M dirty\n"},
	}}
	lc := New(fr)
	err := lc.Cleanup(context.Background(), cleanupInput("A-1"), true)
	require.NoError(t, err)
	require.Contains(t, fr.calledArgs(), []string{"tmux", "kill-session", "-t", "A-1"})
	require.Contains(t, fr.calledArgs(), []string{"git", "-C", "/repo", "worktree", "remove", "--force", ".worktrees/A-1"})
	require.Contains(t, fr.calledArgs(), []string{"git", "-C", "/repo", "branch", "-D", "A-1"})
}

func TestCleanupCleanProceeds(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git -C /repo/.worktrees/A-1 status --porcelain":   {Out: ""},
		"git -C /repo/.worktrees/A-1 log @{u}.. --oneline": {Out: ""},
	}}
	lc := New(fr)
	require.NoError(t, lc.Cleanup(context.Background(), cleanupInput("A-1"), false))
	require.Contains(t, fr.calledArgs(), []string{"git", "-C", "/repo", "worktree", "remove", ".worktrees/A-1"})
}

func TestCleanupNoWorktreeOnlyKillsTmux(t *testing.T) {
	fr := &FakeRunner{}
	lc := New(fr)
	// A no-worktree session (e.g. buildkite-debug): empty Worktree/Branch.
	tgt := CleanupTarget{ID: "buildkitedebug-a1b2", Repo: "/repo", TmuxSession: "buildkitedebug-a1b2"}
	require.NoError(t, lc.Cleanup(context.Background(), tgt, false))
	require.Contains(t, fr.calledArgs(), []string{"tmux", "kill-session", "-t", "buildkitedebug-a1b2"})
	// No git guard or prune for a session without a worktree.
	for _, argv := range fr.calledArgs() {
		require.NotEqual(t, "git", argv[0], "no-worktree cleanup must not touch git")
	}
}

func cleanupInput(id string) CleanupTarget {
	return CleanupTarget{ID: id, Repo: "/repo", Worktree: ".worktrees/" + id, Branch: id, TmuxSession: id}
}

func TestCleanupGuardAbortsWhenNoUpstream(t *testing.T) {
	// `git log @{u}..` errors when no upstream is configured → treat as unpushed.
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git -C /repo/.worktrees/A-1 status --porcelain":   {Out: ""},
		"git -C /repo/.worktrees/A-1 log @{u}.. --oneline": {Err: errStub("no upstream configured")},
	}}
	err := New(fr).Cleanup(context.Background(), cleanupInput("A-1"), false)
	require.ErrorIs(t, err, ErrUnpushedCommits)
}

func TestSpawnPRReviewWithExplicitBranch(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git worktree list --porcelain": {Out: noOtherWorktrees},
	}}
	s, err := New(fr).Spawn(context.Background(), SpawnRequest{
		Type: store.TypePRReview, Repo: "/repo", PR: "12345", Branch: "feature-x",
	})
	require.NoError(t, err)
	require.Equal(t, "feature-x", s.Branch)
	// Given a branch, pr-review checks it out directly (no detached + gh).
	require.Contains(t, fr.calledArgs(), []string{"git", "worktree", "add", s.Worktree, "feature-x"})
	require.NotContains(t, fr.calledArgs(), []string{"gh", "pr", "checkout", "12345"})
}

func TestInputSendsKeys(t *testing.T) {
	fr := &FakeRunner{}
	lc := New(fr)
	require.NoError(t, lc.Input(context.Background(), "A-1", "what is your status?"))
	require.Contains(t, fr.calledArgs(), []string{"tmux", "send-keys", "-t", "A-1", "--", "what is your status?", "Enter"})
}

func TestOutputCapturesPane(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"tmux capture-pane -p -t A-1 -S -200": {Out: "line1\nline2\n"},
	}}
	lc := New(fr)
	out, err := lc.Output(context.Background(), "A-1", 200)
	require.NoError(t, err)
	require.Equal(t, "line1\nline2\n", out)
}

func TestShellQuoteArg(t *testing.T) {
	require.Equal(t, `'hi there'`, shellQuoteArg("hi there"))
	require.Equal(t, `'a'\''b'`, shellQuoteArg("a'b"))
	require.Equal(t, "'line1\nline2'", shellQuoteArg("line1\nline2"))
}

func TestParseType(t *testing.T) {
	require.Equal(t, store.TypeDevelopment, parseType("development"))
	require.Equal(t, store.TypePRReview, parseType("pr-review\n"))
	require.Equal(t, store.TypeAnalysis, parseType("This is an analysis task."))
	require.Equal(t, store.TypeBuildkiteDebug, parseType("Label: buildkite-debug"))
	require.Equal(t, store.TypeOther, parseType("I am not sure"))
	require.Equal(t, store.TypeOther, parseType(""))
}

func TestClassifyCallsClaudeP(t *testing.T) {
	prompt := "build a REST API for orders"
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"claude -p " + classifyArg(prompt): {Out: "development\n"},
	}}
	got, err := New(fr).Classify(context.Background(), prompt)
	require.NoError(t, err)
	require.Equal(t, store.TypeDevelopment, got)
	require.Contains(t, fr.calledArgs(), []string{"claude", "-p", classifyArg(prompt)})
}

func TestClassifyDefaultsToOtherOnError(t *testing.T) {
	prompt := "whatever"
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"claude -p " + classifyArg(prompt): {Err: errStub("claude not found")},
	}}
	got, err := New(fr).Classify(context.Background(), prompt)
	require.Error(t, err)
	require.Equal(t, store.TypeOther, got)
}

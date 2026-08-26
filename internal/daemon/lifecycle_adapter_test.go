package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/srjn45/warden/internal/agentbackend/backends" // register codex for fork resolution
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// TestAdapterInteractiveSpawnStaysFreeForm guards the wire-DTO → lifecycle
// translation for an empty-prompt (interactive) spawn. The adapter must NOT
// normalize an empty Type: NormalizeType("") collapses to "other", which would
// flip lifecycle.Spawn off its free-form cwd-launch path onto the typed/managed
// path (requiring a repo + worktree) and break interactive spawn. This path is
// not covered by the route tests (they use fakeLife) or the lifecycle tests
// (they call Spawn directly), so the adapter glue is exercised here.
func TestAdapterInteractiveSpawnStaysFreeForm(t *testing.T) {
	lc := lifecycle.New(&lifecycle.FakeRunner{}, &lifecycle.FakeConfig{})
	a := NewLifecycleAdapter(lc, newFakeStore())

	sess, err := a.Spawn(context.Background(), SpawnRequest{Cwd: t.TempDir()})
	require.NoError(t, err, "empty-prompt free-form spawn must launch in cwd, not require a repo")
	require.Equal(t, store.Type(""), sess.Type, "interactive spawn stays untyped (classifying), not 'other'")
	require.Equal(t, "interactive", sess.Subject)
}

// TestAdapterTypedSpawnNormalizes confirms the typed path still normalizes an
// unknown type down to empty.
func TestAdapterTypedSpawnNormalizes(t *testing.T) {
	lc := lifecycle.New(&lifecycle.FakeRunner{}, &lifecycle.FakeConfig{})
	a := NewLifecycleAdapter(lc, newFakeStore())

	sess, err := a.Spawn(context.Background(), SpawnRequest{Type: "bogus", Repo: t.TempDir(), Cwd: "/work"})
	require.NoError(t, err)
	require.Equal(t, store.Type(""), sess.Type, "unknown type collapses to empty")
}

// newForkAdapter wires a real codex-capable lifecycle (FakeRunner reporting no
// pre-existing worktree) behind the adapter, with a fakeStore holding one pinned
// source codex agent. It returns the adapter and runner so a fork test can inspect
// the git/tmux argv the resolved fork produced.
func newForkAdapter(t *testing.T, src *store.Session) (Lifecycle, *lifecycle.FakeRunner) {
	t.Helper()
	fr := &lifecycle.FakeRunner{Responses: map[string]lifecycle.FakeResp{
		"git worktree list --porcelain": {Out: "worktree /repo\nHEAD abc\nbranch refs/heads/main\n"},
	}}
	lc := lifecycle.New(fr, &lifecycle.FakeConfig{})
	lc.PromptsDir = "/state/prompts"
	st := newFakeStore()
	if src != nil {
		require.NoError(t, st.Insert(context.Background(), src))
	}
	return NewLifecycleAdapter(lc, st), fr
}

// TestAdapterForkResolvesSource is the daemon-side fork seam: the adapter (which owns
// the store) resolves the source agent ONCE and hands lifecycle the read-back values
// (pinned session id → the launch line; branch → the worktree base; repo → where the
// sibling worktree lives), keeping lifecycle store-free.
func TestAdapterForkResolvesSource(t *testing.T) {
	src := &store.Session{
		ID: "src-agent", Backend: "codex", Repo: "/repo",
		Branch: "src-branch", ClaudeSessionID: "11111111-2222-3333-4444-555555555555",
	}
	a, fr := newForkAdapter(t, src)

	_, err := a.Spawn(context.Background(), SpawnRequest{
		Type: "development", Ticket: "fork-1", Backend: "codex", ForkFrom: "src-agent",
	})
	require.NoError(t, err)

	var sawWorktree, sawLaunch bool
	for _, c := range fr.Calls {
		argv := c.Argv
		if len(argv) == 7 && argv[0] == "git" && argv[1] == "worktree" && argv[2] == "add" &&
			argv[5] == "fork-1" && argv[6] == "src-branch" {
			sawWorktree = true
		}
		if len(argv) >= 6 && argv[0] == "tmux" && argv[1] == "send-keys" && argv[3] == "fork-1" &&
			strings.Contains(argv[4], "codex fork '11111111-2222-3333-4444-555555555555'") {
			sawLaunch = true
		}
	}
	require.True(t, sawWorktree, "fork worktree based off the source's branch")
	require.True(t, sawLaunch, "launch forks the source's pinned session id")
}

// TestAdapterForkThreadsWorkdirAndBackend proves PR-2's additions to the daemon seam:
// the adapter hands lifecycle the source's WORKDIR (the read-side of the dirty-tree
// carry — `git stash create` in the source, applied into the fork) and pins the fork
// to the source's BACKEND, so the SessionForker that minted the session is the one
// that branches it and the wrappers needn't restate --backend (the request below
// carries none).
func TestAdapterForkThreadsWorkdirAndBackend(t *testing.T) {
	src := &store.Session{
		ID: "src-agent", Backend: "codex", Repo: "/repo", Workdir: "/repo/.worktrees/src-agent",
		Branch: "src-branch", ClaudeSessionID: "11111111-2222-3333-4444-555555555555",
	}
	a, fr := newForkAdapter(t, src)
	fr.Responses["git stash create warden fork dirty-carry"] = lifecycle.FakeResp{Out: "stashsha\n"}

	_, err := a.Spawn(context.Background(), SpawnRequest{
		Type: "development", Ticket: "fork-1", ForkFrom: "src-agent", // no Backend on the request
	})
	require.NoError(t, err)

	var sawStashCreate, sawStashApply, sawCodexFork bool
	for _, c := range fr.Calls {
		if c.Dir == "/repo/.worktrees/src-agent" && len(c.Argv) >= 3 &&
			c.Argv[0] == "git" && c.Argv[1] == "stash" && c.Argv[2] == "create" {
			sawStashCreate = true
		}
		if c.Dir == "/repo/.worktrees/fork-1" && len(c.Argv) >= 4 &&
			c.Argv[1] == "stash" && c.Argv[2] == "apply" && c.Argv[3] == "stashsha" {
			sawStashApply = true
		}
		if len(c.Argv) >= 5 && c.Argv[0] == "tmux" && c.Argv[1] == "send-keys" &&
			strings.Contains(c.Argv[4], "codex fork") {
			sawCodexFork = true
		}
	}
	require.True(t, sawStashCreate, "carry builds a stash from the source workdir")
	require.True(t, sawStashApply, "carry applies the source stash into the fork worktree")
	require.True(t, sawCodexFork, "fork inherits the source's codex backend even with no --backend on the request")
}

// TestAdapterForkSourceNotPinned proves the §5 guard: a source whose backend session
// id is not yet discovered → ErrForkSourceNotPinned, before any spawn side effects.
func TestAdapterForkSourceNotPinned(t *testing.T) {
	src := &store.Session{ID: "src-agent", Backend: "codex", Repo: "/repo", Branch: "src-branch"} // ClaudeSessionID ""
	a, _ := newForkAdapter(t, src)

	_, err := a.Spawn(context.Background(), SpawnRequest{
		Type: "development", Ticket: "fork-1", Backend: "codex", ForkFrom: "src-agent",
	})
	require.ErrorIs(t, err, lifecycle.ErrForkSourceNotPinned)
}

// TestAdapterForkSourceMissing surfaces a clean store error when fork_from names an
// agent that does not exist.
func TestAdapterForkSourceMissing(t *testing.T) {
	a, _ := newForkAdapter(t, nil)
	_, err := a.Spawn(context.Background(), SpawnRequest{
		Type: "development", Ticket: "fork-1", Backend: "codex", ForkFrom: "ghost",
	})
	require.Error(t, err)
	require.True(t, errors.Is(err, store.ErrNotFound) || strings.Contains(err.Error(), "not found"))
}

// TestAdapterForkSourceNoBranch rejects a fork whose source has no branch to base the
// sibling worktree on (§7 requires basing off the source branch HEAD).
func TestAdapterForkSourceNoBranch(t *testing.T) {
	src := &store.Session{ID: "src-agent", Backend: "codex", Repo: "/repo", ClaudeSessionID: "uuid"} // no Branch
	a, _ := newForkAdapter(t, src)
	_, err := a.Spawn(context.Background(), SpawnRequest{
		Type: "development", Ticket: "fork-1", Backend: "codex", ForkFrom: "src-agent",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no branch")
}

func TestAdapterHotSwap(t *testing.T) {
	workdir := t.TempDir()
	sess := &store.Session{
		ID:          "swap-agent",
		TmuxSession: "swap-agent",
		Backend:     "claude",
		Model:       "opus",
		Repo:        workdir,
		Workdir:     workdir,
		Branch:      "feat/swap",
		Worktree:    ".worktrees/swap-agent",
	}

	st := newFakeStore()
	require.NoError(t, st.Insert(context.Background(), sess))

	fr := &lifecycle.FakeRunner{Responses: map[string]lifecycle.FakeResp{}}
	lc := lifecycle.New(fr, &lifecycle.FakeConfig{})
	lc.ProjectsDir = t.TempDir()
	lc.PromptsDir = filepath.Join(t.TempDir(), "prompts")

	a := NewLifecycleAdapter(lc, st)

	res, err := a.HotSwap(context.Background(), sess, lifecycle.SwapRequest{
		Backend: "codex",
		Model:   "gpt-5-codex",
		Reason:  lifecycle.SwapReasonManual,
	})
	require.NoError(t, err)
	require.Equal(t, "codex", res.ToBackend)
	require.Equal(t, "gpt-5-codex", res.ToModel)
	require.Equal(t, "codex", sess.Backend)

	// Verify store was updated
	stored, err := st.Get(context.Background(), "swap-agent")
	require.NoError(t, err)
	require.Equal(t, "codex", stored.Backend)
	require.Equal(t, "gpt-5-codex", stored.Model)
}

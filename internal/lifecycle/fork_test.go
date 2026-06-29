package lifecycle

import (
	"context"
	"strings"
	"testing"

	_ "github.com/srjn45/warden/internal/agentbackend/backends" // register codex + claude
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// newForkLC builds a Lifecycle whose FakeRunner reports no pre-existing worktree, so
// ensureWorktree takes the create path. It returns the runner so tests can inspect
// the exact git/tmux argv the fork produced.
func newForkLC() (*Lifecycle, *FakeRunner) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git worktree list --porcelain": {Out: noOtherWorktrees},
	}}
	lc := New(fr, &FakeConfig{})
	lc.PromptsDir = "/state/prompts"
	return lc, fr
}

// TestSpawnForkCodexWorktreeBaseAndLaunch is the core fork-branch test: a codex fork
// (the only SessionForker backend) must (1) create its worktree as a fresh sibling
// based off the SOURCE agent's branch HEAD (§7) — `git worktree add <rel> -b <id>
// <source-branch>` — and (2) launch via `codex fork <explicit source uuid>` (never
// --last, §4.3), with the resolved source session id quoted into the pane.
func TestSpawnForkCodexWorktreeBaseAndLaunch(t *testing.T) {
	lc, fr := newForkLC()
	s, err := lc.Spawn(context.Background(), SpawnRequest{
		Type: store.TypeDevelopment, Ticket: "fork-1", Repo: "/repo", Backend: "codex",
		Model:               "qwen2.5-coder:3b",
		ForkFrom:            "src-agent",
		ForkSourceSessionID: "11111111-2222-3333-4444-555555555555",
		ForkSourceBranch:    "src-branch",
	})
	require.NoError(t, err)
	require.Equal(t, "codex", s.Backend)

	// Worktree is a sibling off the SOURCE branch: the start point is appended.
	require.Contains(t, fr.calledArgs(),
		[]string{"git", "worktree", "add", ".worktrees/fork-1", "-b", "fork-1", "src-branch"},
		"fork worktree is based off the source agent's branch HEAD")

	// Launch line forks the explicit source uuid, never --last, and pins -C to the
	// fork's OWN worktree so codex skips the cross-cwd working-dir picker (§8.4).
	launch := forkLaunchLine(t, fr, "fork-1")
	require.Contains(t, launch, "codex fork '11111111-2222-3333-4444-555555555555'",
		"fork launches with the explicit, quoted source session id")
	require.Contains(t, launch, "-C '/repo/.worktrees/fork-1'",
		"fork pins -C to its own worktree to suppress codex's working-dir picker")
	require.NotContains(t, launch, "--last", "fork must use the explicit id, not cwd-scoped --last")
}

// TestSpawnNonForkCodexUnchanged is the byte-identical regression-lock for the
// non-fork path: a normal codex spawn (no fork fields) bases its worktree off the
// repo default (NO start point) and launches plain `codex`, not `codex fork`.
func TestSpawnNonForkCodexUnchanged(t *testing.T) {
	lc, fr := newForkLC()
	_, err := lc.Spawn(context.Background(), SpawnRequest{
		Type: store.TypeDevelopment, Ticket: "plain-1", Repo: "/repo", Backend: "codex",
		Model: "qwen2.5-coder:3b",
	})
	require.NoError(t, err)

	// No start point on the worktree add — exactly the pre-fork argv.
	require.Contains(t, fr.calledArgs(),
		[]string{"git", "worktree", "add", ".worktrees/plain-1", "-b", "plain-1"},
		"a normal spawn appends no source-branch start point")

	launch := forkLaunchLine(t, fr, "plain-1")
	require.NotContains(t, launch, "codex fork", "a normal spawn launches plain codex")
	require.True(t, strings.HasPrefix(launch, "codex "), "launch is a plain codex invocation")
}

// TestSpawnForkNonForkerBackendCannotFork is the Claude regression-lock at the
// lifecycle seam: a fork_from spawn against a backend that does NOT implement
// SessionForker (Claude, the default) degrades to the clean "cannot fork" error from
// buildLaunch — the fork path never launches a bare agent.
func TestSpawnForkNonForkerBackendCannotFork(t *testing.T) {
	lc, _ := newForkLC()
	_, err := lc.Spawn(context.Background(), SpawnRequest{
		Type: store.TypeDevelopment, Ticket: "fork-claude", Repo: "/repo", // Backend "" = claude
		ForkFrom:            "src-agent",
		ForkSourceSessionID: "src-uuid",
		ForkSourceBranch:    "src-branch",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot fork a session")
	require.Contains(t, err.Error(), "claude")
}

// TestSpawnForkFreeFormRejected guards the free-form path: a fork has no worktree in
// free-form mode, which would break dir-scoped discover-then-pin (§5), so it is
// rejected (free-form fork is deferred per design §4.2).
func TestSpawnForkFreeFormRejected(t *testing.T) {
	lc, _ := newForkLC()
	_, err := lc.Spawn(context.Background(), SpawnRequest{
		Cwd: t.TempDir(), Backend: "codex",
		ForkFrom:            "src-agent",
		ForkSourceSessionID: "src-uuid",
		ForkSourceBranch:    "src-branch",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "free-form fork is not supported")
}

// TestSpawnForkRequiresOwnWorktree guards the §5/§7 correctness requirement: a fork
// whose type would NOT get its own worktree (e.g. the free-form catch-all "other")
// is rejected rather than sharing a tree and mis-pinning both ends.
func TestSpawnForkRequiresOwnWorktree(t *testing.T) {
	lc, _ := newForkLC()
	_, err := lc.Spawn(context.Background(), SpawnRequest{
		Type: store.TypeOther, Ticket: "fork-other", Repo: "/repo", Backend: "codex",
		ForkFrom:            "src-agent",
		ForkSourceSessionID: "src-uuid",
		ForkSourceBranch:    "src-branch",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "fork requires its own worktree")
}

// TestSpawnForkDirtyCarryAppliesStashIntoFork is the PR-2 dirty-tree-carry test: a
// fork whose source has UNCOMMITTED tracked changes carries them into the fork
// worktree via the non-destructive stash primitive — `git stash create` in the SOURCE
// (which leaves it untouched) returns a commit sha, then `git stash apply <sha>` runs
// in the FORK worktree (§7).
func TestSpawnForkDirtyCarryAppliesStashIntoFork(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git worktree list --porcelain":            {Out: noOtherWorktrees},
		"git stash create warden fork dirty-carry": {Out: "deadbeefcafe\n"},
	}}
	lc := New(fr, &FakeConfig{})
	lc.PromptsDir = "/state/prompts"
	_, err := lc.Spawn(context.Background(), SpawnRequest{
		Type: store.TypeDevelopment, Ticket: "fork-1", Repo: "/repo", Backend: "codex",
		Model:               "qwen2.5-coder:3b",
		ForkFrom:            "src-agent",
		ForkSourceSessionID: "11111111-2222-3333-4444-555555555555",
		ForkSourceBranch:    "src-branch",
		ForkSourceWorkdir:   "/repo/.worktrees/src-agent",
	})
	require.NoError(t, err)

	// The stash is BUILT in the source worktree (non-destructive: a commit object, the
	// tree untouched).
	require.True(t, forkCalledInDir(fr, "/repo/.worktrees/src-agent",
		[]string{"git", "stash", "create", "warden fork dirty-carry"}),
		"dirty-carry builds a stash commit in the source worktree")
	// ...and APPLIED into the fork worktree with that sha.
	require.True(t, forkCalledInDir(fr, "/repo/.worktrees/fork-1",
		[]string{"git", "stash", "apply", "deadbeefcafe"}),
		"dirty-carry applies the source stash into the fork worktree")
	// The source tree is otherwise untouched: the ONLY command run against it is the
	// non-destructive create (no stash push / reset / checkout that would perturb it).
	for _, c := range fr.Calls {
		if c.Dir == "/repo/.worktrees/src-agent" {
			require.Equal(t, []string{"git", "stash", "create", "warden fork dirty-carry"}, c.Argv,
				"the only command run in the source worktree is the non-destructive stash create")
		}
	}
}

// TestSpawnForkDirtyCarryCleanSourceNoApply: a CLEAN source tree makes `git stash
// create` return empty — there is nothing to carry, so NO `git stash apply` runs and
// the fork stays HEAD-only (the PR-1 behavior).
func TestSpawnForkDirtyCarryCleanSourceNoApply(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git worktree list --porcelain": {Out: noOtherWorktrees},
		// `git stash create` returns "" (clean tree) via the default empty response.
	}}
	lc := New(fr, &FakeConfig{})
	lc.PromptsDir = "/state/prompts"
	_, err := lc.Spawn(context.Background(), SpawnRequest{
		Type: store.TypeDevelopment, Ticket: "fork-1", Repo: "/repo", Backend: "codex",
		Model:               "qwen2.5-coder:3b",
		ForkFrom:            "src-agent",
		ForkSourceSessionID: "11111111-2222-3333-4444-555555555555",
		ForkSourceBranch:    "src-branch",
		ForkSourceWorkdir:   "/repo/.worktrees/src-agent",
	})
	require.NoError(t, err)

	// create still runs (to learn the tree is clean)…
	require.True(t, forkCalledInDir(fr, "/repo/.worktrees/src-agent",
		[]string{"git", "stash", "create", "warden fork dirty-carry"}),
		"dirty-carry always probes the source with a non-destructive stash create")
	// …but a clean tree carries nothing, so no apply.
	for _, c := range fr.Calls {
		require.False(t, len(c.Argv) >= 3 && c.Argv[0] == "git" && c.Argv[1] == "stash" && c.Argv[2] == "apply",
			"a clean source carries nothing — no stash apply")
	}
}

// TestSpawnForkNoCarryWhenSourceWorkdirUnset locks the carry-off path: with
// ForkSourceWorkdir empty (HEAD-only), NO stash command runs at all — byte-identical
// to PR-1's HEAD-only fork.
func TestSpawnForkNoCarryWhenSourceWorkdirUnset(t *testing.T) {
	lc, fr := newForkLC()
	_, err := lc.Spawn(context.Background(), SpawnRequest{
		Type: store.TypeDevelopment, Ticket: "fork-1", Repo: "/repo", Backend: "codex",
		Model:               "qwen2.5-coder:3b",
		ForkFrom:            "src-agent",
		ForkSourceSessionID: "11111111-2222-3333-4444-555555555555",
		ForkSourceBranch:    "src-branch",
		// ForkSourceWorkdir intentionally empty → carry off.
	})
	require.NoError(t, err)
	for _, c := range fr.Calls {
		require.False(t, len(c.Argv) >= 2 && c.Argv[0] == "git" && c.Argv[1] == "stash",
			"no stash command when dirty-carry is off (HEAD-only fork)")
	}
}

// forkCalledInDir reports whether the runner saw an exact git/tmux argv run in dir.
func forkCalledInDir(fr *FakeRunner, dir string, argv []string) bool {
	for _, c := range fr.Calls {
		if c.Dir != dir || len(c.Argv) != len(argv) {
			continue
		}
		match := true
		for i := range argv {
			if c.Argv[i] != argv[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// forkLaunchLine returns the command string typed into the agent's tmux pane (the
// send-keys payload), failing the test if no launch was sent.
func forkLaunchLine(t *testing.T, fr *FakeRunner, id string) string {
	t.Helper()
	for _, c := range fr.Calls {
		if len(c.Argv) >= 6 && c.Argv[0] == "tmux" && c.Argv[1] == "send-keys" && c.Argv[3] == id {
			return c.Argv[4]
		}
	}
	t.Fatalf("no tmux send-keys launch recorded for %q", id)
	return ""
}

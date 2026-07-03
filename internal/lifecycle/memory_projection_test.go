package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/memory"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// sessionIDRE normalizes the per-spawn random --session-id UUID so two spawns'
// launch lines can be compared for byte-identity of everything else.
var sessionIDRE = regexp.MustCompile(`--session-id '[^']*'`)

func normSessionID(launch string) string {
	return sessionIDRE.ReplaceAllString(launch, "--session-id 'X'")
}

// memStoreWith returns a memory.Store rooted at a fresh temp repo carrying the given
// .warden/memory.md body — the hermetic stand-in for the git-shelling default so the
// projection path needs no real repo. An empty body still writes the file (to exercise
// the "present but renders nothing" case); pass writeFile=false via memStoreEmpty for
// the absent-file case.
func memStoreWith(t *testing.T, body string) *memory.Store {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".warden"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".warden", "memory.md"), []byte(body), 0o644))
	return &memory.Store{RepoRoot: func(context.Context, string) (string, error) { return dir, nil }}
}

// memStoreAbsent roots at a temp dir with NO memory.md — Locate succeeds, the read
// misses, projection renders "".
func memStoreAbsent(t *testing.T) *memory.Store {
	t.Helper()
	dir := t.TempDir()
	return &memory.Store{RepoRoot: func(context.Context, string) (string, error) { return dir, nil }}
}

// findLaunch returns the launch string from the tmux send-keys call for id.
func findLaunch(t *testing.T, fr *FakeRunner, id string) string {
	t.Helper()
	for _, a := range fr.calledArgs() {
		if len(a) == 6 && a[0] == "tmux" && a[1] == "send-keys" && a[3] == id {
			return a[4]
		}
	}
	t.Fatalf("no tmux send-keys launch found for %q", id)
	return ""
}

const memEntry = "- The daemon API is spec-first: edit openapi.yaml then run make generate."

// TestMemoryGuidanceRenders exercises the guidance-assembly gate directly: a curated
// entry renders (header + fact); disabled, empty, and absent all render "".
func TestMemoryGuidanceRenders(t *testing.T) {
	ctx := context.Background()

	on := New(&FakeRunner{}, &FakeConfig{})
	on.MemStore = memStoreWith(t, memEntry+"\n")
	got := on.memoryGuidance(ctx, "/repo/.worktrees/x")
	require.Contains(t, got, "warden project memory")
	require.Contains(t, got, "daemon API is spec-first")

	off := New(&FakeRunner{}, &FakeConfig{MemoryInjectOff: true})
	off.MemStore = memStoreWith(t, memEntry+"\n")
	require.Equal(t, "", off.memoryGuidance(ctx, "/repo/x"), "disabled projects nothing")

	empty := New(&FakeRunner{}, &FakeConfig{})
	empty.MemStore = memStoreWith(t, "<!-- header only, no entries -->\n")
	require.Equal(t, "", empty.memoryGuidance(ctx, "/repo/x"), "header-only file projects nothing")

	absent := New(&FakeRunner{}, &FakeConfig{})
	absent.MemStore = memStoreAbsent(t)
	require.Equal(t, "", absent.memoryGuidance(ctx, "/repo/x"), "absent file projects nothing")
}

// TestSpawnProjectsMemoryIntoClaudeLaunch proves the core win: a curated
// .warden/memory.md rides Claude's --append-system-prompt seam into the launch.
func TestSpawnProjectsMemoryIntoClaudeLaunch(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git worktree list --porcelain": {Out: noOtherWorktrees},
	}}
	lc := New(fr, &FakeConfig{})
	lc.MemStore = memStoreWith(t, memEntry+"\n")
	s, err := lc.Spawn(context.Background(), SpawnRequest{
		Type: store.TypeDevelopment, Ticket: "PROJ-350", Repo: "/repo",
	})
	require.NoError(t, err)

	launch := findLaunch(t, fr, s.ID)
	require.Contains(t, launch, "warden project memory", "memory header rides the launch")
	require.Contains(t, launch, "daemon API is spec-first", "memory fact rides the launch")
	require.Contains(t, launch, "--append-system-prompt", "memory rides Claude's flag seam")
}

// TestSpawnFileBacksMemory verifies memory rides the SAME file-backed hints path as
// its siblings when a HintsDir is set — so the ~KB of memory text lands in the hints
// FILE, not inline on the tmux launch line (the 1024-byte macOS MAX_CANON ceiling).
func TestSpawnFileBacksMemory(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git worktree list --porcelain": {Out: noOtherWorktrees},
	}}
	lc := New(fr, &FakeConfig{})
	lc.HintsDir = "/state/hints"
	lc.MemStore = memStoreWith(t, memEntry+"\n")
	s, err := lc.Spawn(context.Background(), SpawnRequest{
		Type: store.TypeDevelopment, Ticket: "PROJ-350", Repo: "/repo",
	})
	require.NoError(t, err)

	launch := findLaunch(t, fr, s.ID)
	require.NotContains(t, launch, "daemon API is spec-first", "memory text must not inline on the launch line")
	require.Less(t, len(launch), 1024, "file-backed launch line must fit the tty line limit")

	// The memory text is in the per-agent hints file, appended after the sibling hints.
	hintFile := "/state/hints/" + s.ID
	var wrote string
	for _, a := range fr.calledArgs() {
		if len(a) == 6 && a[0] == "sh" && a[5] == hintFile {
			wrote = a[4]
		}
	}
	require.Contains(t, wrote, "daemon API is spec-first", "memory reached the file-backed hints file")
}

// TestSpawnClaudeByteIdenticalWhenMemoryOffOrEmpty is the LOAD-BEARING regression-lock:
// with memory_inject off, or an empty/absent .warden/memory.md, Claude's launch is
// byte-identical to today's (pipeline+collab+git hints only, no memory fragment).
func TestSpawnClaudeByteIdenticalWhenMemoryOffOrEmpty(t *testing.T) {
	spawn := func(cfg *FakeConfig, ms *memory.Store) string {
		fr := &FakeRunner{Responses: map[string]FakeResp{
			"git worktree list --porcelain": {Out: noOtherWorktrees},
		}}
		lc := New(fr, cfg)
		if ms != nil {
			lc.MemStore = ms
		}
		s, err := lc.Spawn(context.Background(), SpawnRequest{
			Type: store.TypeDevelopment, Ticket: "PROJ-350", Repo: "/repo",
		})
		require.NoError(t, err)
		return findLaunch(t, fr, s.ID)
	}

	// The golden "today" launch: exactly the three existing hints, no memory.
	// (Session-id is per-spawn random, so normalize it before comparing.)
	prefix := "claude --model 'claude-sonnet-4-6' --permission-mode 'auto' --session-id '"
	baseline := spawn(&FakeConfig{}, memStoreAbsent(t))
	require.True(t, strings.HasPrefix(baseline, prefix))
	require.NotContains(t, baseline, "warden project memory")
	baseNorm := normSessionID(baseline)

	// memory_inject OFF (even with a populated file) ⇒ identical to baseline.
	off := spawn(&FakeConfig{MemoryInjectOff: true}, memStoreWith(t, memEntry+"\n"))
	require.Equal(t, baseNorm, normSessionID(off), "memory off must be byte-identical to today")

	// memory_inject ON but empty file ⇒ identical to baseline.
	empty := spawn(&FakeConfig{}, memStoreWith(t, "<!-- header only -->\n"))
	require.Equal(t, baseNorm, normSessionID(empty), "empty memory must be byte-identical to today")
}

// TestMemoryFlowsThroughFileDropForCodex proves the file-drop half of the matrix: a
// non-flag backend (Codex) receives the SAME memory text in its AGENTS.md warden
// block, while a backend implementing neither seam (Aider) degrade-skips it.
func TestMemoryFlowsThroughFileDropForCodex(t *testing.T) {
	ctx := context.Background()
	lc := New(&FakeRunner{}, &FakeConfig{})
	lc.MemStore = memStoreWith(t, memEntry+"\n")
	mem := lc.memoryGuidance(ctx, "/anywhere")
	require.NotEmpty(t, mem)

	codex, err := agentbackend.Get("codex")
	require.NoError(t, err)
	dir := t.TempDir()
	require.NoError(t, lc.injectContext(codex, dir, mem))
	got, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	require.NoError(t, err)
	require.Contains(t, string(got), "<!-- warden:begin -->")
	require.Contains(t, string(got), "daemon API is spec-first", "memory reached the codex rules file")

	// Aider implements neither seam ⇒ memory is dropped (no flag, no file).
	aider, err := agentbackend.Get("aider")
	require.NoError(t, err)
	require.Equal(t, "", systemPromptHint(aider, true, mem), "aider degrade-skips the flag seam")
	aiderDir := t.TempDir()
	require.NoError(t, lc.injectContext(aider, aiderDir, mem))
	_, err = os.Stat(filepath.Join(aiderDir, "AGENTS.md"))
	require.True(t, os.IsNotExist(err), "aider writes no rules file")
}

// TestMemoryProjectionRespectsRenderBudget verifies the §4.3 budget flows through:
// an over-budget memory is trimmed to <= DefaultBudget before it ever reaches a
// launch, and the trim note is emitted.
func TestMemoryProjectionRespectsRenderBudget(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 400; i++ {
		b.WriteString("- durable navigational fact number ")
		b.WriteString(strings.Repeat("x", 40))
		b.WriteByte('\n')
	}
	lc := New(&FakeRunner{}, &FakeConfig{})
	lc.MemStore = memStoreWith(t, b.String())
	got := lc.memoryGuidance(context.Background(), "/repo/x")
	require.NotEmpty(t, got)
	require.LessOrEqual(t, len(got), memory.DefaultBudget, "projection must fit the render budget")
	require.Less(t, strings.Count(got, "- durable navigational fact"), 400, "over-budget memory is trimmed, not projected whole")
}

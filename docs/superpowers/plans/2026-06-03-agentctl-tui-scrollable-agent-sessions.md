# Scroll-Native Agent tmux Sessions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make cockpit-TUI agent output scrollable by creating agent tmux sessions with `mouse on` and a raised (only-raise) global `history-limit`.

**Architecture:** Consolidate the three `new-session` call sites in `internal/lifecycle/lifecycle.go` behind one `newAgentSession` helper. The helper ensures the global `history-limit` is ≥ 50000 *before* `new-session` (tmux fixes a pane's history at creation), then sets `mouse on` on the session *after* creation. Option-setting failures are non-fatal; only `new-session` failure aborts the spawn.

**Tech Stack:** Go, tmux CLI, `internal/lifecycle` `Runner` seam + `FakeRunner` test double, testify (`require`).

---

## File Structure

- **Modify:** `internal/lifecycle/lifecycle.go`
  - Add `strconv` import.
  - Add `const agentHistoryLimit = 50000`.
  - Add `newAgentSession(ctx, runDir, id, cwd string) error` and `ensureScrollback(ctx) `.
  - Replace inline `new-session` at lines 450, 473, 485 with `newAgentSession` calls.
- **Modify (tests):** `internal/lifecycle/lifecycle_test.go`
  - Add a `callIndex` helper and the new test functions.

No new files. The two helpers live with the other tmux orchestration in `lifecycle.go`, following the existing pattern.

---

## Task 1: Add the helpers (mouse on + only-raise history-limit)

**Files:**
- Modify: `internal/lifecycle/lifecycle.go` (add import, const, two methods)
- Test: `internal/lifecycle/lifecycle_test.go`

- [ ] **Step 1: Add a test-helper for call ordering**

Add to `internal/lifecycle/lifecycle_test.go` (next to `calledArgs`):

```go
// callIndex returns the index of the first recorded call whose argv joins to
// key (space-separated), or -1 if absent. Used for ordering assertions.
func (f *FakeRunner) callIndex(key string) int {
	for i, c := range f.Calls {
		if strings.Join(c.Argv, " ") == key {
			return i
		}
	}
	return -1
}
```

- [ ] **Step 2: Write the failing test (mouse on, typed no-worktree path)**

Add to `internal/lifecycle/lifecycle_test.go`:

```go
func TestSpawnSetsMouseOnAgentSession(t *testing.T) {
	fr := &FakeRunner{}
	s, err := New(fr).Spawn(context.Background(), SpawnRequest{Type: store.TypeBuildkiteDebug, Repo: "/repo"})
	require.NoError(t, err)
	// mouse on lets the wheel drive copy-mode in the (nested) cockpit detail pane.
	require.Contains(t, fr.calledArgs(), []string{"tmux", "set-option", "-t", s.ID, "mouse", "on"})
	// ...and it is set after the session exists.
	require.Greater(t,
		fr.callIndex("tmux set-option -t "+s.ID+" mouse on"),
		fr.callIndex("tmux new-session -d -s "+s.ID+" -c /repo"),
		"mouse on must be set after new-session")
}
```

- [ ] **Step 3: Run it; verify it fails**

Run: `go test ./internal/lifecycle/ -run TestSpawnSetsMouseOnAgentSession -v`
Expected: FAIL — `set-option ... mouse on` is not among the recorded calls.

- [ ] **Step 4: Add the const and helpers**

In `internal/lifecycle/lifecycle.go`, add `"strconv"` to the import block (it already imports `"strings"`, `"fmt"`, `"context"`). Then add, near the other tmux orchestration helpers:

```go
// agentHistoryLimit is the scrollback depth (lines) agent panes get, so long
// agent output can be scrolled back to in the cockpit detail pane. tmux fixes a
// pane's history at creation, so ensureScrollback raises the global option
// before new-session.
const agentHistoryLimit = 50000

// newAgentSession creates the detached tmux session for an agent in cwd and
// applies scroll-friendly options. Only new-session failing aborts the spawn;
// option-setting failures are non-fatal so a tmux quirk never blocks a launch.
func (l *Lifecycle) newAgentSession(ctx context.Context, runDir, id, cwd string) error {
	l.ensureScrollback(ctx) // before new-session: the new pane inherits the limit
	if out, err := l.run.Run(ctx, runDir, "tmux", "new-session", "-d", "-s", id, "-c", cwd); err != nil {
		return fmt.Errorf("tmux new-session: %w: %s", err, out)
	}
	// mouse is a live session option: the wheel enters copy-mode, and the cockpit
	// session can forward the wheel into this nested attach. Non-fatal.
	_, _ = l.run.Run(ctx, "", "tmux", "set-option", "-t", id, "mouse", "on")
	return nil
}

// ensureScrollback raises the global tmux history-limit to agentHistoryLimit
// when it is currently lower (only-raise: a user-configured larger value is left
// untouched). Must run before new-session. All failures are ignored — deep
// scrollback is a nicety, not a precondition for spawning.
func (l *Lifecycle) ensureScrollback(ctx context.Context) {
	if out, err := l.run.Run(ctx, "", "tmux", "show-options", "-g", "-v", "history-limit"); err == nil {
		if cur, perr := strconv.Atoi(strings.TrimSpace(out)); perr == nil && cur >= agentHistoryLimit {
			return // already large enough
		}
	}
	_, _ = l.run.Run(ctx, "", "tmux", "set-option", "-g", "history-limit", strconv.Itoa(agentHistoryLimit))
}
```

- [ ] **Step 5: Wire the typed/managed path to the helper**

In `internal/lifecycle/lifecycle.go`, replace the typed-path block (currently around line 473):

```go
	if out, err := l.run.Run(ctx, req.Repo, "tmux", "new-session", "-d", "-s", id, "-c", workdir); err != nil {
		return nil, fmt.Errorf("tmux new-session: %w: %s", err, out)
	}
```

with:

```go
	if err := l.newAgentSession(ctx, req.Repo, id, workdir); err != nil {
		return nil, err
	}
```

- [ ] **Step 6: Run the test; verify it passes**

Run: `go test ./internal/lifecycle/ -run TestSpawnSetsMouseOnAgentSession -v`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/lifecycle/lifecycle.go internal/lifecycle/lifecycle_test.go
git commit -m "feat(lifecycle): scroll-friendly agent tmux sessions (typed path)

Add newAgentSession + ensureScrollback helpers: mouse on per session,
global history-limit raised only-raise to 50000 before new-session.
Wire the typed/managed spawn path."
```

---

## Task 2: Wire the prompt-mode and resume paths

**Files:**
- Modify: `internal/lifecycle/lifecycle.go` (lines ~450 prompt-mode, ~485 `resumeInTmux`)
- Test: `internal/lifecycle/lifecycle_test.go`

- [ ] **Step 1: Write the failing tests (prompt-mode + resume)**

Add to `internal/lifecycle/lifecycle_test.go`:

```go
func TestSpawnPromptModeSetsMouseOn(t *testing.T) {
	fr := &FakeRunner{}
	l := New(fr)
	l.PromptsDir = "/state/prompts"
	s, err := l.Spawn(context.Background(), SpawnRequest{Prompt: "do a thing", Cwd: "/work/project"})
	require.NoError(t, err)
	require.Contains(t, fr.calledArgs(), []string{"tmux", "set-option", "-t", s.ID, "mouse", "on"})
}

func TestResumeInTmuxSetsMouseOn(t *testing.T) {
	fr := &FakeRunner{}
	err := New(fr).resumeInTmux(context.Background(), "ag1", "/cwd", "claude-id")
	require.NoError(t, err)
	require.Contains(t, fr.calledArgs(), []string{"tmux", "set-option", "-t", "ag1", "mouse", "on"})
	require.Greater(t,
		fr.callIndex("tmux set-option -t ag1 mouse on"),
		fr.callIndex("tmux new-session -d -s ag1 -c /cwd"),
		"mouse on must follow new-session")
}
```

- [ ] **Step 2: Run them; verify they fail**

Run: `go test ./internal/lifecycle/ -run 'TestSpawnPromptModeSetsMouseOn|TestResumeInTmuxSetsMouseOn' -v`
Expected: FAIL — both paths still call `new-session` inline (no `set-option mouse on`).

- [ ] **Step 3: Wire the prompt-mode path**

In `internal/lifecycle/lifecycle.go`, replace the prompt-mode block (currently around line 450):

```go
		if out, err := l.run.Run(ctx, "", "tmux", "new-session", "-d", "-s", id, "-c", req.Cwd); err != nil {
			return nil, fmt.Errorf("tmux new-session: %w: %s", err, out)
		}
```

with:

```go
		if err := l.newAgentSession(ctx, "", id, req.Cwd); err != nil {
			return nil, err
		}
```

- [ ] **Step 4: Wire the resume path**

In `internal/lifecycle/lifecycle.go`, replace the `resumeInTmux` block (currently around line 485):

```go
	if out, err := l.run.Run(ctx, "", "tmux", "new-session", "-d", "-s", id, "-c", cwd); err != nil {
		return fmt.Errorf("tmux new-session: %w: %s", err, out)
	}
```

with:

```go
	if err := l.newAgentSession(ctx, "", id, cwd); err != nil {
		return err
	}
```

- [ ] **Step 5: Run them; verify they pass**

Run: `go test ./internal/lifecycle/ -run 'TestSpawnPromptModeSetsMouseOn|TestResumeInTmuxSetsMouseOn' -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/lifecycle/lifecycle.go internal/lifecycle/lifecycle_test.go
git commit -m "feat(lifecycle): scroll-friendly tmux for prompt-mode + resume paths"
```

---

## Task 3: history-limit ordering, only-raise, and non-fatal behavior

**Files:**
- Test: `internal/lifecycle/lifecycle_test.go`

These tests pin the spec's nuanced requirements. No production code should be needed — if any test fails, the bug is in Task 1's helpers; fix there.

- [ ] **Step 1: Write the ensure-before-create test**

```go
func TestSpawnRaisesHistoryLimitBeforeNewSession(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"tmux show-options -g -v history-limit": {Out: "2000"},
	}}
	s, err := New(fr).Spawn(context.Background(), SpawnRequest{Type: store.TypeBuildkiteDebug, Repo: "/repo"})
	require.NoError(t, err)
	setIdx := fr.callIndex("tmux set-option -g history-limit 50000")
	newIdx := fr.callIndex("tmux new-session -d -s " + s.ID + " -c /repo")
	require.NotEqual(t, -1, setIdx, "history-limit must be raised when current is lower")
	require.Less(t, setIdx, newIdx, "history-limit must be raised BEFORE new-session")
}
```

- [ ] **Step 2: Write the only-raise test**

```go
func TestSpawnDoesNotLowerHistoryLimit(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"tmux show-options -g -v history-limit": {Out: "100000"},
	}}
	_, err := New(fr).Spawn(context.Background(), SpawnRequest{Type: store.TypeBuildkiteDebug, Repo: "/repo"})
	require.NoError(t, err)
	require.NotContains(t, fr.calledArgs(), []string{"tmux", "set-option", "-g", "history-limit", "50000"},
		"a larger user-configured history-limit must be left untouched")
}
```

- [ ] **Step 3: Write the non-fatal-option tests**

```go
func TestSpawnSucceedsWhenHistoryLimitSetFails(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"tmux set-option -g history-limit 50000": {Err: errors.New("boom")},
	}}
	_, err := New(fr).Spawn(context.Background(), SpawnRequest{Type: store.TypeBuildkiteDebug, Repo: "/repo"})
	require.NoError(t, err, "history-limit failure must not fail the spawn")
}

func TestResumeSucceedsWhenMouseSetFails(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"tmux set-option -t ag1 mouse on": {Err: errors.New("boom")},
	}}
	err := New(fr).resumeInTmux(context.Background(), "ag1", "/cwd", "cid")
	require.NoError(t, err, "mouse-on failure must not fail the resume")
}

func TestResumeFailsWhenNewSessionFails(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"tmux new-session -d -s ag1 -c /cwd": {Err: errors.New("boom")},
	}}
	err := New(fr).resumeInTmux(context.Background(), "ag1", "/cwd", "cid")
	require.Error(t, err, "new-session failure stays fatal")
}
```

Ensure `"errors"` is imported in the test file (add it to the import block if absent).

- [ ] **Step 4: Run the whole package**

Run: `go test ./internal/lifecycle/ -v`
Expected: PASS — all new tests plus the pre-existing suite (the existing `new-session` / `send-keys` assertions still hold; the helper issues the identical `new-session` argv).

- [ ] **Step 5: Commit**

```bash
git add internal/lifecycle/lifecycle_test.go
git commit -m "test(lifecycle): pin history-limit ordering, only-raise, non-fatal options"
```

---

## Task 4: Full build + vet

**Files:** none (verification only)

- [ ] **Step 1: Vet and build the whole module**

Run: `go vet ./... && go build ./...`
Expected: no output, exit 0.

- [ ] **Step 2: Run the full test suite**

Run: `go test ./...`
Expected: PASS across packages (only `internal/lifecycle` changed behavior).

- [ ] **Step 3: Commit (only if vet/build produced fixups)**

```bash
git add -A
git commit -m "chore: go vet/build fixups for scroll-native sessions"
```

---

## Manual verification (after merge + daemon restart)

These are not automated (they need a real tmux + daemon). Do them after `make release`/`install` and a daemon restart:

1. Spawn a fresh agent, open the cockpit TUI, select it.
2. In the detail pane, scroll the **mouse wheel** up → agent output scrolls back (copy-mode).
3. Press **`a`** to full-screen the agent → wheel-scroll and `Ctrl-b [` both scroll deep history; **`Ctrl-b Enter`** returns to the cockpit.
4. Confirm text selection inside claude now needs **Shift** (expected mouse-on tradeoff).
5. `tmux show-options -g history-limit` reports `50000` (or your prior larger value).

---

## Self-Review Notes

- **Spec coverage:** mouse on (Task 1/2) ✓; history-limit only-raise before create (Task 1 helper, Task 3 tests) ✓; helper consolidation of all 3 sites (Task 1 typed, Task 2 prompt+resume) ✓; non-fatal options / fatal new-session (Task 3) ✓; testing strategy via FakeRunner (Tasks 1–3) ✓.
- **Placeholders:** none — every code/step is concrete.
- **Type consistency:** `newAgentSession(ctx, runDir, id, cwd string) error` and `ensureScrollback(ctx)` names/signatures are identical across all references; `callIndex` helper defined once in Task 1.
- **Note:** `resumeInTmux` returns `error` (not `(*Session, error)`), so its wiring uses `return err`; the two Spawn paths use `return nil, err`. Reflected in Tasks 1–2.

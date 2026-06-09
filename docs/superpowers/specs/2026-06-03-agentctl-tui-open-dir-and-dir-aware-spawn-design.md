# TUI: open-a-directory (`o`) + dir-aware new-agent (`n`)

**Date:** 2026-06-03
**Status:** Approved (brainstorm complete, ready for implementation plan)
**Scope:** `internal/tui` (cockpit list pane + classic single-pane), plus a small `internal/client` addition.

## Problem

The cockpit and classic TUIs already group the agents list by source directory
(`sourceDir` = `Repo`, else `Workdir`). But the grouping is purely cosmetic in
two ways:

1. **Spawning ignores the grouping.** Pressing `n` always spawns the new agent
   with `Cwd = os.Getwd()` — the pane's own launch dir — regardless of which
   group the cursor is in. You cannot direct a new agent at a particular dir
   from inside the TUI.
2. **You can only target dirs that already have agents.** There is no way to
   stage a directory that has no agents yet as a spawn target.

This change makes directory groups *active*: you can open a dir as a workspace
(`o`), and `n` defaults the new agent's launch dir to the group the cursor is
currently in.

## Goals

- `o` opens a directory as a (possibly empty) group in the list, chosen via a
  path input with `/fs/dirs`-backed tab-completion.
- `n` defaults the new agent's launch dir to the cursor's current group, shows
  it in the form, and lets you override it inline.
- Land in **both** the cockpit list pane (`listPaneModel`) and the classic
  single-pane `Model`.

## Non-goals

- No persistence of opened dirs across TUI restarts (session-only, in memory).
- No change to typed/worktree spawn flows — `o`/`n` drive prompt spawns with an
  explicit `Cwd` only.
- No extraction of a shared `listCore` struct. We follow the existing
  parallel-twin structure of the two models (see "Code organization").

## Decisions (from brainstorm)

| Question | Decision |
|---|---|
| Selection model for empty opened dirs | **Implicit + empty-group rows.** An empty opened dir renders as a selectable placeholder row under its header; the cursor walks "items" (agents + placeholders). |
| How `o` picks a dir | **Text input + tab-completion** backed by `GET /fs/dirs`. |
| Lifecycle of an opened-but-empty dir | **Session-only, sticky.** Lives in TUI memory until you close it (`x`) or quit. Placeholder reappears whenever the dir's agent count returns to zero. |
| New-agent form | **Show resolved dir, override on demand.** Header shows the target dir; `tab` opens the completing path input to change it. |
| Scope | **Both** cockpit and classic. |
| Code organization | **Pure helpers + parallel wiring** (Option 1). |

## Code organization

The genuinely complex/shared logic is written **once** as pure functions in
`internal/tui/list.go`. The stateful wiring (new model fields, key handlers,
form rendering) is added to each of the two models separately, matching the
existing parallel structure (`selected`/`selectedID`/`repin`/the `n`,`s`,`x`
cases are already duplicated across both models today).

A future extraction of a shared `listCore` is a clean follow-up if the
duplicated wiring becomes painful; the pure functions below carry over
unchanged.

## Design

### 1. Per-model state (both `Model` and `listPaneModel`)

Add:

- `openedDirs map[string]time.Time` — cleaned absolute dir → time it was opened.
  Session-only, in memory.
- `tp textinput.Model` — a **path** input, distinct from `ta` (prompt textarea)
  and `ti` (send message), used by both the `o` flow and the new-agent
  dir-override.
- `dirCandidates []string` — completion candidates currently displayed.
- `targetDir string` — the resolved launch dir for the pending new agent.

Two new modes alongside the existing `mode` constants:

- `modeOpenDir` — the `o` path input.
- `modeNewAgentDir` — the dir-override sub-state reached via `tab` from
  `modeNewAgent`.

### 2. Item / cursor model (pure, `list.go`)

The cursor stops indexing `[]*store.Session` directly and starts indexing a
derived item list:

```go
// item is one navigable row: a real agent (session != nil) or a placeholder
// for an empty opened dir (session == nil). dir is always set.
type item struct {
    session *store.Session
    dir     string
}

// buildItems orders groups by most-recent activity and emits navigable items.
// A real group contributes its agents (in input order); an opened dir with zero
// agents contributes exactly one placeholder item. Pure.
func buildItems(sessions []*store.Session, opened map[string]time.Time) []item
```

Ordering generalizes today's `groupSort`: a real group's sort key is its newest
agent's `UpdatedAt`; an empty opened dir's key is its `openedAt`. A
freshly-opened dir therefore floats to the top. An opened dir that currently has
agents emits its agents and **no** placeholder.

Derived accessors change to read the item list:

- `selected() *store.Session` → `items[cursor].session` (may be nil).
- `selectedID() string` → `""` when the item is a placeholder.
- `activeDir() string` → `items[cursor].dir`.

`renderList`/`buildRows` are refactored to walk `[]item` rather than
`(sessions, cursor)`. The placeholder renders as a dimmed, selectable
`(no agents — n to spawn here)` line under its group header. Group header counts
reflect agent counts only.

`repin` keeps the cursor on the same session ID if it still exists; for a
placeholder it keeps the cursor on the same `dir`. Cursor is clamped to the item
count as today.

### 3. The `o` flow + completion

- `o` (normal mode) → `modeOpenDir`: reset and focus `tp`, seed
  `dirCandidates` from the pane's cwd (or home).
- Typing + `tab` → `api.ListDirs(ctx, parentOf(typed))`, filter the returned
  entries by the typed leaf prefix, complete the **longest common prefix** into
  the input, and display the matches as `dirCandidates`.
- `enter` → `filepath.Clean` + leading-`~` expansion, validate the path with
  `ListDirs(path)` (must resolve to a readable dir), then set
  `openedDirs[path] = time.Now()` and set `pendingSelect` so the cursor parks on
  the new placeholder. On validation failure, set `status` and stay in the mode.
- `esc` → back to normal mode, no change.

Opened dirs are stored as cleaned absolute paths so they match `sourceDir`
(`Workdir`) exactly when an agent later spawns there.

### 4. The `n` flow

- `n` (normal mode) → `targetDir = activeDir()`, falling back to the pane's own
  cwd (`os.Getwd()`) when the group dir is unknown (`—`). This preserves today's
  behavior for the ungrouped bucket. Enter `modeNewAgent`, focus `ta`.
- Form header: `New agent — <targetDir>  (tab: change dir)`.
- `ctrl+s` → spawn with `client.SpawnParams{Prompt: ta.Value(), Cwd: targetDir}`.
- `tab` (in `modeNewAgent`) → `modeNewAgentDir`: seed `tp` with `targetDir`,
  reuse the §3 completion. `enter` sets `targetDir` and returns to
  `modeNewAgent` (refocus `ta`); `esc` returns to `modeNewAgent` unchanged.

### 5. The `x` overload + lifecycle

In normal mode, `x` branches on the item under the cursor:

- **Placeholder** → close (forget) the opened dir: `delete(openedDirs, dir)`,
  `status = "closed " + dir`.
- **Real agent** → unchanged `modeConfirmKill` (kill & remove).

Stickiness is implicit: an opened dir stays in `openedDirs` until closed or
quit, so its placeholder reappears whenever the dir's agent count returns to
zero. Spawning into an opened dir merges naturally — the prompt agent's
`Workdir` equals the cleaned dir, so it joins that group and the placeholder
disappears on the next list refresh.

### 6. Client / api plumbing

Add a `ListDirs` method to the TUI's `api` interface and to `*client.Client`,
mirroring the `/fs/dirs` response the way `SpawnParams` mirrors `/spawn`:

```go
// In internal/client: a DirListing mirror + method.
type DirListing struct {
    Path    string
    Parent  string
    Entries []struct{ Name, Path string }
}
func (c *Client) ListDirs(ctx context.Context, path string) (DirListing, error)
```

Add `ListDirs(ctx, path) (client.DirListing, error)` to the `api` interface in
`model.go` and to the test fake.

### 7. Footer / help

Add `o open dir` to the footer string (`list_pane.go` View, classic `view.go`)
and to `helpText()`.

## Testing (TDD, pure-function-first)

Pure functions in `list.go` are tested directly:

- `buildItems`: grouping; ordering with a freshly-opened dir on top; placeholder
  emitted only at zero agents; opened dir with agents emits no placeholder;
  empty `opened` map reproduces today's behavior.
- `activeDir`: returns the cursor item's dir; fallback path covered separately.
- completion filter: candidate selection by leaf prefix; longest-common-prefix
  completion.
- cursor navigation across placeholders (up/down); `repin` by session id and by
  dir.

Model-level handler tests (per model, mirroring existing `model_test.go` /
`list_pane_test.go` style):

- `o` adds a dir and parks the cursor on its placeholder.
- `x` on a placeholder closes the dir; `x` on an agent still triggers confirm.
- `n` resolves `targetDir` from the active group; fallback when group is `—`.
- `tab` in the new-agent form overrides `targetDir`.

Existing `renderList`/`list_test.go` tests update to the item-based signature.

## Edge cases

- **Path normalization:** opened dirs stored `Clean`ed + absolute so they match
  `sourceDir` (`Workdir`) for merging.
- **Open an already-populated dir:** recorded in `openedDirs` (idempotent via the
  map) but no placeholder shows while it has agents.
- **Opening `—`:** disallowed; it is not a real directory.
- **Invalid / unreadable path on `enter`:** surfaced via `status`, stays in
  `modeOpenDir`.

## Touch points

- `internal/tui/list.go` — `item`, `buildItems`, generalized ordering,
  `renderList`/`buildRows` refactor, completion filter, `activeDir`.
- `internal/tui/list_pane.go` — cockpit wiring: new fields, modes, `o`/`n`/`x`
  handlers, form rendering, footer.
- `internal/tui/model.go`, `keys.go`, `view.go` — classic wiring (parallel).
- `internal/client/client.go` — `DirListing` mirror + `ListDirs`.
- `internal/tui/model.go` — `api` interface gains `ListDirs`; test fake updated.
- Tests across `list_test.go`, `model_test.go`, `list_pane_test.go`,
  `cmds_test.go` as needed.

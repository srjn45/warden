# Agent Sub-tree Grouping in the TUI (parent → spawned children)

**Date:** 2026-06-27
**Status:** Implemented (phases 1–6 shipped)
**Owner:** Srajan Pathak
**Branch / worktree:** `feat/agent-subtree-design` (`.worktrees/agent-subtree-design`)
**Scope:** `internal/store`, `internal/client`, `internal/daemon`, `internal/mcp`,
`internal/tui`. Daemon API is spec-first — the `ParentID` field goes through
`openapi.yaml` + `make generate`, never hand-written DTOs.

---

## 1. Problem

When an agent spawns another agent (via the `spawn_agent` MCP tool), the TUI
list shows the spawned agents **flat, mixed in with operator-spawned agents**.
There is no visual signal that an agent was orchestrated by a parent, so the
operator cannot tell which agents are "theirs" vs. which were created by another
agent. This is confusing precisely when it matters most — a busy fleet where one
orchestrator has fanned out several workers.

We want a **collapsible sub-tree view**: spawned agents nest under the agent that
created them, making the orchestration structure explicit.

## 2. Goals

- **Group spawned agents under their parent** in the TUI list, with a
  collapsible header (`▸ / ▾`) — the same affordance pipelines already use.
- **Support arbitrary depth** (A → B → C …): orchestration trees can be more
  than one level deep, so render nested indentation per depth.
- **Survive parent deletion gracefully.** A parent with live descendants must
  not simply vanish and orphan them; it remains as a *terminated tombstone*
  header — a list row with **no terminal/attach pane**, exactly like a completed
  pipeline job renders today.
- **Zero change to the flat case.** An agent with no parent and no children
  looks and behaves exactly as it does now.

## 3. Non-goals

- No change to *how* agents are spawned, isolated, or run. This is presentation +
  one new provenance field.
- No web-UI (`web/`) work in this spec. The `ParentID` field will be available
  over the API, so a follow-up can mirror the tree there; out of scope here.
- No new orchestration semantics (no "kill the whole subtree", no cascade
  policies beyond the tombstone rule in §6). Could be a follow-up.

---

## 4. What exists today (grounding)

The pieces we need are already present for **pipelines**; we mirror them.

- **Grouping UI.** `pipelineItems()` (`internal/tui/list.go:255`) emits a
  collapsible header row (`item.pipeline`) followed by indented job rows
  (`item.pjJob`). Collapse state lives in `m.collapsed` (`pipeline id → hidden`),
  toggled by the `▸/▾` keys in `list_pane.go`. Pipeline groups are prepended
  ahead of the flat list at `list_pane.go:93`.
- **Terminal-but-kept rows.** A pipeline job whose status is terminal
  (`jobIsTerminal`, `list_pane.go:906`) has **no live tmux**: pressing Enter
  renders its *stored detail* via `openJobDetailCmd` instead of attaching
  (`list_pane.go:636`, `:924`). This is the exact "list item, no terminal view"
  behaviour we want for tombstone parents.
- **Orphan fallback.** When a pipeline is deleted, its jobs fall back to the flat
  list (`list_pane.go:91`) rather than disappearing — precedent for "keep the
  children, drop the parent" handling.
- **The `item` struct** (`internal/tui/list.go:160`) is the single row model the
  cursor walks; `itemKey()` (`:178`) is the stable re-pin identity across
  refreshes; `buildItems()` (`:202`) flattens groups into the cursor list.

**What is missing:** there is **no parent linkage** anywhere. `store.Session`
(`internal/store/types.go:142`) records `PipelineID`/`JobID`/`Tags` but nothing
that says *which agent spawned this one*. So spawned agents are today
indistinguishable from hand-spawned ones.

**The capture hook is clean.** The `spawn_agent` MCP handler
(`internal/mcp/server.go:264`) runs inside the *calling* agent's own process, and
`sessionID()` (`server.go:189`) already reads that agent's `WARDEN_SESSION_ID`
from the env. So when agent A calls `spawn_agent`, the handler can stamp A as the
parent — we simply don't thread it through `SpawnParams` today.

---

## 5. Design

### 5.1 Data model — `ParentID`

Add one field to `store.Session` (`internal/store/types.go:142`):

```go
ParentID string `json:"parent_id,omitempty"` // agent that spawned this one (empty = operator/CLI-spawned root)
```

- Empty `ParentID` = a **root** agent (operator- or CLI-spawned). Today every
  agent is a root, so existing sessions deserialize unchanged (omitempty +
  zero-value) — **no migration needed**.
- `ParentID` references another `Session.ID`. It may point at a session that has
  since been deleted; see the tombstone rule (§6).

### 5.2 Capturing the parent at spawn

Thread `ParentID` along the existing `Tags`/`PipelineID` path:

1. `client.SpawnParams` (`internal/client/client.go`) gains `ParentID string`.
2. The daemon spawn path persists it onto the new `store.Session`.
3. **Spec-first:** add `parent_id` to the spawn request/response schemas in
   `openapi.yaml`, then `make generate` (per the spec-first rule — never
   hand-edit `internal/daemon/oapi`).
4. The **`spawn_agent` MCP handler** (`server.go:266`) stamps the parent
   automatically: `ParentID: sessionID()`. Because the handler runs in the
   calling agent's process, `sessionID()` is exactly the orchestrator's id. For
   operator/CLI spawns (`wd spawn`), `ParentID` is left empty → root.
   - Optional explicit override: a `parent` arg on `spawnArgs` for unusual cases,
     defaulting to `sessionID()`. Probably unnecessary for v1; note and skip.

No other spawn behaviour changes.

### 5.3 List grouping — full tree nesting

Add an `agentTreeItems()` builder alongside `pipelineItems()`
(`internal/tui/list.go`). It takes the flat session set (those **not** owned by a
pipeline — pipeline jobs keep their existing grouping and take precedence) and
produces a depth-first pre-order traversal of the parent→child forest:

- Build `childrenByParent map[string][]*store.Session` keyed on `ParentID`.
- Roots = sessions whose `ParentID` is `""` **or** whose parent id is not present
  in the current session set (a missing parent → treat the child as a root, the
  orphan-fallback precedent). Tombstone parents (§6) are present, so their
  children stay nested.
- Recurse from each root in the existing creation-order sort, emitting one
  `item` per node, carrying a **depth** so the renderer can indent.
- A node with children renders as a **collapsible header**; collapsed nodes skip
  their entire subtree. Reuse `m.collapsed` keyed on the agent id.
- Ordering across roots reuses the current group ordering (newest-agent
  `CreatedAt`), so the tree slots into today's list without reshuffling
  unrelated rows.

#### `item` struct changes

Extend `item` (`internal/tui/list.go:160`) with:

```go
depth      int  // nesting level for indentation (0 = root); agent-tree rows only
hasKids    bool // this agent has children → render a ▸/▾ header
tombstone  bool // parent kept only as a header (terminated, no terminal pane)
```

`session` is still the row's payload; the three fields above only decorate
agent-tree rows. `itemKey()` stays `it.session.ID` for these rows (stable re-pin
already works).

#### Rendering

- `renderItemLine()` (`internal/tui/list.go:495`) gains a leading indent of
  `depth × 2` spaces and a `▸/▾` glyph when `hasKids` — mirroring the pipeline
  header/job styling already in that function.
- A **tombstone** row (`it.tombstone`) renders the parent's stored
  name/type/subject with a muted "terminated" badge and **no live state/token
  gauge** — visually the same class as a terminal pipeline job.

#### Cursor / keys (the part full-nesting complicates)

This is where arbitrary depth costs more than the one-level pipeline model:

- **Collapse/expand** (`list_pane.go:650`) already toggles `m.collapsed`; extend
  to any agent-tree header. Collapsing a node hides its *whole* subtree; re-pin
  the cursor to the collapsed node (reuse the `repin` pattern at `:662`).
- **Enter / attach** (`list_pane.go` cockpit detail): a live agent row attaches
  to its tmux as today; a **tombstone** row has no tmux → render stored detail,
  reusing the terminal-job code path (`cockpitDetailCmd` → stored detail at
  `:636`). We generalise that branch from "terminal pipeline job" to "any row
  with no live tmux".
- **Delete** (`list_pane.go`): see §6.

### 5.4 Tombstone state — reuse existing status (no new enum)

Per decision, **no new `StatusTerminated`**. A tombstone parent keeps its record
but is marked with an existing terminal status (`StatusDone`, or
`StatusOrphaned` when it was force-killed) and has its tmux torn down. The TUI
derives "header-only, no terminal" structurally — **tmux gone AND has live
children** — exactly the way `jobIsTerminal` already gates the pipeline path.
This avoids touching the `Status` enum, `Valid()`, and every status switch.

---

## 6. Edge case: deleting the parent

**Parents are deletable.** The rule, mirroring the orphan-pipeline precedent:

| Situation | Behaviour |
|---|---|
| Delete parent with **live descendants** | Do **not** remove the record. Tear down its tmux, mark it terminal (§5.4). It stays as a **tombstone header** — a row with no terminal pane — and its children remain nested under it and keep running / stay attachable. |
| Delete parent with **no live descendants** | Normal hard delete; the row disappears. |
| Delete / finish a **child** | The subtree shrinks. When a tombstone parent's last live descendant is gone, the tombstone is eligible for reaping (next prune / on next delete). |
| Parent record genuinely missing (older data, hard-deleted) | Children whose `ParentID` resolves to nothing are treated as **roots** (orphan fallback) — no dangling group. |

The daemon delete path (`Client.Delete`, `internal/client/client.go:472`) grows a
guard: if the target has live descendants, convert to tombstone instead of
removing. "Has live descendants" = any session with `ParentID == target` (or
transitively) that is not itself terminal.

**Decision:** `delete` on a parent-with-live-children is **silently
tombstoned** (matches "keep the list item" — no confirmation prompt, no block).
The header label surfaces the live-child count (e.g. `▾ agent-x  (terminated · 3
running)`) so the operator still sees why the row persists.

---

## 7. Touch list (no code yet — planning only)

- `internal/store/types.go` — add `ParentID` field.
- `openapi.yaml` + `make generate` — `parent_id` on spawn request/response (and
  the agent/session read DTO so the TUI/web can see it).
- `internal/client/client.go` — `SpawnParams.ParentID`; delete-path tombstone
  guard.
- `internal/daemon/*` — persist `ParentID` on spawn; tombstone-on-delete logic.
- `internal/mcp/server.go` — stamp `ParentID: sessionID()` in the `spawn_agent`
  handler.
- `internal/tui/list.go` — `agentTreeItems()` builder; `item` fields
  (`depth`/`hasKids`/`tombstone`); indentation + glyph + tombstone styling in
  `renderItemLine()`.
- `internal/tui/list_pane.go` — collapse/expand for agent headers; Enter/attach
  vs. stored-detail for tombstones; delete → tombstone wiring.
- Tests: `list_test.go` / `list_pane_test.go` (tree flatten, collapse hides
  subtree, tombstone renders header-only), store round-trip, mcp spawn stamps
  parent, daemon tombstone-on-delete.

## 8. Risks / call-outs

- **Cursor logic is the main cost of full nesting.** One-level (pipeline-style)
  grouping would be markedly simpler; arbitrary depth means collapse must hide a
  *recursive* subtree and re-pin correctly. Budget test coverage here.
- **Tombstone reaping** must not leak: a tombstone whose last child dies should
  be collectible, or the list slowly fills with dead headers. Tie reaping to the
  existing prune path.
- **Interaction with pipelines.** A pipeline-owned session already groups under
  its pipeline; it must not *also* be pulled into an agent tree. Rule: pipeline
  ownership wins; `agentTreeItems()` only sees non-pipeline sessions.
- **Self/cycle safety.** `ParentID` should never equal the agent's own id, and
  the traversal must guard against a cycle (defensive; shouldn't occur since a
  parent always predates its child).

## 9. CLAUDE.md "definition of done" (for the eventual implementation PR)

- **Docs:** `README.md` feature surface; `docs/FEATURES.md` (both catalogs) +
  website mirror; a TUI guide page under `site/src/content/docs/` describing the
  sub-tree view and tombstones; `reference/cli.md` if any flag/help changes.
- **Skill:** `skills/warden/` — note that `spawn_agent` from within an agent now
  records parentage and nests in the TUI.
- **CLI help:** check `wd spawn` / list help text for any wording on grouping.
- **Tag & release:** one tag, **minor** bump (new user-facing capability);
  confirm with maintainer before pushing the `v*` tag.

> Planning artifact only — **no code changes** in this branch.

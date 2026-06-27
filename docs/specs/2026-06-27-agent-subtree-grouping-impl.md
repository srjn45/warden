# Implementation Plan — Agent Sub-tree Grouping

**Companion to:** `2026-06-27-agent-subtree-grouping.md` (design)
**Branch / worktree:** `feat/agent-subtree-design` (`.worktrees/agent-subtree-design`)
**Status:** Plan (no code yet)

This breaks the design into ordered, independently-reviewable phases. Each phase
compiles and is testable on its own; the feature is only *visible* after Phase 4.

---

## Plumbing chain (traced, for reference)

`ParentID` rides the exact path `Tags` already travels:

```
spawn_agent MCP handler  (internal/mcp/server.go:266, stamps sessionID())
  → client.SpawnParams.ParentID            (internal/client/client.go:348)
    → POST body parent_id                   (openapi SpawnRequest, line 1674)
      → oapi.SpawnRequest                    (generated)
        → spawnRequestFromOAPI               (internal/daemon/strict_lifecycle.go:22)
          → daemon SpawnRequest.ParentID     (internal/daemon/api.go:34)
            → lifecycleAdapter.Spawn         (internal/daemon/lifecycle_adapter.go:25)
              → lifecycle.SpawnRequest       (internal/lifecycle/lifecycle.go:376)
                → store.Session.ParentID     (internal/lifecycle/lifecycle.go:895)
```

Grep `Tags` across those eight sites — `ParentID` gets a sibling line at each.

---

## Phase 1 — Provenance field (store + spawn path)

**Goal:** every agent-spawned agent records its parent. No UI yet.

1. `internal/store/types.go:142` — add
   `ParentID string `json:"parent_id,omitempty"`` to `Session`. Zero value =
   root; omitempty ⇒ existing JSON deserializes unchanged (**no migration**).
2. `internal/daemon/apidocs/openapi.yaml`:
   - `SpawnRequest` (line ~1691, after `tags`): `parent_id: { type: string,
     description: "id of the agent that spawned this one; empty = root" }`.
   - `Session` schema is `x-go-type: store.Session`, so the read side picks up
     `ParentID` automatically — **no schema edit needed for reads**.
   - Run `make generate`.
3. `internal/daemon/api.go:34` — add `ParentID string `json:"parent_id"`` to the
   internal `SpawnRequest`.
4. `internal/daemon/strict_lifecycle.go:22` — map `ParentID: b.ParentId` in
   `spawnRequestFromOAPI`.
5. `internal/daemon/lifecycle_adapter.go:25` — pass `ParentID: req.ParentID` into
   `lifecycle.SpawnRequest`.
6. `internal/lifecycle/lifecycle.go:376` — add `ParentID string` to
   `SpawnRequest`; set it on the `store.Session` at `:895`. Guard against
   self-parenting (`if req.ParentID != id`).
7. `internal/client/client.go:348` — add `ParentID` to `SpawnParams`, include it
   in the POST body.
8. `internal/mcp/server.go:266` — in the `spawn_agent` handler, set
   `ParentID: sessionID()` on `client.SpawnParams`. (Operator/CLI spawns leave
   it empty → root.)

**Tests:** store round-trip with/without `ParentID`; mcp handler stamps the
caller's session id (extend `TestSpawnAgentToolSendsPrompt` family,
`server_test.go:39`); daemon spawn persists `parent_id`.

**Done when:** an agent spawned via MCP has `parent_id` set in its stored JSON;
`make generate` is clean; all existing tests pass.

---

## Phase 2 — Tombstone on delete (daemon)

**Goal:** deleting a parent that has live children keeps it as an active,
terminal, tmux-less record instead of removing/archiving it.

Key constraint discovered: `store.Archive` moves the record to `closed/` and
`store.List` returns **only active** sessions (`internal/store/file.go:280`).
So a tombstone must **stay active** — it is *not* an archive.

1. Add a helper `liveChildren(ctx, store, id) []*Session` (daemon) — scan
   `store.List` for `ParentID == id` (direct children) with `liveStatus(...)`.
   Direct children suffice: a live grandchild implies its live parent is also a
   (tombstone or live) child, so the chain stays anchored.
2. `internal/daemon/strict_lifecycle.go:196` `DeleteSession` — before the
   delete/archive branch: if `len(liveChildren(...)) > 0`, **tombstone instead**:
   - tear down the agent's tmux (`s.life.Teardown` or the terminate path) so no
     live pane remains;
   - CAS-update the record to a terminal status (`StatusDone`, or
     `StatusOrphaned` if it was force-killed) — keep it **active** so `List`
     still returns it;
   - record audit `ActionDelete` with a `tombstoned=true` detail;
   - return `200 {status:"tombstoned"}`.
   Otherwise fall through to today's hard-delete / archive logic unchanged.
3. **Silent** — no confirmation, per decision. The child count goes in the TUI
   header label (Phase 4), not a prompt.

**Tests:** delete parent with a live child → record persists, active, terminal
status, tmux torn down; delete childless parent → unchanged (hard/archive);
delete parent whose only child is already terminal → treated as childless.

**Done when:** the daemon never orphans children and never leaves a live pane on
a tombstoned parent.

---

## Phase 3 — Tombstone reaping

**Goal:** a tombstone disappears once its last live descendant ends, so the list
doesn't accumulate dead headers.

- **Lazy reap (primary):** wherever a child transitions to a terminal status
  (the poller/reconciler that updates statuses), check `ParentID`: if the parent
  is a tombstone (terminal + tmux gone) and now has zero live children, archive
  it (normal `store.Archive`). Locate the status-transition site near the
  existing terminal-status handling in the poller.
- **Safety-net reap:** include tombstones-with-no-live-children in the existing
  prune path so a missed transition still gets collected.

**Tests:** last child finishing reaps the tombstone; a tombstone with ≥1 live
child is retained.

**Done when:** no permanent dead-header leak.

---

## Phase 4 — TUI sub-tree rendering (the visible feature)

**Goal:** spawned agents nest under their parent, collapsible, with tombstone
headers rendering terminal-pane-less.

1. `internal/tui/list.go:160` — extend `item` with:
   `depth int`, `hasKids bool`, `tombstone bool` (agent-tree rows only).
2. New `agentTreeItems(sessions, collapsed)` in `list.go`, sibling to
   `pipelineItems` (`:255`):
   - operate only on **non-pipeline** sessions (pipeline ownership wins; pipeline
     groups are still prepended at `list_pane.go:93`);
   - build `childrenByParent[ParentID]`;
   - roots = `ParentID == ""` **or** parent id absent from the set (orphan
     fallback → promote to root);
   - DFS pre-order from each root (root order = current creation-order sort),
     assigning `depth`; mark `hasKids`; mark `tombstone` when tmux gone AND has
     children; a collapsed node (`collapsed[id]`) skips its whole subtree;
   - cycle/self guard in the recursion (defensive).
   - Replace the flat `buildItems(flatSessions(...))` call at `list.go:93-94`
     with the tree builder for the non-pipeline set (dirs/opened-dir grouping is
     preserved by feeding the tree output through the same dir-grouping, or by
     treating roots as today's group entries — decide during impl; prefer
     keeping `buildItems`' dir grouping for roots and nesting children beneath).
3. `internal/tui/list.go:495` `renderItemLine` — for agent-tree rows: indent
   `depth*2` spaces; `▸/▾` glyph when `hasKids` (reuse pipeline glyph styling);
   tombstone rows render muted name/type/subject + `(terminated · N running)`
   badge, **no** live state/token gauge.
4. `internal/tui/list.go:178` `itemKey` — unchanged (`it.session.ID` still the
   stable re-pin key for these rows).

**Tests (`list_test.go`):** tree flatten order; collapse hides the whole
subtree; deep nesting (A→B→C) indents correctly; tombstone renders header-only;
orphan child (missing parent) promoted to root.

---

## Phase 5 — TUI interaction (keys)

`internal/tui/list_pane.go`:

1. **Collapse/expand** (`:650`/`:656`) — extend the `▸/▾` handlers to any
   agent-tree header (`it.hasKids`), toggling `m.collapsed[it.session.ID]`;
   re-pin the cursor to the collapsed node (reuse the `repin` pattern at `:662`).
2. **Enter / attach** (`cockpitDetailCmd`, `:631`/`detailTarget` `:900`) —
   generalise the existing "terminal pipeline job has no tmux → stored detail"
   branch (`:636`, `jobIsTerminal` `:906`) to "any row with no live tmux",
   covering tombstone parents. A live agent still attaches to its tmux as today.
3. **Delete** — operator delete on a parent flows to the daemon, which decides
   tombstone vs. remove (Phase 2). The TUI needs no special branch beyond
   refreshing the list afterward.

**Tests (`list_pane_test.go`):** collapse/expand on an agent header; Enter on a
tombstone opens stored detail (not attach); cursor re-pin survives collapse.

---

## Phase 6 — Docs, skill, release (CLAUDE.md definition-of-done)

- **README.md** — note sub-tree grouping in the TUI feature list.
- **docs/FEATURES.md** (both catalogs) + **website mirror** under
  `site/src/content/docs/` — a TUI guide page on the sub-tree view + tombstones,
  and any reference entry.
- **skills/warden/** — document that `spawn_agent` from inside an agent now
  records parentage and nests in the TUI.
- **CLI help** — check `wd spawn` / list help wording; sync
  `site/src/content/docs/reference/cli.md` if anything changed.
- **Tag & release** — one tag, **minor** bump (new user-facing capability);
  confirm with maintainer before pushing the `v*` tag (goreleaser trigger).

---

## Sequencing & review notes

- Phases 1→2→3 are backend and can land as one or three PRs; **Phase 1 is safe to
  ship alone** (records data, changes nothing visible).
- Phase 4 depends on Phase 1 (needs `ParentID`) and reads tombstone state from
  Phase 2, but renders sanely even before Phase 2/3 (a tombstone just won't exist
  yet).
- The riskiest unit is the **DFS + collapse + cursor re-pin** in Phases 4–5
  (cost of full nesting vs. one-level). Front-load its tests.
- Keep commits scoped to files in this worktree (peers share `main`).

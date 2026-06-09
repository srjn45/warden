# agentctl Layered Teardown — Design

**Date:** 2026-06-02
**Status:** Approved design (pre-implementation)
**Owner:** Srajan Pathak (personal project)
**Sub-project 3 of 5** (1 storage ✅ → 2 session-id ✅ → 4 restore ✅ → **3 layered teardown** → 5 monitoring/notify).

---

## 1. Goal

Replace today's monolithic `done`/cleanup (which kills tmux **+** removes the worktree/branch **+** archives the record in one shot) with **three independent, composable teardown levels**, where **worktree removal is never implicit**:

1. **terminate** — close claude + tmux; keep the record and worktree.
2. **delete** — clear the stored record for a session id.
3. **remove-worktree** — remove the git worktree + branch; always explicit.

## 2. The three levels

### terminate
- Kills the tmux session (`tmux kill-session`), which kills the claude process inside it. Touches no git.
- Keeps the record and any worktree.
- Sets status → `store.StatusDone` (a terminal state: the poller's `isTerminal` skips it, so it won't be re-flagged `orphaned`; and it stays restorable via sub-project 4's `restore`).
- Always safe; no guard.

### delete
- Removes the session record from the store. **Default: archive** (`store.Archive` → moves to `closed/<id>.json`, recoverable / audit trail). `--hard` → `store.Delete` (purges the JSON).
- Allowed regardless of agent state, but **not silent**: if the tmux session is still alive, the result includes a warning ("tmux still alive; now untracked — terminate first") so the caller knows they've left a running, untracked session. It does **not** auto-kill (that's `terminate`).
- **Ordering note:** `delete` discards the Repo/Worktree/Branch info, so to also remove the worktree, run `remove-worktree` *before* `delete`.

### remove-worktree
- `git worktree remove` + `git branch -D` for the session's worktree (reads Repo/Worktree/Branch from the record).
- **Always explicit** and guarded:
  - Refuses if the agent's tmux is **still alive** (`tmux has-session` succeeds) → terminate first. Override with `--force`.
  - Refuses if the worktree has **uncommitted or unpushed** work (the existing `guard`) → push/commit first. Override with `--force`.
- On success, clears the `Worktree`/`Branch` fields on the record (so it reflects no worktree).
- "Always ask": the **CLI** prompts `y/N` (skip with `--yes` for scripts); the **MCP tool + skill** require the orchestrator to confirm with the user before calling it (guardrail). No-op/error for sessions that have no worktree.

## 3. Composition & the behavior change

- **`done <id>`** is repurposed as a convenience = **terminate + delete (archive)**; `done --hard` = terminate + delete-hard.
- **Behavior change (intentional):** `done` **no longer removes the worktree** — worktree removal is now always the explicit `remove-worktree`. Documented in the command help + skill.
- Full cleanup of a worktree agent = `terminate` → `remove-worktree` (confirm) → `delete`.

## 4. Code shape

| Layer | Change |
|---|---|
| `lifecycle` | Split `Cleanup` into **`Terminate(ctx, tmuxSession)`** (kill tmux only) and **`RemoveWorktree(ctx, CleanupTarget, force)`** (alive-check + uncommitted/unpushed `guard` + `worktree remove` + `branch -D`). Keep internal **`Teardown`** (spawn rollback) as the force-combined path (recomposed from the two, or unchanged). |
| `daemon` | Replace `POST /cleanup` with **`POST /sessions/{id}/terminate`**, **`POST /sessions/{id}/delete`** (`{hard}`), **`POST /sessions/{id}/remove-worktree`** (`{force}`). Each composes lifecycle + store with the ordering/guard rules; 409 on alive/guard violations; terminate sets status `done`; delete archives|purges. Update the daemon `Lifecycle` interface (drop `Cleanup`, add `Terminate`/`RemoveWorktree`; keep `Teardown`). |
| `client` | `Terminate(id)`, `Delete(id, hard)`, `RemoveWorktree(id, force)`. |
| `cli` | `terminate <id>`, `delete <id> [--hard]`, `remove-worktree <id> [--force] [--yes]` (y/N prompt unless `--yes`), and `done <id> [--hard]` (= terminate + delete). Replace the old `done` flag set. |
| `mcp` | Replace `cleanup_agent` with **`terminate_agent`**, **`delete_agent`** (`hard`), **`remove_worktree`** (`force`). |
| `skill` | Update intent→tool table (terminate = default "stop"; delete = clear record; remove-worktree = explicit, confirm-first) + guardrails (worktree removal always confirm; never bulk; terminate is reversible via restore). |

## 5. Status

`terminate` → `store.StatusDone` (existing terminal status; keeps the enum small and is poller-ignored + restorable). A dedicated `terminated` status was considered and rejected as unnecessary surface for now.

## 6. Edge cases

- **No-worktree session** + `remove-worktree` → clear error ("session has no worktree").
- **`remove-worktree` after `delete`** → record gone → `ErrNotFound` (paths unknown); the message suggests removing it manually. (Hence the ordering note: remove-worktree before delete.)
- **Internal `Teardown`** (spawn rollback) is unchanged in behaviour (force-remove everything) — it is not user-facing teardown.

## 7. Testing

- **lifecycle:** `Terminate` issues only `tmux kill-session` (no git). `RemoveWorktree` matrix: tmux alive → refuse (unless force); dirty/unpushed → refuse (unless force); clean+dead → `worktree remove` + `branch -D`; no-worktree target → error.
- **daemon:** each endpoint maps correctly (terminate → status `done` + record kept; delete archive vs `--hard`; remove-worktree guard/alive → 409, success clears Worktree/Branch); `done` convenience = terminate + delete.
- **client/cli/mcp:** round-trips; CLI `remove-worktree` honors `--yes` (and the prompt path is covered by a flagged confirm); the three MCP tools call the right endpoints.
- **live smoke:** spawn a worktree agent → `terminate` (tmux gone; record + worktree remain; status `done`) → `restore` brings it back → `terminate` again → `remove-worktree` (confirm; worktree+branch gone; record's Worktree cleared) → `delete` (record archived).

## 8. Out of scope

- Monitoring / notifications (sub-project 5).
- Bulk teardown across many agents (explicitly avoided; per-agent only).
- Changing the internal spawn-rollback `Teardown`.
- A separate `terminated` status.

# warden snapshot / checkpoint — design

**Status:** shipped
**Feature:** FUTURE_ENHANCEMENTS #46 ("Checkpoint worktree (git stash) + transcript; restore")
**Surfaces:** `wd snapshot create/list/restore` · MCP `snapshot_create`/`snapshot_list`/`snapshot_restore`

## Motivation

An agent doing real work accumulates uncommitted changes in its worktree and a
long session transcript. Before a risky step (a big refactor, a speculative
rewrite, a `wd sync` over a moving base) the operator wants a **known-good point**
to roll back to — without committing half-finished work onto the branch and
without losing the conversation context that explains *why* the tree is the way
it is.

Snapshots give exactly that: a non-destructive capture of the worktree **and** the
session transcript, plus a guarded restore. It is the dev-loop safety net that sits
*below* `wd commit` — you snapshot work-in-progress you are not ready to commit.

## Model

A **snapshot** is one persisted record:

- `git stash create` SHA — a commit object capturing the working tree + index
  **without modifying either** (no stash entry pushed, no index change). This is
  the load-bearing primitive: the agent's tree is untouched, yet the SHA fully
  reconstructs it. Empty when the tree is clean.
- `HEAD` SHA + branch + the dirty-file list (`git status --porcelain`).
- The session transcript, captured from the agent's tmux pane
  (`tmux capture-pane -p -S -`, full scrollback), stored as a blob alongside the
  metadata.
- `id` (`snap-<8hex>`), owning `session_id`, `created_at`, optional `message`, and
  the absolute `workdir` the snapshot was taken in (the restore target).

Persistence mirrors the existing `store.FileStore` conventions: one
pretty-printed JSON file per snapshot under `<data_dir>/snapshots/<id>.json`, the
transcript as `<id>.transcript`, atomic temp-file+rename writes, an `RWMutex`
serializing the daemon's concurrent callers. **No new database.**

## Capture flow

`wd snapshot create [name]` → client `POST /snapshots {session, dir, message}` →
daemon resolves the authoritative worktree (the same `pinnedWorkdir` the git verbs
use, so an agent can only snapshot *its own* worktree) → `snapshot.Manager.Capture`:

1. `git rev-parse --abbrev-ref HEAD` (branch; "" ⇒ not a repo, error).
2. `git rev-parse HEAD` (recorded HEAD).
3. `git stash create "warden snapshot"` (non-destructive stash SHA).
4. `git status --porcelain` (dirty-file list).
5. `tmux capture-pane -p -S - -t <tmux>` (transcript; **best-effort** — a
   missing/closed pane never fails the snapshot, the git state is the load-bearing
   half).
6. Persist metadata + transcript blob; append a `snapshot` event to the agent.

Every git/tmux call runs under a 5s `context.WithTimeout`.

## Restore flow

`wd snapshot restore <id> [--force]` → `POST /snapshots/{id}/restore {force}` →
`snapshot.Manager.Restore`:

1. Load the snapshot (404 if missing); its `workdir` is the target.
2. Refuse a **protected branch** (`main`/`master`) — restore onto an agent branch.
3. Refuse a **dirty tree** unless `--force` (`ErrDirtyWorktree` → 409 with hint).
4. Report HEAD drift (`current HEAD` vs the recorded one).
5. If a stash SHA was recorded, `git stash apply <sha>`; a partial apply leaves
   the conflicting paths in the tree and hands them back (the `wd sync` handoff —
   deterministic detect, Claude resolves the hunks). A clean capture re-applies
   nothing and just reports HEAD.
6. Surface the saved transcript path regardless.

**Reversible-safe by construction:** `git stash apply` neither resets `HEAD` nor
drops the stash, so a restore is purely additive — the snapshot stays usable and
the only thing at risk is uncommitted work (hence the dirty-tree guard).

## Reuse, not rebuild

- **`lifecycle.Runner`** is the single command seam (reused for the exec runner and
  the test `FakeRunner`) — git/tmux are mocked exactly as the lifecycle git verbs
  are.
- **`lifecycle.IsProtectedBranch`** is the shared `main`/`master` rail.
- **`store.FileStore` conventions** (atomic write, RWMutex, per-id JSON) are
  mirrored for the snapshot store, not reinvented.
- **`pinnedWorkdir` / `resolveGitTarget`** (the git routes' worktree-pinning + the
  agent-event bookkeeping) are reused so capture inherits the same security
  boundary and audit trail as `wd commit`.
- **Thin CLI verbs + a `var _ iface = (*client.Client)(nil)` compile-time check**
  follow the `rotate.go` pattern.

## Invariants

- Capture **never mutates** the agent's worktree (`git stash create`, never
  `push`/`save`) — asserted in tests.
- Restore **never** runs on `main`/`master` and **never** clobbers a dirty tree
  without `--force` — asserted in tests.
- A snapshot id is path-traversal-safe before it touches the filesystem
  (`safeID`), since it reaches the store from user input.
- The feature is config-gated (`snapshots`, default on); a disabled daemon returns
  403 from every snapshot endpoint.

## Testing

- **`internal/snapshot`** unit tests: pure helpers (`parsePorcelainPaths`,
  `countLines`, `newID`/`safeID`), capture happy path / clean tree / non-repo /
  best-effort transcript failure, list ordering + session filter, restore
  refusals (dirty, protected branch), missing snapshot, conflict surfacing, and an
  **end-to-end test against a real `git init` repo** exercising the actual
  `ExecRunner` + git (capture a dirty change, discard it, restore it back).
- **`internal/daemon`** route tests: worktree-pinning + event bookkeeping, the
  403 gate, the 404 on a missing snapshot, and session-filtered list.
- **`internal/cli`** tests: target resolution, render helpers, and the
  fake-client interface shape (rotate-style).

## References

- FEATURES.md §23 (Snapshots / checkpoints)
- `internal/snapshot/{snapshot,store,snapshot_test}.go`
- `internal/daemon/snapshot_routes.go`
- `internal/lifecycle/git.go` (the rail + result-struct pattern this mirrors)

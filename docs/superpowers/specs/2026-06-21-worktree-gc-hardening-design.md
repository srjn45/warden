# Worktree GC & Lifecycle Hardening — Design

**Date:** 2026-06-21
**Status:** Approved (brainstorming → spec)

## Problem

warden spawns most agents into a dedicated git worktree under
`.worktrees/<id>` on a fresh branch (`worktreeRel`, `lifecycle.go:249`;
`ensureWorktree`, `lifecycle.go:332`). Creation is careful: `ensureWorktree`
returns a `created` flag so a failed spawn rolls back **only** worktrees warden
made, never an adopted, pre-existing one (`cleanupFailedSpawn`/`rollbackWorktree`,
`lifecycle.go:725`/`748`). Teardown, however, is not careful, and there is **no
garbage collector anywhere**. Over a normal workflow the `.worktrees/` directory
slowly fills with full checkouts and dangling branches that nothing will ever
remove.

Four concrete defects compound into a slow, silent disk and branch leak, plus a
latent data-loss hazard:

1. **Orphaned-worktree leak (headline).** Three of the teardown paths drop or
   detach the session record while leaving the worktree on disk, and there is no
   reconcile/prune step to ever reclaim it.
2. **Stale git worktree admin metadata.** `git worktree prune` is never run, so a
   worktree removed out-of-band leaves `.git/worktrees/<id>` administrative files
   that can later break a `git worktree add`.
3. **pr-review gh-created branch leak.** A detached pr-review records `Branch==""`
   but `gh pr checkout` creates a real local branch that `RemoveWorktree` never
   deletes.
4. **created-vs-adopted is not persisted.** The `created` flag that protects
   rollback lives only on the spawn stack, so a user-invoked `RemoveWorktree`
   always `git branch -D`s the recorded branch — even for an adopted worktree on a
   branch warden never created (data loss under `--force`).

This spec adds a `warden prune` garbage-collection command, runs `git worktree
prune` at the right moments, captures the gh-created pr-review branch, persists
the created/adopted provenance, and proposes two companion features (`warden
worktree ls` and a worktree retention policy).

## Current behavior (with file:line refs)

### Creation records provenance — but only transiently
- `ensureWorktree` (`internal/lifecycle/lifecycle.go:332`) returns
  `(branch, created bool, err)`. `created` is `true` only when warden ran
  `git worktree add`; an adopted pre-existing worktree returns `created=false`
  (`lifecycle.go:337-342`).
- For a **detached pr-review** with no branch, it runs `git worktree add
  --detach` then `gh pr checkout <PR>` and returns `branch=""`, `created=true`
  (`lifecycle.go:343-358`). The local branch `gh` creates is **not captured**.
- The `created` flag is consumed only by `cleanupFailedSpawn(sess, killTmux,
  worktreeCreated)` (`lifecycle.go:725`) → `rollbackWorktree` (`lifecycle.go:748`)
  on the failure path, then discarded. It is **never written to the store**.

### Teardown paths that strand the worktree
- **`warden done`** (`internal/cli/lifecycle.go:230`) = `Terminate` + `Delete`.
  The help text is explicit: "terminated + record cleared; worktree, if any,
  **kept** — use remove-worktree" (`lifecycle.go:244`). By design, but with no GC
  the worktree is now unreachable from any live record.
- **`handleDelete`** (`internal/daemon/lifecycle_routes.go:307`): `--hard`
  (`req.Hard`) calls `store.Delete` which `os.Remove`s the active record
  (`internal/store/file.go:483`). The worktree + branch are now referenced by
  **nothing** — permanently.
- **archive** (the non-hard `handleDelete` branch → `store.Archive`,
  `file.go:467`) moves the record to the `closed/` collection and keeps the
  worktree. The closed record still names the worktree, but nothing ever acts on
  closed records to reclaim it.

### Removal exists but is never automatic and is provenance-blind
- `RemoveWorktree` (`internal/lifecycle/lifecycle.go:978`) is always an explicit,
  separate step (`handleRemoveWorktree`, `lifecycle_routes.go:345`; CLI
  `remove-worktree`, `cli/lifecycle.go:~200`). Unless `force`, it refuses while
  the tmux session is alive and runs `guard` (`lifecycle.go:944`: dirty via
  `git status --porcelain`, unpushed via `git log @{u}..`; no upstream → treated
  as unpushed).
- It then `git worktree remove [--force]` and, **if `t.Branch != ""`**,
  `git branch -D t.Branch` (`lifecycle.go:990-1001`). Two consequences:
  - A detached pr-review (`Branch==""`) **skips** the branch delete, leaking the
    gh-created branch (gap 3).
  - For an adopted worktree, `t.Branch` is a branch warden did not create, yet
    `--force` will `branch -D` it anyway (gap 4).
- `git worktree prune` is **not called** here or anywhere (gap 2).

### The store has no provenance fields
- `store.Session` (`internal/store/types.go:88`) carries `Worktree` and `Branch`
  but **no** `WorktreeCreated`/`BranchCreated` flags. Provenance is lost the
  moment a spawn returns.
- `List` (`internal/store/file.go:220`) returns only **active** sessions;
  archived records live in a separate `closed/` collection (`file.go:66`,
  `closedPath`) with no public list method on the `Store` interface
  (`internal/store/store.go`).

## Goal

A worktree exists on disk **iff** a live or archived session legitimately needs
it, and the only branches warden deletes are branches warden created. Achieve
this with:

- a reconcile-and-reclaim command (`warden prune`) that is safe by default
  (same dirty/unpushed guard, `--dry-run`, `--force`);
- `git worktree prune` wired into both prune and `RemoveWorktree`;
- captured pr-review branch provenance;
- persisted created/adopted flags that gate branch deletion;
- optional retention policy so the common case self-cleans.

## Decisions

- **Reconciliation is the model, not bookkeeping-at-teardown.** Rather than make
  every teardown path remember to remove a worktree, we accept that records and
  worktrees drift and add a periodic/triggered reconcile that compares ground
  truth (`git worktree list --porcelain`) against the union of active + archived
  records. This survives crashes, `--hard` deletes, and out-of-band `rm -rf`.
- **An orphan = a `.worktrees/*` worktree with no owning active OR archived
  record.** Archived (`done`) records still legitimately own their worktree, so a
  freshly-`done` agent's worktree is **not** an orphan — it is reclaimed by the
  retention policy (opt-in) or an explicit `warden prune --include-archived`, not
  by the default sweep. This keeps "I archived it but still want to inspect the
  diff" working.
- **Rejected: making `done`/archived worktrees sweep-eligible by default.** It
  would let the default prune self-heal the headline leak without an opt-in (a
  genuine upside, since `done` is the common teardown path while `--hard`/crash
  are the minority), and removing a *clean, pushed* checkout is cheap —
  reconstructable via `git worktree add .worktrees/<id> <branch>`. We rejected it
  for two reasons. (1) It deletes exactly what `done` exists to preserve — the
  current contract is explicit that `done` keeps the worktree for in-place
  inspection (`cli/lifecycle.go:244`) — and under `worktree_auto_prune` an
  unattended sweep could remove it minutes after archiving. (2) **The guard does
  not see gitignored files** (see below), so a "clean" `done` worktree a human
  deliberately kept can still hold irrecoverable local-only files (`.env`,
  secrets, scratch); auto-sweeping it is silent, git-unrecoverable data loss.
  Unpushed *commits* are safe (the guard's "no upstream → treat as unpushed" rule
  catches them); gitignored *working files* are the residual hazard. So `done`
  stays protected by default and self-healing is a knowing opt-in
  (`worktree_keep_done=false`, `--include-archived`, `worktree_auto_prune`). If
  this is ever flipped, `worktree_auto_prune` must still never touch archived
  worktrees unattended — manual `--include-archived` only.
- **The guard is the same one, everywhere.** prune reuses `guard`
  (`lifecycle.go:944`) per worktree; `--force` is the only override, matching
  `remove-worktree`. No new, weaker safety path. **Known guard blind spot:**
  `git status --porcelain` does not report gitignored paths, so a worktree the
  guard calls "clean" can still contain local-only files git will never restore
  (`.env`, secrets, build/scratch state). This is acceptable for active agents
  and for record-less orphans the user never asked to keep, but it is the reason
  `done`/archived worktrees are not swept by default (see the rejected
  alternative above).
- **Branch deletion requires positive provenance.** After this change warden
  `branch -D`s a branch only when it recorded `BranchCreated=true` (or the user
  passes an explicit override). Adopted branches are left alone even under
  `--force` worktree removal.
- **Scope `.worktrees/*` only.** prune and `worktree ls` filter
  `git worktree list` to paths under the repo's `.worktrees/` prefix
  (`worktreeRel`), never the user's primary worktree or unrelated ones.

## Proposed design — per gap

### Gap 1 — Orphaned-worktree leak → `warden prune`

A new command reconciles git's worktree list against warden's records and
reclaims the orphans, with the existing guard.

**Algorithm** (new `Lifecycle.PruneWorktrees(ctx, repo string, opts PruneOpts)
([]PruneResult, error)`):

1. Run `git worktree list --porcelain` in `repo`; parse into `{path, branch,
   detached, locked}` entries (reuse/extend the parser behind `worktreeExists`,
   `lifecycle.go:313`).
2. Keep only entries whose `path` is under `filepath.Join(repo, ".worktrees")`.
3. Build the **owned set**: the `Worktree` (abs) of every active session
   (`store.List`) plus — when `--include-archived` — every archived session
   (requires a new `store.ListClosed`/`ListAll`; see Store changes).
4. For each `.worktrees/*` entry **not** in the owned set → it is an **orphan**.
   For each orphan, classify state by running the guard (dirty / unpushed /
   clean) and resolve its branch.
5. Action per orphan:
   - `--dry-run`: report only, remove nothing.
   - clean orphan: `git worktree remove`, then delete its branch **only if**
     warden can prove it owns it — for orphans (no record) provenance is unknown,
     so branch deletion is gated behind `--force` (a record-less worktree on
     `main` must never cost the user `main`; we only `branch -D` a branch that is
     (a) not the repo's default branch and (b) named `<id>` / matches the
     worktree dir under `--force`).
   - dirty/unpushed orphan: **skipped** with a reason unless `--force`.
6. After processing, always run `git worktree prune` (gap 2) to clear admin
   metadata for anything removed here or out-of-band.

**Command UX**

```
warden prune [--dry-run] [--force] [--include-archived] [--repo <path>] [--yes]
```

| Flag | Effect |
|---|---|
| `--dry-run` | Report what would be removed; change nothing. Exit 0. |
| `--force` | Override the dirty/unpushed guard AND permit branch deletion for record-less orphans (never the default branch). Mirrors `remove-worktree --force`. |
| `--include-archived` | Treat archived (`done`) records as still-owning; only sweep worktrees with no active record (default already does this — this flag instead makes archived worktrees **eligible** when combined with retention; see below). |
| `--repo <path>` | Repo whose `.worktrees/` to reconcile (default: the daemon's known repos / cwd's repo). |
| `--yes` | Skip the confirmation prompt for a non-dry-run removal. |

Without `--yes`, a non-dry-run prune prints the plan and asks for confirmation
(matching `remove-worktree`'s `--yes`, `cli/lifecycle.go:226`).

**Dry-run output (example)**

```
$ warden prune --dry-run
Scanning .worktrees in /home/u/dev/warden …
3 warden worktrees, 2 orphaned (no live or archived session):

  ORPHAN  .worktrees/dev-a1b2c3d4   branch dev-a1b2c3d4   clean         → would remove (worktree + branch)
  ORPHAN  .worktrees/pr-9f8e7d6c    (detached)            unpushed      → SKIP (use --force)
  keep    .worktrees/code-1122aabb  branch code-1122aabb  owned by code-1122aabb (live)

Would also run: git worktree prune  (clear stale admin metadata)

Summary: 1 removable, 1 blocked (unpushed), 1 kept. Re-run without --dry-run to apply.
```

Non-dry-run prints the same table with `removed` / `SKIPPED` / `keep` and a final
`Removed N worktree(s), reclaimed M branch(es); K skipped.`

**Daemon surface.** A `POST /prune` route (`internal/daemon/lifecycle_routes.go`)
accepting `{dry_run, force, include_archived, repo}` and returning the
`[]PruneResult`. The CLI command calls it through the existing client so prune
runs in the daemon process that owns the store lock. Conflict states
(dirty/unpushed skips) return in the per-entry result, not as an HTTP error;
HTTP error is reserved for "repo not found"/git failures.

### Gap 2 — stale admin metadata → run `git worktree prune`

`git worktree prune` is idempotent and cheap. Wire it in two places:

- At the end of `PruneWorktrees` (above), unconditionally (even on `--dry-run`?
  **No** — dry-run changes nothing; report that it *would* run).
- At the end of `RemoveWorktree` (`lifecycle.go:978`), after the
  `git worktree remove` + branch delete succeed: run `git -C t.Repo worktree
  prune` (best-effort, log on failure — a stale-admin cleanup must not fail the
  removal). This also self-heals cases where a previous out-of-band removal left
  metadata that would otherwise break the next `worktree add` for a reused id.

### Gap 3 — pr-review gh-created branch leak → capture the branch

The detached pr-review path (`lifecycle.go:343-358`) currently returns
`branch=""`. Change it to capture the branch `gh pr checkout` created and record
it:

- After `gh pr checkout <PR>` succeeds, read the checked-out branch:
  `git -C <abs> rev-parse --abbrev-ref HEAD`. (gh leaves HEAD on the new local
  branch, not detached.)
- Return that branch from `ensureWorktree` with `branchCreated=true` (gh made it,
  so warden owns its deletion), and persist it to `Session.Branch` +
  `Session.BranchCreated` (gap 4).
- `RemoveWorktree`'s existing `if t.Branch != ""` block (`lifecycle.go:997`) then
  deletes it correctly on cleanup — no leak.
- Fallback: if `rev-parse` fails or returns `HEAD` (detached), leave `Branch=""`
  and let `warden prune` sweep the dangling branch by name later. So gap 3 is
  fixed at the source **and** backstopped by prune.

### Gap 4 — created-vs-adopted not persisted → provenance fields

Add two booleans to `store.Session` (`internal/store/types.go:88`):

```go
WorktreeCreated bool `json:"worktree_created,omitempty"` // warden ran `git worktree add` (vs adopted a pre-existing one)
BranchCreated   bool `json:"branch_created,omitempty"`   // warden/gh created Branch (vs checked out a user branch)
```

- Spawn writes them from `ensureWorktree`'s `created` return (already threaded as
  `worktreeCreated` through the spawn funcs, `lifecycle.go:700-711`) plus the new
  branch-created signal (development/new-branch and gh-checkout → `true`; adopt
  and "checkout existing branch" pr-review → `false`).
- `RemoveWorktree` (`lifecycle.go:997`) changes its branch-delete condition from
  `t.Branch != ""` to `t.Branch != "" && t.BranchCreated`. `CleanupTarget`
  (`lifecycle.go:931`) gains a `BranchCreated bool` field, filled from the
  session in `handleRemoveWorktree` (`lifecycle_routes.go:358`) and
  `rollbackWorktree` (`lifecycle.go:748`).
- New error/escape hatch: when a user explicitly wants to delete an adopted
  branch, `remove-worktree --delete-adopted-branch` (CLI flag) sets a parameter
  that overrides the `BranchCreated` gate. Default behavior leaves adopted
  branches untouched even under `--force` (which only governs the dirty/unpushed
  guard + worktree-remove, not branch provenance). This closes the data-loss
  hole: warden never silently `branch -D`s a branch a human created.
- **Back-compat / migration:** records written before this change have both flags
  defaulting to `false`. That means an old development worktree would stop having
  its branch auto-deleted. To avoid a regression for the common case, `Load`-time
  backfill (or a one-shot migration) infers `WorktreeCreated=true`,
  `BranchCreated=(Branch != "" && Branch == ID)` for existing development/code/
  etc. records — i.e. when the recorded branch equals the session id (warden's
  default branch name, `lifecycle.go:368-369`), treat it as warden-created. A
  branch the user named explicitly (`req.Branch`) that differs from the id is
  conservatively treated as adopted.

### Store changes (supporting all gaps)

- `Store.ListClosed(ctx) ([]*Session, error)` (and/or `ListAll`) reading the
  `closed/` collection (`file.go:66`), so prune can honor archived ownership.
- `Session` gains `WorktreeCreated`, `BranchCreated` (above). `SpawnRequest`
  → spawn → `store.Create` threads them.

## Companion features (proposed)

### `warden worktree ls`

Read-only inventory of every warden worktree, independent of records — the
diagnostic that makes the leak visible before prune acts:

```
$ warden worktree ls
PATH                       BRANCH             OWNER                STATE
.worktrees/code-1122aabb   code-1122aabb      code-1122aabb (live) clean
.worktrees/dev-a1b2c3d4    dev-a1b2c3d4       orphan               clean
.worktrees/pr-9f8e7d6c     (detached)         orphan               ahead 2, dirty
.worktrees/feat-x          feat-x             feat-x (archived)    clean
```

- Source: `git worktree list --porcelain` filtered to `.worktrees/*`, joined
  against active + archived records by absolute path.
- `OWNER` is the owning session id with its lifecycle (`live`/`archived`) or
  `orphan`. `STATE` is from `guard`: `clean` / `dirty` / `ahead N` / `unpushed`.
- Shares the porcelain parser and join logic with `prune` (prune = `worktree ls`
  + an action). Implement `worktree ls` first; `prune` consumes it.

### Worktree retention policy (config)

Mirror the existing `pipeline_keep_done` pattern (`config.go:47,86,120`) for
worktrees, so the headline leak self-heals for users who want it to:

| YAML key | Type | Default | Effect |
|---|---|---|---|
| `worktree_keep_done` | bool | `true` | When `false`, archiving/`done`-ing an agent triggers a guarded `RemoveWorktree` of its worktree (clean only; dirty/unpushed kept + logged). Default keeps today's behavior. |
| `worktree_auto_prune` | bool | `false` | When `true`, the daemon runs a guarded `PruneWorktrees` sweep on a slow cadence (e.g. hourly) and on startup, reclaiming clean orphans automatically. Dirty/unpushed always require manual `--force`. |

- `worktree_keep_done=false` wires into `handleDelete`/`done`: after a successful
  archive of a session that owns a worktree, attempt a guarded removal (reusing
  `RemoveWorktree` with `force=false`, `BranchCreated` gate). Never blocks the
  archive — a guard failure just leaves the worktree and logs.
- Both default to today's (keep) behavior so this change is non-breaking; a user
  opts into auto-cleanup. Add to the schema slice (`config.go:69`) with hint
  comments and to `defaults()` (`config.go:102`); the drift-guard test
  (config-file design) covers the new YAML tags automatically.

## Out of scope (YAGNI)

- Reclaiming non-`.worktrees/` worktrees or the primary checkout — never touched.
- A general `git gc` / object-pruning wrapper — only worktree + branch + worktree
  admin metadata.
- Cross-repo prune in one invocation beyond iterating the daemon's known repos.
- Undo/trash for pruned worktrees — the guard (dirty/unpushed) is the safety net;
  a clean, record-less checkout is reconstructable from its branch.
- Auto-deleting **adopted** branches without the explicit
  `--delete-adopted-branch` opt-in.

## Test plan

**Pure / parser**
- porcelain parser: multi-worktree output incl. detached, locked, and bare
  entries; filter to `.worktrees/*` only; absolute-path join correctness.

**`PruneWorktrees` (fake `Runner` + temp store)**
- orphan classification: live-owned kept; archived-owned kept (and kept even
  without `--include-archived`); record-less → orphan.
- clean orphan removed (worktree + branch) only under the branch-provenance rule;
  the repo default branch is **never** `branch -D`'d even under `--force`.
- dirty orphan and unpushed orphan **skipped** without `--force`, removed with it.
- `--dry-run` removes nothing and returns the same plan that a real run executes.
- `git worktree prune` invoked after a real run; **not** on `--dry-run`.

**`RemoveWorktree` provenance (gap 3/4)**
- `Branch!="" && BranchCreated` → `branch -D` runs.
- `Branch!="" && !BranchCreated` → `branch -D` is **skipped** even with `force`;
  runs only with the explicit `--delete-adopted-branch` override.
- detached pr-review with captured branch → branch deleted on cleanup (no leak).
- `git worktree prune` runs after a successful remove (best-effort; a prune
  failure does not fail the remove).

**pr-review capture (gap 3)**
- `ensureWorktree` detached path records the `gh`-checked-out branch with
  `BranchCreated=true`; `rev-parse` returning `HEAD` falls back to `Branch=""`.

**Store**
- new provenance fields round-trip through create/read/list.
- `ListClosed`/`ListAll` returns archived records; prune honors them.
- migration backfill: legacy record with `Branch==ID` → `BranchCreated=true`;
  legacy record with a user-named branch ≠ id → `BranchCreated=false`.

**Config**
- `worktree_keep_done` / `worktree_auto_prune` defaults, parse, and drift-guard
  (yaml tags ↔ schema slice) pass.
- `worktree_keep_done=false` → archiving a clean-worktree session triggers a
  guarded removal; a dirty worktree is kept and logged (archive still succeeds).
- `worktree_auto_prune=true` background sweep reclaims clean **record-less**
  orphans but **never** archived-owned worktrees (those require an explicit,
  interactive `--include-archived`); a dirty/unpushed orphan is always skipped.

**Command / daemon**
- `warden worktree ls` joins git truth to records, labels live/archived/orphan,
  and reports guard state; empty `.worktrees/` → friendly "none".
- `POST /prune` returns per-entry results; dirty/unpushed come back as skipped
  entries, not HTTP errors; missing repo → HTTP error.
- `warden prune` without `--yes` prompts; `--yes` and `--dry-run` paths.

**Integration (real git temp repo)**
- spawn → `done` (archive) leaves the worktree; `warden prune` keeps it;
  `warden prune --include-archived` (with retention) reclaims it when clean.
- spawn → `delete --hard` → `warden prune` reclaims the now record-less worktree.
- out-of-band `git worktree remove` of one tree → `warden prune` runs
  `git worktree prune` and a subsequent `worktree add` for a reused id succeeds.

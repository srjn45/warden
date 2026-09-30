# ScrivaDB Canonical Plans — Phase 12 Acceptance Report

**Date:** 2026-09-30  
**Plan:** `scrivadb-canonical-plans-repo-sync` / task `end-to-end-acceptance`  
**Design freeze:** [`2026-09-30-scrivadb-canonical-plans.md`](./2026-09-30-scrivadb-canonical-plans.md)  
**Gate test:** `TestScrivaDBCanonicalPlans_Phase12Acceptance` in
`internal/daemon/scrivadb_canonical_acceptance_test.go`

## Verdict: GREEN

All Phase 12 acceptance criteria are covered by the integrated gate plus the
focused package suites from Phases 2–11. No production behavior changes in this
phase beyond the acceptance corpus, documentation, and explicit follow-up
issues.

## Representative corpus

The gate seeds one project with:

| Fixture | Purpose |
|---|---|
| Legacy YAML under `plans/{pending,in_progress,completed,archived}/` | Every lifecycle state |
| Existing ScrivaDB Plan + execution events | Pre-cutover canonical data |
| Unexported Plan (`FilePath` / `RepoExport` empty) | DB-native path |
| Stale replica YAML disagreeing with canonical | Replica inertness |
| Matching replica of an existing Plan | Duplicate / idempotent import |
| `operator-wip.txt` + `NOTES.md` | Unrelated dirty operator files |

## Assertion table

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Import is idempotent | **PASS** | Gate A: second `ImportLegacy` imports 0; hash-match skips; mutated YAML → conflicted without mutating revision/hash/goal |
| 2 | Normal Plan CRUD + execution need no `plans/` | **PASS** | Gate C: create → run → task status → complete on a root with no `plans/`; directory never created |
| 3 | DB revision conflicts are structured | **PASS** | Gate D: `*RevisionConflictError` with PlanID/Expected/Actual; HTTP PATCH → 409 `PlanMutationConflict` |
| 4 | Replicas never influence listing or execution | **PASS** | Gate B: Get/List serve `canonical goal`, not `HIJACKED FROM DISK` |
| 5 | `sync_to_repo` → exactly one isolated PR per changed revision | **PASS** | Gate E: first sync 1 PR; after revision bump, second success → `prCalls==2` |
| 6 | Repeated sync is a no-op | **PASS** | Gate E: second sync `outcome=skipped`, `reused=true`, no extra PR |
| 7 | Failed sync preserves canonical data | **PASS** | Gate F: GitHub auth failure → Goal/ContentHash/Revision unchanged; `RepoExport` stays nil |
| 8 | Restore works on a clean data directory | **PASS** | Gate G: backup → restore into fresh ScrivaDB dir (no Git / no `plans/`); List + Get succeed |
| 9 | No Hub transport is invoked | **PASS** | Gate H: `plansync.Default()` is local/disabled; Push/Pull/Discover succeed offline; Syncer ∉ PlanSyncProvider; `SyncedAt`/`RemoteID` unset |

## Focused suites (run as part of acceptance)

| Surface | Package / tests |
|---|---|
| Store / service | `./internal/planstore/` |
| Sync / export | `./internal/planexport/` |
| Backup / restore | `./internal/planbackup/` |
| Hub boundary | `./internal/plansync/` |
| Daemon / API | `./internal/daemon/` (incl. Phase 12 gate + CRUD/sync/backup routes) |
| MCP | `./internal/mcp/` (`tools_plans_test.go`) |
| CLI | `./internal/cli/` (`plan_test.go`) |
| TUI | `./internal/tui/` (`plan_tree_test.go`) |
| Repo check | `make verify-fast` |

## Design-freeze §14 checklist

- [x] No new code treats directory placement as lifecycle authority
- [x] No execution path calls `LoadPlan`/`ReadPlanTasks` on a repo export
- [x] Replica edit tests prove inertness
- [x] Startup does not scan `plans/` for authority
- [x] `sync_to_repo` never required for create/run
- [x] Restore works without Git
- [x] This spec is cited when changing Plan authority comments in `planstore`
- [x] `plan-execution-entity-redesign` workers did not reintroduce YAML SoT

## Explicitly deferred (follow-up issues — not hidden non-goals)

| Item | Why deferred | Tracking |
|---|---|---|
| **JSON export format** | Freeze §10 — YAML remains v1 replica; JSON not an implementation target of this plan | [#585](https://github.com/srjn45/warden/issues/585) |
| **Hub network transport** | Freeze D6 / Phase 10 — local `PlanSyncProvider` boundary only; zero network in default install | [#586](https://github.com/srjn45/warden/issues/586) |

See also `docs/RELEASE_NOTES-plans-scrivadb.md` and FEATURES §37.11.

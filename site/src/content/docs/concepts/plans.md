---
title: Plans
description: ScrivaDB-canonical Plans — definition, lifecycle, revision, optional inert YAML/JSON replicas, and execution modes.
---

Plans are **first-class ScrivaDB records**: goal, task DAG, lifecycle status, revision, and execution evidence live in the daemon store. Repository files under `plans/**/*.{yaml,yml,json}` are an **optional inert export** for review — they are never required to create, list, run, or complete a Plan, and editing them does not change canonical execution.

Design freeze: [`docs/specs/2026-09-30-scrivadb-canonical-plans.md`](https://github.com/srjn45/warden/blob/main/docs/specs/2026-09-30-scrivadb-canonical-plans.md).

## Canonical architecture

| Concern | Authority |
|---|---|
| **Definition** (`goal`, `tasks`, `constraints`, `done_when`) | ScrivaDB Plan record |
| **Lifecycle** (`pending` → `in_progress` → `completed` / `archived`) | ScrivaDB `Status` field |
| **Revision / content hash** | ScrivaDB (`revision`, `content_hash`) |
| **Execution evidence** | ScrivaDB events, summaries, task progress |
| **Repository YAML / JSON** | Optional replica via `wd plan sync-to-repo` (YAML default; JSON opt-in) — inert; `import-legacy` accepts legacy YAML only |

The daemon does **not** scan `plans/` on startup. Implicit directory-as-status and "YAML is the source of truth" are retired.

## Lifecycle

```
pending → in_progress → completed
                      ↘ archived ⇢ (unarchive) back to pending / in_progress / completed
pending →                archived
```

An autopilot plan stays `in_progress` while its run is `finalizing` and then
`awaiting_merge` (final PR green, waiting for you to merge it). It becomes `completed`
only when that PR is observed merged.

- **`pending`** — authored, not started; **definition is mutable** (`wd plan update` / `edit` / `task`, or the matching API)
- **`in_progress`** — execution active (or was; stays until completed/archived); definition is **immutable** (409 Conflict)
- **`completed`** — all tasks done, code merged (for autopilot: the final PR is merged and the integration branch deleted; `wd plan show` records how it ended); definition immutable
- **`archived`** — de-prioritised or superseded; definition immutable. A plan whose executor is still live is refused (409; `wd plan stop` first); branches with unmerged commits are kept. `wd plan unarchive` restores the status it was archived from

Drive transitions with `wd plan run` / `wd plan complete` / `wd plan archive` / `wd plan unarchive` (or the matching MCP/API). `wd plan delete` removes a plan that is not `in_progress`. The old `plan scan` / `status` / `import` / `sync_to_repo` commands remain only as hidden aliases. See [Using plans](/warden/guides/using-plans/) for the full journey. See [Plan Modification API](https://github.com/srjn45/warden/blob/main/docs/specs/2026-10-04-plan-modification-api.md) for mutation contracts (DAG validation, optimistic concurrency).

## Stable plan identity

```
ID = "plan-" + hex(sha256(projectID + "\x00" + planName))[:8]
```

Identity is preserved across legacy import so execution links survive cutover.

## Execution modes

When a plan is run with `wd plan run <id> --mode <mode>`, warden starts from the **ScrivaDB definition** (snapshot-at-start) and links an execution entity.

The **task list is always a DAG** (`after:` edges). Multi-task plans without explicit edges are auto-chained in declaration order on create/update. Every mode honors that DAG:

| Mode | What warden creates | How the DAG is enforced |
|---|---|---|
| `autopilot` | Live Autopilot + manager Agent (`PlanID` required) | Brain digest lists `after:`; only ready tasks should be spawned |
| `pipeline` | Pipeline `P:<plan-name>` (one job per task ID + `depends_on`) | Executor spawns only when dependencies are done |
| `orchestrator_worker` | Agent `O:<plan-name>` (`role=orchestrator`) | Prompt lists ready vs blocked; `wd plan task status` gated on deps |
| `manual` | Agent `M:<plan-name>` (`role=general`) | Same ready/blocked prompt + task-status gating |

Watch a running plan with `wd plan show <id> --watch` (executor state, backoff, integration branch, per-task worker/PR). Completion for autopilot/pipeline is watched by the daemon; orchestrator/manual complete via `wd plan complete <id>`.


## Optional replica export

`wd plan sync-to-repo` renders a revision onto a dedicated `warden/plan-sync/...` branch and opens/updates a PR. YAML is the default; pass `--format json` for an opt-in JSON replica (same envelope fields; top-level `"warden_plan_export":"replica only — not authoritative"`; path `plans/{lifecycle}/<slug>.json`). Editing that replica does nothing to listing or execution. `import-legacy` discovers **YAML only** — JSON files are never loaded as execution authority ([#585](https://github.com/srjn45/warden/issues/585)).

## Restarting an executor

`stop` is not terminal for the plan: an `in_progress` autopilot or pipeline plan
can be brought back with `wd plan restart` (destructive; `--yes`; `--force` for a
healthy executor). Agents and worktrees are replaced; task progress, landings and
branches with commits are kept, and the new agents receive a restart context. The
autopilot [progress watchdog](/warden/concepts/autopilot/#guardian) parks a
heartbeating-but-stalled run as `no_progress` and points at this command. See
[Recovering a stuck plan](/warden/guides/autopilot/#recovering-a-stuck-plan).

## Recovery

Prefer `wd plan backup export` / `wd plan backup restore` — Plans are operable without Git. See [Plan backup and restore](/warden/guides/plan-backup-restore/) and the [migration playbooks](/warden/guides/plans-migration/).

## Deferred follow-ups

Explicitly out of scope for the ScrivaDB cutover (not hidden non-goals):

- **Hub network transport** — opt in with `plan_sync.provider: hub`; explicit Push, Pull, and Discover calls transport canonical envelopes. The cockpit shows Hub Discover results as a read-only **Remote Plans** section, scoped to each project and limited to pending/in-progress work. Default local installs make no network calls and show no remote entries.

**Closed follow-up:** JSON export format ([#585](https://github.com/srjn45/warden/issues/585)) — shipped as opt-in inert replicas; YAML remains the default. See Phase 12 acceptance § Follow-up #585.

Phase 12 acceptance evidence lives in the repo at `docs/specs/2026-09-30-scrivadb-canonical-plans-acceptance.md`.

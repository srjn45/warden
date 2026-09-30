---
title: Plans
description: ScrivaDB-canonical Plans — definition, lifecycle, revision, optional inert YAML replicas, and execution modes.
---

Plans are **first-class ScrivaDB records**: goal, task DAG, lifecycle status, revision, and execution evidence live in the daemon store. Repository files under `plans/**/*.yaml` are an **optional inert export** for review — they are never required to create, list, run, or complete a Plan, and editing them does not change canonical execution.

Design freeze: [`docs/specs/2026-09-30-scrivadb-canonical-plans.md`](https://github.com/srjn45/warden/blob/main/docs/specs/2026-09-30-scrivadb-canonical-plans.md).

## Canonical architecture

| Concern | Authority |
|---|---|
| **Definition** (`goal`, `tasks`, `constraints`, `done_when`) | ScrivaDB Plan record |
| **Lifecycle** (`pending` → `in_progress` → `completed` / `archived`) | ScrivaDB `Status` field |
| **Revision / content hash** | ScrivaDB (`revision`, `content_hash`) |
| **Execution evidence** | ScrivaDB events, summaries, task progress |
| **Repository YAML** | Optional replica via `wd plan sync_to_repo` — inert unless an explicit `import-legacy` is invoked |

The daemon does **not** scan `plans/` on startup. Implicit directory-as-status and "YAML is the source of truth" are retired.

## Lifecycle

```
pending → in_progress → completed
                      ↘ archived
pending →                archived
```

- **`pending`** — authored, not started
- **`in_progress`** — execution active (or was; stays until completed/archived)
- **`completed`** — all tasks done, code merged
- **`archived`** — de-prioritised or superseded

Drive transitions with `wd plan run` / `wd plan complete` / `wd plan archive` (or the matching MCP/API). Deprecated `wd plan scan` / `wd plan status` / `wd plan import` remain for one release as migration aids and **cannot affect canonical execution after import**.

## Stable plan identity

```
ID = "plan-" + hex(sha256(projectID + "\x00" + planName))[:8]
```

Identity is preserved across legacy import so execution links survive cutover.

## Execution modes

When a plan is run with `wd plan run <id> --mode <mode>`, warden starts from the **ScrivaDB definition** (snapshot-at-start) and links an execution entity:

| Mode | What warden creates | Completion |
|---|---|---|
| `autopilot` | Live Autopilot + manager Agent (`PlanID` required) | Daemon watches run `completed` |
| `pipeline` | Pipeline `P:<plan-name>` (one job per task ID + deps) | Daemon watches pipeline `done` |
| `orchestrator_worker` | Agent `O:<plan-name>` (`role=orchestrator`) | `wd plan complete <id>` |
| `manual` | Agent `M:<plan-name>` (`role=general`) | `wd plan complete <id>` |

## Optional replica export

`wd plan sync_to_repo` renders a revision onto a dedicated `warden/plan-sync/...` branch and opens/updates a PR. Editing that YAML does nothing to listing or execution until you deliberately run `wd plan import-legacy`.

## Recovery

Prefer `wd plan backup export` / `wd plan backup restore` — Plans are operable without Git. See [Plan backup and restore](/warden/guides/plan-backup-restore/) and the [migration playbooks](/warden/guides/plans-migration/).

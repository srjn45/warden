---
title: Plans
description: Two-layer architecture behind warden's tracked plans — git-directory state encoding, ScrivaDB execution state, the state machine, and execution modes.
---

Plans are goal-oriented YAML files in your repo that warden turns into **first-class tracked entities** — visible in every UI, linked to their execution (autopilot runs, pipelines, orchestrators), and fully recoverable after a reinstall.

## Two-layer architecture

Plans use two storage layers with different responsibilities:

| Layer | What lives here | Source of truth |
|---|---|---|
| **Plan definition** | `goal`, `tasks`, `constraints`, `done_when` | YAML file content in the repo |
| **Plan status** | Which directory the YAML lives in | Git (directory placement) |
| **Execution state** | Linked run/pipeline IDs, task progress, timestamps | ScrivaDB `plans` collection |

The key design decision is that **status is directory-placement** — the file's location inside `plans/{pending,in_progress,completed,archived}/` is the authoritative status. This means:

- Team members who don't use warden can see plan status from `git log`
- Plans recover perfectly from a DB wipe by re-scanning the directory layout
- A state transition is a `git mv` — reviewable, revertable, auditable

Execution state (which autopilot run is linked, what tasks are done, when it started) lives in ScrivaDB only. It is optional context layered on top of the git-authoritative status.

## Directory layout

```
plans/
  pending/          # authored but not started
  in_progress/      # execution active (or was active; stays here until completed/archived)
  completed/        # all tasks done, code merged
  archived/         # de-prioritised or superseded
```

The daemon scans these four directories on startup for each registered project, deriving status from directory name. Files in flat `plans/*.yaml` (no subdirectory) are treated as `pending` and can be migrated with `wd plan scan --migrate-flat`.

## Stable plan identity

A plan's stable ID is derived from its **name** (from the YAML `name:` field, or filename stem if absent), not from its file path:

```
ID = "plan-" + hex(sha256(projectID + "\x00" + planName))[:8]
```

Moving a file between status directories does **not** change the plan ID, which means execution links (autopilot run ID, pipeline ID) remain intact across status transitions.

When the daemon sees the same plan name under a different directory, it updates `FilePath` and `Status` in the existing record rather than creating a new one.

## State machine

```
pending → in_progress → completed
                      ↘ archived
pending →                archived
```

- **`pending`** — plan authored, not yet started
- **`in_progress`** — execution active (or was active; file stays here until completed or archived)
- **`completed`** — all tasks done, code merged to main
- **`archived`** — de-prioritised or superseded; hidden from default TUI views

Any state can transition to `archived`. `completed` and `archived` cannot move back to `in_progress` without an explicit reset. The daemon performs the `git mv` atomically with the DB update when `wd plan status` is called.

## Execution modes

When a plan is run with `wd plan run <id> --mode <mode>`, warden creates an execution entity and links it to the plan record. Mode is stored in ScrivaDB only (not in the YAML, not in the directory name) and set at start time:

| Mode | What warden creates | Completion detection |
|---|---|---|
| `autopilot` | Creates a live Autopilot + manager Agent (`PlanID` required); workers parented via `ParentID` | Daemon watches for run `completed` event → auto git-mv to `completed/` |
| `pipeline` | Creates a pipeline named `P:<plan-name>` (one job per YAML task, same IDs + deps); job lifecycle updates Plan evidence | Daemon watches for pipeline `done` event → auto git-mv to `completed/` |
| `orchestrator_worker` | Spawns `O:<plan-name>` (`role=orchestrator`, `PlanID`); workers are `role=worker` with `ParentID` set | Manual: `wd plan complete <id>` |
| `manual` | Spawns `M:<plan-name>` (`role=general`, `PlanID`); no Autopilot | Manual: `wd plan complete <id>` |

A plan with no execution mode is treated as `manual`.

## The plan data model

The daemon stores each plan as a `Plan` record in the ScrivaDB `plans` collection:

```
Plan {
  id              string          // plan-<8hex>, stable across moves
  project_id      string
  name            string          // from YAML name: field or filename stem
  file_path       string          // current path relative to project root
                                  // e.g. "plans/in_progress/brain-consult.yaml"
  status          string          // pending|in_progress|completed|archived
  execution_mode  string          // autopilot|pipeline|orchestrator_worker|manual (DB only)

  // At most one execution link is set at a time
  autopilot_run_id string
  pipeline_id      string
  orchestrator_id  string         // agent ID of the orchestrator

  // Per-task progress: task_id → "pending"|"in_progress"|"done"|"skipped"
  // Informational; populated by wd plan assess or manually
  task_progress   map[string]string

  created_at      time
  updated_at      time
  started_at      time (optional)
  completed_at    time (optional)

  // Hub sync seam — reserved for future warden-hub sync; OOS
  synced_at       time (optional)
  remote_id       string
}
```

## Brain-assisted progress assessment

`wd plan assess <plan-id>` reconstructs `task_progress` after a reinstall or when progress is otherwise unknown:

1. Reads the plan YAML — extracts task IDs and descriptions
2. Calls the Consultor (the same `internal/brainconsult` used by pipeline stuck-recovery) with intent `plan_progress_assessment`, the task list, recent `git log --oneline origin/main -50`, and open PR titles as evidence
3. Updates `task_progress` in the DB record with the brain's assessment

The brain reads git and PR metadata only — it does **not** make code changes. This is opt-in and never automatic.

## Recovery ladder

After a reinstall, plan **status** is perfectly recovered from git (the directory layout is committed). The only gap is task-level progress for `in_progress` plans:

| Scenario | Recovery |
|---|---|
| Same machine, DB intact | Normal operation |
| Same machine, DB wiped | `wd plan scan` re-seeds all plans with correct status |
| New machine / reinstall | `git pull` → daemon auto-scans → plans appear with correct status |
| In-progress task progress lost | `wd plan assess <plan-id>` reconstructs from git/PRs |
| Full backup + restore | `wd snapshot restore` restores ScrivaDB including execution links |

## Finalization and ExecutionSummary

Completing a plan (`wd plan complete`) is daemon-owned: reconcile observed Git /
GitHub evidence, seal the active execution, reduce an immutable
`ExecutionSummary` from typed `PlanExecutionEvent`s, then tear down disposable
executors. The summary and event ledger stay on the Plan — deleting the Agent,
Pipeline, or Autopilot does not erase audit history.

## Upgrade note

Upgrading from pre-redesign data preserves agents, terminals, archives, project
membership, Plan YAML, and registered autopilot runs (migrated to live
`Autopilot` with required `PlanID` when resolvable). Config `backend_default`
still populates `ai_cli_default` for one release. See FEATURES §38 and the
plan-execution-entity redesign spec for the full migration table.

## Non-goals

The following are intentionally out of scope:

- **Warden-hub plan sync** — the `synced_at`/`remote_id` fields are reserved but not yet implemented
- **Editing or validating plan YAML content** from the daemon — authoring stays in the editor (or `wd plan create`)
- **Per-task execution** — plans run as a whole; `task_progress` is informational only (`wd plan done` / `update_task_status`)

## API surface

Plans are managed via the Plan CRUD REST API:

```
GET    /api/v1/plans?project_id=&status=
POST   /api/v1/plans
GET    /api/v1/plans/{plan_id}
PATCH  /api/v1/plans/{plan_id}
POST   /api/v1/plans/{plan_id}/run
POST   /api/v1/plans/{plan_id}/control
POST   /api/v1/plans/{plan_id}/tasks/{task_id}/status
POST   /api/v1/plans/{plan_id}/complete
POST   /api/v1/plans/{plan_id}/archive
```

Plan lifecycle control (`pause` / `resume` / `stop`) goes through `/control`.
Deprecated `/api/v1/autopilot/runs` register/unregister/retarget aliases remain for
one release; prefer PlanID-based run/control.

A legacy project-scoped surface remains for scan/assess/status:

```
POST   /api/v1/projects/{project_id}/plans/scan
POST   /api/v1/projects/{project_id}/plans/{plan_id}/assess
PATCH  /api/v1/projects/{project_id}/plans/{plan_id}
```

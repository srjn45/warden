# Plans as First-Class Citizens

**Date:** 2026-09-28  
**Status:** Approved design; implementation plan — **authority model superseded**  
**Scope:** Documentation of the design only — no production code changes are approved by this document.

> **Supersession (2026-09-30):** Decisions D1–D3 (YAML definition authority +
> directory-encoded lifecycle) are superseded by
> [`2026-09-30-scrivadb-canonical-plans.md`](./2026-09-30-scrivadb-canonical-plans.md)
> (ScrivaDB-canonical Plans; repository YAML as optional inert export). Retain
> this document as historical context for the legacy `plans/` layout and import
> corpus.

---

## 1. Problem

Warden plans today are YAML files stored in `plans/` inside the repo. They define goals, tasks, constraints, and done-criteria, but warden has no runtime awareness of them:

- No way to list plans in the TUI, CLI, or API
- No lifecycle tracking — a plan is either a file or it isn't
- No link between a plan and its execution entity (autopilot run, pipeline, agent)
- Cross-machine/reinstall recovery is incomplete — status is lost
- Plans do not appear in the project tree alongside agents and terminals

The goal is to make plans a **tracked entity** in the daemon — visible in every UI, linked to their execution, and recoverable — without moving the spec content out of the repo or enforcing a warden-only workflow for team members who don't use warden.

---

## 2. Locked Design Decisions

### D1. Two-layer architecture

| Layer | What lives here | Source of truth |
|---|---|---|
| **Plan definition** | `goal`, `tasks`, `constraints`, `done_when` | YAML file content in repo |
| **Plan status** | Which directory the YAML lives in | Git (directory placement) |
| **Execution state** | Linked run/pipeline IDs, task progress, timestamps | ScrivaDB (`plans` collection) |

Plan YAML files are committed to the repo. Status is encoded in the **directory** they live under — this is the key design choice that makes state fully git-trackable, team-visible, and recoverable without hub sync.

### D2. Directory layout encodes state

```
plans/
  pending/
    feature-x.yaml
    feature-y.yaml
  in_progress/
    brain-consult.yaml
  completed/
    old-feature.yaml
  archived/
    stale-plan.yaml
```

A state transition is a file move (`git mv plans/pending/x.yaml plans/in_progress/x.yaml`) committed to the repo. The daemon scans all four subdirectories and infers status from directory name.

### D3. Plan lifecycle states

```
pending → in_progress → completed
                      ↘ archived
pending →                archived
```

- `pending` — plan authored but not started
- `in_progress` — execution is active (or was active; file stays here until completed/archived)
- `completed` — all tasks done, code merged to main
- `archived` — de-prioritised or superseded; hidden from default TUI views

Transitions: any state can move to `archived`; `completed` and `archived` cannot move back to `in_progress` without an explicit reset.

`wd plan status <plan-id> <new-status>` performs the git mv and creates a commit.

### D4. Execution modes (closed enum)

| Mode | Description |
|---|---|
| `autopilot` | Fully autonomous — registers an autopilot run against the plan |
| `pipeline` | Semi-automatic — creates a pipeline where each task is a job |
| `orchestrator_worker` | Orchestrator+worker with human approval gates at each step |
| `manual` | State tracking only — human drives all prompting |

Execution mode is stored in ScrivaDB only (not in the YAML, not encoded in the directory). It is set when a plan starts (`in_progress`). A plan with no execution mode is `manual`.

### D5. Stable plan identity

The plan's stable ID is derived from its **name** (from the YAML `name:` field, or filename stem if absent), not its file path. This ensures that moving a file between status directories does not break the link between a plan and its execution entity (autopilot run ID, pipeline ID, etc.).

```
ID = "plan-" + hex(sha256(projectID + "\x00" + planName))[:8]
```

When the daemon sees the same plan name under a different status directory (post-move), it updates `FilePath` and `Status` in the existing record rather than creating a new one.

### D6. Plan data model

```go
type PlanStatus string

const (
    PlanStatusPending    PlanStatus = "pending"
    PlanStatusInProgress PlanStatus = "in_progress"
    PlanStatusCompleted  PlanStatus = "completed"
    PlanStatusArchived   PlanStatus = "archived"
)

type PlanExecutionMode string

const (
    PlanModeAutopilot          PlanExecutionMode = "autopilot"
    PlanModePipeline           PlanExecutionMode = "pipeline"
    PlanModeOrchestratorWorker PlanExecutionMode = "orchestrator_worker"
    PlanModeManual             PlanExecutionMode = "manual"
)

type Plan struct {
    ID        string `json:"id"`         // plan-<8hex>, stable across moves
    ProjectID string `json:"project_id"`
    Name      string `json:"name"`       // from YAML name: field or filename stem
    FilePath  string `json:"file_path"`  // current path relative to project root
                                         // (updated on status move; e.g. "plans/in_progress/brain-consult.yaml")

    Status        PlanStatus        `json:"status"`          // inferred from directory
    ExecutionMode PlanExecutionMode `json:"execution_mode,omitempty"` // DB only

    // At most one execution link is set at a time
    AutopilotRunID string `json:"autopilot_run_id,omitempty"`
    PipelineID     string `json:"pipeline_id,omitempty"`
    OrchestratorID string `json:"orchestrator_id,omitempty"` // agent ID of the orchestrator

    // Per-task progress: task id -> "pending"|"in_progress"|"done"|"skipped"
    // Informational only; populated manually or by brain assess (D10)
    TaskProgress map[string]string `json:"task_progress,omitempty"`

    CreatedAt   time.Time  `json:"created_at"`
    UpdatedAt   time.Time  `json:"updated_at"`
    StartedAt   *time.Time `json:"started_at,omitempty"`
    CompletedAt *time.Time `json:"completed_at,omitempty"`

    // Hub sync seam — OOS; reserved for future warden-hub sync
    SyncedAt *time.Time `json:"synced_at,omitempty"`
    RemoteID string     `json:"remote_id,omitempty"`
}
```

### D7. Scan and import

`wd plan scan [--project <id>]` walks `plans/{pending,in_progress,completed,archived}/*.yaml` and upserts:
- Derives plan name from YAML `name:` field or filename stem
- Computes stable ID from project + name
- If no record exists → create with status inferred from directory
- If record exists → update `FilePath` and `Status` only; never overwrite execution links or task progress
- Files outside these four dirs are ignored (flat `plans/*.yaml` files still work for authoring — they appear as `pending` implicitly, or the user migrates them with `wd plan scan --migrate-flat`)

Auto-scan on daemon start for each project (directory walk only, no YAML parsing beyond the `name:` field).

### D8. Recovery ladder

| Scenario | Recovery |
|---|---|
| Same machine, DB intact | Normal operation |
| Same machine, DB wiped | `wd plan scan` re-seeds all plans with correct status from directory |
| New machine / reinstall | `git pull` → daemon start auto-scans → plans visible with correct status |
| In-progress task progress | `wd plan assess <plan-id>` runs a brain to reconstruct task progress from git/PRs |
| Full backup + restore | `wd snapshot restore` restores ScrivaDB including execution links |

After a reinstall, status is recovered perfectly from git. The only missing piece is **task-level progress** for `in_progress` plans, which `wd plan assess` handles on demand.

### D9. Brain-assisted progress assessment

`wd plan assess <plan-id>` (also `wd plan scan --assess` to run for all in_progress plans):

1. Reads the plan YAML — extracts task IDs and descriptions
2. Calls `Consultor.Consult` (reusing `internal/brainconsult`) with:
   - Intent: `"plan_progress_assessment"`
   - Situation: plan name + task list
   - Evidence: recent `git log --oneline origin/main -50`, open PR titles, any existing task progress
   - Allowed actions: `update_task_progress` (returns a map of task_id → status)
3. Updates `task_progress` in DB with the brain's assessment
4. Brain does **not** make code changes — it reads git/PR metadata only

This is opt-in. It is never run automatically (spawning a brain on every daemon start or scan is too expensive and surprising).

### D10. Flat-plan migration

Existing repos with `plans/*.yaml` (flat, no subdirectory) are handled by:
- Daemon start scan: flat files appear as `pending` (correct default)
- `wd plan scan --migrate-flat`: moves all flat `plans/*.yaml` files to `plans/pending/` and commits

Users can also manually `git mv` their plans into the right subdirectory before scanning.

### D11. API surface (spec-first)

All routes go into `openapi.yaml` first; `make generate` produces `internal/daemon/oapi/`.

```
GET    /api/v1/projects/{project_id}/plans
POST   /api/v1/projects/{project_id}/plans
GET    /api/v1/projects/{project_id}/plans/{plan_id}
PATCH  /api/v1/projects/{project_id}/plans/{plan_id}
DELETE /api/v1/projects/{project_id}/plans/{plan_id}
POST   /api/v1/projects/{project_id}/plans/scan
POST   /api/v1/projects/{project_id}/plans/{plan_id}/assess
POST   /api/v1/projects/{project_id}/plans/{plan_id}/run
```

`DELETE` removes the DB record only — it never touches the YAML file.
`POST /scan` walks the directory and upserts; accepts `{migrate_flat: bool, assess: bool}`.
`POST /assess` triggers brain progress assessment for one plan.
`POST /run` starts execution in the chosen mode.

### D12. CLI surface

```
wd plan list    [--project <id>] [--status pending|in_progress|completed|archived]
wd plan show    <plan-id>
wd plan import  <file>           # copies file into plans/pending/ and scans
wd plan scan    [--project <id>] [--migrate-flat] [--assess]
wd plan status  <plan-id> <new-status>   # git mv + commit + DB update
wd plan archive <plan-id>                # shorthand for status → archived
wd plan assess  <plan-id>               # brain-based task progress recovery
wd plan run     <plan-id> --mode <mode>  # Phase 5
```

### D13. MCP tools

```
mcp__warden__list_plans           (project_id, optional status filter)
mcp__warden__get_plan             (plan_id)
mcp__warden__create_plan          (project_id, name, file_path)
mcp__warden__scan_plans           (project_id, optional migrate_flat, assess)
mcp__warden__update_plan_status   (plan_id, status)   # git mv + commit + DB update
mcp__warden__archive_plan         (plan_id)
mcp__warden__assess_plan          (plan_id)
mcp__warden__run_plan             (plan_id, mode)     # Phase 5
```

### D14. TUI project tree placement

Plans appear **above agents** in the project tree:

```
▼ my-project
  ▼ Plans
    ▶ In Progress  (1)
      · brain-consult
    ▶ Pending      (2)
      · feature-x
      · feature-y
    ▶ Completed    (3)
    ▶ Archived      (hidden by default, expand with key)
  ▼ Agents
    · agent-1
  ▼ Terminals
    · term-1
```

- Collapsed status groups show count badges
- `Archived` is collapsed by default, no badge
- Selecting a plan opens a detail pane: name, file path, status, execution mode, linked IDs, task progress, timestamps
- Keybindings in plan context: `a` to archive, `s` to scan, `A` to assess (brain), `r` to run (mode picker), `enter` to open detail pane

### D15. Execution mode wiring (Phase 5)

| Mode | What `wd plan run` does |
|---|---|
| `autopilot` | Calls `register_autopilot_run` with plan `file_path`; stores `AutopilotRunID`; sets status `in_progress`; git-mv to `plans/in_progress/` |
| `pipeline` | Creates a pipeline per YAML task; stores `PipelineID`; sets status `in_progress`; git-mv |
| `orchestrator_worker` | Spawns orchestrator agent with plan as context; each worker requires human approval; stores `OrchestratorID`; git-mv |
| `manual` | git-mv to `plans/in_progress/`; no execution entity |

Completion detection:
- `autopilot`: daemon watches for run `completed` event → git-mv to `plans/completed/` + mark completed
- `pipeline`: daemon watches for pipeline `done` event → git-mv to `plans/completed/` + mark completed
- `orchestrator_worker` / `manual`: user calls `wd plan status <id> completed`

### D16. Non-goals

- Warden-hub plan sync (deferred; `synced_at`/`remote_id` fields reserved)
- Editing or validating plan YAML content from the daemon
- Creating a plan YAML from the CLI (authoring stays in the editor)
- Per-task execution (plans run as a whole; task progress is informational)

---

## 3. Done-When

- `plans/{pending,in_progress,completed,archived}/` directory layout is the canonical state encoding.
- `wd plan scan` infers status from directory placement; idempotent; preserves execution links.
- A `plans` ScrivaDB collection exists with full CRUD.
- `GET /api/v1/projects/{id}/plans` returns plans grouped by status.
- `wd plan status` performs git-mv, commits, and updates DB atomically.
- `wd plan list/show/import/scan/status/archive/assess` all work end-to-end.
- MCP tools `list_plans`, `get_plan`, `create_plan`, `scan_plans`, `update_plan_status`, `archive_plan`, `assess_plan` available to agents.
- TUI project tree shows Plans above Agents, grouped by status, with detail pane.
- `wd plan run --mode autopilot|pipeline|orchestrator_worker|manual` links plan to execution entity and git-mvs to `in_progress/`.
- Completion events from autopilot/pipeline auto-advance plan to `completed/` (git-mv + DB update).
- `wd plan assess` uses `internal/brainconsult` to reconstruct task progress from git/PRs.
- `make verify-fast` passes on main after each phase.

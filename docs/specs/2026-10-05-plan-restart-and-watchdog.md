# Plan Restart & Progress Watchdog

**Date:** 2026-10-05  
**Status:** Implemented (t2–t6 landed on `autopilot/plan-restart-and-watchdog`); reconciled with the shipped code and docs in t7  
**Feature branch:** `autopilot/plan-restart-and-watchdog`  
**Docs:** [Recovering a stuck plan](https://srjn45.github.io/warden/guides/autopilot/#recovering-a-stuck-plan), `docs/USAGE.md` §18, `docs/specs/autopilot.md` §2.3.  
**Depends on:** `plan-6c78ea4d` (autopilot-self-heal-fixes) and `plan-a08816a9`
(plan-execution-cli) — reuse their failure classification, needs-attention
condition, and `PlanExecutorStatus` block; do not reinvent them.

---

## Problem

Today an operator who stops a stuck plan (`wd plan stop`) has no path back to a
live executor: `wd plan resume` refuses a stopped run, and `wd plan run` requires
a pending plan. Autopilot's guardian only escalates on a **stale manager
heartbeat** (`superviseRun` in `internal/autopilot/guardian.go`). A manager that
is alive but idle — looping, waiting forever, making no task progress — is never
escalated. Pipeline cancel is likewise terminal: `Executor.Resume` only accepts
`StatusPaused`, so a canceled plan-bound pipeline cannot be reopened.

Operators need (1) an explicit, work-preserving **restart** that tears down old
agents and starts a new set with enough context to continue, and (2) a
**progress watchdog** that walks the existing heal ladder when a live run has
made no progress for a generous window.

---

## Goals

1. **`wd plan restart <plan-id>`** (CLI + REST + MCP) for `in_progress` plans in
   **autopilot** or **pipeline** mode whose executor is stopped, degraded,
   healing, or parked (needs-attention); optionally force-restart an active run.
2. Restart **never loses work**: plan stays `in_progress`; task progress,
   integration branch, merged landings, and any task branch with commits beyond
   the integration branch are preserved. Only agent sessions and their worktrees
   are removed.
3. Restart creates a **completely new set of agents** — no reuse of old manager,
   workers, or pipeline job agents.
4. New agents receive a durable **Restart context** (finished work, kept
   branches/PRs, why the previous run ended, bounded decision journal).
5. **Guardian progress watchdog** escalates when a live run has no progress and
   no working agent for a configurable window, without double-firing against the
   heartbeat ladder or overwatch in the same grace window.
6. Spec-first daemon API; CLI/MCP parity; audit + `wd plan show` visibility.

## Non-Goals

- Editing a running plan's definition.
- Restart for `orchestrator_worker` or `manual` modes (refuse with a clear
  message).
- Changing heal-ladder order (`nudge → restart → rotate → backoff`).
- Changing `pipeline resume` / `pipeline retry` public semantics (restart may
  add a **restart-only** reopen path for canceled pipelines).
- Replacing guardian HotSwap manager-restart with operator restart (they are
  different operations — see §3).

---

## Authority & reuse

| Source | Role |
|---|---|
| ScrivaDB `planstore` | Canonical plan definition and `TaskProgress`; never read repo YAML for a plan with an id |
| Autopilot ledger (`autopilot.<run_id>.{tasks,landings,journal,integration_branch}`) | Live task/landing/journal state under shared context |
| `PlanBoundRunID(repo, planID)` | Stable run id — **reused** across restart |
| `cleanupPlanExecutors` / `TeardownLive` | Reference teardown patterns; restart must **not** delete the plan or (for autopilot) lose the run identity |
| `PlanExecutorStatus` (`plan_executor_status.go`) | Executor kind/state surfaced on `GET /plans/{id}` and `wd plan show` |
| Self-heal failure kinds / `NeedsAttention` | Parked reason text and classification reused for restart-context "why" |

---

## 1. Preconditions and refusal matrix

### 1.1 Allowed without `--force`

| Dimension | Allowed values |
|---|---|
| Plan status | `in_progress` only |
| Execution mode | `autopilot`, `pipeline` |
| Autopilot run state | `stopped`, `degraded` (including parked `needs_attention`), `healing` |
| Pipeline status | `canceled`, `stalled`, or any state where the pipeline is not actively making progress **and** there is no live job agent in `working`/`spawning` — concretely after `plan stop` (`canceled`), and when jobs are failed/needs-attention with no running agents. **Open question OQ-1** formalizes "degraded pipeline" if needed. |

### 1.2 `--force` additionally allows

| Dimension | Extra values |
|---|---|
| Autopilot | `active`, `starting`, `paused` |
| Pipeline | `running`, `paused` (terminates live job agents first) |

### 1.3 Refusal matrix (typed errors → HTTP)

| Condition | HTTP | Error token / message |
|---|---|---|
| Plan not found | 404 | `plan not found` |
| Status `pending` / `completed` / `archived` | 409 | `plan is not in_progress` |
| Mode `orchestrator_worker` or `manual` | 409 | `restart is not supported for <mode> plans; use stop and a new run path` |
| No bound executor (no `AutopilotRunID` / `PipelineID`) | 409 | `plan has no active executor to restart` |
| Autopilot `active`/`starting`/`paused` without force | 409 | `executor is healthy and active; pass --force to restart` |
| Pipeline `running`/`paused` with live working agents, without force | 409 | same force message |
| Autopilot `complete` | 409 | `complete run is terminal` |

`paused` without `--force` is refused: pause is intentional operator hold;
resume remains the undo. Force-restart from paused is allowed when the operator
explicitly wants a fresh agent set.

---

## 2. Run identity (autopilot)

| Artifact | Across restart |
|---|---|
| Autopilot run id | **Reuse** — `PlanBoundRunID(repo, planID)` is deterministic |
| Manager slot / `slotScope` | **Reuse** — spawn into the same manager slot (`StartFromPlan` / slot adoption path) |
| Integration branch | **Reuse** — ledger `integration_branch` and run field unchanged |
| Plan ScrivaDB row | **Unchanged** — stays `in_progress`; `TaskProgress` for done tasks unchanged |
| Ledger `landings` | **Preserved** |
| Ledger tasks with state `landed` | **Preserved** |
| Ledger tasks not `landed` | **Reset** to `pending`; clear `worker_id`; keep `branch`/`pr` in restart context (not necessarily on the reset row — see §4) |
| Heal ladder / backoff / `tried` / `needsAttention` | **Cleared** (`recover`-equivalent + `clearParked`) |
| Shared-context restart blob | **Written** before teardown; survives daemon restart |

Restart must **not** call `TeardownLive` as-is: that deletes the live Autopilot
store row and drops the in-memory run. Implement a dedicated
`Controller.RestartRun` (name TBD) that stops agents while keeping the run
record, or stop+respawn under the same id without `live.Delete`.

---

## 3. Teardown sequences

Operator restart is **not** guardian stage-2 HotSwap (`rotateBrain` / in-place
manager swap). It terminates **all** run-tagged agents and their worktrees, then
spawns a new manager (and later new workers).

### 3.1 Autopilot — exact order

Hold `Controller.mu` consistently with `Enable` / `guardianTick` for the critical
section so the guardian cannot respawn a manager mid-restart.

1. **Validate** preconditions (§1); resolve run via `p.AutopilotRunID`.
2. **Capture restart context** (§4) from ledger, landings, journal, run state,
   agent branches/PRs — **before** any terminate.
3. **Persist** restart context to shared context
   (`autopilot.<run_id>.restart_context`).
4. **Enumerate** agents with tag `run:<run_id>` (manager + workers).
5. For each agent: `Lifecycle.Terminate` → mark done →
   `RemoveWorktree(..., force=true, deleteAdoptedBranch=false)` — worktree gone,
   branch decision in step 6.
6. **Branch policy** (per unfinished task branch / worker branch):
   - If branch has commits **not reachable from** the integration branch →
     **keep** local (and remote if present); leave any open PR open.
   - If branch has **no** commits beyond the integration branch → **delete**
     local branch (and remote if warden created it); no PR to close.
   - Open PRs are **never** auto-closed by restart.
7. **Reset ledger** unfinished tasks → `pending` (preserve landed rows +
   landings list).
8. **Clear** heal stage, backoff fields, tried-backend set, `needsAttention`,
   `parkedPlanKey`.
9. **Hydrate** plan definition from ScrivaDB (`planSource` /
   `ExecutionSnapshot`) — never from repo YAML.
10. **Select backend** (optional override from request) and **spawn** a fresh
    manager into the same slot with digest including restart context.
11. Set run `active`; persist; audit (§7).

Idempotency: re-issuing restart after a mid-way failure re-runs teardown
(idempotent terminate/archive/missing-ok) then spawn. Context capture may refresh
from whatever agents still exist.

**Functions to add/change:**

- New: `internal/autopilot/restart.go` — `(*Controller).RestartRun`
- New: `internal/autopilot/restart_context.go` — assemble + persist + render
- Change: `internal/daemon/plan_restart.go` (or extend `plan_control.go`) — HTTP
  handler
- Reuse patterns from: `stopRunLocked`, `teardownPlanAgent`,
  `planBoundAgents`, `cleanupPlanExecutors` (but keep run + plan)

### 3.2 Pipeline — exact order

1. Validate (§1); resolve `p.PipelineID`.
2. Capture restart context from pipeline jobs (done handoffs, failed/skipped
   branches/PRs, cancel reason).
3. Terminate every live job agent; remove worktrees (same branch policy as §3.1
   step 6, base = job's base / integration Branch).
4. **Keep** jobs in `done` with recorded emit/handoff outputs untouched.
5. **Reset** to `pending` (clear agent binding): `failed`, `needs_attention`,
   `skipped`, `canceled`-implied unfinished, and any non-done unfinished status
   (`pending` already ok; `running` → terminate then pending).
6. Record kept branch per reset job for worktree base on next spawn.
7. Attach restart context fragment to each reset job's prompt composition path
   (`pipeline/compose.go` or plan-pipeline adapter).
8. **Reopen** pipeline: set status from `canceled` → `running` via a
   **restart-only** internal helper (e.g. `Executor.ReopenForRestart`). Do **not**
   change `Resume` (still paused-only) or `Retry` (still failed/needs-attention
   single-job).
9. `Reconcile` so ready jobs spawn; upstream handoffs of done jobs are
   re-delivered by existing compose/Plan logic.
10. Sync plan `TaskProgress` with job state after reset.
11. Audit (§7).

**Functions to change:**

- `internal/daemon/executor.go` — `ReopenForRestart` (new)
- `internal/daemon/plan_pipeline_adapter.go` / `plan_agent_adapters.go` — prompt
  injection hooks as needed
- `internal/pipeline/compose.go` — optional restart section in composed prompt

---

## 4. Restart context

### 4.1 Fields

```go
type RestartContext struct {
    RestartCount   int       `json:"restart_count"`
    RestartedAt    time.Time `json:"restarted_at"`
    Reason         string    `json:"reason"` // enum-ish free text; see below
    ReasonKind     string    `json:"reason_kind,omitempty"` // operator_stop | needs_attention | degraded_backoff | operator_force | watchdog | unknown

    FinishedTasks []RestartFinishedTask `json:"finished_tasks"`
    Unfinished    []RestartUnfinishedTask `json:"unfinished_tasks"`
    Journal       []JournalEntry `json:"journal"` // newest first, bounded
}

type RestartFinishedTask struct {
    ID       string `json:"id"`
    PR       int    `json:"pr,omitempty"`
    MergeSHA string `json:"merge_sha,omitempty"`
    Branch   string `json:"branch,omitempty"`
}

type RestartUnfinishedTask struct {
    ID              string `json:"id"`
    PreviousBranch  string `json:"previous_branch,omitempty"`
    HasUnmergedCommits bool `json:"has_unmerged_commits"`
    OpenPR          int    `json:"open_pr,omitempty"`
    PRBase          string `json:"pr_base,omitempty"`
}
```

**Reason sources:**

| Situation | `reason_kind` |
|---|---|
| Prior `StopRun` / plan stop | `operator_stop` |
| `needsAttention` non-empty | `needs_attention` (include parked text) |
| `StateDegraded` with backoff | `degraded_backoff` |
| `--force` while active | `operator_force` |
| Watchdog exhaustion → operator restart | `watchdog` (if last park was no-progress) |

**Journal bound:** last N entries (default **20**, same ballpark as
`digestAuditLimit`); newest first.

### 4.2 Storage

- Key: `autopilot.<run_id>.restart_context` (dot form; shared context / ledger
  `CtxStore`).
- Pipeline plans without an autopilot run id: store under
  `pipeline.<pipeline_id>.restart_context` (or plan-scoped
  `plan.<plan_id>.restart_context` — **OQ-2**).
- Survives daemon restart; cleared only when the plan completes/archives or an
  explicit future clear (not required for v1).

### 4.3 Injection

| Consumer | Where | Format |
|---|---|---|
| Autopilot manager | `ComposeDigest` (`digest.go`) | New `## Restart context` section when key present; no-op when absent |
| Autopilot worker | Spawn prompt / file-backed hints | Same delimited section + instructions below |
| Pipeline job | Composed job prompt | Same section for reset jobs only |

**Explicit instructions in the section:**

1. Finished tasks must not be redone.
2. For an unfinished task with a kept branch: create the worktree **from that
   branch** and continue from existing commits.
3. If an open PR exists for that branch: **reuse it** (push to it); never open a
   duplicate.
4. If kept work is unusable: report that, start again from the integration
   branch, and close the old PR with a comment.

Keep the tmux launch line under the **1024-byte** limit via the existing
file-backed prompt/hints mechanism (`Lifecycle` HintsDir / prompt files) —
restart text must not be inlined on the launch line.

---

## 5. Progress watchdog

### 5.1 Progress definition

Any of the following updates **last progress time** for the run:

1. Ledger task state change (CAS/WriteTasks / WriteTaskState).
2. New landing recorded (`land` handler).
3. Plan `TaskProgress` status change for this plan.
4. Worker (or job agent) **spawned** under the run.

Heartbeat alone is **not** progress. Overwatch nudges are **not** progress.

### 5.2 "An agent is working"

Reuse `isAgentBusy` (`overwatch.go`): poller state `spawning` or `working` on
**any** agent tagged `run:<run_id>` (manager or worker). If any such agent is
busy, the watchdog does **not** escalate.

### 5.3 Config (`autopilot.guardian`)

| Key | Default | Hot-reload |
|---|---|---|
| `progress_watchdog_enabled` | `true` | yes (next tick) |
| `progress_watchdog_window` | `2h` | yes (next tick) |

Wire into `AutopilotGuardianConfig`, defaults, validation, docs, and
`Controller.Reconfigure` / guardian field copy (same path as
`HeartbeatTimeout`).

### 5.4 Escalation mapping

On each `guardianTick` / `superviseRun`, **after** heartbeat handling (or in a
sibling check that shares grace):

Preconditions to consider wedged-by-watchdog:

- Run state is `active` (skip `paused`, `stopped`, `complete`, parked
  `needsAttention`).
- Watchdog enabled.
- `now - lastProgressAt >= window`.
- No run-tagged agent is busy (`isAgentBusy`).

Then treat as wedged and call the **same** `escalate` ladder, but:

- Stage-1 nudge text is watchdog-specific (name stalled unfinished tasks).
- Advance using existing `healNextAt` / `HeartbeatTimeout` grace so heartbeat
  and watchdog cannot both climb a rung in the same grace window (**single
  ladder per run**).
- Any progress event clears watchdog wedge state and calls `recover` (or
  equivalent clear of heal stage when appropriate).

If the ladder is exhausted via watchdog escalations **without** progress, park
as needs-attention with reason kind `no_progress`, notify the operator once,
pointing at `wd plan restart`. Persist `lastProgressAt` on the run record (and/or
ledger key `autopilot.<run_id>.last_progress_at`) so a daemon restart does not
reset or falsely trigger the window.

### 5.5 Surfaces

- `RunStatus`: `last_progress_at`, `watchdog` state (idle / armed / escalating)
  — exact field names in OpenAPI.
- `PlanExecutorStatus` / `wd plan show`: show last progress and restart count /
  last restart reason when present.

**Functions to change:**

- `internal/autopilot/guardian.go` — watchdog check inside/beside `superviseRun`
- `internal/autopilot/controller.go` — persist `lastProgressAt`; touch points on
  land / ledger writes / spawn
- `internal/config/config.go` — new keys + accessors
- Tests with fake clock (see plan task t6)

---

## 6. API, CLI, MCP

### 6.1 REST (spec-first)

Prefer a **dedicated** route (body + slow path) rather than overloading the
path-enum `/{action}`:

```yaml
# internal/daemon/apidocs/openapi.yaml
POST /api/v1/plans/{plan_id}/restart
operationId: RestartPlan
requestBody:
  RestartPlanRequest:
    force: boolean
    backend: string   # autopilot only; optional manager backend override
responses: 200 Plan | 400 | 404 | 409
```

Also update `ControlPlan` description to mention restart exists alongside
pause/resume/stop. Optionally allow `action: restart` as an alias that requires
the same body — **not required** if `RestartPlan` is primary (**OQ-3**).

Add `/restart` to `isSlowPath` (`middleware.go`) and cover in
`middleware_test.go`. Client uses `longTimeout`.

### 6.2 Client

`PlansRestart(ctx, planID, RestartPlanRequest) (*PlanView, error)` in
`internal/client/plans_crud.go`.

### 6.3 CLI

```
wd plan restart <plan-id> [--force] [--backend <id>] [--yes] [--json]
```

- Without `--yes`: print agents to terminate, branches kept vs deleted, ask for
  confirmation; non-interactive without `--yes` → refuse.
- Long help: per-mode teardown/keep semantics; unsupported modes.
- Update `wd plan resume` Long + runtime error when executor is stopped → point
  at `wd plan restart`.
- Update `wd plan stop` Long and root `Typical journey` to name restart as the
  way back.
- `wd plan show`: restart count, last restart reason, last progress / watchdog.

Location: `internal/cli/plan.go` next to `newPlanControlCmd`.

### 6.4 MCP

Add **`restart_plan`** tool (destructive; params `plan_id`, `force`, `backend`)
**or** extend `control_plan` with `action=restart` plus force/backend.
Recommendation: **dedicated `restart_plan`** so `control_plan` stays a simple
enum and schemas stay clear. Document as destructive.

### 6.5 Resume behavior change

When `ResumeRun` / plan resume hits a **stopped** autopilot run (or canceled
pipeline), return 409 with message instructing `wd plan restart` (today:
`cannot resume run in state stopped` / `not paused`). Keep pause→resume
unchanged.

---

## 7. Audit and observability

| Event | When | Detail fields |
|---|---|---|
| `plan_restart` (new `audit.Action…`) | Successful restart | plan_id, mode, reason_kind, force, agents_removed, branches_kept, branches_deleted, backend |
| Existing guardian audit | Watchdog escalations | Distinct detail text including `watchdog` |
| `autopilot.needs_attention` | Watchdog exhaustion park | `no_progress: …; use wd plan restart` |

`wd plan show` / executor status: restart count, last reason, last progress time.

---

## 8. Documentation & codegen

Delivered in t7:

- README, FEATURES, USAGE, `docs/specs/autopilot.md` guardian section
- Site guide "Recovering a stuck plan" + reference
- `skills/warden` Autopilot section
- `make generate` after OpenAPI; `make gendocs` after CLI help changes

---

## 9. Test plan (contract; implemented in t2–t6)

| Area | Cases |
|---|---|
| Autopilot restart | From stopped / degraded backoff / parked / healing; refuse active without force; force from active; old agents gone; kept vs deleted branches; landed ledger preserved; mid-way re-issue |
| Pipeline restart | After plan stop (canceled); mix of done/failed/skipped; handoffs reach dependents; no old agents; kept branch as worktree base |
| Restart context | Assembly unit tests; digest/prompt render; no section when absent |
| Watchdog | Fake clock: busy agent → no escalate; recent progress → no escalate; window expiry → ladder; progress clears; park after exhaustion; persist across controller rebuild |
| API/CLI | Confirmation refuse; `--yes`; resume points at restart; gendocs-check |

---

## Open questions — all resolved

| ID | Resolution (as built) |
|---|---|
| **OQ-1** | Resolved with the proposed default. `restartNeedsForce` (`internal/daemon/executor_restart.go`): no force when no job agent is `working`/`spawning` **and** the pipeline is `canceled`/`stalled`, or every non-done job is `done`/`failed`/`needs_attention`/`skipped`; otherwise `--force`. |
| **OQ-2** | Resolved: pipeline-only plans use `plan.<plan_id>.restart_context` (`PlanRestartContextKey`); autopilot runs use `autopilot.<run_id>.restart_context`. |
| **OQ-3** | Resolved: dedicated `POST /api/v1/plans/{plan_id}/restart` (`RestartPlan`) only; `/{action}` stays `pause\|resume\|stop`. Added to the slow-path set. |
| **OQ-4** | Resolved: `--force` from `paused` restarts and leaves the executor active (new agents; effectively un-pauses). Without `--force` a paused run is refused. |
| **OQ-5** | Resolved: empty task branches are deleted locally, and on the remote only when warden created the branch (`BranchCreated`, best-effort). A branch whose state cannot be determined is kept. PRs are never closed. |
| **OQ-6** | Resolved: only operator/API `RestartPlan` increments `restart_count`; guardian HotSwap does not and writes no restart context. |
| **OQ-7** | Resolved: overwatch nudges are not progress; they do not block the watchdog, and the shared heal grace prevents double ladder steps. |

### Deviations from the original design

- MCP: a dedicated **`restart_plan`** tool (destructive) was added; `control_plan` is unchanged.
- CLI: `wd plan restart <id> [--force] [--backend <id>] [--yes] [--json]`. Without `--yes` it prints the effect and asks; with a non-terminal stdin it refuses.
- Watchdog state values on `RunStatus.watchdog`: `idle`, `armed`, `escalating`, `parked`, `disabled`, plus `last_progress_at`; `wd plan show` prints `last_progress`, `restarts:` (count, last reason, time).
- The watchdog park reuses the needs-attention mechanism with failure kind `no_progress`.
- Docs delivered in t7: README, FEATURES (root + docs), USAGE, `docs/specs/autopilot.md`, site guide *Recovering a stuck plan* with plan/pipeline/concepts pages, and the `skills/warden` Autopilot section and `references/pipelines.md`. The CLI reference is generated (`make gendocs`).

---

## Implementation task map (for follow-on PRs)

Matches the plan decomposition:

1. **t1** — this spec (merge first).
2. **t2** — restart context assemble/persist/inject.
3. **t3** — autopilot `RestartRun` + daemon route.
4. **t4** — pipeline restart + `ReopenForRestart`.
5. **t5** — CLI/MCP/resume messaging/gendocs.
6. **t6** — progress watchdog + config.
7. **t7** — docs/skill; reconcile open questions as resolved.

---

## Definition of done

See plan `done_when`. The feature is code-complete (t2–t6) and documented (t7).

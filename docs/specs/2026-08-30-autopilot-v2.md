# Autopilot V2 — Named, Multi-Instance, Persistent Runs

**Date:** 2026-08-30  
**Status:** Design  
**Supersedes:** `docs/specs/autopilot.md` (v1 — global on/off, single plan per repo)

---

## 0. Motivation

The v1 autopilot design has several hard limitations discovered in production:

1. **Single plan per repo** — the `autopilot on/off` switch is global and permits at most one active run per repo. Running two independent autopilot goals concurrently (e.g., "Project Groups" and "Backend Hardening") requires separate repos or serial execution.
2. **Flat plan file** — `autopilot.plan.yaml` is a single file at the repo root, creating naming collisions when multiple plans are desired.
3. **No per-run lifecycle controls** — there is no way to pause, resume, or stop an individual autopilot run without killing the entire autopilot feature for the repo.
4. **No TUI presence** — the autopilot manager and its workers appear as ordinary agents in the flat agent list, making it impossible to understand what an autopilot run is doing at a glance.
5. **Invisible guardian** — the overwatch/guardian agent cannot be managed if it leaks.

---

## 1. Core Concepts (V2)

| Term | V1 | V2 |
|---|---|---|
| **Plan** | Single `autopilot.plan.yaml` at repo root | Named YAML files anywhere under `plans/` (e.g., `plans/project-groups.yaml`). Registered individually in the daemon store. |
| **Run** | One per repo; keyed by repo path | One per registered plan; keyed by `run_id = sha256(repo + plan_path)`. Multiple runs per repo are now permitted. |
| **Registry** | File sentinel + `config.yaml` `plans[]` list | A proper `AutopilotRunStore` (ScrivaDB collection) persisting run state across daemon restarts. |
| **Lifecycle** | `autopilot on` / `autopilot off` (global) | Per-run `register`, `start`, `pause`, `resume`, `stop` commands. |
| **Guardian/Overlook** | Hidden daemon goroutine + scheduled agent | Promoted to a **demoted-visible** system-tier node under its run in the TUI. |
| **TUI representation** | Flat agent list entry for the brain | First-class `AutopilotRun` node in the TUI, styled like a pipeline, with checklist + nested sub-agents. |

---

## 2. Implementation Phases

### Phase 1 — Named Plan Registry & Multi-Run Store

**Goal:** Replace the single-plan-per-repo model with a proper named plan registry.

**Tasks:**
1. **New `AutopilotRunRecord` type** in `internal/autopilot/store.go`:
   ```
   RunID             string    // sha256(repo+plan_path)
   Name              string    // human-readable, derived from filename (e.g., "project-groups")
   Repo              string    // absolute repo root
   PlanFile          string    // absolute path to the plan YAML
   State             string    // registered|active|paused|complete|stopped
   BrainID           string    // agent id of the current manager
   GuardianID        string    // agent id of the overlook/guardian agent
   IntegrationBranch string
   CreatedAt         time.Time
   PausedAt          time.Time
   CompletedAt       time.Time
   ```
2. **`AutopilotRunStore`** ScrivaDB collection under `<data_dir>/autopilot/runs/`. Replaces the flat file sentinel and the `config.yaml` `plans[]` list.
3. **Remove the "at most one active run per repo" invariant.** Replace with "at most one active run per plan file".
4. **`warden autopilot register <plan-file> [--name <name>]`** — registers a plan, validates it, and persists the record. Does NOT start execution.
5. **`warden autopilot list`** — shows all registered runs for all repos with their current state.
6. **Migration:** `autopilot init` writes plans to `plans/` subdirectory. Old `autopilot.plan.yaml` is auto-migrated on first daemon boot.

---

### Phase 2 — Per-Run Lifecycle Controls

**Goal:** Give each run its own `start`, `pause`, `resume`, `stop` state machine.

**State Machine (per run):**
```
registered ──start──▶ active ──pause──▶ paused ──resume──▶ active
                         │                                      │
                       stop                                   stop
                         ▼                                      ▼
                       stopped                               stopped
active ──all tasks done──▶ complete
```

**Tasks:**
1. **`warden autopilot start <name|run-id>`** — spawns the brain and guardian for this specific run. Runs preflight.
2. **`warden autopilot pause <name|run-id>`** — stops scheduling new workers. In-flight workers complete their current PR.
3. **`warden autopilot resume <name|run-id>`** — guardian resumes. Brain re-reads plan from disk and continues.
4. **`warden autopilot stop <name|run-id>`** — graceful teardown. Record is archived.
5. **OpenAPI-first:** Add `/api/v1/autopilot/runs` (list), `/api/v1/autopilot/runs/{run_id}/start`, `pause`, `resume`, `stop` endpoints.
6. **MCP tools:** `list_autopilot_runs`, `start_autopilot_run`, `pause_autopilot_run`, `resume_autopilot_run`, `stop_autopilot_run`.

---

### Phase 3 — Guardian/Overlook as a Demoted-Visible System Agent

**Goal:** Make the guardian agent inspectable and manageable without cluttering the primary agent view.

**New design:**
1. When `start` spawns a run's brain, it records the guardian's agent ID on `AutopilotRunRecord.GuardianID`.
2. The guardian agent is tagged with `system:true` and `autopilot-run:<run_id>`.
3. **Default TUI view:** guardian is hidden from the flat agent list (filtered by `system:true` tag).
4. **Visible when:** (a) user expands the Autopilot run node in the TUI, or (b) user presses a toggle key (e.g., `S` for "system agents").
5. **Never truly hidden:** `warden ls --all` always lists it. `warden ls` omits it by default.
6. **Auto-cleanup:** When a run completes or is stopped, the guardian is terminated. A daemon-boot reconcile kills any guardian whose run record no longer exists.

---

### Phase 4 — TUI: Autopilot Run as a First-Class Node

**Goal:** Render each autopilot run in the TUI like a named pipeline with a live checklist.

**Mockup:**
```
┌─ Autopilot Runs ───────────────────────────────────────────────┐
│  ▼ project-groups          active   3/5 tasks   antigravity    │
│     ✅ Phase 1: Database & Registry         (landed PR #363)    │
│     ✅ Phase 2: Orchestrator Daemon Loop    (landed PR #364)    │
│     ⏳ Phase 3: Peer Awareness              (worker running)    │
│     ⬜ Phase 4: Delegation Ergonomics                           │
│     ⬜ Phase 5: Cleanup                                         │
│     ├─ 🧠 brain  ap-693ba635498a  working   antigravity        │
│     ├─ 🔧 worker phase3-impl      working   claude             │
│     └─ ⚙️ guardian [system]       idle      (collapsed)        │
│                                                                │
│  ▶ backend-hardening       paused   0/4 tasks                  │
└────────────────────────────────────────────────────────────────┘
```

**Tasks:**
1. **Autopilot section** in the TUI Projects frame. Each run is a collapsible node.
2. **Live checklist:** Brain writes per-task `status` back to the plan file. TUI re-renders in real time via SSE events from the daemon.
3. **Sub-agent nesting:** Agents tagged `autopilot-run:<run_id>` are rendered as children of the run node. Brain 🧠, workers 🔧, guardian ⚙️ (collapsed by default).
4. **Inline run controls:** `P` to pause, `R` to resume, `S` to stop, `Enter` to expand/collapse — scoped to the selected run node.
5. **Web parity:** A new `AutopilotRuns` tab in the web cockpit.

---

### Phase 5 — Brain Plan-File Write-Back Protocol

**Goal:** Brain reliably marks tasks as done in the plan file so the TUI checklist stays current.

**Current state:** `status: complete` is written once at the end of the entire run. Individual task status is not persisted.

**New design:**
1. Extend plan YAML schema: each task gains a `status` field (`pending|active|done|failed`) and a `landed_pr` field.
2. Brain writes task status back via a new MCP tool: `update_task_status { run_id, task_id, status, landed_pr }`.
3. The daemon is authoritative: validates the write (brain cannot claim a task done if the PR hasn't landed) and performs the file mutation atomically.
4. On restart, brain re-reads the plan file to reconstruct progress — the checklist IS the individual-task ledger.

---

### Phase 6 — Config & CLI Cleanup

**Goal:** Remove now-redundant global autopilot config.

**Tasks:**
1. **Deprecate** `autopilot.plans[]` from `config.yaml`. Plans are now registered via `warden autopilot register`.
2. **`warden autopilot init [--name <name>]`** scaffolds `plans/<name>.yaml` (not `autopilot.plan.yaml`) and registers it automatically.
3. **Backwards compat:** On first daemon boot after upgrade, auto-register any `autopilot.plan.yaml` found at repo roots listed in `config.yaml`. Print a migration hint to stderr.
4. **Remove** the deprecated `autopilot.brain.backends` and `autopilot.brain.allow_pay_per_use` config keys.

---

## 3. Done-When Criteria

- Multiple autopilot runs can be active simultaneously in the same repository.
- Each run has its own plan file registered in the daemon's `AutopilotRunStore`.
- `warden autopilot register/start/pause/resume/stop` all work and are reflected in `warden autopilot list`.
- Guardian agents are hidden in `warden ls` by default but always visible with `--all` and in the expanded TUI node.
- TUI renders a live checklist per run, with brain/worker/guardian sub-nodes correctly nested.
- Brain writes per-task `status` back to the plan file after each landed PR.
- `make verify-fast` passes on the integration branch.
- Old `autopilot.plan.yaml` at repo root is auto-migrated on daemon boot with a deprecation warning.

---

## 4. Dependency Order

```
Phase 1 (Store)
    └─▶ Phase 2 (Lifecycle)
            ├─▶ Phase 3 (Guardian visibility)  ─┐
            ├─▶ Phase 5 (Write-back protocol)   ├─▶ Phase 4 (TUI)
            └─▶ Phase 6 (Config cleanup) — last ┘
```

Phases 3 and 5 can be parallelized once Phase 2 is done.

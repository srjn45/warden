# Warden Schema-2 `warden-db` Collection Contract

**Date:** 2026-10-10
**Status:** Spec freeze / Contract — **design only** (Plan `01-warden-db-architecture-and-contract`, Task `contract`)
**Integration branch:** `autopilot/01-warden-db-architecture-and-contract`
**Depends on:** [`2026-10-10-warden-db-data-source-inventory.md`](2026-10-10-warden-db-data-source-inventory.md) (Task `inventory`; collection list in its §1.2/§3)
**Scope:** Canonical collection schemas, primary keys, secondary indexes, ownership rules, referential invariants, archive semantics, and the authoritative-versus-derived field contract for the single schema-2 ScrivaDB instance `<dataDir>/warden-db/`. No production storage path changes in this task.

Keywords **MUST**, **MUST NOT**, **MAY** are normative.

---

## 1. Global Conventions

### 1.1 Records, keys, and types

- A record is a JSON object. Every record has a string primary key stored as `id` unless the collection table says otherwise (composite keys are `:`-joined; see §3).
- Timestamps are RFC 3339 UTC strings (`created_at`, `updated_at`, …). `updated_at` MUST be restamped on every write by the owning store, never by callers.
- Field names are the existing schema-1 `json` names unless §3 states a rename. Unknown fields MUST be preserved on read-modify-write (forward compatibility) but MUST NOT be indexed.
- Empty optional references are **omitted**, never stored as `""`. An index MUST NOT contain the empty string (sparse index).
- Every collection record carries `schema: 2` implicitly through `<dataDir>/schema.json`; individual records do not store a version.

### 1.2 Single-writer ownership

Each collection has exactly **one owning package** (the only code allowed to write it). Other subsystems read through the owner's API or through read-only index queries. Writes that span collections go through the cross-collection transaction facility (§6) invoked by the owner of the *primary* effect; secondary collections are touched only via the owning package's transactional entry points (e.g. `agentstore.TxDelete`), never by direct engine access.

### 1.3 Forbidden: authoritative inverse membership lists

> **A parent record MUST NOT store an authoritative list of its children's ids.**

Every relationship is stored **only** as a foreign key on the child (child → parent). Parent-side membership is derived by an indexed query on the child collection. Concretely the following schema-1 fields are **removed** from schema-2 and MUST NOT be reintroduced, even as "caches", inside a `warden-db` record:

| Removed field | Replaced by (indexed child FK) |
|---|---|
| `Project.agents[]` | `agents.project_id` |
| `Project.pipelines[]` | `pipelines.project_id` |
| `Project.terminals[]` | `terminals.project_id` |
| `Project.plans[]` | `plans.project_id` |
| `Project.autopilots[]` | `autopilot_runs.project_id` |
| `Agent.child_agents[]` | `agents.parent_id` |
| `Agent.child_pipelines[]` | `pipelines.parent_agent_id` |
| `Agent.child_autopilots[]` | `autopilot_runs.parent_agent_id` |
| `ProjectGroup.project_ids[]` | `projects.group_id` (+ `projects.group_order`) |
| `Pipeline.jobs[]` (embedded) | `pipeline_jobs.pipeline_id` |
| `Plan.plan_branches[]` as membership | not a membership list; see §4 (derived/informational only) |

Rationale: inverse lists force read-modify-write of the parent on every child create/delete (lock contention), drift under partial failure, and require two writes to keep one fact. An FK has a single source of truth.

A **derived** read-model (e.g. `ProjectView`, Task 3) MAY materialize children lists in API responses; those are projections, never persisted into the parent record.

Enforcement: the schema validator (§8) rejects any record write whose top-level keys intersect the removed-field set; `warden repair` (Task 4) strips them from imported/legacy data and counts them in its report.

### 1.4 Authoritative vs derived vs observed (field classes)

Every field in every collection is exactly one of:

| Class | Meaning | Writer | Recomputable? | Repair behaviour |
|---|---|---|---|---|
| **A — Authoritative** | The only source of truth for the fact. | Owner package | No | Never overwritten by repair; corruption → quarantine record |
| **D — Derived** | Computed from other authoritative data (same or other collection). | Owner package, at write time, inside the same transaction as its inputs | Yes | Recomputed and overwritten by `repair`; mismatch is a reported inconsistency, not data loss |
| **O — Observed** | Snapshot of an external system's state (tmux, git, CLI provider, OS). Authoritative only as "last seen". | Owner package, reconciler | Re-observable | Replaced on next observation; stale values tolerated |
| **I — Index key** | Field that exists only to serve an index (sparse/normalized copy). | Owner package | Yes | Rebuilt |

Rules:
1. A D field MUST name its inputs in §3 (column "Class/inputs"). A D field MUST NOT be used as an input to an A field.
2. Readers MAY trust A fields without verification, MUST treat D fields as hints when a transaction boundary was crossed (e.g. read from a replica/cache), and MUST treat O fields as potentially stale (compare against `observed_at` where present).
3. Counters, "last N" lists, and denormalized names (`schedule_name`, `Pipeline.name`) are D and are stored only where §3 lists them.

---

## 2. Collection Catalogue

Twenty-six collections. "Archive" refers to §5. Retention classes are those of inventory §2.2.

| # | Collection | Entity (schema-1 source) | Owner package | Key | Archive model |
|---|---|---|---|---|---|
| 1 | `agents` | `agentstore.Agent` | `internal/agentstore` | `id` | status+`archived_at` in-place |
| 2 | `terminals` | `terminalstore.Terminal` | `internal/terminalstore` | `id` | hard delete |
| 3 | `projects` | `projectstore.Project` | `internal/projectstore` | `id` (canonical path/URL) | status `closed` (not deletion) |
| 4 | `project_groups` | `projectstore.ProjectGroup` | `internal/projectstore` | `id` | hard delete |
| 5 | `plans` | `planstore.Plan` | `internal/planstore` | `id` | status `archived` in-place |
| 6 | `plan_exports` | `planexport.Export` | `internal/planexport` | `plan_id` | cascades with plan |
| 7 | `pipelines` | `pipeline.Pipeline` (header) | `internal/pipeline` | `id` | terminal status; pruned |
| 8 | `pipeline_jobs` | `pipeline.Job` | `internal/pipeline` | `pipeline_id:job_id` | follows parent pipeline |
| 9 | `autopilot_runs` | `autopilotstore.Autopilot` + `autopilot.RunRecord` | `internal/autopilotstore` | `id` | terminal status; pruned |
| 10 | `context_entries` | `ctxstore.Entry` | `internal/ctxstore` | `key` | hard delete / TTL |
| 11 | `messages` | `mailbox.Message` | `internal/mailbox` | `to:id` | read-retention compaction |
| 12 | `backends` | `backendstore.Backend` | `internal/backendstore` | `id` | disable, not delete |
| 13 | `backend_models` | `backendstore.ModelEntry` | `internal/backendstore` | `backend_id:model_id` | disable / delete custom |
| 14 | `backend_roles` | `backendstore.RoleTierMapping` | `internal/backendstore` | `role_name` | n/a |
| 15 | `backend_settings` | `Settings`, `HandoverSettings` | `internal/backendstore` | `key` | n/a (singletons) |
| 16 | `usage_snapshots` | `backendusage.UsageSnapshot` | `internal/backendusage` | `domain_key:revision` | TTL 30d |
| 17 | `quota_impact_fences` | `capacity.FenceRecord` | `internal/capacity` | `snapshot_revision:agent_id` | incident-bounded |
| 18 | `snapshots` | `snapshot.Metadata` | `internal/snapshot` | `id` | hard delete (+blob) |
| 19 | `savings_events` | `savings.Event` | `internal/savings` | `id` (monotonic) | none (append-only) |
| 20 | `savings_calibration` | `savings.Calibration` | `internal/savings` | `key` | n/a |
| 21 | `spend_records` | `spend.Entry` | `internal/spend` | `session_id` | day-pruned |
| 22 | `schedules` | `schedule.Schedule` | `internal/schedule` | `id` | hard delete |
| 23 | `known_prompts` | `knownprompts.Entry` | `internal/knownprompts` | `id` (hash) | LRU cap |
| 24 | `plan_sync_envelopes` | `plansync.Envelope` | `internal/plansync` | `scope:plan_id` | superseded in place |
| 25 | `metrics_samples` | `metrics.Sample` | `internal/metrics` | `taken_at:seq` | TTL 14–30d |
| 26 | `id_sequences` | new (monotonic counters) | `internal/warden-db` (engine adapter) | `name` | n/a |

Decisions closing inventory open questions:
- **`archived` agents** are **not** a separate collection: one `agents` collection with `status` + `archived_at` (§5.1). The schema-1 `agents-db/archived` collection is merged on migration.
- **`pipeline_jobs` is a separate collection** (not embedded). This removes the forbidden embedded child list, makes per-job status writes O(1), and lets `agents.job_id`/`pipeline_jobs.agent_id` be checked as FKs.
- **`autopilot_runs`** unifies schema-1 `autopilots` (identity/links) and `runs-db` (run state). Their ids already coincide (`ap-<12hex>`).
- **Project group membership** is `projects.group_id` (a project belongs to zero or one group — existing behaviour), not a list on the group.
- **`id_sequences`** is added to back the monotonic ids needed by `messages` and `savings_events` and to keep them out of any parent record.

---

## 3. Collection Schemas

Notation: field type; **req**/opt; class (**A**/**D**/**O**/**I**); `→ coll.field` marks a foreign key (child → parent). Indexes are listed per collection; `unique` is enforced by the engine. All indexes are sparse (empty values not indexed). Fields not listed keep their schema-1 definition and class **A** unless noted in §4.

### 3.1 `agents`

Key: `id` (`agent-<8hex>` | `worker-<8hex>`), unique, immutable.

| Field | Req | Class | Notes |
|---|---|---|---|
| `id`, `name`, `type`, `ticket`, `prompt`, `task`, `role`, `tags`, `created_at` | req/opt | A | |
| `project_id` | opt | A | → `projects.id`; empty ⇒ unscoped legacy agent (allowed, see §6.2 R3) |
| `plan_id` | opt | A | → `plans.id` |
| `pipeline_id`, `job_id` | opt | A | → `pipelines.id`; (`pipeline_id`,`job_id`) → `pipeline_jobs.id` |
| `parent_id` | opt | A | → `agents.id` (self FK; spawner) |
| `schedule_id` | opt | A | → `schedules.id` (weak: schedule may be deleted; see R7) |
| `schedule_name` | opt | D | from `schedules.name` at spawn; informational only |
| `autopilot_run_id`, `autopilot_slot`, `autopilot_task_id` | opt | A | `autopilot_run_id` → `autopilot_runs.id` |
| `status`, `exit_code`, `pid`, `hibernated` | req/opt | O | observed from tmux/process; `status` also carries archive state (§5.1) |
| `tmux_session`, `ai_cli_session_id`, `worktree`, `branch`, `base_branch`, `repo`, `workdir`, `pr` | opt | A (identity) / O (`pr`) | `worktree_created`/`branch_created` are A (cleanup ownership) |
| `permission_mode`, `execution_profile`, `auto_approve`, `force_compact`, `auto_restart`, `model`, `ai_cli`, `quota_binding` | opt | A | launch-time decisions |
| `activity`, `subject`, `last_pane_excerpt`, `context_tokens`, `context_state`, `context_checked_at`, `last_compact_at` | opt | O | pane/transcript observation |
| `restart_count`, `last_restart_at` | opt | D | derived from `events[]` restarts; recomputable |
| `rate_limited_at`, `rate_limit_restore_at`, `rate_limit_retry_count`, `backend_recovery`, `backend_recovery_generation` | opt | O | capacity subsystem observation; generation is monotonic A within the agent |
| `seed_status`, `seed_error` | opt | O | |
| `events` | opt | A | append-only bounded log (cap per `agentstore`); owned by agent |
| `archived_at` | opt | A | **new**; set iff archived (§5.1) |
| `archived_from_status` | opt | A | **new**; status prior to archive |
| `updated_at` | req | D | |
| ~~`child_agents`, `child_pipelines`, `child_autopilots`~~ | — | removed | §1.3 |

Indexes: `project_id`; `(project_id,status)`; `plan_id`; `pipeline_id`; `(pipeline_id,job_id)` unique-when-present; `parent_id`; `autopilot_run_id`; `schedule_id`; `status`; `tmux_session` unique-when-present; `archived_at`.

### 3.2 `terminals`

Key: `id` (`term-<8hex>`). Fields per schema-1: `project_id` (A, → `projects.id`), `name`, `tmux_session` (A, unique), `workdir`, `shell`, `pid`/`status`/`exit_code` (O), `created_at`, `updated_at`. No AI-agent fields permitted.
Indexes: `project_id`; `status`; `tmux_session` unique.

### 3.3 `projects`

Key: `id` (canonical absolute path or remote URL), immutable.

| Field | Class | Notes |
|---|---|---|
| `id`, `name`, `path`, `created_at` | A | |
| `status` (`open`\|`closed`) | A | operator intent; `closed` = hibernated |
| `group_id` | A | **new**, opt, → `project_groups.id` |
| `group_order` | A | **new**, opt int; ordering within the group (ties broken by `created_at`,`id`) |
| `updated_at` | D | |
| ~~`agents`, `pipelines`, `terminals`, `plans`, `autopilots`~~ | removed | §1.3 |

Indexes: `status`; `group_id`; `path` unique.

### 3.4 `project_groups`

Key: `id` (minted random). Fields: `name` (A), `created_at`, `updated_at`. **No `project_ids`.** Indexes: `name`.
Group membership ordering/listing: `projects WHERE group_id = ? ORDER BY group_order, created_at, id`.

### 3.5 `plans`

Key: `id` (`plan-<8hex>`). Fields per schema-1 `planstore.Plan`; classes:
A: `project_id` (→ `projects.id`, req), `name`, `goal`, `constraints`, `done_when`, `tasks`, `execution_mode`, `status`, `orchestrator_id`, `created_at`, `started_at`, `completed_at`, `archived_at`, `archived_from`, `revision`.
D: `content_hash` (hash of canonical definition), `updated_at`, `task_progress` (derived from agents/pipeline job state — recomputed by the plan service from `pipeline_jobs`/`autopilot_runs`; never an input).
O: `file_path`, `plan_branches` (git refs observed; informational, not a membership list).
FK: `autopilot_run_id` → `autopilot_runs.id` and `pipeline_id` → `pipelines.id` are **A back-pointers of exactly the current executor** (cardinality 0..1, a *current-binding* field, not an inverse list; the full history is `autopilot_runs.plan_id` / `pipelines.plan_id`). `orchestrator_id` → `agents.id`.
Indexes: `project_id`; `(project_id,status)`; `status`; `autopilot_run_id`; `pipeline_id`.
Revision: `revision` is incremented by exactly 1 on every authoritative definition change (compare-and-swap on `revision`).

### 3.6 `plan_exports`

Key: `plan_id` (→ `plans.id`, 1:1, cascade). Fields: `export_path`, `branch`, `pr`, `exported_revision`, `exported_hash` (D: `plans.content_hash` at export), `exported_at` (O). Indexes: `export_path`. Replica YAML files are **inert exports**; the DB record is never derived from file content.

### 3.7 `pipelines`

Key: `id`. Fields: `name`, `repo`, `tags` (A); `status` (D: computed from its jobs' statuses by the pipeline executor in the same transaction as the job write); `project_id` (→ `projects.id`), `plan_id` (→ `plans.id`), `parent_agent_id` (→ `agents.id`), `schedule_id` (weak → `schedules.id`) (A); `schedule_name` (D); `created_at`, `updated_at`; `finished_at` (D). **No `jobs`.**
Indexes: `project_id`; `plan_id`; `parent_agent_id`; `status`; `schedule_id`.

### 3.8 `pipeline_jobs`

Key: `pipeline_id:job_id`. Fields per schema-1 `pipeline.Job` plus `pipeline_id` (A, → `pipelines.id`, req) and `seq` (A; stable ordering index within the pipeline). `depends_on` is a list of **sibling job ids on the child** (child → parent-dependency direction; not an inverse list) and MUST reference jobs with the same `pipeline_id`. `agent_id` (→ `agents.id`, opt, A) is the **current** executing agent; `status`, `output`, `branch`, `workdir`, `auto_retry_count` (O/D by executor); `digest` (D).
Indexes: `pipeline_id`; `(pipeline_id,status)`; `agent_id`; `status`.

### 3.9 `autopilot_runs`

Key: `id` (`ap-<12hex>`). Fields: `project_id` (→ `projects.id`), `plan_id` (→ `plans.id`), `parent_agent_id`, `manager_agent_id`, `brain_agent_id` (→ `agents.id`) (A); `name`; run state (`status`, `phase`, …) from `RunRecord` (O/D by controller); `diagnostics` (D); timestamps.
Indexes: `project_id`; `plan_id`; `manager_agent_id`; `parent_agent_id`; `status`.
Invariant: at most one non-terminal run per `plan_id` (partial-unique on `plan_id` where `status ∉ terminal`).

### 3.10 `context_entries`

Key: `key` (dot-namespaced). Fields: `value`, `by` (advisory, A), `at` (D = `updated_at`), `prefix` (I; first dotted segment(s), lowercase), `version` (A; CAS counter), `expires_at` (opt). Indexes: `prefix`; `expires_at`. CAS/append are single-record transactions.
Keys of the form `autopilot.<run_id>.…` / `pipeline.<id>.…` are **namespaced references**, not FKs; they are removed by the owner of the namespace on terminal archive/delete (§5.4), not by referential enforcement.

### 3.11 `messages`

Key: `to:id`; `id` drawn from `id_sequences["messages:<to>"]` (zero-padded, lexicographically monotonic). Fields: `from`, `to` (→ `agents.id`, **weak**: mailbox may outlive agent briefly; see R6), `body`, `ts`, `read`, `read_at` (D). Indexes: `to`; `(to,read)`; `ts`. Cap 500 per `to`; read messages older than 24 h compacted.

### 3.12 `backends`, `backend_models`, `backend_roles`, `backend_settings`

- `backends` key `id`: `installed`, `binary_path`, `detected_at` (O); `tier`, `default`, `enabled`, `is_local` (A); `limited_until` (O). Index: `enabled`, `default` (partial-unique where `default=true` — at most one default).
- `backend_models` key `backend_id:model_id`; `backend_id` → `backends.id` (req, cascade on backend delete); `tier`, `enabled`, `auto_assign`, `is_custom`, `display_name`, `quota_scope` (A). Indexes: `backend_id`; `tier`; `quota_scope`. (Schema-1 embedded-in-backends model catalogue is split out.)
- `backend_roles` key `role_name`: `default_tier` (A). Index: none.
- `backend_settings` key `policy` | `handover` (replaces `__settings__`/`__handover_settings__`). Exactly these two keys exist.

### 3.13 `usage_snapshots`, `quota_impact_fences`

- `usage_snapshots` key `domain_key:revision`; fields per `UsageSnapshot` (O: observed provider state; `authoritative` flag records whether the provider reported the reset window authoritatively). Indexes: `domain_key`; `recorded_at`; `(domain_key,recorded_at)`. Retention 30 d.
- `quota_impact_fences` key `snapshot_revision:agent_id`; `agent_id` → `agents.id` (weak); `domain_key`, `bucket_key`, `recovery_generation`, `source`. Indexes: `domain_key`; `bucket_key`; `agent_id`. Uniqueness of the key is the dedup fence: insert-if-absent is the claim operation.

### 3.14 `snapshots`

Key: `id` (`snap-<8hex>`). `agent_id` → `agents.id` (req; **retained after agent archive**, see R4), `name`, `message`, `head`, `branch`, `dirty_files`, `stash_sha` (A), `transcript_path` (A pointer to the external blob; the blob itself is outside the DB), `created_at`. Indexes: `agent_id`; `created_at`.

### 3.15 `savings_events`, `savings_calibration`, `spend_records`

- `savings_events` key `id` (from `id_sequences["savings"]`): `ts`, `feature`, `agent_id` (weak → `agents.id`), `raw_tokens`, `kept_tokens`, `net_tokens` (D = raw−kept−cost), `cost_tokens`, `raw_sample`, `kept_sample`. Append-only: no update, no delete except operator purge. Indexes: `ts`; `agent_id`; `feature`.
- `savings_calibration` key `current`: `bytes_per_token`, `sample_count`, `calibrated_at` (all O).
- `spend_records` key `session_id`: `input`, `output`, `backend`, `model`, `repo`, `day` (O, cumulative gauges; `day` I). `session_id` is the agent's `ai_cli_session_id` (weak ref). Indexes: `repo`; `day`; `backend`.

### 3.16 `schedules`, `known_prompts`, `plan_sync_envelopes`, `metrics_samples`

- `schedules` key `id`: per schema-1 `Schedule`; `project_id` (opt, → `projects.id`), `enabled`, `mode`, `next_run_at` (D), `last_run_at` (O). Indexes: `enabled`; `mode`; `next_run_at`.
- `known_prompts` key `id` (hash of normalized question+options): `backend`, template, `hits` (D), `last_seen_at` (O). Indexes: `backend`; `last_seen_at`. Cap 500 (LRU on `last_seen_at`). This is a **Capacity-Capped Cache**; losing it is non-fatal.
- `plan_sync_envelopes` key `scope:plan_id`: `plan_id` (→ `plans.id`, weak), `scope`, `payload`, `synced_at`. Index: `synced_at`; `plan_id`.
- `metrics_samples` key `taken_at:seq`: per `metrics.Sample`; `taken_at` index; TTL.

### 3.17 `id_sequences`

Key `name`; field `next` (uint64, A). Incremented transactionally with the record that consumes it. Never reset except by `migrate`.

---

## 4. Authoritative-versus-Derived Matrix (summary)

| Fact | Authoritative location | Derived/observed copies (never authoritative) |
|---|---|---|
| Agent belongs to project | `agents.project_id` | project member lists in `ProjectView`, TUI tree |
| Agent spawned by agent | `agents.parent_id` | children listings |
| Agent ran pipeline job | `pipeline_jobs.agent_id` **and** `agents.(pipeline_id,job_id)` (see R2: set in one tx) | job tables in UI |
| Pipeline contains jobs | `pipeline_jobs.pipeline_id` | `Pipeline.jobs` in API payloads |
| Pipeline status | **D** from `pipeline_jobs.status` | — |
| Plan definition & task DAG | `plans` | YAML export, `plan_exports.exported_hash`, plan-sync envelopes |
| Plan task progress | derived from jobs/runs (`plans.task_progress` **D**) | TUI progress |
| Project group membership | `projects.group_id` | group member lists |
| Agent liveness | tmux/process (observed) → `agents.status` **O** | — |
| Transcript contents | external `.transcript` blob | `snapshots.transcript_path` pointer |
| Backend installed/limited | CLI detection / provider (**O**) | `backends.installed`, `limited_until` |

A write that changes an A field MUST, in the same transaction, update every D field that lists it as an input.

---

## 5. Archive Semantics

### 5.1 Agents

- Archiving = **in-place status transition** inside `agents`: set `status = archived`, `archived_at = now`, `archived_from_status = <previous>`; clear `pid`, `hibernated`; keep every FK untouched. The record is **never deleted by archive**.
- Active-set queries MUST exclude archived records (`status != archived`), served by the `(project_id,status)` / `status` indexes; the sparse `archived_at` index serves retention scans only. All "member of project" queries default to active only; `?include_archived=1` opts in.
- Unarchive (restore) clears `archived_at`/`archived_from_status`, restoring `status = archived_from_status` mapped to `stopped` (a restored agent is never `running` until relaunched).
- An archived agent still satisfies FKs from `snapshots.agent_id`, `savings_events.agent_id`, `pipeline_jobs.agent_id`, `agents.parent_id`, `quota_impact_fences.agent_id`. Therefore **child records MUST NOT be cascaded away by archive**.
- Hard delete (`prune`, operator purge) is a separate, later operation (R4/R5) and is permitted only when R5's preconditions hold.

### 5.2 Plans, projects, pipelines, autopilot runs

- `plans`: status `archived` + `archived_at` + `archived_from`, in-place (matches schema-1). Audit-Grade: never auto-pruned. `plan_exports` remain (pointing at archived plan); the exported YAML is moved to the `archived/` replica folder by the export subsystem (external, non-transactional).
- `projects`: **close** = `status: closed`. This is *not* archive of children: children are untouched; "hibernated" is derived at read time as `project.status = closed`. Delete project is allowed only with zero referencing children (R1).
- `pipelines`/`autopilot_runs`: terminal statuses (`done`, `failed`, `canceled`; run `finished`/`stopped`) are the archived form; retention pruning removes them with their dependents (R5).

### 5.3 Cascade matrix

| Parent action | Children |
|---|---|
| archive agent | none touched; open mailbox `messages.to` retained until read-compaction |
| delete agent (purge) | delete `messages(to)`, `quota_impact_fences(agent_id)`, `snapshots(agent_id)` (+ blob after commit), null out `agents.parent_id` of children, null `pipeline_jobs.agent_id`, `savings_events.agent_id` kept (weak) |
| delete pipeline | delete its `pipeline_jobs`; null `agents.pipeline_id/job_id` for agents that still exist |
| delete plan | restricted if non-terminal `autopilot_runs`/`pipelines` reference it; else delete `plan_exports`, `plan_sync_envelopes` |
| delete project | **restricted** unless no agents, terminals, plans, pipelines, autopilot runs, schedules reference it; `group_id` is on the project itself so no group update is needed |
| delete project group | null `projects.group_id` for members (same tx) |
| delete backend | cascade `backend_models` |

### 5.4 Context namespace cleanup

On terminal archive/delete of a run or pipeline, its owner deletes `context_entries` with prefix `autopilot.<id>.` / `pipeline.<id>.` in the same transaction where possible; leftovers are collected by `repair` (orphaned-namespace sweep).

---

## 6. Referential Invariants

### 6.1 Strength

- **Strong FK**: the target MUST exist at write time and the engine rejects violations (transaction abort). Applies to: `terminals.project_id`, `plans.project_id`, `pipelines.project_id`/`plan_id`, `pipeline_jobs.pipeline_id`, `autopilot_runs.project_id`/`plan_id`, `backend_models.backend_id`, `plan_exports.plan_id`, `agents.project_id` (when non-empty), `agents.parent_id`, `agents.plan_id`, `snapshots.agent_id`, `projects.group_id` (when non-empty).
- **Weak FK**: a dangling target is tolerated and reported by `repair`; reads MUST handle a missing target. Applies to: `messages.to`, `quota_impact_fences.agent_id`, `savings_events.agent_id`, `spend_records.session_id`, `plan_sync_envelopes.plan_id`, `agents.schedule_id`, `pipelines.schedule_id`, `autopilot_runs.brain_agent_id`.

### 6.2 Rules

- **R1 Restricted parent delete.** A strong-FK parent cannot be deleted while strong-FK children exist (except where §5.3 specifies cascade). Delete returns a typed `ErrReferenced{collection, field, count}`.
- **R2 Bidirectional-pointer atomicity.** The only permitted pair of mutual pointers is `pipeline_jobs.agent_id` ↔ `agents.(pipeline_id,job_id)`. They MUST be set/cleared in one transaction; neither is derived from the other. The agent side is the identity (A), the job side is the *current binding* (A); `repair` validates agreement and reports, never silently picks a winner (job side wins only if the agent record is archived).
- **R3 Project scoping.** `project_id` is optional on `agents` only for legacy migration; new writes through schema-2 APIs MUST set it. `terminals`, `plans`, `pipelines`, `autopilot_runs` require it.
- **R4 Retained referents.** `snapshots.agent_id`, `parent_id`, `pipeline_jobs.agent_id` may point at **archived** agents. Archive is never a FK violation.
- **R5 Purge preconditions.** An agent may be hard-deleted only when archived and (a) not referenced by `agents.parent_id` of a non-archived agent, (b) not the `manager_agent_id`/`brain_agent_id` of a non-terminal run.
- **R6 Mailbox tolerance.** Sending to an unknown `to` fails at the API; a message to an agent that is later deleted is removed by the delete cascade (§5.3).
- **R7 Schedule tolerance.** Deleting a schedule does not touch historical agents/pipelines; they keep `schedule_id` and the denormalized `schedule_name`.
- **R8 Acyclicity.** `agents.parent_id` MUST be acyclic and MUST NOT reference itself; `pipeline_jobs.depends_on` MUST be acyclic and intra-pipeline.
- **R9 Uniqueness.** Declared `unique`/partial-unique indexes are invariants, not hints: one default backend; one live run per plan; one agent per `(pipeline_id,job_id)`; one agent/terminal per `tmux_session`.
- **R10 No inverse lists.** §1.3. Any schema validator hit is a hard error on write and a repair finding on read.
- **R11 Immutability.** `id`, `created_at`, `project_id` (of an agent, terminal, plan), `pipeline_jobs.pipeline_id` are immutable after create. Re-parenting a plan/agent between projects is an explicit, audited operation (not an update).

### 6.3 Transaction boundaries

Atomic (single `warden-db` transaction): agent create + parent/FK validation; agent delete + mailbox/fence/snapshot-metadata cascade; pipeline job status + pipeline status recompute; pipeline job ↔ agent binding (R2); autopilot run + manager agent create; project delete restriction check + delete; group delete + member `group_id` nulling; plan `revision` CAS.
Non-atomic (outside DB; handled by operation journal, Task 4): tmux session creation/kill, git worktree/branch/stash, transcript blob write/removal, plan YAML export, hook/prompt staging files. A DB record naming an external effect MUST be written so that a crash between the two is recoverable (record first with an `O`-class status like `starting`, reconcile later).

---

## 7. Index Summary and Query Contract

The following queries MUST be satisfied by an index (no collection scan) and are the performance contract for Task 3:

| Query | Index |
|---|---|
| active agents of project | `agents(project_id,status)` |
| children of agent | `agents(parent_id)` |
| agents of plan / pipeline / run | `agents(plan_id)` / `(pipeline_id)` / `(autopilot_run_id)` |
| terminals / plans / pipelines / runs of project | `*.project_id` |
| projects in group (ordered) | `projects(group_id)` + in-memory sort on `group_order` |
| jobs of pipeline | `pipeline_jobs(pipeline_id)` |
| job by agent | `pipeline_jobs(agent_id)` |
| inbox drain | `messages(to,read)` |
| latest usage per domain | `usage_snapshots(domain_key,recorded_at)` |
| expired context/metrics/usage | `expires_at` / `taken_at` / `recorded_at` |

Index maintenance is performed by the engine within the same transaction as the record; no index is authoritative (class I).

---

## 8. Validation, Migration, and Compatibility Notes

- **Schema validator.** A single registry in the `warden-db` adapter declares for each collection: key function, FK declarations (strong/weak), forbidden fields (§1.3), immutable fields (R11), unique indexes. All writes pass through it; tests assert the registry matches this document (collection list, forbidden set, FK strength table).
- **Import (schema-1 → schema-2).** Per inventory §7.2, with these contract rules: strip forbidden inverse lists and **verify** each listed child's FK actually points back (if the child lacks the FK and the parent list names it, set the child FK iff the child exists and is unambiguous, otherwise record a repair finding and keep the child unscoped); merge `archived` into `agents`; split `Pipeline.jobs[]` into `pipeline_jobs`; split `backends` embedded models; convert `ProjectGroup.project_ids[]` into `projects.group_id/group_order` (a project listed in several groups keeps the first by group `created_at`, others are reported); convert singleton settings keys; synthesize `id_sequences` from the maximum existing id.
- **Idempotence.** Import is keyed by primary key; re-running yields identical state.
- **Unchanged contracts.** External blobs, config files, and runtime files stay as classified in inventory §5; this contract does not move any of them into the database.
- **Companion contracts:** `ProjectView` projections and cache invalidation are specified in [`2026-10-10-project-view-projection-contract.md`](2026-10-10-project-view-projection-contract.md). Operation journal and saga recovery (Task 4), adversarial fixtures, and migration matrix validation (Task 5) remain out of scope here.

# Schema 3: ownership normalization and offline update contract

Status: proposed; human approval required. This document is the **design**
deliverable only, not authorization to implement or operate a migration.

Bound assignment: Plan `plan-4a55e219`, revision `3`, content hash
`sha256:8c4f9b7a4f96e707b02722c8bc28c546eab0153c4efdb1f5b9f678cfe9da2ea0`,
task `design`, parent `orchestrator-8bed8d7b`. The `migrate` and `cutover` tasks
remain pending. Approval must reference this document's commit; changes to these
decisions require renewed approval. No fleet update or live-data inspection was
performed for this design.

## 1. Decisions and boundary

Projects store project attributes only. Every project-owned entity stores its
own immutable `project_id` and structural parent references. Indexed child
queries produce membership; no parent record stores a member list. There is no
move, attach-to-another-project, detach, or reparent operation. Creating a
replacement entity requires a new identity; updates cannot repurpose an old ID.

Materialize `pipeline_jobs` and new `plan_tasks` as full canonical rows, remove
embedded mutable jobs/tasks in the same release, and consolidate operational
autopilot authority into **`autopilot_runs`**. Delete `autopilots` from the active
database and runtime registration. Plan execution evidence remains Plan-owned;
an operational executor cannot decide task completion from a private ledger.

The only migration entry point is **`warden update`**. Its target binary may run
an internal migration subprocess, but standalone `migrate --apply`, `--resume`,
and `--restore` must refuse schema-3 mutations without the matching updater
transaction. Read-only planning/checking remains available. The daemon never
migrates or backfills ownership at boot. Rollback restores the paired old binary
and full data generation offline; there is no reverse record transformation.

## 2. Audit of actual schema 2

Source paths below are repository-relative and identify symbols as well as
files, so the implementation handoff does not depend on stale line numbers.

* [storage.Open](../../internal/storage/storage.go) registers package store types
  with `NewStoreFromDB`/`NewRunStoreFromDB`; it registers **both** autopilot
  stores. Its `requireSharedMode` also accepts some missing-ledger and newer
  layouts. Schema 3 must require an exact compatible committed format before
  opening an existing database; an existing DB without a ledger is ambiguous.
* [wardendb/project_schema.go](../../internal/wardendb/project_schema.go),
  `RegisterProjectSchema`, describes small normalized relationship records.
  It is not called by the production composition root. Those structs omit
  business fields and are not lossless runtime persistence codecs. They must
  not be used as a rewrite format that discards prompts, runtime, permissions,
  plan evidence, or timestamps. Its `PipelineJobRecord` lacks `project_id`.
* [projectstore/types.go](../../internal/projectstore/types.go), `Project` and
  `ProjectGroup`, still serialize inverse arrays, including explicit empty
  arrays via `MarshalJSON`. [membership.go](../../internal/projectstore/membership.go)
  performs Add/Remove/Reparent RMW; [groups.go](../../internal/projectstore/groups.go)
  owns inverse group membership. The actual unique index is `id`, not the
  proposed `id,path` declaration; preserve remote projects with empty paths.
* [agentstore/types.go](../../internal/agentstore/types.go), `Agent`, and
  [store/types.go](../../internal/store/types.go), `Session`, both carry
  `child_agents`, `child_pipelines`, `child_autopilots`, `parent_id` and ownership
  backrefs; conversion and custom marshaling preserve the inverse arrays.
  [agentstore/shared.go](../../internal/agentstore/shared.go) registers
  `agents` and `archived_agents`, not just the normalized header type.
* [pipeline/pipeline.go](../../internal/pipeline/pipeline.go), `Pipeline.Jobs`,
  is a complete embedded job array. `Job.MarshalJSON` writes `agent_id` and
  legacy `session_id`; `AgentRef` and `SetAgentID` accept/mirror both.
  [migrate/staging_export.go](../../internal/migrate/staging_export.go),
  `loadPipelines`, intentionally keeps jobs embedded and also writes an
  **additive projection** `pipeline_jobs`, with `id=pipelineID+":"+jobID`,
  `pipeline_id`, local `job_id`, and `seq`. The runtime shared pipeline store
  does not use those rows as authority. They may be stale after runtime writes.
* [planstore/store.go](../../internal/planstore/store.go), `Plan`, embeds
  `tasks`, `task_progress`, `task_outcomes`, active/history execution evidence,
  summaries, and executor links. There is no `plan_tasks` in the
  [wardendb catalogue](../../internal/wardendb/registry.go).
  [canonical.go](../../internal/planstore/canonical.go), `ComputeContentHash`,
  hashes ordered task definitions. [execution.go](../../internal/planstore/execution.go)
  contains immutable snapshot-at-start tasks as well as execution progress.
* [autopilot/store.go](../../internal/autopilot/store.go), `RunRecord`, stores
  registration/lifecycle under `run_id` in `autopilot_runs`; its `brain_id`
  historically names the manager. [autopilotstore/types.go](../../internal/autopilotstore/types.go),
  `Autopilot`, separately stores `id`, `plan_id`, parent, manager/brain IDs and
  overlapping diagnostics in `autopilots`. [autopilotstore/migrate.go](../../internal/autopilotstore/migrate.go)
  can copy legacy runs to live entities, Plan history or an external legacy
  archive. Neither that importer nor "latest timestamp wins" resolves schema-3
  consolidation.
* [autopilot/ledger.go](../../internal/autopilot/ledger.go) also writes reserved
  `context_entries` keys `autopilot.<run>.tasks`, per-task state/branch overlays
  and `integration_branch`; these are another mutable authority outside both
  autopilot collections. `landings` is daemon-authored merge idempotency evidence,
  `journal` is decision prose, [fixstate.go](../../internal/autopilot/fixstate.go)
  contains fix-dispatch idempotency, and restart context (from
  [restart_context.go](../../internal/autopilot/restart_context.go)) captures
  recovery context. Their dispositions are specified below.
* [cli/daemon.go](../../internal/cli/daemon.go) and
  [cli/doctor.go](../../internal/cli/doctor.go) call
  `daemon.ReconcileProjectMembership`; [project_backfill.go](../../internal/daemon/project_backfill.go)
  and [project_membership.go](../../internal/daemon/project_membership.go)
  infer ownership and maintain inverse lists. [autopilot_reconcile.go](../../internal/daemon/autopilot_reconcile.go)
  can rekey managers, infer run/task ownership from tags and clear dead parents.
  Those identity-repair behaviors conflict with immutable schema-3 ownership.
* [schema/schema.go](../../internal/schema/schema.go) embeds format 2;
  [migrate/registry.go](../../internal/migrate/registry.go),
  [runner.go](../../internal/migrate/runner.go), [cutover.go](../../internal/migrate/cutover.go)
  and [schema2.go](../../internal/migrate/schema2.go) provide the existing
  registry/journal, offline lock, immutable backup and staged promotion model.
  Schema-2 inventory and compatibility code cannot silently become schema-3
  codecs; retained schema-1 fixtures/importers must remain version-specific.
* [cli/update.go](../../internal/cli/update.go), `runUpdateMigrations`, invokes
  the installed target's `migrate --apply`. [updater/transaction.go](../../internal/updater/transaction.go)
  implements stop/snapshot/swap/migrate/restart/readiness, but can tolerate no
  service manager and its failure rollback restores without first explicitly
  stopping a newly started daemon. Both need stricter cross-schema handling.
  [snapshot.go](../../internal/updater/snapshot.go) has a file-hash manifest and
  special-cases only `pre-schema2-`; [rollback.go](../../internal/updater/rollback.go)
  selects the latest snapshot and may prefer `.bak` over its captured binary.
  These are insufficient pairing/retention rules for schema 3.

## 3. Collection inventory and exact retirement list

This covers every name in `wardendb.Catalogue` at the audited base. Collections
without ownership changes are copied losslessly and integrity-checked, rather
than decoded through reduced relationship structs. New collections are explicit.

| Collection | Schema-3 action; exact removed persistence |
| --- | --- |
| `projects` | Retain `id,name,path,status,permission_posture,created_at,updated_at`; add optional `group_id,group_order`. Remove **`agents,pipelines,terminals,plans,autopilots`**, including nil/empty authority semantics. |
| `project_groups` | Retain `id,name,created_at,updated_at`; remove **`project_ids`**. Order moves to the project's `group_order`. |
| `agents` | Keep full Agent payload; normalize **`parent_id` → `parent_agent_id`**. Remove **`child_agents,child_pipelines,child_autopilots`**. Keep direct `project_id,plan_id,pipeline_id,job_id,autopilot_run_id,autopilot_task_id` where applicable. |
| `archived_agents` | Merge disjoint historical agents into `agents`, preserving archival fields; then remove the collection and registration. Duplicate IDs across the two collections block, even if one is archived. No second archived membership authority. |
| `terminals` | Retain full Terminal payload with required immutable `project_id`; leaf, no parent lists. |
| `plans` | Remove **`tasks,task_progress,task_outcomes`** from the current Plan row after materialization. Remove current executor mirrors **`pipeline_id,autopilot_run_id,orchestrator_id`**; resolve executor from current execution's selection and child links. Preserve remaining full definition, revision/hash, lifecycle and execution evidence. |
| `plan_tasks` (new) | Canonical current task definition, progress and outcome, with direct project and Plan ownership; see §4. |
| `pipelines` | Retain full pipeline header including direct `project_id,parent_agent_id,plan_id`. Remove **`jobs`** from stored headers. |
| `pipeline_jobs` | Replace schema-2 additive projection with full canonical job rows. Remove **`agent_id,session_id`** inverse assignment mirrors; agent attempts own job links. Retain every other Job business/runtime field. |
| `autopilot_runs` | Sole operational authority; full unified registration, parent, diagnostics and lifecycle. Keep stable `run_id` as identity. Remove **`brain_id`** manager mirror; derive manager/brain through agent run+slot/role refs. Flatten overlapping diagnostics into one agreed set, never two writable states. |
| `autopilots` | **Remove entire collection**, catalogue entry and runtime store registration after one-to-one consolidation and reference mapping (§5). No shadow identity table. |
| `plan_events`, `plan_notes`, `plan_exports` | Keep payloads/keys and direct `plan_id`; add immutable `project_id` from Plan. Evidence references (`execution_id`, attribution) are historical, not live membership. Sources: `planstore/event.go`, `planexport/record.go`. |
| `context_entries`, `messages` | Global collaboration data; retain keys, sender/recipient/attribution payloads except retired reserved autopilot keys below. No inferred project membership from content/tags. Sources: `ctxstore/shared.go`, `mailbox/shared.go`. |
| `backends`, `backend_models`, `backend_roles`, `backend_settings`, `backend_quotas`, `backend_rl_cooldowns` | Global backend configuration, policy and accounting; preserve existing backend parent references/indexes. Source: `backendstore/shared.go`. |
| `model_catalog_scopes`, `model_catalog_snapshots`, `model_catalog_observations`, `model_catalog_proposals`, `model_catalog_revisions` | Global catalog evidence/policy; preserve source scopes, fingerprints, revision chains and indexes. Sources: `backendstore/catalog*.go`. |
| `usage_snapshots`, `quota_impact_fences` | Global usage/capacity accounting; preserve. Sources: `backendusage/snapshot_store.go`, `capacity` shared store. |
| `snapshots` | Global capture artifacts; `session_id` is historical attribution, not membership. Bare-directory captures remain supported. Preserve transcript paths; never infer project from captured workdir. Sources: `snapshot/snapshot.go`, `snapshot/shared.go`. |
| `savings_events`, `savings_calibration`, `spend_records`, `metrics_samples` | Global historical telemetry/accounting; retain payloads, including project/session labels as event context, not owner FKs. Sources: `savings/shared.go`, `spend/shared.go`, `metrics` shared store. |
| `schedules` | Global launch templates; `project_id` configures a future launch, not schedule ownership. Preserve template/payload and validate the launched entity's ownership on creation. Sources: `schedule/schedule.go`, `schedule/shared.go`. |
| `known_prompts`, `plan_sync_envelopes` | Global learned prompt cache and inert sync transport envelopes. Do not turn envelope-embedded Plans/tasks into a second live authority. Sources: `knownprompts/shared.go`, `plansync/hub.go`. |
| `id_sequences`, `operations`, `audit_events` | Global sequence/journal/audit records. Preserve IDs, ordering and recovery semantics; see pending-operation rule §6. Sources: `mailbox/shared.go`, `wardendb/operations.go`, `audit/auditstore/shared.go`. |

No `project_membership` or `agent_children` table is present in this catalogue;
do not claim to drop one. `ProjectViewProjection` in
`wardendb/project_projection.go` is an in-memory derived cache, not a persisted
membership table. Historical `ProjectEvent.project_ids` is an invalidation fanout
envelope, not group membership: preserve old event/audit facts; remove future
move/reparent producers and their `PreviousProjectID` API input.

Remove the old active write/read paths in the same release: projectstore
`addMember/removeMember/reparentMember`, all Add/Remove member and Reparent
helpers and callers; group `ProjectIDs` RMW; agent inverse marshaling/conversion;
daemon membership add/remove/backfill and doctor repair; `child_pipelines.go`
inverse bookkeeping; pipeline embedded-job codec and `SessionID` mirroring;
Plan embedded-current-task codec; dual autopilot store/controller write-through;
tag/path-based ownership recovery and parent-clearing/rekey behavior at boot.
Tree/API/TUI/export adapters may assemble arrays in responses, but cannot persist
them or use a prior response/cache as write authority. Replace the older hierarchy
spec's conflicting authoritative-array guidance and relevant fixtures/goldens.

## 4. Owner contracts, keys, indexes and invariants

Structural parent fields and `project_id` are write-once at creation. Mutable
status, prompts, output and attempt evidence remain mutable under existing
lifecycle/CAS rules. Optional structural fields are fixed too: empty root cannot
later become parented. Manager recovery creates an agent with a new identity
instead of reusing/reparenting an existing entity. Closing a project changes its
status, not children's ownership; reject new work in closed projects while
allowing cancellation, archival and evidence writes.

One canonical full record codec per collection is registered at bootstrap.
Relationships use these same records/transactions, not separately writable
header copies. Strict schema-3 codecs reject forbidden fields on both decode
and encode, even `null` or empty arrays; unknown ownership-bearing fields block.
All other current business fields remain explicitly represented in full codecs.
Collection record-version declarations and the data-format ledger are separate;
affected collection specs become record version 3, untouched specs retain their
versions. `wardendb` and `schema` format constants must agree on 3.

| Owner record | Immutable references and key | Required indexes beyond primary key |
| --- | --- | --- |
| Project | `id`; optional `group_id` set at creation (migration initializes it) | unique nonempty `path`; `status,group_id`; application uniqueness of `(group_id,group_order)` for grouped projects |
| Group | `id` | `name` lookup (not unique) |
| Agent | `id,project_id,parent_agent_id?,plan_id?,pipeline_id?,job_id?,autopilot_run_id?,autopilot_task_id?` | `project_id,parent_agent_id,plan_id,pipeline_id,job_id,autopilot_run_id,autopilot_task_id,status,lifecycle`; unique live manager slot per run, enforced transactionally |
| Terminal | `id,project_id` | `project_id,status` |
| Plan | `id,project_id` | `project_id,status,execution_mode` |
| Plan task | key = canonical JSON array `[plan_id,task_id]`; fields `id` (encoded key), `task_id,plan_id,project_id` | unique `(plan_id,task_id)` and `(plan_id,seq)`; `plan_id,project_id,status` |
| Pipeline | `id,project_id,parent_agent_id?,plan_id?` | `project_id,parent_agent_id,plan_id,status` |
| Pipeline job | key = canonical JSON array `[pipeline_id,job_id]`; fields `id` (encoded key), `job_id,pipeline_id,project_id` | unique `(pipeline_id,job_id)` and `(pipeline_id,seq)`; `pipeline_id,project_id,status` |
| Autopilot run | primary `run_id,project_id,plan_id?,parent_agent_id?` | unique `run_id`; `project_id,plan_id,parent_agent_id,state`; at most one operational executor per Plan/current execution |
| Plan event/note/export | existing primary keys; `plan_id,project_id` | preserve existing indexes; add `project_id`, ensure `plan_id` |

Use unique `id` for ordinary primary-key rows. Compound/conditional uniqueness
must be enforced in the shared transaction validator if Scriva cannot express
it; separate unique indexes on constituent columns are **not** equivalent.
Do not install a unique index over empty optional paths. `seq` is zero-based
declaration order and immutable for jobs; pending Plan definition edits may
resequence tasks atomically. Status remains the existing domain enum; an agent's
`lifecycle` distinguishes active/archive state. Do not replace meaningful job
status with a generic lifecycle string. Queries for a project use indexed
`project_id` on jobs/tasks directly, then deterministic `(seq,id)` ordering.

Full task rows contain `prompt,after,seq,status,outcome?` in addition to ownership.
Current `TaskProgress` and `TaskOutcomes` move here, not into an autopilot ledger.
Omitted progress becomes `pending`; extra progress/outcome keys without a task
block. DAG dependencies `after`/`depends_on` are local peer references, not inverse
membership, and remain. Validate uniqueness, same-owner resolution and cycles.
Pipeline jobs retain all fields from `Job` except the two assignment mirrors.
Agents are attempt records; zero or one live agent may execute a given job, while
past attempts remain queryable by immutable job reference. Replacing a worker
creates a new agent rather than altering an existing agent's owner.

Hydrate Plan/ Pipeline API objects from children inside a consistent read. Plan
definition edits update header+tasks+hash atomically. Recompute hashes using the
**existing** `canonical.go` byte algorithm over tasks ordered by `seq`; migration
must preserve content hashes and revisions, not simulate a definition edit.
Execution snapshot `tasks`, historical execution `task_progress`, terminal
summaries/outcomes and immutable replica envelopes remain historical evidence.
They are not editable current task collections. Existing active execution
progress must agree with current rows at migration; after cutover do not persist
a second mutable `active_execution.task_progress` (derive it in responses).
Preserve snapshot-at-start definitions even if different from the current Plan.

Reserved autopilot context retirement is exact: remove
`autopilot.<run>.tasks`, `autopilot.<run>.tasks.<task>.state`,
`autopilot.<run>.tasks.<task>.branch`, and
`autopilot.<run>.integration_branch` after validation/materialization. Remove
their writer/CAS methods, fallback readers and persona instructions in the same
release; generic context writes must reject these retired reserved keys.
Task `dispatch` metadata on `plan_tasks` retains the ledger's distinct operational
enum (`pending,assigned,in_progress,pr_open,gated,landed`), worker/branch/PR/note
and update time. This enum does not replace Plan task status
(`pending,in_progress,done,skipped`), and `landed` alone cannot authorize `done`.
The task's current execution scopes dispatch metadata; retain older execution
evidence in existing history. Derive task-state/branch overlays at read time.
Run `integration_branch` is the sole mutable merge-target value.
The consolidated run's full stored field set is `run_id,project_id,plan_id,
parent_agent_id,name,repo,plan_file,state,integration_branch,gate,strategy,
delete_branch,slot_scope,last_progress_at,progress_fp,last_error,created_at,
updated_at`. Flatten live `diagnostics.{state,integration_branch,gate,strategy,
slot_scope,last_error,repo,plan_file}` into the corresponding top-level fields;
do not persist `diagnostics` again. Preserve RunRecord's other fields unchanged.
Canonical `name` retains the registration name when present; `AP:<plan-name>`
is a derived display name, and a live entity's differing display name is not a
conflict with registration name. For a consolidated pair, `created_at` is the
earliest source creation and `updated_at` the latest source update, with both
original timestamps in migration provenance; timestamps never choose lifecycle
or ownership. `manager_agent_id,brain_agent_id` and old `brain_id` become indexed
agent relationships, not fields on this record. New runtime-only diagnostics
must be classified explicitly before extending this codec.
Keep `landings`, `journal`, `fix.<task>` and restart context as historical
or operational recovery evidence in context, with run identity mapped where
needed and all payloads retained. They cannot override Plan status, task status,
owner references or run configuration. Do not fold the landing idempotency ledger
into a lossy status projection. Unknown keys in the reserved autopilot namespace
require explicit classification; ordinary unrelated collaboration keys survive.

`active_execution.executor_id` is the selection for one execution, not an inverse
set; keep it and historical executor IDs as execution evidence. Resolve and
validate its type/mode against the relevant child. Remove top-level Plan executor
mirrors listed in §3. Agent attribution IDs and historical snapshots need not
resolve to live agents. Operational parent FKs must resolve, same project, with
no ancestry cycle. Deletion of a referenced owner is restricted: archive instead
or atomically finalize dependent records into explicit historical evidence.
Never silently clear immutable parent/project refs on deletion. Existing
`HistoricalRef` payloads remain historical and cannot authorize new work.

Explicit `scope=global|system` agents may have no project only when already
explicitly scoped; they cannot own project-scoped children. Missing project is
not automatically system scope. All other entities in this table require a
resolving project. New global project-owned entity exceptions require approval.

## 5. Migration mapping and conflict policy

Read raw schema-2 records, not best-effort `List` methods that skip decode errors.
Validate storage checksums and decode every row. Produce deterministic findings
with source collection/key, field, candidate values, evidence and disposition.
Any blocking finding aborts promotion of the **whole generation**. No salvage,
quarantine-as-success, path guess, newest timestamp winner, or per-row live fix.

1. Collect existing direct project/parent references and every legacy inverse
   claim. A missing direct owner can be initialized only from one unambiguous,
   resolving inverse/structural-parent claim whose project agrees across all
   evidence. A nonempty direct reference is never overwritten. Disagreement,
   duplicate claims across owners, explicit-empty authoritative list contradicting
   children, dangling listed IDs, missing parents, cross-project links, or cycles
   block. A nil/missing legacy list supplies no claim. Duplicate same-owner list
   entries can be deduplicated with a recorded finding. Omission from a nonempty
   legacy authoritative list also blocks when a direct child claims that owner.
   Do not discover projects from cwd, repo, worktree, tags, or path prefixes.
2. Move group membership to `projects.group_id,group_order`, using each group's
   list order. Two group claims or dangling project IDs block. Unlisted projects
   remain ungrouped. No runtime group-move API is introduced by this design.
3. Materialize jobs from each **embedded** schema-2 `Pipeline.Jobs`, preserving
   every field and order. Existing `pipeline_jobs` is the documented derived
   projection, so its stale values do not override embedded jobs. Record all
   differences (including missing/extra projection rows) in the manifest and
   rebuild it entirely. This exception is permitted only for the known schema-2
   projection shape/provenance; unknown job fields/layout block. Conflicting
   `agent_id` versus `session_id`, or a selected agent whose direct job reference
   disagrees, blocks. Missing direct agent bindings can be initialized from a
   unique consistent assignment. Collision-safe tuple keys replace old colon
   keys; manifest maps old→new keys and checks uniqueness. Agent `job_id` remains
   the local ID interpreted together with immutable `pipeline_id`.
4. Materialize ordered Plan definitions/progress/outcomes into `plan_tasks`.
   Duplicate task IDs/sequence, unknown dependencies, cycles, hash mismatch,
   dangling progress/outcome keys, or active execution progress disagreement
   block. Capture original definition/hash/revision and demonstrate equality of
   the rehydrated definition. Retain old execution snapshot definitions verbatim.
   Reconcile reserved context task rows and overlays against the exact Plan and
   task; missing association, unknown task, different worker/branch/PR claims,
   or disagreement with the authoritative ledger array blocks. An absent overlay
   supplies no claim. Preserve the operational dispatch enum independently of
   lifecycle status and verify landed claims against retained landing evidence.
   Conflicting context versus RunRecord integration branches also block.
5. Consolidate `autopilots` into `autopilot_runs` using exact identity or a unique
   same-Plan **active-execution** link, never name/path/"only run in project".
   Keep existing `run_id` as the canonical identity. An unpaired live `autopilots`
   row may become a run with its existing `id` as `run_id` only if its Plan and
   active execution resolve uniquely. Unpaired historical RunRecords remain
   historical rows in the same sole collection; planless rows require a proven
   project and cannot become executable until a new plan-bound run is created.
   State policy v1 accepts exactly `registered,disabled,starting,active,paused,
   stopped,degraded,healing,complete` from RunRecord and the corresponding
   operational subset from live diagnostics; names map identically, absent
   diagnostics supply no claim, unknown/alias spellings block. A live row paired
   with a terminal RunRecord blocks rather than reviving it. A registered row is
   dormant, not completed history. For all shared nonempty fields,
   incompatible state, parent, project, Plan, gate, strategy, integration branch,
   slot or manager claims block. Missing fields may be filled from the agreeing
   counterpart. `brain_id` maps to manager slot only under its documented old
   semantics; never equate it with `brain_agent_id` by spelling.
6. Journal one-to-one executor ID mappings and rewrite **current operational**
   references (agent run refs and current execution selection) consistently.
   Preserve historical executor IDs in immutable evidence plus manifest mapping;
   do not rewrite signed/hash-covered history. Remove manager/brain inverse IDs
   after validating/initializing agents' run+slot references. Multiple live agents
   for one slot block. Preserve terminal history and diagnostics once, and prove
   both source populations are accounted for before deleting `autopilots`.

Pending `operations` containing old-layout intents must block migration unless
schema-2 recovery first completes them under the old binary. Never replay an
old inverse-write intent after cutover. Committed historical operations/audits
are retained verbatim. Unknown collection names or unknown persisted ownership
shapes block classification; known global data is copied without reinterpretation.
Any unsupported case returns actionable evidence to the operator; schema-3
mutation never invokes generic repair to resolve it automatically.

## 6. Update, journal, backup, resume and rollback

Release `manifest.json` must declare `schema_version=3`, `min_schema=2`,
`breaking=true`, notes naming ownership normalization and paired-data rollback,
and the first updater-capable version in `min_upgrade_from`. That version must
be selected from compatibility tests, not invented in this design. Schema-1
users must update through a schema-2 waypoint; no undocumented 1→3 shortcut.
Extend manifest requirements with machine-checkable capabilities for stopped
daemon, schema-3 transaction handoff, verified paired backup and generation
restore. Old updaters that ignore these requirements must be excluded by the
version gate and release path. Existing `requires.manual` prose alone cannot
enforce this. Release/build metadata and target `version --json` must agree.

The approved updater owns a durable transaction ID and canonical data-dir
identity. It stops the daemon, confirms process exit/health absence, prevents
service auto-restart, and obtains the data-dir owner lock before snapshot or
mutation. A failed/unmanaged stop is blocking; accepting `ErrNoServiceManager`
as success is forbidden when a process may still be running. Hold exclusive
ownership through migration/restore; arrange explicit lock transfer to the
target subprocess instead of competing locks. Child agents must not retain DB
write handles. Restart releases ownership only after ledger commit. Authentication
of the internal migration handoff must bind transaction, target binary hash,
data directory, manifest hash and expected source format; an environment flag
alone is not authority. Public migrate mutation flags must reject absent/stale
handoffs. Recovery also runs through `warden update`, not direct migrate commands.

Use a new immutable `backups/pre-schema3-<transaction>/` bundle, exclusive create,
with copied data under `data/`, the exact old binary under `bin/warden`, and
`MANIFEST.json` written last. No mutable hard links. Include the full old
`warden-db`, ledger, affected config and all files changed by the updater.
Inventory external runtime/transcript artifacts with explicit untouched
dispositions. Never follow symlinks or special files; reject unsafe paths and
insufficient disk space before changes. Sync contents and directories before
claiming backup completeness. Never include backup/staging recursively.
Compute the deterministic read-only mapping plan and expected output digests
under the lock before sealing this manifest; later `mapped`/`verified` steps
prove the staged output against those expectations. Preserve the original
pre-journal ledger in the backup; journal markers are transaction metadata,
not the source ledger to restore.

The versioned immutable manifest must include:

* Transaction/migration ID (`0003-ownership-normalization` proposed), source 2,
  target 3, source ledger/history hash, source/target binary version and SHA-256,
  updater capability version, canonical data-dir identity and timestamp.
* Relative file inventory: disposition, type, size, permissions, SHA-256,
  collection/key counts, canonical record digests, source index definitions and
  target required indexes. Include all unchanged collections and source-only
  collections being retired; distinguish absent from empty.
* Input format/codec versions, approved mapping-policy version, key/executor
  mappings, field/table removals, projection differences, nonblocking findings,
  evidence for inferred missing refs, expected output counts and canonical
  digests, plus Plan revision/hash equivalence proofs. Source rows must each
  map to retained evidence, a child row, or an explicitly retired projection.
* Backup binary/data pairing, restore format version, required restore paths,
  exclusions and external artifacts, restore deletion set (target-only paths),
  and a manifest digest stored in the journal outside the immutable manifest.

Separate immutable backup identity from the append-only migration journal.
Extend the current `schema.InProgress` reference or point it at a versioned
transaction journal; its present `migration/step/snapshot` alone cannot verify
generation identity. Record and fsync intent **before** each step and verified
completion after: `locked → backed_up → mapped → staged → verified →
old_generation_parked → promoted → ledger_committed → readiness_verified`.
Journal records include monotonic step number, manifest hash, input/output
digests, exact source/staging/parked paths, and expected ledger/binary identities.
Build `.schema3-staging-<transaction>` afresh, validate all records/FKs/indexes
and hash equivalence, then close and sync DB handles before promotion.

Promotion is a recoverable sequence of directory renames: park old `warden-db`,
rename staging to `warden-db`, then atomically commit ledger/history to 3.
There is no claim that two directory renames and a ledger write are one atomic
filesystem operation. The journal and boot guard close every gap. A journaled
pending transaction blocks ordinary startup even if the ledger already says 3.
No ordinary bootstrap may create missing collections in a partly promoted DB.
Only the updater can start the target for readiness after commit; require exact
target version, format 3 and ownership-integrity success, then finalize the
transaction. Fresh empty installs initialize schema 3 directly; unknown existing
DBs or incompatible newer ledgers refuse before opening stores.

Resume through update verifies the immutable manifest, exact binary/policy,
owner lock and all path/digest identities. A step is skipped only if its output
is verified identical; divergent staging/targets are blockers, never overwritten
or merged. Recognize both sides of each rename from journaled paths/digests.
Never take a second backup of a partially converted generation as the baseline.
Repeated successful update on schema 3 is an integrity-checked no-op.

Automatic failure rollback first stops the target (including a partially
started daemon) and reacquires exclusive ownership. Verify the paired bundle,
stage and verify full old data, replace the entire target generation (including
removing target-only `plan_tasks` and rewritten indexes), restore ledger/config
and paired binary, then restart and verify old version+schema. Generic overlay
copy is insufficient. Snapshot selection is transaction-based, not "latest" or
an unrelated `.bak`. If verification, restore or stop fails, leave daemon stopped,
retain all generations/journal, and report the exact recovery bundle and failure;
never claim successful rollback or boot old code on format 3. Interrupted restore
is itself journaled/resumable. Later operator rollback warns about post-update
writes lost and uses this same pairing contract; it is not an entity move API.

Pin this backup while its migration/rollback is pending and after successful
adoption until explicit retention authorization. Update
`isUpdaterSnapshotDir`/`LatestSnapshot`/pruning to distinguish schema-3 bundles;
the current `pre-` test excludes only schema-2 bundles. Retained backup/parked
bytes are recovery artifacts, never a runtime fallback or an active obsolete
collection. Target database files and segments must contain no retired inverse
fields or tables, including old tombstoned payloads: create a fresh generation,
do not rely on a logical delete plus deferred compaction.

## 7. Acceptance matrix and concrete handoff

All tests use isolated fixtures/temp data and fake service/binary controllers.
No acceptance command in this design authorizes running update on the fleet.

| Fixture/fault | Required observable result | Task |
| --- | --- | --- |
| Actual schema-2 shared-store codecs with every business field populated | Lossless payload mapping; no reduced-header data loss; every catalogue name classified | migrate |
| Project/group/agent inverse arrays: nil, empty, populated, duplicates | Deterministic policy §5; all persisted old arrays absent after success | migrate |
| Two owner claims, direct/list mismatch, dangling or cross-project ref, parent cycle | Whole update blocked with source/key evidence; source unmodified | migrate |
| Disjoint archives and duplicate archive/live IDs | Single `agents` lifecycle population; duplicates block | migrate |
| Embedded jobs plus stale/missing/extra projection; same local job IDs in different pipelines | Embedded authority preserved, differences reported, collision-safe keys and direct project refs | migrate |
| Unknown job layout; disagreeing session/agent assignment; two active workers | Block; preserve source and evidence | migrate |
| Tasks in nonlexical order, all progress/outcome variants, invalid DAG/hash | Stable ordered hydration/hash/revision; invalid cases block | migrate |
| Active and past execution snapshots differ from current definition | Immutable evidence preserved; active progress agreement checked; no live mirror after cutover | both |
| Matched/unpaired/live/historical/planless autopilot populations; ID/state/slot conflicts | One authority, complete accounting and mapping; unsupported ambiguity blocks | migrate |
| Reserved context tasks/overlays/branch claims, landing and fix evidence | Materialize dispatch once; conflicts block; exact retired keys absent; landing/fix idempotency survives restart | both |
| Pending old-format operation or malformed record hidden by ordinary List | Refuse, never skip record or replay old write on schema 3 | migrate |
| Failure before/after each backup write, journal write, rename, ledger commit | Update resumes identically or restores verified original; daemon boot refuses partial state | migrate |
| Tampered/missing bundle, wrong binary/hash, divergent staging, disk-full, symlink | No unsafe promotion/restore; evidence retained; daemon stopped when rollback unsafe | migrate |
| Managed/unmanaged live daemon; stop failure or supervisor auto-restart | No source mutation without confirmed exclusive ownership | both |
| Restart crash, wrong version/schema, readiness timeout | Stop target, restore full paired generation, prove old readiness | cutover |
| Rollback after schema-3 writes, interrupted rollback, unrelated newer snapshot/.bak | Explicit data-loss handling; transaction pairing; no target-only residue | cutover |
| Fresh install, schema-2 boot, missing ledger with DB, newer schema, repeated update | Fresh 3; all incompatible existing starts refuse; update idempotent | cutover |
| All store mutations/import/restore/CAS paths try project/parent change | Reject before commit; creation defaults stamped once; no reparent endpoint | cutover |
| API/MCP/TUI/tree, pipeline watcher/retries, Plan service/finalize/export/backup/hub | Child-index reads and transactions; DTO arrays only; Plan lifecycle/task dimension retained | cutover |
| Bootstrap, doctor and manager recovery after update | No legacy importer, inverse rebuild, tag/path inference, parent clearing or rekey | cutover |
| Reopen actual target DB and inspect raw segment/record bytes/catalogue | No retired fields/collections; declared indexes present and used | cutover |
| Full repository checks | `wd check` test/lint/fmt/build pass on integrated implementation | both |

`migrate` receives this approved design plus its exact commit. Implement frozen
schema-2 raw readers, deterministic mapping/reporting, complete schema-3 staging
writers, backup/journal/resume/restore and isolated fixtures first. Do not enable
format-3 runtime startup before the `cutover` changes can read the complete target.
Deliver the manifest/report examples and fault-injection evidence for human review.

`cutover` then changes `storage.Open`, full store codecs, schema guards and
collection specs/indexes together; replaces embedded task/job read/write paths;
switches controller/daemon adapters to one autopilot store; removes the exact
fields/helpers/bootstrap repairs in §3; updates API hydration and execution
selection; and integrates updater-only mutation authority, readiness and paired
rollback. Test binaries from both sides of the boundary, not only mocked ledger
values. The intermediate migrate branch is not a deployable release. No phase
lands or runs fleet update without its required human approval.

Review decisions needing explicit human acceptance: canonical `autopilot_runs`
(retiring `autopilots`); removal of Plan executor mirrors and job/manager inverse
assignment mirrors; collision-safe tuple keys; strict conflict refusal including
contradictory empty lists; explicit global/system exception; immutable group
ownership; and the schema-2 waypoint with updater capability gating.
The concrete first compatible updater/release version and manifest capability
encoding remain release-integration inputs to resolve and test before shipping.
There are no claims about whether current live data satisfies this policy;
read-only updater planning must produce that evidence later.

## 8. Design validation evidence

Audited repository base: `b7c2f61403f316710a83e1d624759c77214596ae`.
All 39 actual catalogue entries are covered by the inventory, and all 34 local
source links resolve. Self-review checked actual shared-store registration,
embedded job projection provenance, reserved context authority, original
definition hashing, updater handoff and both rollback paths against source.
`git diff --check` passed. `wd check` passed all configured checks (`test`,
`lint`, `fmt`, `build`) after generating the normal web assets with `make ui`;
the initial attempt was blocked by absent `web/dist`. Only this design document
is included in the change. These checks validate the existing checkout and
documentation hygiene; schema-3 acceptance tests in §7 are future implementation
requirements, not tests claimed to exist or pass here.

The worker remains Plan-bound (`role=worker`, task `design`); task progress stays
`in_progress` pending the human gate. Commit/branch and validation evidence are
sent to `orchestrator-8bed8d7b` through warden messaging for this exact Plan
revision/hash. No `job done`, task completion, merge or follow-on implementation
is authorized by this document.

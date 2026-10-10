# Warden Schema-2 Migration Acceptance Fixtures and Compatibility Matrix

**Date:** 2026-10-10
**Status:** Spec freeze / Acceptance contract -- **design only** (Plan `01-warden-db-architecture-and-contract`, Task `acceptance`)
**Integration branch:** `autopilot/01-warden-db-architecture-and-contract`
**Depends on:** [`2026-10-10-warden-db-data-source-inventory.md`](2026-10-10-warden-db-data-source-inventory.md), [`2026-10-10-warden-db-collection-contract.md`](2026-10-10-warden-db-collection-contract.md), [`2026-10-10-project-view-projection-contract.md`](2026-10-10-project-view-projection-contract.md), [`2026-10-10-warden-db-operation-journal-and-reconciliation.md`](2026-10-10-warden-db-operation-journal-and-reconciliation.md)
**Scope:** Implementable adversarial acceptance fixtures, migration compatibility matrix, rollback/resume criteria, and blocking conditions for migrating schema-1 persistent Warden state into the schema-2 single ScrivaDB instance `<dataDir>/warden-db/`. This document does not change production data paths, record schemas, or writer behavior.

Keywords **MUST**, **MUST NOT**, **MAY**, **PASS**, **FAIL**, and **BLOCK** are normative.

---

## 1. Decision

Schema-2 migration is accepted only when an implementation can prove, with deterministic fixtures, that every structured legacy data source either:

1. lands in the schema-2 collection named by the data-source inventory;
2. remains outside `warden-db` as an intentional external artifact named by the inventory; or
3. is quarantined with a typed, operator-visible reason before any schema ledger is advanced.

The migration acceptance suite MUST exercise benign fixtures and adversarial fixtures. A benign fixture proves data equivalence. An adversarial fixture proves that migration refuses unsafe input, preserves rollback material, resumes idempotently after interruption, and never fabricates cross-collection invariants.

ScrivaDB cross-collection transactions are required for every internal multi-collection invariant. The acceptance suite MUST fail any implementation that migrates a multi-collection invariant through separate non-atomic writes when those writes can leave schema-2 in an observable partial state.

---

## 2. Acceptance Harness Contract

### 2.1 Fixture layout

Each fixture directory represents one complete `<dataDir>` snapshot:

```text
fixtures/schema2-migration/<fixture_id>/
+-- legacy/                 # Input dataDir before migration
+-- expected/               # Canonical expected schema-2 summary
|   +-- collections.json    # Sorted records by collection/key, excluding volatile O fields
|   +-- external.json       # Expected outside-DB artifact status
|   +-- findings.json       # Expected warnings, quarantines, or blocking errors
|   +-- project_views.json  # Expected ProjectView summaries/revisions where applicable
+-- README.md               # Fixture purpose and adversarial trigger
```

The harness MUST run migration against a copy of `legacy/`, never in place. Expected collection summaries MUST be canonical JSON: records sorted by collection then primary key, maps serialized with deterministic key ordering, timestamps normalized to RFC 3339 UTC, and volatile observed fields either fixed by the fixture or omitted by an explicit allowlist.

### 2.2 Required phases

Every fixture executes these phases:

1. **Preflight:** acquire `owner.lock`, discover legacy stores, validate segment readability, enumerate external artifacts, and compute a backup manifest.
2. **Backup:** create `<dataDir>/backups/pre-schema2-<timestamp>/` with a manifest of every copied file, size, mode, and checksum.
3. **Import transaction groups:** import normalized records into `<dataDir>/warden-db/`, using ScrivaDB transactions for each internal invariant group.
4. **Validation:** run the schema-2 validator, referential checks, uniqueness checks, operation-journal checks, and ProjectView rebuild checks.
5. **Ledger stamp:** update `<dataDir>/schema.json` to schema version 2 only after validation passes.

PASS requires phase ordering evidence. The harness MUST be able to inject a crash before and after every phase boundary and before, during, and after each multi-collection transaction group.

### 2.3 Pass, fail, block

| Outcome | Meaning | Required artifacts |
|---|---|---|
| **PASS** | Migration produced schema-2 records and external artifact statuses equivalent to the fixture expectation. | `collections.json`, `external.json`, empty or expected `findings.json`, green validator, schema ledger stamped to 2. |
| **FAIL** | The implementation completed but violated an acceptance expectation. | Diff naming collection/key/field or artifact path, plus the failed invariant. |
| **BLOCK** | Migration refused to proceed because input cannot be safely normalized. | No schema-2 ledger stamp; original legacy data intact; backup present if backup phase completed; typed blocking finding with remediation text. |

BLOCK is successful behavior for fixtures whose purpose is to prove refusal. FAIL is never acceptable for a release gate.

---

## 3. Legacy Source to Destination Compatibility Matrix

The following matrix is the mandatory coverage list. Every row MUST have at least one benign data-equivalence fixture and, where the "Adversarial coverage" column names a category, at least one adversarial fixture.

| Legacy source | Schema-2 destination or external status | Migration rule | Adversarial coverage |
|---|---|---|---|
| `<dataDir>/agents-db/agents` | `warden-db/agents` | Import active agents; strip `child_agents`, `child_pipelines`, `child_autopilots`; preserve child-owned FKs; require unique `tmux_session` when present. | crashes, conflicts, orphan references, data equivalence |
| `<dataDir>/agents-db/archived` | `warden-db/agents` | Merge into `agents` with `status=archived`, `archived_at`, and `archived_from_status` where derivable; archive never cascades children. | upgrade, downgrade, data equivalence |
| legacy `<dataDir>/sessions-db` and `<dataDir>/sessions/` | `warden-db/agents` or `warden-db/terminals` after existing sentinel semantics | Import only if prior sentinels show migration has not already occurred; preserve provenance; terminal-like sessions become `terminals`. | resume, rollback, duplicate sentinel conflicts |
| `<dataDir>/terminals-db` | `warden-db/terminals` | Import terminal panes with required `project_id` unless the legacy record is explicitly unscoped by the existing contract; reject AI-agent-only fields. | orphan references, data equivalence |
| `<dataDir>/projects` collection `projects` | `warden-db/projects` | Strip `agents`, `pipelines`, `terminals`, `plans`, `autopilots`; derive membership solely from child FKs; preserve `status`. | conflicts, orphan references, ProjectView equivalence |
| `<dataDir>/projects` collection `project_groups` | `warden-db/project_groups` plus `projects.group_id/group_order` | Convert `project_ids[]` into child-side group fields; first group by group `created_at` wins on duplicate membership, later groups produce findings. | conflicts, data equivalence |
| `<dataDir>/plans/plans-db` | `warden-db/plans` | Import canonical plans; preserve revisions; keep YAML exports inert. | crashes, data equivalence, downgrade |
| `<dataDir>/plans/plan-exports-db` | `warden-db/plan_exports`; exported YAML remains external | Import export metadata; verify referenced plan exists or block when the plan record is absent and the export claims authority. | orphan references, rollback |
| `plans/{pending,in_progress,completed,archived}/*.yaml` | Intentional external artifact | Do not import as authority; compare only as optional export evidence through `plan_exports`. | corruption, downgrade |
| `<dataDir>/pipelines-db` and legacy `pipelines/*.json` | `warden-db/pipelines`, `warden-db/pipeline_jobs` | Split embedded `jobs[]`; set `pipeline_jobs.pipeline_id`; recompute pipeline status from jobs; preserve job ordering through `seq`. | crashes, conflicts, orphan references, data equivalence |
| `<dataDir>/autopilots-db` and `<dataDir>/autopilot/runs-db` | `warden-db/autopilot_runs` | Merge identity and run-state records by `ap-<12hex>` id; require one non-terminal run per plan. | conflicts, resume, data equivalence |
| `<dataDir>/context` | `warden-db/context_entries` | Import KV entries; namespace references are not FKs; expired entries MAY be pruned only if legacy behavior would prune them. | corruption, data equivalence |
| `<dataDir>/inbox` | `warden-db/messages`, `warden-db/id_sequences` | Import messages by recipient; synthesize sequence next values from max ids; tolerate weak dangling recipients with findings. | orphan references, resume |
| `<dataDir>/backends` | `warden-db/backends`, `backend_models`, `backend_roles`, `backend_settings` | Split embedded model catalogue and singleton settings; enforce one default backend. | conflicts, corruption, data equivalence |
| `<dataDir>/usage-snapshots/usage-snapshots.jsonl` | `warden-db/usage_snapshots` | Import valid JSONL rows inside retention window; malformed rows quarantine with offset and checksum. | corruption, resume |
| `<dataDir>/quota-impact/quota-impact-fences.jsonl` | `warden-db/quota_impact_fences` | Import insert-if-absent fences; duplicate key with identical value dedupes; divergent duplicate blocks. | conflicts, corruption |
| `<dataDir>/snapshots-db` and legacy `snapshots/*.json` | `warden-db/snapshots`; `.transcript` blobs remain external | Import metadata and transcript pointers; missing transcript is a finding unless snapshot policy requires a block. | orphan references, rollback, external artifacts |
| `<dataDir>/snapshots/*.transcript` | Intentional external artifact | Never inline; verify pointer path, ownership, and checksum when available. | corruption, rollback |
| `<dataDir>/savings/ledger.jsonl` | `warden-db/savings_events`, `warden-db/id_sequences` | Import append-only events; synthesize sequence from max id; malformed rows quarantine by offset. | corruption, resume |
| `<dataDir>/savings/calibration.json` | `warden-db/savings_calibration` | Import singleton `current`; invalid numeric values block because downstream cost estimates would be unsafe. | corruption |
| `<dataDir>/spend/spend.json` | `warden-db/spend_records` | Import session map by key; duplicate/invalid sessions produce findings or block when totals cannot be parsed. | corruption, data equivalence |
| `<dataDir>/schedules-db` and legacy `schedules.json` | `warden-db/schedules` | Import schedules; require valid mode and next-run parse; weak historical references remain on spawned agents/pipelines. | upgrade, data equivalence |
| `<dataDir>/known-prompts-db` | `warden-db/known_prompts` | Import cache entries up to cap using LRU rules; loss is non-fatal but must be reported. | rollback, data equivalence |
| `<dataDir>/plan-sync-hub` | `warden-db/plan_sync_envelopes` | Import envelopes keyed by `scope:plan_id`; weak plan refs allowed with findings. | orphan references, corruption |
| `<dataDir>/metrics/YYYY-MM-DD.jsonl` | `warden-db/metrics_samples` | Import within retention window; malformed rows quarantine by file and offset. | corruption, resume |
| `<dataDir>/schema.json` | Intentional external artifact | Read before DB open; stamp to schema 2 only after full validation. | crash, rollback, upgrade, downgrade |
| `<dataDir>/owner.lock` | Intentional external artifact | Must be acquired before preflight; never copied into `warden-db`. | conflicts |
| `<dataDir>/audit.jsonl` | Intentional external artifact | Remains independent append-only audit stream; migration may append migration audit entries but must not rewrite history. | corruption, rollback |
| `<dataDir>/prompts`, `hints`, `exits`, `settings`, `session-logs`, `ratelimit-captures` | Intentional external runtime artifacts | Enumerate and preserve; do not import as structured records except bounded pointers already present in destination collections. | rollback, external artifacts |
| Git worktrees, branches, `.git/objects`, stash refs, PR identity | Intentional external VCS artifacts, referenced by records and operations | Preserve as external resources; validate only owned pointers named by records or operation-journal entries. | conflicts, rollback |
| `<dataDir>/backups`, `<dataDir>/quarantine`, `<dataDir>/tmp` | Intentional maintenance artifacts | Existing backups/quarantine remain external; migration creates new backup and quarantine output as needed. | rollback, corruption |

---

## 4. Adversarial Fixture Catalogue

### 4.1 Crashes

Crash fixtures MUST inject process death at these points:

| Fixture id | Trigger | PASS criteria | Blocking conditions |
|---|---|---|---|
| `crash-before-backup` | daemon exits after preflight, before backup creation | Legacy data unchanged; no schema ledger stamp; rerun starts from preflight. | Any partial `warden-db` accepted as schema 2. |
| `crash-mid-backup` | exit while copying backup manifest | Rerun discards or marks incomplete backup and creates a complete backup; legacy data unchanged. | Incomplete backup reused as authoritative rollback source. |
| `crash-mid-agent-pipeline-tx` | exit during transaction that imports `agents` plus `pipeline_jobs` binding | Transaction is absent or complete; no half-bound `agents.(pipeline_id,job_id)` without matching `pipeline_jobs.agent_id`. | Partial R2 bidirectional binding visible after restart. |
| `crash-after-import-before-ledger` | all collections imported, exit before `schema.json` stamp | Rerun validates existing `warden-db`, proves idempotence, then stamps ledger. | Reimport creates duplicates, rewrites ids, or diverges D fields. |
| `crash-after-ledger-before-cleanup` | ledger stamped, exit before legacy cleanup/marking | Startup opens schema 2; legacy stores remain preserved but ignored or marked migrated; no downgrade guesswork. | Startup opens schema 1 stores because they still exist. |

### 4.2 Conflicts

Conflict fixtures prove that incompatible duplicate facts are not silently merged:

| Fixture id | Input conflict | PASS/BLOCK criteria |
|---|---|---|
| `conflict-default-backend` | two legacy backends have `default=true` | BLOCK unless deterministic legacy policy names a winner; finding names both backend ids. |
| `conflict-live-autopilot-per-plan` | two non-terminal autopilot runs reference one plan | BLOCK; cross-collection uniqueness would reject one live run per plan. |
| `conflict-tmux-session` | agent and terminal share a non-empty `tmux_session` | BLOCK unless one record is archived/terminal and contract allows non-active reuse; finding names both records. |
| `conflict-pipeline-job-agent` | `pipeline_jobs.agent_id` derived from embedded job disagrees with `agents.pipeline_id/job_id` | BLOCK for active agents; finding includes both sides. Archived-agent exception follows collection contract R2. |
| `conflict-project-group-membership` | project appears in several group `project_ids[]` arrays | PASS with first group by `created_at` assigned and findings for skipped groups. |
| `conflict-quota-fence-duplicate` | duplicate JSONL fence key with different payload | BLOCK; duplicate identical rows dedupe. |

### 4.3 Orphan references

Orphan fixtures distinguish strong, weak, and retained references:

| Fixture id | Orphan | Required outcome |
|---|---|---|
| `orphan-plan-project` | `plans.project_id` missing | BLOCK: strong FK cannot be invented. |
| `orphan-agent-project` | legacy agent lacks `project_id` | PASS as unscoped legacy agent with finding; new schema-2 writes would reject missing project. |
| `orphan-pipeline-job-parent` | job references missing pipeline | BLOCK. |
| `orphan-message-recipient` | message `to` missing agent | PASS with weak-FK finding; reads must tolerate missing recipient until compaction/purge. |
| `orphan-snapshot-agent-archived` | snapshot points to archived agent | PASS; archived agents satisfy retained references. |
| `orphan-snapshot-agent-missing` | snapshot points to absent agent | BLOCK unless an explicit legacy policy quarantines the snapshot metadata and transcript pointer together. |
| `orphan-plan-sync-plan` | envelope points to absent plan | PASS with weak-FK finding. |

### 4.4 Corruption

Corruption fixtures MUST include malformed ScrivaDB segments, malformed JSON/JSONL rows, invalid timestamps, non-canonical ids, and checksum mismatches.

PASS for recoverable corruption requires quarantining the smallest corrupt unit: JSONL row by file offset, flat JSON source by key where possible, ScrivaDB record by collection/key, or whole source only when record boundaries cannot be trusted. BLOCK is required when a corrupt source contains audit-grade or strong-FK parent data whose safe destination cannot be determined.

The harness MUST assert that `audit.jsonl`, `schema.json`, `owner.lock`, transcripts, Git objects, and backup/quarantine artifacts remain external and are not rewritten to "fix" corruption unless an explicit repair command, outside this migration acceptance contract, is invoked.

### 4.5 Rollback

Rollback fixtures validate both automatic refusal rollback and operator-requested rollback:

| Fixture id | Trigger | PASS criteria |
|---|---|---|
| `rollback-pre-ledger-block` | BLOCK occurs before `schema.json` stamp | Legacy stores remain authoritative; any partial `warden-db` is removed, marked incomplete, or ignored on next startup; backup manifest verifies. |
| `rollback-post-backup-user-abort` | operator aborts after backup, before import | Legacy stores unchanged; backup may remain; no schema ledger stamp. |
| `rollback-external-artifacts` | migration imports snapshot metadata then blocks on another source | Transcript blobs, worktrees, branches, plan YAML exports, audit log, and runtime files are byte-identical to input. |
| `rollback-operation-journal` | partial operation-journal rows exist from a prior experimental schema-2 run | Non-terminal rows prevent destructive cleanup; rollback reports them and leaves external effects untouched. |

Rollback MUST NOT delete Git worktrees, branches, transcripts, prompts, hints, exit sentinels, audit rows, or provider-visible artifacts merely because their schema-2 metadata was not committed.

### 4.6 Resume and idempotence

Resume fixtures rerun the same migration after a crash, after a validation failure that has been manually fixed in legacy input, and after a complete PASS. Required criteria:

- primary keys are stable and no duplicate records appear;
- `id_sequences.next` remains exactly one greater than the maximum imported id for each sequence namespace;
- D fields (`pipeline.status`, plan/export hashes, `updated_at` where fixture-controlled) converge to the same canonical values;
- weak-FK findings are stable and not multiplied on rerun;
- quarantine outputs use stable content checksums or run ids so rerun does not hide the original evidence;
- the backup phase never overwrites the only rollback source.

### 4.7 Upgrade and downgrade

Upgrade fixtures cover schema-1 inputs from every known legacy sentinel combination in the data-source inventory. PASS requires the implementation to avoid double-importing a source already covered by a sentinel and to import missing sources when sentinels are absent but the source exists.

Downgrade compatibility is intentionally limited:

| Scenario | Required behavior |
|---|---|
| Schema-2 ledger present, schema-1 daemon starts | Refuse to start with a clear "schema 2 unsupported by this binary" error; never write schema-1 stores. |
| Schema-2 migration PASS, operator requests export for older binary | MAY produce explicit export artifacts only through a separate downgrade/export command; not part of automatic startup. |
| Schema-2 migration BLOCK/abort before ledger stamp | Schema-1 daemon may continue using untouched legacy stores. |
| Plan YAML exports after schema-2 migration | Remain inert human-review artifacts; older binaries must not be told to treat them as canonical plan DB state. |

### 4.8 Data equivalence

Data-equivalence fixtures compare schema-1 input to schema-2 output at the semantic level:

- every schema-1 authoritative fact has exactly one schema-2 authoritative destination or an external-artifact classification;
- forbidden inverse lists are absent from every schema-2 record;
- child membership in ProjectView equals indexed child FKs, not legacy parent arrays;
- embedded pipeline jobs become standalone `pipeline_jobs` with stable ordering and dependencies;
- backend embedded models/settings split into their target collections without changing enabled/default/tier semantics;
- JSONL/event streams preserve event order, ids, timestamps, and retention exclusions;
- outside artifacts are byte-identical unless the migration explicitly appends an audit event or creates backup/quarantine output.

---

## 5. Operation Semantics Acceptance

Migration may create operation-journal records when it discovers schema-2 metadata that names an external effect requiring reconciliation, or when the implementation uses the journal to make migration-side external work durable. The following acceptance rules apply:

1. No tmux, Git, filesystem, native CLI, or provider effect may run while a ScrivaDB transaction is open.
2. Any internal reservation that requires an external effect MUST commit the domain row and operation row atomically.
3. Replaying a migration fixture MUST not create duplicate tmux sessions, Git worktrees/branches, transcript blobs, provider submissions, or prompt/hint/runtime files.
4. Existing foreign external resources MUST produce `manual_intervention` or a blocking finding, not cleanup.
5. Non-terminal operation rows block pruning, rollback cleanup, and ledger stamping unless the reconciler can prove a terminal outcome.

Acceptance fixtures MUST include at least one operation row per effect family named by the operation-journal contract: tmux, Git, native CLI, provider, and filesystem.

---

## 6. Relationship Rules Acceptance

The validator run after import MUST prove:

- all strong FKs exist or point to retained archived records where the collection contract allows it;
- weak FKs are reported but do not block;
- `agents.parent_id` and `pipeline_jobs.depends_on` are acyclic;
- unique and partial-unique constraints hold;
- immutable fields from legacy sources are stable across resume;
- `pipeline_jobs.agent_id` and `agents.(pipeline_id,job_id)` are set or cleared in one transaction;
- restricted deletes and cascades are not performed by migration except for documented quarantine of invalid records;
- no schema-2 record contains a forbidden inverse list or embedded `Pipeline.jobs[]`;
- ProjectView rebuild from schema-2 records produces the fixture's project summaries.

Any violation of a strong invariant after import is a FAIL, not a repair opportunity, unless the fixture explicitly expects a BLOCK before ledger stamp.

---

## 7. Migration Blocking Conditions

Migration MUST block before stamping schema 2 when any of the following are true:

1. `owner.lock` cannot be acquired or indicates another live daemon/CLI owns the data directory.
2. Backup cannot be completed and verified.
3. `schema.json` is unreadable, internally inconsistent, newer than the binary supports, or already stamped with a different in-progress migration generation.
4. A legacy source needed for an audit-grade or strong-FK parent record is corrupt beyond record-level quarantine.
5. A strong FK target is missing and no explicit quarantine rule removes the dependent record safely.
6. A uniqueness constraint would be violated: duplicate live tmux identity, duplicate default backend, duplicate active job-agent binding, duplicate live autopilot run per plan, duplicate `(kind,idempotency_key)` operation.
7. A normalized relationship would require trusting a forbidden inverse list over an existing contradictory child FK.
8. A transaction group cannot be committed atomically by the single schema-2 ScrivaDB instance.
9. An external artifact pointer names a path outside allowed data roots, crosses a symlink boundary unexpectedly, or has an ownership/checksum mismatch for an artifact the destination record treats as authoritative.
10. Non-terminal operation-journal rows or ambiguous external effects require reconciliation before migration can prove data equivalence.

On BLOCK, the migration MUST leave a machine-readable finding and human-readable remediation. It MUST NOT advance `schema.json` to 2.

---

## 8. Release Gate Checklist

A schema-2 migration implementation is releasable only when:

- the compatibility matrix in section 3 has fixture coverage;
- every adversarial category in section 4 has a passing fixture or an expected BLOCK fixture;
- the operation semantics in section 5 are verified;
- ProjectView equivalence is verified for at least one multi-project fixture with agents, terminals, plans, pipelines, jobs, schedules, and autopilot runs;
- rollback and resume are verified with crash injection;
- downgrade refusal is covered by startup compatibility tests;
- the test report lists every legacy source and marks it `migrated`, `external`, `quarantined`, or `blocked`;
- `wd check` passes for the implementation branch before PR.

This acceptance contract intentionally does not prescribe exact Go test names or fixture file formats beyond the canonical summary requirements above. Implementations may use table-driven Go tests, golden files, or a migration harness binary, but the observable pass/fail/block semantics are fixed by this document.

# Schema-3 migration implementation and cutover handoff

Bound assignment: Plan `plan-4a55e219`, revision 3, hash
`sha256:8c4f9b7a4f96e707b02722c8bc28c546eab0153c4efdb1f5b9f678cfe9da2ea0`,
task `migrate`, parent `orchestrator-8bed8d7b`.
Approved design: `dfe0d081c6596ca5ca822ba6f12ae6f70c5450f8`,
[ownership design](2026-10-25-schema-3-ownership-design.md).
This implementation is an intermediate stack layer, subject to human review.
It neither completes the canonical task nor authorizes cutover or fleet update.

## Implemented boundary

`internal/migrate/schema3` contains frozen source field/kind declarations and
raw readers, a deterministic whole-generation mapper, full payload staging
writers and an independent target validator. It does not use the reduced
relationship structs as persistence codecs. All 39 source catalogue collections
are explicitly classified. Global records survive without domain reinterpretation;
source absence and present-empty collections have different digests. Readers
verify segment checksums and transaction integrity, open only private copies,
select current raw revisions without best-effort List decoders, preserve JSON
numbers and reject a source that changes during inspection.

The mapper merges disjoint archives (stamping `lifecycle=archived`), initializes
active lifecycle on the remaining agents, normalizes parent references, validates
legacy inverse claims, moves group order to projects, and creates collision-safe
JSON tuple keys for full job/task rows. Embedded jobs are the schema-2 authority;
projection differences and old/new keys are reported. Hash proofs use a frozen
copy of the existing canonical definition byte algorithm, checked against the
production implementation. Current execution progress is validated then removed
from active execution storage; snapshot-at-start definitions remain intact.

Operational authority is consolidated into `autopilot_runs`, retaining the
registration name, complete diagnostics, original timestamp provenance and exact
executor mappings. The existing manager slot spelling is `autopilot`; `brain`
identifies the optional brain relationship in the target. Worker slots are not
unique. Current dispatch moves to Plan task `dispatch`, with its own operational
state and execution ID. Dispatch from a uniquely identified past execution is
retained as that execution's `task_dispatch` evidence, without changing its
executor ID, snapshot or task lifecycle. Landing/fix/restart/journal payloads
survive in context. Reserved current task/overlay/branch keys are removed only
after checking their claims. Ambiguous reserved keys block; reserved keys of a run
that no longer exists in any form are dropped.

### Stale schema-2 bookkeeping

Long-lived schema-2 installs accumulate references that carry no ownership. The
mapper resolves each of them deterministically and records a finding instead of
refusing the whole generation; the paired backup keeps every original row.

| Source condition | Disposition |
| --- | --- |
| Retired forward list (`projects.agents`, `agents.child_agents`, …) misses a child that points back | `derived from child`: the child's back-reference wins |
| Retired forward list names a record that does not exist | `dropped missing child` |
| Agent bound to an autopilot run that does not exist | `cleared missing run` (run, slot and task binding removed) |
| Agent with an autopilot task binding but no run | `cleared task without run` |
| Archived agent still without a project after structural resolution | `deleted archived without project`; references to it are `cleared deleted parent` |
| Plan executor mirror naming a missing executor | `dropped missing executor mirror` |
| Completed/archived plan whose last executor is gone | `executor gone (finished plan)`; `active_execution` kept as history |
| Plan with no stored `content_hash` | `initialized` from the frozen canonical hash |

Genuine conflicts still block: two owners for one record, cross-project
references, a present hash that disagrees, a **live** agent without a project, an
unfinished plan whose executor is missing, and every other condition below.

`internal/updater/schema3_transaction.go` implements the private offline
transaction core: stopped managed service and absent-health checks, sole owner
lock, deterministic precomputed mapping, immutable planned manifest identity,
exclusive backup copies, exact old binary pairing, fsynced intent/completion
journal, fresh physical staging, index/reference/hash verification, old-generation
parking, target promotion, ledger commit with a pending transaction, resumable
full-generation restore, and exact readiness finalization. A pending transaction
has an extended schema-ledger reference binding transaction, journal, manifest,
formats and target binary hash. Resume verifies both sides of interrupted
renames and never overwrites divergent output. Partial backups can be completed
only against the unchanged original generation and immutable backup plan.
Torn/corrupt journals, incomplete divergent files, and unverifiable bundles refuse
recovery and retain evidence. A failed target generation is preserved separately.
Restoring the entire old database removes target-only collections and indexes.
Schema-3 bundles are excluded from generic latest-snapshot selection and pruning.

No production call site invokes this private core yet. The format constants,
production collection registration and runtime codecs remain at schema 2.
`migrate --check --target 3` and `migrate --plan --target 3` provide read-only
reports. Standalone format-3 apply/resume/restore/staging export refuse, including
attempts to use the legacy restore runner on a schema-3 journal. Update planning,
target preflight, the old target-migration subprocess hook and release manifest
generation refuse the incomplete schema-3 boundary. No environment variable can
unlock it. Tests use isolated directories and fake service/binary controllers;
no fleet update, live inspection or live migration has been performed.

## Capability protocol and release inputs

Release requirements use an additive `requires.capabilities` JSON object with
exact integer protocol versions:

```json
{
  "schema_version": 3,
  "min_schema": 2,
  "breaking": true,
  "requires": {
    "capabilities": {
      "stopped-daemon": 1,
      "schema3-transaction-handoff": 1,
      "verified-paired-backup": 1,
      "generation-restore": 1
    }
  }
}
```

This is an envelope excerpt, not a publishable manifest. Unknown capabilities,
missing required capabilities and different protocol versions refuse. The
schema-3 release also requires explicit `min_upgrade_from`, a preceding updater
waypoint and release notes. The ordinary three-minor support-window default is
not evidence of updater compatibility. `cmd/genmanifest` currently refuses
schema 3 entirely; no release version was invented.

The chosen mutation boundary is an in-process updater transaction: only private
updater code owns the migration and owner lock. It can carry the authenticated
transaction, target binary SHA-256, canonical data directory, manifest hash and
source format directly; public migration commands cannot construct that
handoff. This avoids transferring the lock to the existing unauthenticated
`migrate --apply` subprocess. If cutover chooses a subprocess instead, it must
implement an authenticated descriptor handoff binding those identities; the old
environment/CLI invocation is not an acceptable substitute.

Before shipping, cutover/release integration must supply and test:

* Full runtime format-3 codecs, child reads/writes, index declarations, bootstrap
  guards and removal of inverse/dual-authority paths from the approved design.
* The updater-only call site, actual free-space measurement, full affected-config
  path selection, target binary version/capability inspection and immutable
  manifest validation. The transaction core accepts measured available bytes;
  its tests inject capacity, not a guessed production capacity.
* An authenticated readiness start and lock ownership transition, exact binary
  version/format/ownership response, target-stop-before-rollback orchestration,
  and proof of old readiness after paired restore. These remain cutover work.
* The first real format-2 updater waypoint version, established by testing the
  actual old/new release binaries. Publish it first; then explicitly set
  `min_upgrade_from`, `min_schema=2`, capabilities and matching binary metadata
  on the format-3 release. Old updaters ignore additive requirements, so both
  the version gate and release distribution path must exclude them. Schema-1
  users must reach the schema-2 waypoint first.
* End-to-end repeated-update/finalized-journal handling and operator rollback
  after new writes, including explicit data-loss consent and transaction-based
  selection. The core is idempotent before readiness and restores exact paired
  data; it does not select a generic latest snapshot or unrelated `.bak`.

Scriva v1.6 verifies record checksums after decoding numeric payloads through
float64. Some otherwise-valid JSON numbers (for example `9007199254740993`)
cannot survive that engine round trip. The writer refuses such payloads before
creating staging instead of rounding or writing a corrupt generation. A source
already affected by this engine limitation fails checksum verification. Shipping
must preserve this blocker or use a separately validated storage-engine fix;
this migration does not silently weaken checksum verification.

## Report and manifest examples

These excerpts illustrate the actual serialized fields. Digests and paths are
placeholders; they are not a recovery bundle and cannot be used to authorize an
update or restore. The real manifest includes every inventory file, source-only
collection, unchanged collection and required index; `MANIFEST.json` is written
last. Its digest is in the journal and immutable transaction plan outside the
backup, never recursively inside the manifest itself.

```json
{
  "policy": "ownership-v1",
  "source_counts": {"projects": 1, "plans": 1, "pipelines": 1},
  "findings": [{
    "collection": "pipeline_jobs", "key": "pipe:job",
    "field": "projection", "evidence": null, "disposition": "rebuilt"
  }],
  "keys": {"embedded:[\"pipe\",\"job\"]": "[\"pipe\",\"job\"]"},
  "executors": {"live-executor": "registered-run"},
  "plans": {"plan": {"revision": 7, "before": "sha256:<definition>", "after": "sha256:<definition>"}},
  "retired_collections": ["archived_agents", "autopilots"],
  "removed_fields": {"pipelines": "jobs", "pipeline_jobs": "agent_id session_id"},
  "source_digests": {"plans": "sha256:<source>"},
  "target_digests": {"plan_tasks": "sha256:<target>"},
  "target_counts": {"plan_tasks": 2, "pipeline_jobs": 1}
}
```

```json
{
  "format": 1,
  "transaction": "example-transaction",
  "migration": "0003-ownership-normalization",
  "policy": "ownership-v1",
  "source_format": 2,
  "target_format": 3,
  "capability_version": 1,
  "data_dir": "/isolated/example",
  "binary_path": "/isolated/bin/warden",
  "created_at": "2026-10-26T00:00:00Z",
  "source_version": "<tested-waypoint>",
  "target_version": "<tested-target>",
  "source_binary_sha256": "sha256:<paired-old-binary>",
  "target_binary_sha256": "sha256:<verified-target-binary>",
  "source_ledger_sha256": "sha256:<original-ledger-and-history>",
  "source_tree": "sha256:<original-full-generation>",
  "files": [{"path":"data/schema.json","type":"file","size":123,"mode":384,"sha256":"sha256:<original-ledger-and-history>"}],
  "restore_paths": ["warden-db", "schema.json", "config.yaml"],
  "restore_deletion_set": ["warden-db"],
  "external_untouched": ["session-logs", "snapshots"],
  "source_indexes": {"agents": [{"field":"id","unique":true}]},
  "required_target_indexes": {"pipeline_jobs": [{"field":"pipeline_id","unique":false}]},
  "mapping": {"policy":"ownership-v1","target_counts":{"plan_tasks":2,"pipeline_jobs":1}}
}
```

## Validation and review

`wd check schema3` runs the focused mapper/updater suites. Fixtures exercise full
production Agent, Terminal and Job wire payloads, ordered Plan hashes against
`planstore.ComputeContentHash`, all source collection classifications, conflicting
claims/cycles/archive duplicates, stale bookkeeping (`stale_test.go`), stale projections, assignment
aliases, multiple attempts, current and historical autopilot populations,
manager/worker slot distinctions, dispatch overlays, landing and fix evidence,
unknown reserved fields, pending operations, unsafe paths and numeric refusal.

Transaction tests inject failure before/after backup copying and sealing,
journal intent/completion, staging writes, each promotion rename and ledger
commit; they reload the durable transaction, resume, verify output and restore
the paired original. Restore faults cover generation parking/promotion,
ledger/config replacement and binary replacement. Additional cases cover changed
binary/manifest/bundle/source, torn journal, divergent staging, insufficient
capacity and exclusion from generic snapshot selection. CLI/runner tests verify
read-only previews and mutation refusals without even writing adoption evidence.

Self-review added independent target validation at the staging boundary, retained
unique source index declarations and explicit target index verification,
pre-backup intent journaling with resumable immutable planning, real manager-slot
semantics, historical dispatch preservation and the legacy restore guard. Final
check/PR/CI evidence accompanies the human approval handoff. No result here claims
that the runtime cutover or actual release-binary compatibility tests have run.

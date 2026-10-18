# Schema-2 activation: runtime, updater and safe cutover

**Date:** 2026-10-18
**Status:** Implemented — activates the schema-2 migration built by Plan 06 (see the [contract](2026-10-11-warden-db-schema-2-migration-contract.md) and [acceptance fixtures](2026-10-10-warden-db-schema-2-migration-acceptance.md)). Intended for the schema-breaking major release (v11.0.0).
**Operator view:** [release notes and runbook](../RELEASE_NOTES-schema2-migration.md).

## 1. The gap that was closed

At v10.0.1 the migration existed but nothing activated it: `schema.SchemaVersion` was 1, the updater's `Migrate` hook only reconciled config, `storage.Open` could still open the schema-1 stores, and the daemon had never actually run on `warden-db` (it panicked on a non-file audit trail). Audit of the cutover on realistic data (seeded through the real compat stores) additionally showed it would have **refused or silently dropped** real installs:

| Finding | Effect before | Fix |
| --- | --- | --- |
| `autopilots-db` mapped to `autopilot_runs` | every install with an autopilot record was blocked | two collections: `autopilots`, `autopilot_runs` |
| archive read from `agents-db/archived` (real name `closed`) | archived agents silently dropped | `closed` → `archived_agents` |
| JSONL/JSON sources read as Scriva stores | usage, fences, savings, spend, metrics silently dropped | real flat-file loaders under the runtime's own keys |
| `audit.jsonl` external only | pre-cutover audit invisible to `Audit.Read`, and the file went stale | copied into `audit_events`; the daemon keeps appending to both (`audit.Tee`) |
| project/group membership, embedded `pipeline.jobs`, agent `child_*` stripped | projects appeared empty, pipelines lost jobs | carried verbatim (runtime stores are still schema-1 shaped) |
| `sessions-db` present after the boot importers | every real upgraded install blocked | import-time copies are reconciled and reported, not re-imported |
| updater rollback marker (`in_progress` without a migration id) inherited as the migration's backup | the cutover failed under `wd update` | a migration only inherits its own journaled snapshot |
| `Runner.Restore` copied the backup wrapper (`data/`, `MANIFEST.json`) into the data dir | restore polluted the dir and a re-cutover was blocked | restore from `<backup>/data` |
| updater `PruneSnapshots` / `wd rollback` treated `pre-schema2-*` as an updater snapshot | the immutable backup could be pruned | excluded from both |
| updater rollback after a promoted cutover left `warden-db/` beside the restored schema-1 ledger | rolled-back binary refused to start | promoted `warden-db/` moved to `backups/abandoned-schema2-*` |
| pure preflight refusal left an `interrupted_migration` journal | old binary could not start until `--restore` | refusals before any live change restore the ledger as found |

## 2. Version, ledger and guard

* `SchemaVersion = 2`, `MinSchema = 1`, `LegacyBaseline = 1` (a pre-ledger legacy install is stamped at 1 and must migrate). A fresh dir is stamped at 2 (`baseline-fresh`).
* Guard table (a binary of schema *B* reading a ledger of schema *D*): `D == B` start; `D > B` `data_newer` (an old binary refuses schema 2 from the ledger alone, before any store); `D < B` `needs_migration` (schema-1 → message points at `migrate --plan/--apply`); `warden-db/` beside `D < 2` → `schema2_residue` (outranks needs-migration: a cutover would refuse to merge into it); an `in_progress` migration outranks everything.
* `storage.Open` is warden-db only. It refuses schema-1 ledgers, interrupted journals and legacy stores without a ledger (a data dir with no data is a fresh install). `storage.OpenLegacy` exists for fixtures/offline tooling and refuses anything that is, or is becoming, schema 2.

## 3. Updater flow (`wd update`)

1. `capture` → strict in-process preflight (`Runner.Check`, which now includes `Migration0002.Check` = the strict schema-2 preflight, lenient only about "a daemon owns the dir" because the daemon is still up) → download/verify → **target preflight**: `<target> migrate --check --json` (same checks plus the `schema2_cutover_pending` marker; this updater demotes the marker to a note, an updater that cannot run the cutover treats it as a blocker and stops before any change).
2. Stop the daemon → updater snapshot (`backups/pre-v<ver>-<ts>/`, journaled as a marker) → swap binary → codesign.
3. **`Migrate` hook (`runUpdateMigrations`)**: reconcile config, ask the *installed target binary* for its schema (`version --json`), and when the ledger is behind run `<target> migrate --apply --data-dir <dir>`. The target binary owns the migration because only it knows the target layout. Afterwards the ledger must read the target schema with no journal, or the step fails.
4. Clear the snapshot marker → restart → `waitReady` requires the target **version and schema** (a daemon on the wrong schema is not a finished update).
5. Any failure ⇒ binary restored, snapshot restored (`RestoreSnapshot` also moves a promoted `warden-db/` aside), old service restarted, old version and schema verified healthy; both the cause and the rollback outcome are reported.

Never started in mixed mode: the new daemon only starts after step 3 succeeded, and its boot guard independently refuses unmigrated data.

## 4. Cutover phases (unchanged contract, now wired)

`owner lock → strict preflight → immutable backup + MANIFEST (journaled) → staging export → equivalence (counts/hashes/relationships) → promote (rename) → promoted-target equivalence → ledger := 2`. Resume continues the journaled phase from the same backup; restore verifies the backup, preserves any promoted target, restores the legacy files and clears the journal.

### 4.1 Pipeline reconciliation (added after the first live cutover attempt)

The first real cutover refused with seven blockers: `pipelines/` held six JSON files `pipelines-db` no longer had, and six archived agents still named a deleted pipeline. Evidence from the data: `pipelines-db` carried a delete tombstone for every one of those ids (the legacy import is additive and sentinel-guarded, so the database is authoritative once imported and the JSON directory is a stale backup). The exporter now reconciles instead of ignoring or blindly unioning (`internal/migrate/pipeline_reconcile.go`):

* identical file/record pairs dedupe; a differing pair is a `pipeline_legacy_conflict` (no winner, no "newest");
* a file whose pipeline has a tombstone is superseded, not resurrected; with no tombstone the deletion is unproven (`pipeline_unproven_deletion`) and the export refuses;
* agents/plans of a missing pipeline: archived/finished (`done`/`errored`/`orphaned` agents, `completed`/`archived` plans) keep the history as `historical_pipeline` (`wardendb.HistoricalRef`: id, job id, reason `deleted`|`unrecorded`, evidence, migration id) and lose the dangling `pipeline_id`/`job_id`, which the relationship validator (unchanged) requires to resolve; anything live refuses (`pipeline_active_ref`);
* `agentstore.Agent` and `planstore.Plan` gained the optional `historical_pipeline` field so the runtime round-trips it; the equivalence hashes are computed over the same reconciled rows, so staging, verification and the promoted target agree;
* diagnostics are typed (`FindingClass`), bounded (25 per kind, 10 in the error text, 200 chars of any source text), and never copy record values;
* the refusal error (`migrate.RefusalError`) now carries the findings that the cutover used to drop, and `Migration0002.Check` runs the same mapping on a private copy of the sources (`PreflightStagingMapping`) so the updater sees a blocker before it stops anything.

Also fixed: `messages` is no longer given a unique `id` index in staging (mailbox ids repeat per recipient; the runtime spec has none), which refused any real inbox.

## 5. Direct-disk consumers on schema 2

Anything that read a legacy store directory straight from disk is either re-pointed or refuses: `audit` readers keep working (file still appended), `doctor` ownership uses the data-dir lock, offline `models`/backend registry, `repair sessions|agents|backends|all` and `savings --calibrate` refuse with guidance (`requireSchema1Stores`). `repair warden-db` is the offline verifier/repairer.

## 6. Residual risks

See the runbook's "Known limits": already-shipped updaters cannot run the cutover (waypoint release recommended), runtime stores still schema-1 shaped, `plan-sync-hub` still file-served, `savings --calibrate` needs a daemon API.

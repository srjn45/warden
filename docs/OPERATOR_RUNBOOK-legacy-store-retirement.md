# Operator Runbook: Legacy Store Deprecation and Retirement

This runbook outlines the operational lifecycle, verification steps, retirement criteria evaluation, and safe backup-gated cleanup of legacy (schema-1) stores following the schema-2 migration.

---

## 1. Core Invariants and Deprecation Guarantees

* **Unified Authority:** On schema 2, `<data_dir>/warden-db/` is the single source of truth for all stores.
* **No Runtime Fallback:** Legacy database directories (`agents-db`, `sessions-db`, `projects`, `backends`, `plans/plans-db`, etc.) are inert copies kept from the migration. They are never read or written by the daemon or CLI runtime. Any attempt to open legacy stores on schema 2 (`storage.OpenLegacy`) is permanently blocked and recorded as an adoption violation (`legacy_open_refused`).
* **Offline Repair Deprecation:** Schema-1 repair tools (`warden repair sessions`, `warden repair agents`, `warden repair backends`, `warden repair all`) strictly refuse on schema 2. All offline integrity checks and repairs must be conducted via `warden repair warden-db [--dry-run]`.
* **Support Window:** Legacy stores and the immutable `backups/pre-schema2-*` cutover backup are retained for at least **90 days** post-cutover.
* **Backup-Gated Deletion:** Legacy source data is never removed automatically. Cleanup is explicit, gated on retirement criteria and backup verification, audited, and moves files to quarantine rather than deleting them directly.

---

## 2. Step-by-Step Operator Procedure

### Step 1: Read-Only Legacy Inspection

At any point during the 90-day support window, inspect the state of the retained legacy stores and compare them with the active `warden-db`:

```sh
# Terminal summary
warden inspect legacy

# JSON report
warden inspect legacy --json
```

**What it verifies:**
* Performs read-only inspection using private temporary copies.
* Confirms zero data-dir mutations via SHA-256 tree fingerprinting before and after.
* Re-verifies the cutover backup against its `MANIFEST.sha256`.
* Diffs per-collection record keys between legacy and canonical stores.
* Refuses on non-schema-2 directories to prevent dual authority.

---

### Step 2: Retirement Criteria Evaluation

Before attempting cleanup, evaluate whether the installation is eligible:

```sh
# Terminal summary
warden migrate --retirement

# Machine-readable report
warden migrate --retirement --json
```

**Retirement Gate Criteria (All 9 Required):**
1. `schema2_active`: Ledger is schema ≥ 2 without an open migration journal.
2. `warden_db_present`: The `warden-db/` directory exists.
3. `migration_succeeded`: Cutover entry (`0002-schema2-cutover` or fresh install) is recorded in ledger history.
4. `backup_verified`: Cutover backup re-verifies against its `MANIFEST.sha256` checksums.
5. `no_unresolved_failures`: Zero unrecovered `migration_failed` or `recovery_failed` events since cutover.
6. `no_rollback`: No updater rollbacks or recovery restores recorded, and no abandoned targets.
7. `zero_legacy_runtime_opens`: No attempted opens of legacy stores on schema 2.
8. `schema2_runtime_observed`: Minimum of 3 daemon boots observed on schema 2 (`runtime_boot` telemetry).
9. `support_window_elapsed`: At least 90 days have elapsed since the cutover timestamp.

If `eligible: false`, the report lists the specific `blockers`. Do not proceed with cleanup until all criteria are met.

---

### Step 3: Archive Cutover Backup Off-Host

Confirm that the cutover backup directory recorded in `schema.json` (e.g. `<data_dir>/backups/pre-schema2-<timestamp>/`) has been copied to off-host cold storage or enterprise backup repository.

---

### Step 4: Preview Cleanup (Dry-Run)

Run cleanup in dry-run mode to inspect candidate paths:

```sh
warden migrate --cleanup-legacy --dry-run
```

**Verification:**
* Evaluates the retirement gate and aborts if ineligible.
* Verifies `warden-db` integrity and backup manifest.
* Lists paths that will be moved to quarantine.
* Lists paths that will be kept in place (e.g. `snapshots` holding external transcripts).
* Makes zero filesystem mutations.

---

### Step 5: Execute Backup-Gated Cleanup

Execute the cleanup with explicit confirmation:

```sh
# Interactive prompt (prompts [y/N])
warden migrate --cleanup-legacy

# Non-interactive confirmation
warden migrate --cleanup-legacy --yes
```

**Outcome:**
* Exact migrated legacy paths are moved to `<data_dir>/backups/legacy-quarantine-<UTC-timestamp>/`.
* External files and runtime artifacts (`snapshots/*.transcript`, `session-logs/`, etc.) remain in place.
* `warden-db/` is untouched and continues active operations uninterrupted.
* An audit event (`action: "legacy_cleanup"`) is appended to `audit.jsonl`.
* An adoption evidence event (`kind: "legacy_cleanup"`) is appended to `.adoption-evidence.jsonl`.

---

### Step 6: Verify Active Schema-2 Operation

Verify that the daemon and CLI continue normal operations on `warden-db`:

```sh
# Check store health
warden doctor

# Verify daemon status
warden version
curl -s http://127.0.0.1:port/healthz
```

---

## 3. Disaster Recovery and Reversal

### Scenario A: Immediate Reversal (Pre-Quarantine Deletion)
If legacy stores are required immediately after cleanup, move the files back from the quarantine directory:

```sh
# Move files back from quarantine to data dir
cp -r <data_dir>/backups/legacy-quarantine-<timestamp>/* <data_dir>/
```

### Scenario B: Recovery from Immutable Cutover Backup
If the quarantine directory was deleted or damaged, restore from the immutable cutover backup:

```sh
# Verify backup integrity first
warden inspect legacy

# Restore files from immutable backup
cp -r <data_dir>/backups/pre-schema2-<timestamp>/* <data_dir>/
```

### Scenario C: Deleting the Quarantine Directory
Once the operator has verified continuous healthy schema-2 operation over multiple daemon restarts and confirmed off-host backup retention, the quarantine directory under `<data_dir>/backups/legacy-quarantine-<timestamp>/` may be purged manually:

```sh
rm -rf <data_dir>/backups/legacy-quarantine-<timestamp>
```

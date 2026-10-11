# Schema-2 Migration Contract (guard, manifest, inventory, backup, operator UX)

**Date:** 2026-10-11
**Status:** Implemented contract layer — Plan `06-warden-db-schema-v2-migration-and-cutover`, task `migration-contract`. No data is migrated by this change.
**Depends on:** [acceptance fixtures](2026-10-10-warden-db-schema-2-migration-acceptance.md), [data-source inventory](2026-10-10-warden-db-data-source-inventory.md), [update process §3–4](2026-10-09-update-process.md).

## 1. Rules

1. **Daemon stopped.** The cutover takes the data-dir ownership lock for its whole life (`migrate.BeginSchema2`). There is no online or dual-writer path.
2. **Never silent.** Nothing is overwritten, merged, salvaged, deleted or "fallen back" to. A pre-existing `warden-db/` beside a schema-1 ledger is a *blocking* finding, not something to adopt.
3. **Old binaries refuse before opening legacy stores** (`internal/schema/guard.go`):

| Data dir state | Binary reading schema 1 |
|---|---|
| ledger `schema_version >= 2` | `data_newer` — refuses; message names schema 2, warden-db, and that there is no automatic downgrade |
| ledger absent **and** `warden-db/` present | `schema2_residue` — refuses **before** stamping a fresh schema-1 ledger |
| ledger `1` **and** `warden-db/` present | `schema2_residue` — refuses (two candidate authorities) |
| ledger `in_progress` migration | `interrupted_migration` (unchanged; outranks the above) |

The same table gates `storage.Open` (composition root) and the offline CLI paths that open stores outside the daemon (`warden doctor --reconcile-membership`, via the read-only `schema.Guard`).

## 2. Inventory

`migrate.PlanSchema2` is read-only. It classifies every top-level data-dir entry as `migrated` (a source with a schema-2 destination), `external` (audit stream, ledger, lock, runtime artifacts, plan YAML exports, snapshot transcripts, backups, operator copies, config) or `unclassified` (preserved in place, reported as a note). The target inventory names `warden-db/` and the collection catalogue. Symlinks inside sources are never followed and block.

Blocking findings: data dir owned by a running process; no/corrupt/interrupted/already-2/not-1 ledger; `warden-db/` exists; symlink in a source; planned backup path exists.

## 3. Backup and manifest

`BeginSchema2` creates `<data>/backups/pre-schema2-<UTC ts>/` **exclusively** (an existing directory is an error, never reused), copies every `migrated` source plus `schema.json` into `data/` as read-only files (`0400`) recording path, size, mode and SHA-256, then writes `MANIFEST.json` last, read-only, via temp file + exclusive link. The manifest holds `id`, `from`/`to`, binary version, source and target inventory, and the backup file list with the ledger checksum. A backup directory without a manifest is incomplete and refused by `VerifySchema2Backup`, which also detects tampered, missing and extra files.

## 4. Operator UX

`warden migrate --plan [--json]` prints the plan (sources, target, backup path, the rules above, findings with remediation) and exits non-zero when blocked. It acquires no lock and writes nothing.

## 4a. Strict preflight (`migrate.PreflightSchema2`)

`--plan` and `BeginSchema2` (re-run under the lock, before any backup byte is written) run PlanSchema2 plus a strict, read-only verification. Nothing is repaired, rebuilt, quarantined or deleted; every refusal carries a `class` and remediation text:

| Class | Refused when |
|---|---|
| `corruption` | torn/undecodable segment regions, invalid flat JSON/JSONL, unreadable index with data behind it |
| `ambiguity` | conflicting history (revision regression, write-after-delete) — no winner is chosen |
| `duplicate_identity` | duplicate live key/id in a store, or the same `id` in two flat `sessions/`/`snapshots/` files |
| `stale_derived_state` | an index that open would rebuild/replay (stale tail, missing index) — run `warden repair` first; preflight never salvages |
| `unsupported_layout` | a source of the wrong shape (file vs dir) or a ScrivaDB store outside the inventory (it would be orphaned) |
| `unsafe_artifact` | symlink/special file at top level or in plan/runtime trees, transcript pointer outside the data dir or through a symlink |
| `owned` | the ownership lock is held, including a process that took it during verification |
| `unverifiable` | verification itself failed (I/O, permissions) |

Operator copies (`*.bak-*`, `*-backup`, …), `backups/`, `quarantine/` and `tmp/` are external and not verified. A missing transcript is a note, not a block.

## 5. Out of scope (dependent tasks)

Staging export, import transaction groups, validation, ledger stamp to schema 2, phase journaling and crash-resume, bumping `schema.SchemaVersion`, and the cutover `--apply` wiring.

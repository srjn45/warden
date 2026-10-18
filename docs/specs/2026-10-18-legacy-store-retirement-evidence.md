# Legacy-store retirement: support window, adoption evidence and entry criteria

**Date:** 2026-10-18
**Status:** Implemented (Plan 07, adoption-evidence). Builds on [schema-2 activation](2026-10-18-schema-2-activation.md).
**Scope:** *defines the gate*. It deletes nothing. Removal tooling is a separate, later change that must consume this gate.

## 1. Invariants (unchanged)

* Legacy source data is never removed automatically — not by cutover, not by evaluating this gate.
* After the schema-2 cutover no legacy database is a runtime fallback. `storage.Open` is warden-db only; `storage.OpenLegacy` refuses a schema-2 dir, and every refusal is recorded as evidence (§4).

## 2. Support window

| Item | Commitment |
| --- | --- |
| Migration code (`0002-schema2-cutover`, legacy readers used only by it) | Kept for the whole **v11.x** line. Removal not before **v12.0.0**. |
| Operator upgrade path | Any schema-1 install (v9/v10) upgrades via `wd update` (the target binary runs the cutover) or `warden migrate --apply`. Installs older than the waypoint release must step through it. |
| Per-install retention | After an install's cutover, its legacy stores **and** the immutable `backups/pre-schema2-*` backup are retained for at least **90 days** (`DefaultSupportWindow`). The clock is the cutover timestamp in `schema.json` history. |
| Rollback | `wd rollback` / `migrate --restore` remain valid for the window; a rollback restarts the clock (the rollback is evidence, §3 `no_rollback`). |

## 3. Entry criteria (all must hold, per install)

`warden migrate --retirement [--json]` (read-only) evaluates `migrate.EvaluateRetirement`. Criterion IDs are stable.

`warden inspect legacy [--json]` is the retained read-only legacy inspector for the support window: it lists the retained schema-1 sources, diffs per-collection record keys against warden-db, and re-verifies the cutover backup. It opens legacy and canonical stores only through private copies, fingerprints the data dir before/after (a change is an error), and refuses any data dir not at schema 2 (`ErrLegacyLiveAuthority`) so it can never act as a fallback or dual authority. It has no write flags and deletes nothing.

| ID | Met when |
| --- | --- |
| `schema2_active` | ledger is schema ≥ 2 with no open migration journal |
| `warden_db_present` | the `warden-db/` directory exists |
| `migration_succeeded` | ledger history contains the committed `0002-schema2-cutover` (or `baseline-fresh`) entry; telemetry corroborates when present |
| `backup_verified` | the history-recorded backup re-verifies against its MANIFEST checksums (`VerifySchema2Backup`); a fresh install has no legacy data and passes |
| `no_unresolved_failures` | no `migration_failed` / `recovery_failed` event since the cutover |
| `no_rollback` | no `updater_rollback` / `recovery_restored` since the cutover and no `backups/abandoned-schema2-*` |
| `zero_legacy_runtime_opens` | no `legacy_open_refused` event, ever |
| `schema2_runtime_observed` | ≥ 3 daemon boots on schema 2 since the cutover (`DefaultMinRuntimeBoots`) |
| `support_window_elapsed` | now ≥ cutover + 90 days |

`Eligible` means the *operator may begin* retirement. The report also lists `legacy_present`: migrated legacy paths still on disk (the cleanup candidates). Ledger history is authoritative; installs cut over by v11.0/v11.1, before the log existed, are evaluated from the ledger and backup alone (no telemetry needed for `migration_succeeded`), but they must still accumulate boots.

## 4. Telemetry / evidence schema

Local, append-only JSON Lines at `<data-dir>/.adoption-evidence.jsonl` (mode 0600; hidden, so it is an *external* entry in the schema-2 source inventory). Nothing is transmitted. Package `internal/adoption`.

```json
{"at":"RFC3339","kind":"…","migration":"0002-schema2-cutover","binary_version":"11.0.0",
 "from_schema":1,"to_schema":2,"backup":"backups/pre-schema2-…","step":"promoted","error":"≤300 chars"}
```

| `kind` | Emitted by |
| --- | --- |
| `migration_applied` | `Runner` after the ledger commit |
| `migration_failed` | `Runner` when a started migration errors (run/verify/resume) |
| `recovery_resumed` | `Runner.Resume` finishing an interrupted migration |
| `recovery_restored` / `recovery_failed` | `Runner.Restore` |
| `updater_rollback` | updater transaction and `wd rollback` restoring a data snapshot |
| `legacy_open_refused` | `storage.OpenLegacy` refusing a schema-2 dir |
| `runtime_boot` | `schema.Boot` on a schema-2 dir (daemon start) |

Rules: values never include record contents; error text is bounded; writes are best-effort and never alter a migration's outcome; a **pure refusal before any live change writes nothing** (a refused cutover must leave the data dir byte-identical — the refusal is in the command output); malformed lines are counted (`malformed_log_lines`), never fatal.

## 5. Cleanup entry procedure (for the future removal change)

1. `warden migrate --retirement --json` shows `eligible: true`.
2. Operator confirms the backup is stored off-host.
3. Removal tooling must re-run the evaluation itself, refuse on any unmet criterion, and move (not delete) legacy stores to a quarantine directory first.

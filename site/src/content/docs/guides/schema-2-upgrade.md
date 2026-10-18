---
title: Schema 2 upgrade
description: The one-time, verified cutover from the per-store schema-1 layout to the single warden-db — what it does, how to run it, and how to recover.
---

import { Aside } from '@astrojs/starlight/components';

The v11 major moves every durable store into **one** database, `<data_dir>/warden-db/`
(*schema 2*). A fresh install starts there. An existing install is cut over **once**,
offline, by a journaled migration. There is no dual-writer mode and no silent
fallback: the daemon runs on `warden-db` only.

<Aside type="note">
Nothing is deleted. The schema-1 store directories stay exactly as they were, and an
immutable, checksummed copy is taken first under `backups/pre-schema2-<timestamp>/`.
</Aside>

## The normal path: `warden update`

`warden update` handles the whole thing: it inspects the data directory before stopping the
daemon, recognizing the running daemon and expected live index lag as non-blocking notes
(genuine corruption safely blocks before any binary swap or modification). It then stops the
daemon, takes a data snapshot, swaps the binary, and the **new** binary runs `warden migrate --apply`
under the ownership lock. Malformed records in `savings/ledger.jsonl` with valid line boundaries
are safely quarantined into `<data_dir>/quarantine/savings/` with SHA-256 manifests while valid
events are preserved. Post-stop verification is strict: any foreign lock or unquarantineable
corruption safely refuses before change (`ErrRefusedBeforeChange`). The daemon is restarted only
afterwards and must report the new version **and** `schema_version: 2`. If anything fails, the
previous binary and data are restored without mutation and the old daemon is verified healthy
again (a promoted `warden-db/` is moved to `backups/abandoned-schema2-<timestamp>/`, never deleted).

Preview first with `warden update --plan`.

<Aside type="caution">
An updater built before this release cannot run the cutover. If you are on one, the
update stops **before any change** and tells you the manual steps below.
</Aside>

## The manual path

With the daemon stopped and the new binary installed:

```sh
warden migrate --plan     # read-only: sources, target, backup path, anything that blocks
warden migrate --apply    # preflight → backup → stage → verify → promote → ledger
warden version            # Data schema: 2
```

What `--apply` does, in order: takes the data-dir ownership lock (a running daemon
makes it refuse); strict preflight (corruption, conflicting history, duplicate
identities, stale indexes, unsafe artifacts — nothing is repaired or guessed);
creates and verifies the immutable backup; exports to a staging store and proves it
equal to the legacy data (record counts, canonical hashes, relationships); atomically
promotes it to `warden-db/`; verifies again; and only then moves the schema ledger to 2.

## Guards

| You run… | …on… | Result |
| --- | --- | --- |
| new daemon | schema-1 data | refuses before opening any store: *must be cut over to the single warden-db* |
| old daemon | schema-2 data | refuses (`data_newer`): there is no automatic downgrade |
| any binary | `warden-db/` beside a schema-1 ledger | refuses (`schema2_residue`): two candidate authorities, nothing is adopted |
| any binary | an interrupted migration | refuses until you `--resume` or `--restore` |
| `wd update` | non-quarantineable JSONL or genuine index damage | refuses preflight before any change; points directly to store without broad repair |
| `wd update` | rogue process taking lock post-stop | cutover refuses without mutation; updater rolls back cleanly |

## Pipelines deleted after an earlier import

Schema 1 kept a pipeline twice: `pipelines-db` and the old `pipelines/<id>.json` files.
The cutover treats `pipelines-db` as authoritative only where the files agree with it,
and never guesses between them:

| Situation | Outcome |
| --- | --- |
| a file identical to its `pipelines-db` record | deduplicated |
| a file that differs from its record (`pipeline_legacy_conflict`) | refuses; names the differing fields, never their values |
| a file for a pipeline `pipelines-db` shows was deleted (delete tombstone) | not resurrected; the file stays untouched |
| a file with no record and no tombstone (`pipeline_unproven_deletion`) | refuses: it cannot be proven deleted rather than never imported |
| an archived or finished agent or plan of a deleted pipeline | kept; its `pipeline_id`/`job_id` move to a `historical_pipeline` provenance record |
| a live agent or plan of a missing pipeline (`pipeline_active_ref`) | refuses; the reference is never nulled and no parent is invented |

`warden migrate --check` previews all of this on a private copy before anything is stopped,
and a refused cutover lists its blockers (first ten) in the error. To resolve a
conflict or an unproven deletion, move the stale `pipelines/<id>.json` aside (the
cutover never modifies it) or, if the pipeline must be kept, restore it into
`pipelines-db` with the schema-1 binary first.

## If it is interrupted

Power loss or a killed process leaves the migration journaled. No daemon will start
until you choose:

```sh
warden migrate --resume    # continue from the journaled step with the same backup
warden migrate --restore   # go back; a promoted warden-db is preserved under backups/
```

A refusal before any live change (a corrupt or ambiguous source) leaves the data and
ledger exactly as they were; fix the finding and re-run.

## Offline tools on schema 2

The schema-1 tools (`warden repair sessions|agents|backends|all`, offline
`wd models`, `wd savings --calibrate`) refuse on schema 2 with guidance, because the
legacy directories are now inert copies. Use the daemon-backed interfaces, or
`warden repair warden-db --dry-run` to verify the store offline.

## Legacy store retirement and safe cleanup

After cutover, legacy stores (`agents-db`, `projects`, `backends`, `plans/plans-db`, etc.)
and the immutable `backups/pre-schema2-*` backup are kept for at least **90 days**.
Legacy stores are never an active runtime fallback.

### 1. Inspecting legacy stores (`warden inspect legacy`)

Inspect the retained schema-1 stores and compare per-collection records against `warden-db`:

```sh
warden inspect legacy          # read-only summary
warden inspect legacy --json   # machine-readable inspection
```

This command opens only temporary private copies, fingerprints before and after to guarantee
zero mutations, re-verifies the cutover backup against its SHA-256 manifest, and refuses on any
directory that is not at schema 2.

### 2. Evaluating retirement criteria (`warden migrate --retirement`)

Before removing legacy stores, verify that your installation satisfies all 9 retirement criteria:

```sh
warden migrate --retirement          # check criteria status
warden migrate --retirement --json   # machine-readable criteria evaluation
```

Retirement requires:
- `schema2_active`: ledger is schema ≥ 2 without an open migration journal
- `warden_db_present`: `warden-db/` exists
- `migration_succeeded`: cutover committed in ledger history
- `backup_verified`: `backups/pre-schema2-*` verifies against `MANIFEST.sha256`
- `no_unresolved_failures`: no migration or recovery failures since cutover
- `no_rollback`: no updater rollbacks or abandoned targets
- `zero_legacy_runtime_opens`: zero legacy open attempts on schema 2
- `schema2_runtime_observed`: at least 3 daemon boots on schema 2
- `support_window_elapsed`: at least 90 days elapsed since cutover

### 3. Safe backup-gated cleanup (`warden migrate --cleanup-legacy`)

When `warden migrate --retirement` reports `eligible: true`:

```sh
# Preview what would be moved
warden migrate --cleanup-legacy --dry-run

# Confirm and quarantine legacy stores
warden migrate --cleanup-legacy --yes
```

Safety guarantees:
- **Never deletes directly:** Migrated legacy directories are moved to
  `backups/legacy-quarantine-<timestamp>/`.
- **Preserves external content:** Directories holding external content not in `warden-db`
  (such as `snapshots/*.transcript`) remain in place.
- **Audit logged:** A `legacy_cleanup` event is recorded in `audit.jsonl` and `.adoption-evidence.jsonl`.
- **Idempotent:** Re-running when already clean is a no-op.
- **Uninterrupted operation:** `warden-db` continues serving your agents and pipelines uninterrupted.
- **Recoverable:** Restore by moving directories back from `backups/legacy-quarantine-*` or
  restoring the immutable pre-schema2 backup. Once verified, delete the quarantine directory manually.

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

`warden update` handles the whole thing: it stops the daemon, takes a data snapshot,
swaps the binary, and the **new** binary runs `warden migrate --apply`. The daemon is
restarted only afterwards and must report the new version **and** `schema_version: 2`.
If anything fails, the previous binary and data are restored and the old daemon is
verified healthy again (a promoted `warden-db/` is moved to
`backups/abandoned-schema2-<timestamp>/`, never deleted).

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

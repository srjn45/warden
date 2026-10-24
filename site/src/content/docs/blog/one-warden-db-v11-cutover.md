---
title: "v11: every warden store moves into one database"
description: warden v11 collapses its separate on-disk stores into a single warden-db, and cuts existing installs over once, offline, with a journaled and verified migration. What changes, what's guaranteed, and how to upgrade.
date: 2026-10-22
authors: srjn45
excerpt: v11 is a schema-breaking major. Agents, plans, pipelines, autopilot runs, context and more now live in one warden-db, and an existing install is cut over once — backed up, verified by count and hash, promoted atomically, and rolled back if anything fails.
tags:
  - release
  - storage
  - upgrade
---

warden used to keep its state in a handful of separate stores — sessions,
pipelines, plans, the backend registry, and so on. v11 is the release that
collapses all of them into **one** embedded database, `warden-db`
(a [ScrivaDB](https://github.com/srjn45/scriva) under `<data>/warden-db/`).

It is a major version because the data format changed. warden tracks that
separately from the semver as an integer **schema version**, and v11 is the first
release at **schema 2**.

## What moved

Agents, terminals, projects and groups, plans, pipelines, autopilot runs, shared
context, the mailbox, the backend registry, usage/savings/spend, schedules,
snapshot metadata and the audit log all live in `warden-db` now. The daemon runs
**only** on it: there is no dual-writer mode and no silent fallback to the old
stores.

## How an existing install gets there

A schema-1 data directory is cut over **once**, offline, by a journaled migration:

1. **Strict preflight** — genuine corruption blocks the cutover before anything changes.
2. **Immutable backup** — a SHA-256-manifested copy under `backups/pre-schema2-<ts>/`.
3. **Staged export** — proven by count, hash and reference checks before it is trusted.
4. **Atomic promotion**, with the schema ledger written last.

If any step fails, `warden update` restores the previous binary and data. If the
migration is interrupted, no daemon will start on the half-moved directory; you
choose `warden migrate --resume` or `warden migrate --restore`. The legacy store
directories are kept, untouched, after the cutover — nothing is deleted.

## Upgrading

For most installs it is just:

```sh
warden update --plan   # show the hops
warden update          # runs the cutover via the new binary
```

If you swapped the binary by hand, the new daemon refuses to start with a clear
message and you run `warden migrate --plan`, then `warden migrate --apply` with the
daemon stopped. Older binaries refuse schema-2 data rather than guess, and
`warden update` only calls the new daemon healthy when both the version **and**
the schema match.

The full walkthrough is in the [schema 2 upgrade guide](/warden/guides/schema-2-upgrade/).

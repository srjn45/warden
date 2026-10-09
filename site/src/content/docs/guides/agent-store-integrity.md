---
title: Agent store integrity
description: How warden owns, health-checks and repairs the agent store — and the safe daemon-offline procedure when it degrades.
---

import { Aside } from '@astrojs/starlight/components';

The agent store (`<data_dir>/agents-db`) is opened by **exactly one** warden process: the daemon. Everything else — CLI, MCP, TUI, Web Cockpit — goes through the daemon's API, so normal commands are unaffected by any of what follows.

## Ownership

The daemon takes an exclusive lock (`<data_dir>/.agents-store.lock`) before it imports, reconciles or listens. A second daemon — even on another port — is refused with:

```
agent store <dir> is owned by another warden process (lock <file>); stop the running warden daemon …
next step: stop the running warden daemon (including one on another port) that uses this data directory, then retry; do not delete the lock file
```

Offline tools (`warden doctor --reconcile-membership`, `warden inspect repair agents`) follow the same rule and never open a live-owned store. `warden repair agents` remains a compatibility alias. The lock is released by the kernel when the owner exits or dies; never delete it by hand.

## Seeing the state

| Surface | How |
|---|---|
| REST | `GET /api/v1/store/health` — `healthy`, `failures[]`, `repair_available`, `next_step` (always 200) |
| CLI | `warden doctor` — an "agent store" line: healthy / DEGRADED / owned-while-daemon-down |
| MCP | `store_health` tool (read-only) |
| TUI | header chip `degraded ●` and banner "session store degraded — run `warden doctor`"; the last complete fleet stays on screen |
| Web | fleet requests return 503 with the same actionable message |

Reads are complete-or-error: a degraded store returns an error, never a silently shorter fleet. **Running agents are never affected** — tmux sessions are not relaunched, adopted or killed by a read failure.

## Repair (offline, ScrivaDB v1.4.0)

`warden inspect repair agents` (alias `warden repair agents`) delegates to ScrivaDB v1.4.0's `VerifyDir` / `Repair`. Warden does not parse segments or invent its own rebuild. `repair_available` in store health is now `true`.

<Aside type="caution">
Repair is **offline-only and CLI-only**. It is not exposed over REST, MCP, TUI or Web. It refuses unless you own the data directory (or are root) **and** no warden process owns the store — a daemon-owned store is refused with the ownership error and a "stop the daemon" next step; nothing is opened or changed. Never delete `.agents-store.lock` to get around this.
</Aside>

| Flag | Meaning |
|---|---|
| `--dry-run` | Read-only full verification. Reports findings and the worst severity; changes no file. Run this first. |
| `--backup-dir <dir>` | Parent directory for the backup. Default is the parent of `agents-db` (the data dir). Use a private directory **outside** `data_dir`; a location inside `agents-db` is rejected. |
| `--salvage` | Opt in to ScrivaDB's conflict-safe segment salvage (see below). Off by default. |
| `--on-conflict report\|abort` | `report` (default) repairs what is unambiguous and lists conflicts; `abort` refuses the run on ambiguous history. |
| `--json` | Print the machine-readable ScrivaDB `IntegrityReport` (dry-run) or `RepairReport` (repair). |

**What a real repair does** (only for the `agents` and `closed` collections that verification shows need work):

1. Takes ScrivaDB's exclusive directory lock (a second layer under warden's ownership lock).
2. Copies the affected collections to `<backup-dir>/repair-backup-<UTC>/` and verifies every file byte-for-byte (SHA-256). If the backup does not verify, the run aborts with nothing changed. The report is also saved there as `repair-report.json`.
3. Rebuilds the primary/secondary indexes and `meta.json` atomically from the segments. Segments are the source of truth and are not edited; tombstones stay deleted. A torn, unacknowledged tail on the newest segment is trimmed, as a normal open would.
4. Records progress in a journal in the data directory.

**Restart and idempotence.** If a repair is interrupted (crash, Ctrl-C, power loss), re-run the same command: the journal pins the original backup (it is never retaken, because a later one would capture a half-repaired directory) and the run continues. Re-running on an already repaired store does nothing and takes no new backup.

**Conflicts and salvage.** Duplicate ids, conflicting revisions and unique violations are never resolved by warden or ScrivaDB: with `report` they are listed (and the affected collection is left untouched, shown by `blocked=N` in the output); with `abort` the run is refused. Damaged segment bytes are left alone unless `--salvage` is given; with it, valid records move into a new segment and the damaged originals are moved byte-for-byte to `<collection>/quarantine/<run>/` (with a `MANIFEST.json` of hashes) and also kept in the backup. Nothing is deleted. Warden does not choose among conflicting records — resolve those by hand from the report, keeping the backup.

**Evidence preservation.** Keep the `repair-backup-*` directory, `repair-report.json`, any `quarantine/` directories and the `--json` output. Do not remove them until the incident is closed. Every attempt emits an `audit: agent-store repair attempt` log event (`audit=true`, `action=repair_agents`, `outcome` = `denied` / `refused_owned` / `dry_run` / `repair`, `data_dir`, `uid`); daemon startup refusals emit `audit: daemon startup refused`.

**Lock and filesystem assumptions.** Ownership relies on POSIX `flock` on a local filesystem. NFS, SMB and FUSE mounts without equivalent semantics are unsupported for the data directory, and repair cannot protect you there. Backups need free space for the affected collections and the same-filesystem renames used for quarantine.

## Degraded behaviour (SSE, health, startup)

If verification at daemon start (or a read) finds damage, the daemon **still starts** but the agent store is degraded, and the on-disk store is left **byte-identical** — warden opens a throwaway scratch copy so ScrivaDB's automatic index rebuild cannot mask or alter the damage. Fleet reads and writes return the typed integrity error (REST 503 with the next step) rather than a shorter fleet; the scratch copy is removed on shutdown.

- `GET /api/v1/store/health` stays 200 and reports `healthy:false`, `failures[]`, `repair_available:true` and `next_step`.
- The SSE stream `/api/v1/events` stays open. When the initial snapshot cannot be read, it sends an explicit `event: error` with `{"error":"…","degraded":true}` instead of silence, so a client can keep its last snapshot and show the failure rather than a healthy empty fleet. The next signal retries.
- Running agents and tmux sessions are untouched throughout.

## Daemon-offline procedure

1. `warden doctor` — confirm the store is DEGRADED (or owned).
2. Stop the daemon (`systemctl --user stop warden`, or terminate it). Agents' tmux sessions keep running.
3. Back up the whole data directory: `cp -a ~/.warden ~/.warden.bak-$(date +%s)` (keeps 0700/0600 modes; keep it private).
4. `warden inspect repair agents --dry-run [--json]` — review findings. Nothing changes.
5. `warden inspect repair agents --backup-dir ~/warden-repair-backups` (add `--salvage` only if the dry-run shows damaged segment bytes you accept moving to quarantine). If interrupted, run the identical command again.
6. Review `blocked` and conflicts in the output/`--json`. Resolve by hand if any; do not delete backups.
7. Start the daemon and run `warden doctor`. If still degraded, it keeps serving explicit errors; report to the maintainers with the dry-run `--json`, report and `doctor` output.
8. To roll back, stop the daemon and restore the backup (`repair-backup-*` collections, or your full copy from step 3) over the data directory.

## Upgrading safely

This release pins **ScrivaDB v1.4.0** (from v1.2.1; v1.2.0 must never be used), which supplies the Verify/Repair engine. No data-format migration is required. Before upgrading a daemon that owns an existing data directory, stop it cleanly and take a private backup of the whole data directory. Start only one upgraded daemon for each data directory; a second daemon, `doctor --reconcile-membership`, or `repair agents` will fail fast while the first daemon owns the store. Pre-upgrade stores are verified at first start; a damaged one comes up degraded as described above, not silently rebuilt.

The original incident did **not** establish the identity of a second writer. The plausible paths remain a second daemon and an offline membership reconcile; the ownership guard prevents both from opening a live store, but does not turn that hypothesis into a proven root cause. The **original second writer remains unproven.** Other ScrivaDB-backed warden stores are outside this ownership boundary and remain a follow-up audit.

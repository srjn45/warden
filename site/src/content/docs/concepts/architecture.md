---
title: Architecture & the daemon
description: How warden is built — one binary, a local daemon, an embedded ScrivaDB session store, and Claude Code hooks.
---

One self-contained Go binary that wears several faces, all sharing the same on-disk state:

| Capability | Description |
|---|---|
| **Single-binary distribution** | `warden` bundles the daemon, CLI clients, MCP server, TUI, and (in release builds) the embedded web GUI. `wd` is an installed symlink. |
| **Local daemon** | The single writer to the session store. Serves a loopback REST API (`127.0.0.1:8765`) and runs a background poller that keeps each agent's status and subject fresh. |
| **Embedded ScrivaDB session store** | Sessions persist in an embedded ScrivaDB (`github.com/srjn45/scriva`, `SyncModeNone`) rooted at `~/.warden/sessions-db/` — an `active` collection for live sessions and a `closed` one for archived, each record keyed by session id. A mutation appends one record instead of rewriting a whole JSON file; still no database server to run. On first launch after upgrading, warden imports the legacy `sessions/`+`closed/` JSON once and keeps it as a read-only backup — see [Upgrading & storage migration](#upgrading--storage-migration). |
| **Claude Code lifecycle hooks** | A hook script posts `SessionStart`/`Notification`/`Stop`/`SubagentStop`/`SessionEnd` to the daemon so status updates in real time without polling. Fails soft (never blocks the agent). |
| **launchd auto-start (macOS)** | Installs as an auto-starting, crash-restarting background service. |
| **Stable code identity** | One-time self-signed code-signing cert keeps the macOS TCC (Full Disk Access) grant stable across rebuilds. |
| **Security hardening** | `0700` data dir, slowloris/body/write timeouts (bypassed for SSE/WS/long-poll), refuses non-loopback bind unless `WARDEN_ALLOW_NONLOOPBACK=1`. |
| **`warden doctor`** | Preflight checks: required binaries (`tmux`, `git`, `claude`, `gh`), daemon reachability, data directory. |

![warden architecture](/warden/media/architecture.svg)

## Upgrading & storage migration

The session store used to be one JSON file per session under `~/.warden/sessions/`
and `~/.warden/closed/`. It now runs on an **embedded ScrivaDB** rooted at
`~/.warden/sessions-db/` (`active` + `closed` collections, keyed by session id), so
a mutation appends one record instead of rewriting a whole file. This is an
**internal storage swap** — the daemon's REST API, the CLI/MCP clients, and
`warden inspect export`/`import` are byte-for-byte unchanged.

On the **first daemon launch after upgrading**, warden runs a one-time import:

- Every legacy `sessions/*.json` and `closed/*.json` is decoded individually into
  the new collections (a corrupt or unsafe-id file is skipped with a warning, never
  blocking the upgrade), folding in the former provenance backfill.
- A `~/.warden/.sessions-filedb-imported` sentinel is written **last**, only after
  both collections load. If the import dies partway the sentinel is absent, so the
  next boot wipes the half-built `sessions-db/` and re-imports from the intact
  legacy JSON — nothing is lost.

The legacy `sessions/`/`closed/` directories are **read-only from now on and kept
as a cold backup**; this release never deletes them.

:::caution[Downgrade caveat]
Downgrading to a pre-migration binary still reads your **pre-upgrade** history from
the intact legacy JSON — nothing from before the upgrade is lost. But sessions
**created or mutated after** the upgrade live only in `sessions-db/`, and a
downgrade cannot see them. This is inherent to any forward migration.
:::

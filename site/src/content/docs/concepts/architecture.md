---
title: Architecture & the daemon
description: How warden is built — one binary, a local daemon, a file-based store, and Claude Code hooks.
---

One self-contained Go binary that wears several faces, all sharing the same on-disk state:

| Capability | Description |
|---|---|
| **Single-binary distribution** | `warden` bundles the daemon, CLI clients, MCP server, TUI, and (in release builds) the embedded web GUI. `wd` is an installed symlink. |
| **Local daemon** | The single writer to the session store. Serves a loopback REST API (`127.0.0.1:8765`) and runs a background poller that keeps each agent's status and subject fresh. |
| **File-based JSON store** | Sessions persisted as JSON files under `~/.warden` (`sessions/`, `closed/`) — no database to run. |
| **Claude Code lifecycle hooks** | A hook script posts `SessionStart`/`Notification`/`Stop`/`SubagentStop`/`SessionEnd` to the daemon so status updates in real time without polling. Fails soft (never blocks the agent). |
| **launchd auto-start (macOS)** | Installs as an auto-starting, crash-restarting background service. |
| **Stable code identity** | One-time self-signed code-signing cert keeps the macOS TCC (Full Disk Access) grant stable across rebuilds. |
| **Security hardening** | `0700` data dir, slowloris/body/write timeouts (bypassed for SSE/WS/long-poll), refuses non-loopback bind unless `WARDEN_ALLOW_NONLOOPBACK=1`. |
| **`warden doctor`** | Preflight checks: required binaries (`tmux`, `git`, `claude`, `gh`), daemon reachability, data directory. |

![warden architecture](/warden/media/architecture.svg)

---
title: What is warden?
description: A single Go binary that spawns, monitors, and tears down Claude Code agent sessions from one cockpit.
---

`warden` (aliased as `wd`) lets you run many **Claude Code agent sessions** in parallel and watch them from one place. Each agent is a real `claude` process running inside its own detached **tmux** window. You spawn agents, watch what they're doing, talk to them, and tear them down — without juggling terminals by hand.

It is a single Go binary (`warden`, aliased as `wd`) that spawns, monitors, and tears down Claude Code agent sessions of different task types — creating a git worktree only for the types that need one — backed by a local daemon and a file-based JSON store (no database to run).

One binary, multiple faces: `warden daemon` is the single writer to the on-disk session store, serving a loopback REST API and running a background poller. `warden ls|status|start|done|attach|send|tail` are thin HTTP clients to the daemon. `warden mcp` is a stdio MCP server that bridges MCP tool calls to the same REST API, enabling an orchestrator Claude session to query agents and talk to a specific running agent. A short alias `wd` (a symlink to `warden`) is installed alongside it.

| Face | What it is | You run it… |
|---|---|---|
| **daemon** | The single long-running process. Owns the on-disk session store, serves a loopback REST API on `127.0.0.1:8765`, and runs a background poller that keeps each agent's status and subject fresh. | Once, in the background (usually via launchd). |
| **CLI client** | `ls`, `status`, `start`, `done`, `attach`, `send`, `tail`, `tui` — thin HTTP clients that talk to the daemon. | Whenever you want to act on agents. |
| **MCP server** | `warden mcp` — a stdio bridge so an *orchestrator* Claude session can manage agents through tool calls. | Wired into a Claude session's MCP config. |

Everything flows through the daemon, so **the daemon must be running** before any other command will work.

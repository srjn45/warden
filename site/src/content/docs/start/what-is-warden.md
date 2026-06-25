---
title: What is warden?
description: A single Go binary that spawns, monitors, and tears down Claude Code agent sessions from one cockpit.
---

`warden` (aliased as `wd`) lets you run many **Claude Code agent sessions** in parallel and watch them from one place. Each agent is a real `claude` process running inside its own detached **tmux** window. You spawn agents, watch what they're doing, talk to them, and tear them down — without juggling terminals by hand.

It is a single Go binary (`warden`, aliased as `wd`) that spawns, monitors, and tears down Claude Code agent sessions of different task types — creating a git worktree only for the types that need one — backed by a local daemon and a file-based JSON store (no database to run).

One binary, multiple faces: `warden daemon` is the single writer to the on-disk session store, serving a loopback REST API and running a background poller. `warden ls|status|start|done|attach|send|tail` are thin HTTP clients to the daemon. `warden mcp` is a stdio MCP server that bridges MCP tool calls to the same REST API, enabling an orchestrator Claude session to query agents and talk to a specific running agent. A short alias `wd` (a symlink to `warden`) is installed alongside it.

| Face | What it is | You run it… |
|---|---|---|
| **daemon** | The single long-running process. Owns the on-disk session store, serves a REST API (loopback by default; [token-gated remote access](/warden/guides/remote-access/) optional), and runs a background poller that keeps each agent's status and subject fresh. | Once, in the background (a service). |
| **CLI client** | `ls`, `status`, `start`, `done`, `attach`, `send`, `tail`, the [git/check lifecycle verbs](/warden/guides/lifecycle-and-rails/) (`commit`/`push`/`sync`/`check`), and more — thin HTTP clients that talk to the daemon. | Whenever you want to act on agents. |
| **TUI cockpit** | `warden tui` (or bare `warden`) — a live tmux-based terminal dashboard of the whole fleet. | When you want a terminal cockpit. |
| **Web GUI** | A React dashboard the daemon embeds and serves alongside the API — tabbed mission control with live SSE, interactive terminals, and an attention queue. | Open the daemon's address in a browser. |
| **MCP server** | `warden mcp` — a stdio bridge so an *orchestrator* Claude session can manage agents through tool calls. | Wired into a Claude session's MCP config. |
| **Orchestrator** | `warden orch` — a [local-LLM conductor REPL](/warden/multi-agent/orchestrator-repl/) that turns plain-English intent into confirmed warden actions, spending no Claude tokens. | When you want NL control without an MCP Claude session. |

Settings live in a config file (`~/.warden/config.yaml`; `warden config` prints the resolved values), overridable by environment variables. Everything flows through the daemon, so **the daemon must be running** before any other command will work.

---
title: Environment variables
description: Every warden configuration variable, its default, and what it controls.
---

Set via environment variables (or override the daemon address per-command with `--addr`).

| Variable | Default | Description |
|---|---|---|
| `WARDEN_ADDR` | `127.0.0.1:8765` | Daemon listen/connect address |
| `WARDEN_DATA_DIR` | `~/.warden` | Directory for session JSON files (`sessions/`, `closed/`) and prompt files (`prompts/`) |
| `WARDEN_WORKDIR` | `~/warden-agents` | Where the per-agent prompt file is stored (keyed by agent id). It is **not** where the agent runs — prompt-spawned agents launch in the caller's current directory (or `--dir`) |
| `CLAUDE_PROJECTS_DIR` | `~/.claude/projects` | Root of Claude Code transcript directories; used by the poller to read agent transcripts when generating subjects |
| `WARDEN_NOTIFY` | `off` | macOS desktop notifications when an agent needs attention (`on`/`1`/`true` to enable) |
| `WARDEN_APPROVALS` | `on` | The approvals inbox: the daemon parses recognized Claude Code tool-permission prompts and surfaces them for answering (web AttentionQueue buttons, `warden approvals`/`warden approve`, the TUI **⏳ Approvals** row). Unrecognized prompts fall back to attach. Disable with `0`/`off`/`false` |
| `WARDEN_TOKEN_GUARD` | `on` | The context-size guard master switch: the poller reads each live agent's context-window fill from its transcript, classifies it `ok`/`warning`/`critical`, and shows a state-colored token figure in `ls`/TUI/web. Disable with `0`/`off`/`false` to turn off the whole guard (gauge, alert, auto-compact) |
| `WARDEN_TOKEN_WARN_ALERT` | `on` | Fire a desktop notification (when `WARDEN_NOTIFY` is on) once per upward crossing into the warning or critical band. Disable with `0`/`off`/`false` |
| `WARDEN_TOKEN_AUTO_COMPACT` | `on` | When an agent is `critical` **and** idle/waiting, auto-send `/compact` to reclaim its context (cooldown-guarded). Disable with `0`/`off`/`false` |
| `WARDEN_TOKEN_WARN` | `200000` | Warning threshold in context tokens (inclusive lower bound). If `WARDEN_TOKEN_CRITICAL` is not greater than this, both reset to the defaults |
| `WARDEN_TOKEN_CRITICAL` | `400000` | Critical threshold in context tokens (inclusive lower bound) — the auto-`/compact` trigger band |
| `WARDEN_ALLOW_NONLOOPBACK` | unset | Allow binding a non-loopback address |

All variables can also be overridden with `--addr` on any command.

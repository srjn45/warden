---
title: Interactive mode (orchestrator REPL)
description: warden orch — an interactive conductor with a real line editor, deterministic /commands, and a local-LLM natural-language half, all over your warden fleet.
---

`warden orch` (aliases `warden interactive`, `warden i`, `warden orchestrator`) is warden's **interactive mode**: a proper terminal REPL to drive your fleet. It is a real line editor — **arrow keys, in-line editing, history persisted across sessions, reverse-search, and Tab completion** — that closes cleanly with **Ctrl-D** (or `exit`), returning you to your shell prompt.

```sh
warden orch          # or: warden interactive / warden i
```

It drives the fleet two ways — a reliable deterministic half and a natural-language half:

- **Deterministic `/` commands (no model).** Type `/agents`, `/spawn <prompt>`, `/tell <id> <text>`, `/pipelines`, … and warden runs the exact verb — no LLM in the loop, so it keeps working even when the local model is slow or wrong. Tab-completes the verb names **and live agent ids**. `/help` lists them all.
- **Natural language (local LLM).** Any other line is planned by a local model into **confirmed** warden tool calls — *"spin up two agents on the API and the web, then run the tests"*. It conducts; **it never implements** — there's no edit/write/bash tool in its registry, so all code work is delegated by spawning a Claude agent.

Interactive mode **starts without a local model** — the `/` commands and `!`-shell always work. The natural-language half needs `local_llm: true` (and `local_llm_url`/`_model`/`_timeout`); without it, a bare line tells you so and points you at `/help`. Because execution is always plain warden API calls, the conductor spends **no Claude tokens**.

You can also run it as the cockpit master pane via the `orchestrator` config setting / `--orch` flag; **Alt+t** toggles that slot between interactive mode and a raw `$SHELL` without killing either side.

## Deterministic `/` commands

Every `/` command maps to one warden verb; reads run immediately, mutations pass through the same confirm gate as the model's calls. Type `/help` in-session for the full table. A selection:

| Command | Does |
|---|---|
| `/agents` (`/ls`) | list all agents and their status |
| `/agent <id>` · `/output <id> [lines]` | full detail · recent terminal output |
| `/spawn <prompt…>` | spawn an agent to do a task |
| `/tell <id> <text…>` (`/send`) · `/msg <id> <body…>` | type into a session · send a directed message |
| `/stop <id>` · `/restore <id>` · `/rm <id> [--hard]` | stop (reversible) · restore · clear record |
| `/commit <id> [msg…]` · `/push <id>` · `/sync <id> [base]` · `/check <id> [name]` | the git + check lifecycle |
| `/pipelines` (`/pl`) · `/pipeline <id>` · `/cancel <id>` | list · inspect · cancel a pipeline |
| `/ctx [prefix]` · `/ctx-get <key>` · `/ctx-set <key> <value…>` | the shared-context blackboard |
| `/approvals` · `/approve <id> <option>` · `/inbox <id>` · `/collab` | approvals · inbox · conflict picture |

Unknown `/verbs` are caught with a hint — a typo never silently falls through to the model.

## How it behaves

| Behaviour | Detail |
|---|---|
| **NL → tool-call loop** | Backed by the `internal/llm` Chatter seam (Ollama `/api/chat`, multi-turn tool-calling). A bounded turn budget stops runaway loops; malformed args / unknown tools recover instead of garbling execution. |
| **Read auto-runs, mutate confirms** | Read-only verbs (`list_agents`, `get_agent`, `get_agent_output`, `get_collaboration_status`, `read_inbox`, `list_approvals`, `ctx_get`/`ctx_list`, pipeline reads) auto-execute. |
| **Mandatory confirm gate** | Every mutating verb (`spawn_agent`, `send_to_agent`, `terminate_agent`, `delete_agent`, `restore_agent`, `approve`, `commit`, `push`, `sync`, `check`, `ctx_set`, `send_message`, `pipeline_create`/`_cancel`, `clean_up`) requires explicit operator approval before it runs — non-config-gated, can't be disabled. A batched plan confirms as one unit. **`[e]dit`** walks the call **field by field** — one short prompt per argument showing the current value (Enter keeps it, Ctrl-C finishes), so you can fill in a field the model omitted (e.g. a `branch`) without hand-editing JSON. |
| **Capability-tier routing** | A cheap pre-classify buckets each request's needed tier against the model's tier (`local_llm_tier`). Within tier ⇒ plan locally; over tier ⇒ escalate one planning step to headless Claude (`local_llm_escalate`, default on) or degrade honestly. Execution always stays token-free. |
| **Monitoring verbs** | `fleet_digest` / `agent_digest` summarize state, `pending_for_me` surfaces what needs you, and `clean_up` proposes terminate/delete of finished agents through the same confirm gate. |
| **`!`-shell passthrough** | A `!`-prefixed line runs in a persistent embedded `$SHELL` (cwd/env persist) and tees output to the terminal. The orchestrator takes **no action** on that output — it reports verbatim; the output is visible as context to the next turn. |
| **Real line editor** | Backed by readline: arrow-key cursor movement, ↑/↓ history (persisted to `~/.warden/orch_history`), Ctrl-R reverse-search, Ctrl-A/E/W/K/U editing, Tab completion of `/` commands and live agent ids, Ctrl-C to abandon a line, Ctrl-D to close. The prompt and headings are colourised (honours `NO_COLOR` and non-TTY output). |

> Not sure which model to run? **`warden llm suggest`** auto-detects your machine's **total** and **average free** memory (from the same pool — VRAM, Apple unified memory, or system RAM) and prints a memory-ranked shortlist, marking each model `fits now` / `free memory first` / `too large`. It scores a tool-calling-forward catalog (Qwen3, gpt-oss, Mistral Small, Qwen2.5) by **conductor suitability** — the orchestrator routes tool calls, it never writes code — and stars the best model that runs *comfortably now* with headroom for your real workload. `warden doctor` gives the one-line version. Both only ever recommend — you set `local_llm_model`; warden never silently swaps it.

For the token-spending alternative — driving the same operations from a full Claude session — see [Orchestration: MCP & skill](/warden/multi-agent/mcp-and-skill/).

---
title: Interactive mode (orchestrator REPL)
description: warden orch — an interactive conductor with a real line editor, deterministic /commands, and a local-LLM natural-language half, all over your warden fleet.
---

`warden orch` (aliases `warden interactive`, `warden i`, `warden orchestrator`) is warden's **interactive mode**: a proper terminal REPL to drive your fleet. It is a real line editor — **arrow keys, in-line editing, history persisted across sessions, reverse-search, and Tab completion** — that closes cleanly with **Ctrl-D** (or `exit`), returning you to your shell prompt.

```sh
warden orch          # or: warden interactive / warden i
```

It drives the fleet two ways — a reliable deterministic half and a natural-language half:

- **Deterministic `/` commands (no model).** Type `/agents`, `/spawn <prompt>`, `/tell <id> <text>`, `/pipelines`, … and warden runs the exact verb — no LLM in the loop, so it keeps working even when the local model is slow or wrong. **Typing `/` pops a live menu** that filters as you type — each matching verb with its usage and a one-line summary, right under the prompt (Claude-Code style) — and Tab still completes verb names **and live agent ids**. `/help` lists them all.
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

### Guided argument forms

When a `/` command needs more than you typed, warden **collects the arguments interactively** instead of just printing a usage line. Each field is presented as the right kind of input:

- **a numbered pick-list** for fields with a known domain — `model` (sonnet · opus · haiku · fable), `permission_mode` (auto · default · acceptEdits · bypassPermissions · dontAsk · plan), `type` (development · analysis · pr-review · docs · …), and yes/no booleans like `worktree`;
- **a free-text field** for open-ended ones (the prompt, a name, a branch).

Two triggers:

- **Auto on a missing required argument.** A bare `/spawn` (no prompt) opens the form for the field it needs rather than erroring.
- **`+` to fill everything.** A trailing `+` on the verb — `/spawn+ <prompt>` — opens the **full** form (agent name, type, branch, worktree, model, permission mode, …), so you can set the fields the one-line quick path would leave to config.

The form's **structure is always deterministic** — the fields and their valid options come from warden's own enums, so it works with the local model off. When a model *is* present it adds a **hybrid pre-fill**: each field opens with a suggested value inferred from your words (`/spawn+ review auth for security` pre-selects `type=pr-review`, drafts a name). Press **Enter** to accept a suggestion, type to override, or `-` to clear a field back to its config default. The model can only ever nudge a default — it can never offer a value outside the allowed set. After the form, any mutation still passes the normal confirm gate.

Read results are rendered for a **human**, not dumped as JSON: `/agents` and `/pipelines` print aligned tables (id · status · type · name · what), `/agent <id>` a tight labelled block (empty fields omitted), `/ctx` a key/value table, and `/inbox` / `/collab` / `/approvals` one compact line each. (The local model still receives the structured JSON when it plans in natural language — only the deterministic `/`-command output is reshaped.)

## How it behaves

| Behaviour | Detail |
|---|---|
| **NL → tool-call loop** | Backed by the `internal/llm` Chatter seam (Ollama `/api/chat`, multi-turn tool-calling). A bounded turn budget stops runaway loops. Hardened against small-model slips: hallucinated args (a fabricated `repo`, a bogus `model`/`type`) are scrubbed before the gate, a missing required arg is fed back rather than approved into a doomed call, and a malformed tool call is retried instead of failing the turn. |
| **Read auto-runs, mutate confirms** | Read-only verbs (`list_agents`, `get_agent`, `get_agent_output`, `get_collaboration_status`, `read_inbox`, `list_approvals`, `ctx_get`/`ctx_list`, pipeline reads) auto-execute. |
| **Mandatory confirm gate** | Every mutating verb (`spawn_agent`, `send_to_agent`, `terminate_agent`, `delete_agent`, `restore_agent`, `approve`, `commit`, `push`, `sync`, `check`, `ctx_set`, `send_message`, `pipeline_create`/`_cancel`, `clean_up`) requires explicit operator approval before it runs — non-config-gated, can't be disabled. A batched plan confirms as one unit. **`[e]dit`** walks the call **field by field** — one short prompt per argument showing the current value (Enter keeps it, Ctrl-C finishes), so you can fill in a field the model omitted (e.g. a `branch`) without hand-editing JSON. Fields with a known set (`model`, `permission_mode`, `type`, booleans) show a **numbered pick-list**; a blank field warden fills from config (`model`, `permission_mode`) shows that default in the bracket as `[default: …]`, so you can see what an empty answer will use. This is the same engine behind the [guided argument forms](#guided-argument-forms). |
| **Capability-tier routing** | A cheap pre-classify buckets each request's needed tier against the model's tier (`local_llm_tier`). Classification defaults to a deterministic heuristic; `local_llm_classifier: model` swaps in a one-shot local-model verdict (falls back to the heuristic on any error) to catch hard single-sentence asks the heuristic can't see. Within tier ⇒ plan locally; over tier ⇒ escalate one planning step to headless Claude (`local_llm_escalate`, default on) or degrade honestly. Execution always stays token-free. |
| **Monitoring verbs** | `fleet_digest` / `agent_digest` summarize state, `pending_for_me` surfaces what needs you, and `clean_up` proposes terminate/delete of finished agents through the same confirm gate. |
| **`!`-shell passthrough** | A `!`-prefixed line runs in a persistent embedded `$SHELL` (cwd/env persist) and tees output to the terminal. The orchestrator takes **no action** on that output — it reports verbatim; the output is visible as context to the next turn. |
| **Real line editor** | Backed by readline: arrow-key cursor movement, ↑/↓ history (persisted to `~/.warden/orch_history`), Ctrl-R reverse-search, Ctrl-A/E/W/K/U editing, a **live `/`-command menu** that filters as you type (verb + summary, painted under the prompt) plus Tab completion of `/` commands and live agent ids, **guided argument forms** (pick-lists + free text) when a command needs more input, Ctrl-C to abandon a line, Ctrl-D to close. The prompt and headings are colourised (honours `NO_COLOR` and non-TTY output). |

> Not sure which model to run? **`warden llm suggest`** auto-detects your machine's **total** and **average free** memory (from the same pool — VRAM, Apple unified memory, or system RAM) and prints a memory-ranked shortlist, marking each model `fits now` / `free memory first` / `too large`. It scores a tool-calling-forward catalog (Qwen3, gpt-oss, Mistral Small, Qwen2.5) by **conductor suitability** — calibrated against the [Berkeley Function-Calling Leaderboard](https://gorilla.cs.berkeley.edu/leaderboard.html) (BFCL v4, multi-turn-weighted), since the orchestrator routes tool calls and never writes code — and stars the best model that runs *comfortably now* with headroom for your real workload. `warden doctor` gives the one-line version. Both only ever recommend — you set `local_llm_model`; warden never silently swaps it.

For the token-spending alternative — driving the same operations from a full Claude session — see [Orchestration: MCP & skill](/warden/multi-agent/mcp-and-skill/).

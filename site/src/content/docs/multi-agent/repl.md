---
title: Interactive mode (REPL)
description: warden repl — an interactive REPL with a real line editor, deterministic /commands, and a Fast-Brain natural-language half, all over your warden fleet.
---

:::caution[Experimental]
The REPL is an experimental client. The deterministic `/`-command half is stable; the Fast-Brain natural-language half is still evolving and may change between releases.
:::

`warden repl` (aliases `warden interactive`, `warden i`; also reachable as `warden backend repl`) is warden's **interactive mode**: a proper terminal REPL to drive your fleet. It is a real line editor — **arrow keys, in-line editing, history persisted across sessions, reverse-search, and Tab completion** — that closes cleanly with **Ctrl-D** (or `exit`), returning you to your shell prompt.

```sh
warden repl                  # or: warden i
```

It drives the fleet two ways — a reliable deterministic half and a natural-language half:

- **Deterministic `/` commands (no model).** Type `/agents`, `/spawn <prompt>`, `/tell <id> <text>`, `/memory <question>`, `/pipelines`, … and warden runs the exact verb — no LLM in the loop, so it keeps working even when Fast-Brain is slow or unavailable. **Typing `/` pops a live menu** that filters as you type — each matching verb with its usage and a one-line summary, right under the prompt (Claude-Code style) — and Tab still completes verb names **and live agent ids**. `/help` lists them all.
- **Natural language (Fast-Brain).** Any other line is planned by Fast-Brain into **confirmed** warden tool calls — *"spin up two agents on the API and the web, then run the tests"*. It conducts; **it never implements** — there's no edit/write/bash tool in its registry, so all code work is delegated by spawning an agent.

Natural language works **out of the box** on Fast-Brain — no Ollama, no `local_llm` config. The `/` commands and `!`-shell always work, even if Fast-Brain is unavailable (a bare line then tells you so and points you at `/help`). Because execution is always plain warden API calls, the REPL never spawns a coding agent on its own.

## Deterministic `/` commands

Every `/` command maps to one warden verb; reads run immediately, mutations pass through the same confirm gate as the model's calls. Type `/help` in-session for the full table. A selection:

| Command | Does |
|---|---|
| `/agents` (`/ls`) | list all agents and their status |
| `/agent <id>` · `/output <id> [lines]` | full detail · recent terminal output |
| `/spawn <prompt…>` | spawn an agent to do a task |
| `/tell <id> <text…>` (`/send`) · `/msg <id> <body…>` | type into a session · send a directed message |
| `/stop <id>` · `/restore <id>` · `/rm <id> [--hard]` | stop (reversible) · restore · clear record |
| `/commit <id> [msg…]` · `/push <id> [force]` · `/sync <id> [base]` · `/check <id> [name]` | the git + check lifecycle (`/push <id> force` → `--force-with-lease`) |
| `/memory <question…>` (`/mem`, `/ask`) | answer a project question from `.warden/memory.md` |
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

The form's **structure is always deterministic** — the fields and their valid options come from warden's own enums, so it works with Fast-Brain off. When Fast-Brain is available it adds a **hybrid pre-fill**: each field opens with a suggested value inferred from your words (`/spawn+ review auth for security` pre-selects `type=pr-review`, drafts a name). Press **Enter** to accept a suggestion, type to override, or `-` to clear a field back to its config default. The model can only ever nudge a default — it can never offer a value outside the allowed set. After the form, any mutation still passes the normal confirm gate.

Read results are rendered for a **human**, not dumped as JSON: `/agents` and `/pipelines` print aligned tables (id · status · type · name · what), `/agent <id>` a tight labelled block (empty fields omitted), `/ctx` a key/value table, and `/inbox` / `/collab` / `/approvals` one compact line each. (Fast-Brain still receives the structured JSON when it plans in natural language — only the deterministic `/`-command output is reshaped.)

## How it behaves

| Behaviour | Detail |
|---|---|
| **NL → tool-call loop** | Backed by Fast-Brain's `FastBrainChatter` (`internal/fastbrain`; multi-turn tool-calling over a headless backend CLI, latency-bounded and fail-open). A bounded turn budget stops runaway loops. Hardened against small-model slips: hallucinated args (a fabricated `repo`, a bogus `model`/`type`) are scrubbed before the gate, a missing required arg is fed back rather than approved into a doomed call, and a malformed tool call is retried instead of failing the turn. |
| **Read auto-runs, mutate confirms** | Read-only verbs (`list_agents`, `get_agent`, `get_agent_output`, `get_collaboration_status`, `read_inbox`, `list_approvals`, `ctx_get`/`ctx_list`, pipeline reads) auto-execute. |
| **Mandatory confirm gate** | Every mutating verb (`spawn_agent`, `send_to_agent`, `terminate_agent`, `delete_agent`, `restore_agent`, `approve`, `commit`, `push`, `sync`, `check`, `ctx_set`, `send_message`, `pipeline_create`/`_cancel`, `clean_up`) requires explicit operator approval before it runs — non-config-gated, can't be disabled. A batched plan confirms as one unit. **`[e]dit`** walks the call **field by field** — one short prompt per argument showing the current value (Enter keeps it, Ctrl-C finishes), so you can fill in a field the model omitted (e.g. a `branch`) without hand-editing JSON. Fields with a known set (`model`, `permission_mode`, `type`, booleans) show a **numbered pick-list**; a blank field warden fills from config (`model`, `permission_mode`) shows that default in the bracket as `[default: …]`, so you can see what an empty answer will use. This is the same engine behind the [guided argument forms](#guided-argument-forms). |
| **Planning-tier routing** | A cheap deterministic pre-classify buckets each request's needed planning tier; planning runs on Fast-Brain and execution always stays token-free warden calls. The old `local_llm.tier` / `.escalate` / `.classifier` knobs are retired and ignored. |
| **Monitoring verbs** | `fleet_digest` / `agent_digest` summarize state, `pending_for_me` surfaces what needs you, and `clean_up` proposes terminate/delete of finished agents through the same confirm gate. |
| **Project grounding** | Ask a project question — `/memory <q>` (`/mem`/`/ask`) or the `project_memory` tool — and warden answers it from `.warden/memory.md` (`memory.ground`, default on). Read-only: served by Fast-Brain, it cites each entry's trust (`unverified`/`trusted`/`human`) + provenance, degrades to the matching entries verbatim with no backend available, and answers "not in project memory" for an absent/empty file (never auto-creating it). |
| **`!`-shell passthrough** | A `!`-prefixed line runs in a persistent embedded `$SHELL` (cwd/env persist) and tees output to the terminal. The REPL takes **no action** on that output — it reports verbatim; the output is visible as context to the next turn. |
| **Real line editor** | Backed by readline: arrow-key cursor movement, ↑/↓ history (persisted to `~/.warden/orch_history`), Ctrl-R reverse-search, Ctrl-A/E/W/K/U editing, a **live `/`-command menu** that filters as you type (verb + summary, painted under the prompt) plus Tab completion of `/` commands and live agent ids, **guided argument forms** (pick-lists + free text) when a command needs more input, Ctrl-C to abandon a line, Ctrl-D to close. The prompt and headings are colourised (honours `NO_COLOR` and non-TTY output). |

> **No local model to choose.** `warden backend suggest` is a retired compatibility stub and `warden doctor` no longer recommends or checks a local model: the REPL runs on Fast-Brain. Legacy `local_llm.*` keys still load but are ignored.

For the token-spending alternative — driving the same operations from a full orchestrator agent session (e.g. Claude) — see [Orchestration: MCP & skill](/warden/multi-agent/mcp-and-skill/).

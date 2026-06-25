---
title: Orchestrator REPL (local LLM)
description: warden orch — a local-LLM conductor that turns plain-English operator intent into confirmed warden actions, without spending Claude tokens.
---

`warden orch` (alias `warden orchestrator`) is a warden-aware **local-LLM** conductor REPL. It turns natural-language operator intent — *"spin up two agents on the API and the web, then run the tests"* — into **confirmed** warden tool calls: spawn/monitor/teardown agents, drive pipelines, run the git/check lifecycle. It conducts; **it never implements** — there's no edit/write/bash tool in its registry, so all code work is delegated by spawning a Claude agent.

It's the one warden surface that **requires** a local model: set `local_llm: true` (and `local_llm_url`/`_model`/`_timeout`) in the config. Because execution is always plain warden API calls, it spends **no Claude tokens**.

```sh
warden orch          # standalone REPL
```

You can also run it as the cockpit master pane via the `orchestrator` config setting / `--orch` flag; **Alt+t** toggles that slot between `wd orch` and a raw `$SHELL` without killing either side.

## How it behaves

| Behaviour | Detail |
|---|---|
| **NL → tool-call loop** | Backed by the `internal/llm` Chatter seam (Ollama `/api/chat`, multi-turn tool-calling). A bounded turn budget stops runaway loops; malformed args / unknown tools recover instead of garbling execution. |
| **Read auto-runs, mutate confirms** | Read-only verbs (`list_agents`, `get_agent`, `get_agent_output`, `get_collaboration_status`, `read_inbox`, `list_approvals`, `ctx_get`/`ctx_list`, pipeline reads) auto-execute. |
| **Mandatory confirm gate** | Every mutating verb (`spawn_agent`, `send_to_agent`, `terminate_agent`, `delete_agent`, `restore_agent`, `approve`, `commit`, `push`, `sync`, `check`, `ctx_set`, `send_message`, `pipeline_create`/`_cancel`, `clean_up`) requires explicit operator approval before it runs — non-config-gated, can't be disabled. A batched plan confirms as one unit. |
| **Capability-tier routing** | A cheap pre-classify buckets each request's needed tier against the model's tier (`local_llm_tier`). Within tier ⇒ plan locally; over tier ⇒ escalate one planning step to headless Claude (`local_llm_escalate`, default on) or degrade honestly. Execution always stays token-free. |
| **Monitoring verbs** | `fleet_digest` / `agent_digest` summarize state, `pending_for_me` surfaces what needs you, and `clean_up` proposes terminate/delete of finished agents through the same confirm gate. |
| **`!`-shell passthrough** | A `!`-prefixed line runs in a persistent embedded `$SHELL` (cwd/env persist) and tees output to the terminal. The orchestrator takes **no action** on that output — it reports verbatim; the output is visible as context to the next turn. |

> Not sure which model to run? `warden doctor` best-effort detects your accelerator / host memory and **recommends** a `local_llm_model` from the Qwen2.5-Coder family sized to fit. It only ever recommends — you set the model; warden never silently swaps it.

For the token-spending alternative — driving the same operations from a full Claude session — see [Orchestration: MCP & skill](/warden/multi-agent/mcp-and-skill/).

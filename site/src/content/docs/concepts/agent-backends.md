---
title: Agent backends
description: Warden drives Claude Code by default, but the agent layer is pluggable — pick a backend (Claude or Aider) per agent, with capabilities that degrade gracefully.
---

Warden was built around Claude Code, but the agent layer is an **adapter layer**:
each console coding agent is normalized behind a neutral `Backend` interface
(`internal/agentbackend`), and warden core never references a concrete agent
binary directly. You pick the backend **per agent at spawn time**.

## Selecting a backend

| Backend | id | Tier | Summary |
|---|---|---|---|
| **Claude Code** (default) | `claude` | A | Full fidelity — digests, savings, priced spend, resume, all permission modes |
| **Aider** | `aider` | A | Bring-your-own-model; structured markdown transcript ⇒ real digests; no resume, no priced spend |

```sh
# Claude (default)
warden start "review the auth module"

# Aider against a local Ollama model (free, offline)
export OLLAMA_API_BASE=http://127.0.0.1:11434
warden start "implement the add function" \
  --backend aider --model ollama_chat/qwen2.5-coder:3b --dir .
```

Over MCP, pass the `backend` param to `spawn_agent` (kept at parity with the
`--backend` CLI flag). The selection is stored on the session (`Session.Backend`;
empty means `claude`, so existing stores need no migration), and an unknown
backend id is rejected before any tmux/worktree side effect.

## Capabilities & graceful degradation

Backends disagree on exactly the seams warden depends on, so each declares
**capability flags** and warden degrades a feature rather than crashing when a
capability is missing:

| Capability missing | What warden does instead |
|---|---|
| **Structured transcript** | Digest falls back to a pane-scrape summary; savings (which need real token deltas) are disabled for that agent |
| **Pricing** | `wd spend` shows tokens (heuristic) not dollars; `wd savings` omits the agent |
| **Resume** | `rotate`/`handoff` re-spawn a fresh agent instead of `--resume`; `restore` refuses with a clear message |
| **System-prompt injection** | Warden's pipeline/collab/git hints are skipped (no invalid flags reach the agent) |
| **Session-id control** | Warden discovers the agent-generated id (or uses a workdir-based transcript path) instead of assigning one |

## Aider specifics

- **Bring-your-own-model:** pass `--model` (any provider, or a local Ollama model
  like `ollama_chat/qwen2.5-coder:3b`). Because the model is BYO, warden can't
  price it — spend is tokens-only and savings omits the agent.
- **Tier A transcript:** Aider's `.aider.chat.history.md` is parsed into warden's
  neutral turns, so completion digests work on real structured data.
- **Autonomous, not a loop:** an Aider agent with a prompt runs a one-shot
  `--message` task and exits when done (Aider has no persistent agent loop like
  Claude). Launch it without a prompt for an interactive session you attach to and
  drive by hand.
- **No resume / no session id:** Aider continues from repo history, not a pinned
  id, so warden re-spawns fresh on rotate/handoff rather than resuming.

More backends (Antigravity CLI, Codex CLI, OpenCode) land as isolated adapter PRs
over time — see roadmap item #52 and the design spec under
`docs/superpowers/specs/`.

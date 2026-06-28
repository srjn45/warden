---
title: Agent backends
description: Warden drives Claude Code by default, but the agent layer is pluggable — pick a backend (Claude, Aider, or OpenCode) per agent, with capabilities that degrade gracefully.
---

Warden was built around Claude Code, but the agent layer is an **adapter layer**:
each console coding agent is normalized behind a neutral `Backend` interface
(`internal/agentbackend`), and warden core never references a concrete agent
binary directly. You pick the backend **per agent at spawn time**.

:::caution[Experimental backends]
Warden is fully tested only with **Claude Code**. All other agent backends (Aider, OpenCode, and any future integrations) are **experimental / work-in-progress** — functionality may be reduced or unverified. Any non-`claude` value for `--backend` is experimental.
:::

## Supported agents — status

| Agent | Status |
|---|---|
| Claude Code | ✅ Stable — fully tested, reference backend |
| Aider | 🧪 Experimental (WIP) |
| OpenCode | 🧪 Experimental (WIP) |

## Selecting a backend

| Backend | id | Tier | Summary |
|---|---|---|---|
| **Claude Code** (default) | `claude` | A | Full fidelity — digests, savings, priced spend, resume, all permission modes |
| **Aider** | `aider` | A | 🧪 Experimental. Bring-your-own-model; structured markdown transcript ⇒ real digests; no resume, no priced spend |
| **OpenCode** | `opencode` | A | 🧪 Experimental. Bring-your-own-model; structured JSON transcript (via `opencode export`) ⇒ real digests; **resumes** the worktree's last session; no priced spend (BYO model) |

```sh
# Claude (default)
warden start "review the auth module"

# Aider against a local Ollama model (free, offline)
export OLLAMA_API_BASE=http://127.0.0.1:11434
warden start "implement the add function" \
  --backend aider --model ollama_chat/qwen2.5-coder:3b --dir .

# OpenCode against a local Ollama model (free, offline)
warden start "implement the add function" \
  --backend opencode --model ollama/qwen2.5-coder:3b --dir .
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

## OpenCode specifics

- **Bring-your-own-model:** pass `-m provider/model` via `--model` (any provider,
  or a local Ollama model like `ollama/qwen2.5-coder:3b`). Spend is tokens-only —
  OpenCode tracks its own cost/tokens (first-class for paid providers), but
  warden's spend integration reads them only once the transcript-usage wiring
  lands (see #52).
- **Tier A transcript (SQLite, sourced via `export`):** OpenCode stores
  transcripts in a SQLite DB, not a flat file. The adapter sources the transcript
  through `opencode export <session>` — one command that emits the whole session
  as clean `{info, messages[]}` JSON — and parses it into warden's neutral turns,
  so digests run on real structured data. (This is the design's "TranscriptSource
  = DB query, not file read" case; sourcing via `export` avoids coupling to the DB
  schema.)
- **Resumes — dir-scoped:** unlike Aider, OpenCode **does** resume. OpenCode mints
  its own session id (warden can't assign one), so the adapter keys resume off the
  agent's worktree: `opencode -c` continues *that directory's* last session
  (verified dir-scoped). rotate/handoff/restore therefore work. When a future
  phase captures and pins OpenCode's real `ses_…` id (discover-then-pin, #52), the
  adapter automatically upgrades to exact-id resume/transcript with no changes.
- **Persistent loop:** an OpenCode agent runs its TUI with the task seeded via
  `--prompt`, staying interactive (like Claude), rather than running once and
  exiting (like Aider).
- **Interactive approvals not yet mapped:** headless runs use
  `--dangerously-skip-permissions` (no prompts); the TUI's permission prompts are
  not yet parsed into warden's approval queue, so warden infers idle from
  staleness for OpenCode agents (deferred — see #52).

More backends (Antigravity CLI, Codex CLI) land as isolated adapter PRs over time
— see roadmap item #52 and the design spec under `docs/superpowers/specs/`.

---
title: Agent backends
description: Warden drives Claude Code by default, but the agent layer is pluggable — pick a backend (Claude, Aider, OpenCode, Codex, Crush, Goose, Cursor, or Antigravity) per agent, with capabilities that degrade gracefully.
---

Warden was built around Claude Code, but the agent layer is an **adapter layer**:
each console coding agent is normalized behind a neutral `Backend` interface
(`internal/agentbackend`), and warden core never references a concrete agent
binary directly. You pick the backend **per agent at spawn time**.

:::caution[Backend maturity]
Warden is fully tested only with **Claude Code**. **Codex CLI** and **Antigravity CLI** are **β beta** — live-verified state, approval, and transcript fidelity, still maturing. The remaining non-`claude` backends (Aider, OpenCode, Crush, Goose, Cursor) are **experimental / work-in-progress** — functionality may be reduced or unverified.
:::

## Supported agents — status

| Agent | Status |
|---|---|
| Claude Code | ✅ Stable — fully tested, reference backend |
| Aider | 🧪 Experimental (WIP) |
| OpenCode | 🧪 Experimental (WIP) |
| Codex CLI | β Beta |
| Crush | 🧪 Experimental (WIP) |
| Goose | 🧪 Experimental (WIP) |
| Cursor CLI | 🧪 Experimental (WIP) |
| Antigravity CLI | β Beta |

## Selecting a backend

| Backend | id | Tier | Summary |
|---|---|---|---|
| **Claude Code** (default) | `claude` | A | Full fidelity — digests, savings, priced spend, resume, all permission modes |
| **Aider** | `aider` | A | 🧪 Experimental. Bring-your-own-model; structured markdown transcript ⇒ real digests; no resume, no priced spend |
| **OpenCode** | `opencode` | A | 🧪 Experimental. Bring-your-own-model; structured JSON transcript (via `opencode export`) ⇒ real digests; **resumes** the worktree's last session; no priced spend (BYO model) |
| **Codex CLI** | `codex` | A | β Beta. BYO provider (Codex config / `-m`); structured JSONL transcript (rollout files) ⇒ real digests; **resumes** dir-scoped, upgraded to exact-id via discover-then-pin; live state + approval detection; context injection (AGENTS.md); no priced spend |
| **Crush** | `crush` | A | 🧪 Experimental. BYO model (config-driven TUI); structured JSON transcript (via `crush session show --json`) ⇒ real digests; **resumes** dir-scoped; initial prompt auto-typed post-launch via `PromptSeeder`; no priced spend |
| **Goose** | `goose` | A | 🧪 Experimental. BYO provider (`GOOSE_PROVIDER`/`GOOSE_MODEL` env); structured JSON transcript (via `goose session export`) ⇒ real digests; **resumes** name-deterministic; no model flag on session launch; no priced spend |
| **Cursor CLI** | `cursor` | C | 🧪 Experimental. Hosted plan (`cursor-agent`, billed to your Cursor subscription); rich native permission modes (`plan`/`ask`/`auto-review`/`force`); **resumes** dir-scoped; live state + approval/trust detection; **no structured transcript yet** ⇒ no digests; no priced spend |
| **Antigravity CLI** | `antigravity` | A | β Beta. Google-hosted free tier (`agy`, multi-vendor model menu); structured trajectory JSONL (incl. tool calls / files changed) ⇒ real digests; **resumes** dir-scoped; live state + approval/trust detection; no priced spend |
| **Terminal** (plain shell) | `terminal` | — | Not an AI agent. Opens an interactive `$SHELL` in the agent's directory, managed with warden's normal worktree/git/tmux lifecycle. No AI features (no digests, resume, model, spend); the task prompt is ignored. The "human seat" beside the fleet |

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

# Codex — configure provider in ~/.codex/config.toml first
warden start "implement the add function" --backend codex --dir .

# Crush — configure provider in ~/.config/crush/crush.json first
warden start "implement the add function" --backend crush --dir .

# Goose against a local Ollama model ($0)
GOOSE_PROVIDER=ollama GOOSE_MODEL=qwen2.5-coder:3b \
warden start "implement the add function" --backend goose --dir .

# Cursor — log in once with `cursor-agent login` (hosted, billed to your Cursor plan)
warden start "implement the add function" --backend cursor --dir .

# Antigravity — Google-hosted free tier; `agy` resolves its model from config
warden start "implement the add function" --backend antigravity --dir .

# Terminal — a plain shell in the directory, no AI (prompt is ignored)
warden start --backend terminal --dir .
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
| **System-prompt injection** | Warden delivers its pipeline/collab/git hints via a rules file the agent reads on startup (`InjectContext`: `AGENTS.md` for Codex/OpenCode/Cursor/Antigravity, `CRUSH.md`, `.goosehints`); only a backend that auto-reads no such file (Aider) skips the hints entirely — no invalid flags ever reach the agent |
| **Session-id control** | Warden discovers the agent-generated id post-launch (`DiscoverSessionID`, e.g. Codex) and pins it, or falls back to a workdir-based transcript path, instead of assigning one |

## Backend superpowers (`wd review`, `wd models`, `wd fork`)

Degradation is the *deficit* side — making a feature warden has work everywhere.
The flip side is **surfacing a backend's native strengths** that Claude doesn't
have, as first-class verbs added *on top* (never a restriction):

| Verb | What it surfaces | Backends |
|---|---|---|
| **`wd review`** | the backend's OWN one-shot diff reviewer against the worktree (agent-native counterpart to `wd check` / a `pr-review` agent); `--json` emits neutral machine-readable findings | **Codex** (`codex review` / `codex exec review`) |
| **`wd models`** | the backend's **live** runtime model menu (vs warden's static aliases); ids feed `--model` verbatim, listing spends no quota | **Antigravity** (`agy models`), **Cursor** (`cursor-agent --list-models`) |
| **`wd fork`** | branch the source agent's recorded **conversation/reasoning** into a new managed agent — a fresh sibling worktree off its branch, carrying its uncommitted tracked changes; the source keeps running. Shorthand for `start --fork-from` | **Codex** (`codex fork`) |

`wd review` and `wd models` are **CLI-only by design** — like `wd check` they exec
in the agent's worktree with no daemon round-trip, so they have no MCP/web/TUI twin.
Each is an optional, type-asserted interface (`Reviewer`/`StructuredReviewer`,
`ModelLister`); a backend that doesn't implement one simply isn't offered the verb
(Claude degrades non-zero with a pointer to the alternative). **`wd fork` is the
exception** — it's a managed spawn that crosses the daemon (a thin wrapper over the
`fork_from` spawn field, gated by the `SessionForker` interface), so it has **MCP +
CLI parity** via the `fork_agent` tool. Cursor's server-side `--auto-review` ("Smart
Auto") is surfaced not as a verb but as the `auto-review` **permission mode**. Full
walkthrough: [Backend superpowers](/warden/guides/backend-superpowers/).

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
- **Context injection (AGENTS.md):** OpenCode has no `--append-system-prompt` flag,
  so warden delivers its pipeline/collab/git hints via the `AGENTS.md` rules file
  OpenCode reads on startup (`InjectContext`); `SystemPromptInject` Caps stays
  `false` (it tracks a launch flag specifically).
- **Interactive approvals not yet mapped:** headless runs use
  `--dangerously-skip-permissions` (no prompts); the TUI's permission prompts are
  not yet parsed into warden's approval queue, so warden infers idle from
  staleness for OpenCode agents (deferred — see #52).

## Codex CLI specifics

- **BYO provider:** Codex resolves its model/provider from `~/.codex/config.toml` (or a `-p <profile>`). Pass `-m <model>` via `--model` when needed. No warden-side dollar pricing (spend shows tokens, savings omits).
- **Tier A transcript:** Codex persists sessions as JSONL rollout files (`$CODEX_HOME/sessions/<Y>/<M>/<D>/rollout-*.jsonl`). The adapter locates the newest rollout whose `session_meta.cwd` matches the agent's worktree and parses `response_item` records into neutral Turns.
- **Resumes — dir-scoped, now exact-id (discover-then-pin):** `codex resume --last` continues the most-recent session in the working directory, and warden additionally **discovers** the minted `session_id` from the rollout's `session_meta` header post-launch and pins it (`DiscoverSessionID`), so resume/transcript resolve by exact id rather than dir-scope.
- **Initial prompt:** Codex's TUI accepts a trailing positional prompt at launch (like Claude), so warden can seed the first task normally.
- **Live state + approval detection:** warden classifies the Codex TUI pane (`esc to interrupt` ⇒ working; the numbered "Would you like to …?" permission prompt ⇒ needs-input) and normalizes that prompt into the approvals inbox — so auto-approve works for Codex agents (the headless `codex exec` surface raises no prompts).
- **Context injection (AGENTS.md):** Codex has no `--append-system-prompt` flag, so warden delivers its pipeline/collab/git hints by writing an `AGENTS.md` rules file Codex reads on startup (`InjectContext`). `SystemPromptInject` Caps stays `false` — it tracks the *launch flag* specifically, not whether the addendum is delivered.
- Full gap doc: [`docs/agent-backends/codex.md`](https://github.com/srjn45/warden/blob/main/docs/agent-backends/codex.md)

## Crush specifics

- **BYO model:** the interactive `crush` TUI is config-driven (model set in `~/.config/crush/crush.json`); headless `crush run` accepts `-m`. Pass `--model` for headless use; TUI launch ignores it.
- **Tier A transcript:** Crush stores sessions per-project in `.crush/crush.db` (SQLite). The adapter runs `crush session show <id> --json` to source the session as `{messages[]}` JSON and parses it into neutral Turns.
- **Resumes — dir-scoped:** `crush --continue` continues the most recent session in the working directory (the DB is project-scoped, so no global filter needed). Exact-id resume (`--session <id>`) activates once the id is discovered-then-pinned.
- **Initial prompt seeding:** warden launches the bare `crush` TUI, waits for the ready footer, then auto-types the task prompt via `PromptSeeder`. Headless `crush run "<prompt>"` remains the non-interactive path.
- **Context injection (CRUSH.md):** warden delivers its collab/git/pipeline hints via the `CRUSH.md` context file Crush reads on startup (`InjectContext`); `SystemPromptInject` Caps stays `false` (it tracks a launch flag specifically).
- **No TUI approval parsing yet** — warden infers idle from staleness for Crush agents (`crush run` and `--yolo` raise no prompts).
- Full gap doc: [`docs/agent-backends/crush.md`](https://github.com/srjn45/warden/blob/main/docs/agent-backends/crush.md)

## Goose specifics

- **BYO provider:** `goose session` has no `--model` or `--provider` flag — Goose resolves these from `GOOSE_PROVIDER`/`GOOSE_MODEL` env vars (or `~/.config/goose/config.yaml`). Set those before spawning.
- **Tier A transcript:** Goose stores sessions in a global SQLite DB. The adapter sources the transcript via `goose session export --format json` — one command emitting `{conversation:[...]}` JSON — and parses it into neutral Turns.
- **Resumes — name-deterministic:** warden pins its own agent id as the Goose `--name` at launch (`goose session --name <warden-id>`), so `goose session -r --name <id>` resumes the exact session — strictly more reliable than dir-scoped guessing.
- **No model flag on session launch.** The headless `goose run` path does accept `--model`/`--provider`.
- **Context injection (.goosehints):** warden delivers its collab/git/pipeline hints via the `.goosehints` file Goose auto-loads on startup (`InjectContext`); `SystemPromptInject` Caps stays `false` (it tracks a launch flag specifically).
- **No TUI approval parsing yet** — warden infers idle from staleness for Goose agents (`goose run` in `auto` mode raises no prompts).
- Full gap doc: [`docs/agent-backends/goose.md`](https://github.com/srjn45/warden/blob/main/docs/agent-backends/goose.md)

## Cursor specifics

- **Hosted, not $0-local:** the `cursor-agent` CLI runs against your Cursor subscription (log in once with `cursor-agent login`). There is no free local rig, and warden never surfaces dollars for it (billing is your Cursor plan); spend shows tokens, savings omits the agent.
- **Rich permission modes:** Cursor exposes a finer approval surface than a prompt/auto toggle, and warden surfaces it honestly — `PermissionModes = default | plan | ask | auto-review | force`. The Claude-flavored "just do it" aliases fold onto `-f`/`--yolo`.
- **Tier C — no structured transcript yet:** an interactive Cursor session persists to an undocumented binary SQLite `store.db` with no `export` verb, so `wd digest` shows "no transcript" for Cursor agents rather than guessing. The headless `stream-json` parser is implemented and tested but not wired (the TUI writes no on-disk NDJSON); the day warden gains a `store.db` reader it flips to Tier A.
- **Resumes — dir-scoped:** `cursor-agent --continue` continues the workspace's latest session (verified), so rotate/handoff work; exact-id resume lands with discover-then-pin.
- **Live state + approval/trust detection:** warden classifies the Cursor TUI pane (working / idle / needs-input) and normalizes both its command-allowlist menu and its one-time workspace-trust prompt into the approvals inbox.
- **Context injection (AGENTS.md):** cursor-agent has no `--append-system-prompt` flag, so warden delivers its pipeline/collab/git hints via the `AGENTS.md` rules file cursor-agent reads on startup (`InjectContext`); `SystemPromptInject` Caps stays `false` (it tracks a launch flag specifically).
- **Double-worktree hazard:** cursor-agent ships its own `-w/--worktree`; warden never passes it (warden already owns the worktree) and launches in warden's worktree dir directly.
- Full gap doc: [`docs/agent-backends/cursor.md`](https://github.com/srjn45/warden/blob/main/docs/agent-backends/cursor.md)

## Antigravity specifics

- **Hosted free tier, multi-vendor models:** the `agy` CLI (backend id `antigravity`) runs on Google's free tier (a daily-ish quota cap), and one agent can run Gemini, Claude, *and* GPT-OSS models under a single login (`agy models`). Tokens show in `agy`'s `/usage` TUI only — warden surfaces no dollars; spend shows tokens, savings omits the agent.
- **Tier A transcript:** the durable conversation store is encrypted/proto, but `agy` also writes a **plaintext JSONL trajectory log** that warden parses into neutral Turns — including tool calls and files changed (`tool_calls` on planner records, verified against a captured file-edit fixture) — so digests run on real structured data.
- **Resumes — dir-scoped:** `agy -c` continues the most recent conversation for the workspace (verified); exact-id resume lands with discover-then-pin.
- **Initial prompt:** seeded via `-i`/`--prompt-interactive`, then stays interactive (a persistent loop, like Claude).
- **Live state + approval/trust detection:** warden classifies `agy`'s TUI status bar (idle / working / needs-input) and maps its `Do you want to proceed?` permission menu **and** its launch-time workspace-trust prompt into the approvals inbox (shell-command + trust shapes verified; other prompt variants degrade safely until captured).
- **Context injection (AGENTS.md):** `agy` has no `--append-system-prompt` flag, so warden delivers its pipeline/collab/git hints via the `AGENTS.md` rules file `agy` reads on startup (`InjectContext`); `SystemPromptInject` Caps stays `false` (it tracks a launch flag specifically).
- Full gap doc: [`docs/agent-backends/antigravity.md`](https://github.com/srjn45/warden/blob/main/docs/agent-backends/antigravity.md)

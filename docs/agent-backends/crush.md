# Crush backend (experimental)

Adapter: `internal/agentbackend/backends/crush.go` · Backend ID: `crush` · #52

[Crush](https://github.com/charmbracelet/crush) is Charm's glamorous,
terminal-first, multi-provider AI coding agent (LSP-aware, MCP-capable). This
adapter is **experimental** and breadth-first: warden launches Crush in a tmux
session and layers its orchestration/digest/transcript features **on top** —
warden never strips Crush's own capabilities. This document records what works
today and the honest gaps.

Verified against **Crush v0.80.0**, fully local at **$0** (Ollama).

## Install method that worked

```sh
GOBIN=$HOME/.local/bin go install github.com/charmbracelet/crush@latest
```

(Charm also publishes `npm install -g @charmland/crush` and release binaries;
the `go install` path is what this integration was built and tested against.)

## $0-local viability (Ollama)

Confirmed working end-to-end against Ollama with **no paid provider**. Crush
reads an OpenAI-compatible provider from its config. Global config at
`~/.config/crush/crush.json` (also honored: `./.crush.json`, `./crush.json`):

```json
{
  "$schema": "https://charm.land/crush.json",
  "providers": {
    "ollama": {
      "name": "Ollama",
      "type": "openai",
      "base_url": "http://127.0.0.1:11434/v1/",
      "api_key": "ollama",
      "models": [
        { "id": "qwen2.5-coder:3b", "name": "Qwen2.5 Coder 3B", "context_window": 32768, "default_max_tokens": 4096 }
      ]
    }
  },
  "models": {
    "large": { "model": "qwen2.5-coder:3b", "provider": "ollama" },
    "small": { "model": "qwen2.5-coder:3b", "provider": "ollama" }
  }
}
```

`crush run "<prompt>"` and the transcript read-path (`crush session list/show
--json`) work against this rig. **Caveat (rig, not integration):** the 3B local
models are too weak to reliably emit native tool calls — `qwen2.5-coder:3b`
tended to print the tool call as fenced text, and `llama3.2:3b` did issue a real
`view` tool_call. Real edits land with a capable model/provider; the adapter's
transcript parser is exercised against both a real `tool_call` fixture and a
synthetic write/edit fixture.

## CLI → warden interface mapping

| warden interface         | Crush command                                  | Notes |
|--------------------------|------------------------------------------------|-------|
| `LaunchCmd` (TUI)        | `crush` (`+ --yolo` for auto-approve)          | interactive Bubble Tea TUI — the core requirement |
| `HeadlessCmd`            | `crush run --quiet "<prompt>"`                 | non-interactive one-shot; no permission prompts |
| `ResumeCmd`              | `crush --session <id>` / `crush --continue`    | exact-id when a 16-hex id is pinned; else dir-scoped continue |
| `LaunchPromptArg`        | *(none — returns `""`)*                         | **gap:** the TUI takes no initial prompt (see below) |
| `TranscriptPath`         | `crush session list --json` → `crush session show <id> --json` | SQLite-sourced via the CLI, like OpenCode's `export` |
| `ParseTranscript`        | parses `session show` JSON                      | Tier A |
| `SystemPromptFlag`       | *(unsupported)*                                 | no `--append-system-prompt` equivalent |
| `Pricing`                | *(unsupported)*                                 | BYO multi-provider |
| `DetectState` / `ParseApproval` | *(degraded)*                            | TUI prompts not captured |

Permission-mode folding: warden's `yolo` / `auto` / `acceptEdits` /
`bypassPermissions` / `dangerously-skip-permissions` / `dontAsk` →  Crush's
`--yolo` (auto-accept all). Cautious modes stay interactive.

## Transcript / session storage

- **Location:** per-project **SQLite** at `<workdir>/.crush/crush.db`. Crush
  writes a `.crush/.gitignore` (`*`) so the store never pollutes the repo.
  (Global ephemeral state — `projects.json`, `providers.json` — lives in
  `~/.local/share/crush/` and holds no transcripts.)
- **Read path:** `crush session show <id> --json` emits the whole session as
  clean `{meta, messages[]}` JSON. The adapter sources the transcript through
  this command (querying the agent, **not** reading the DB schema — same
  decoupled pattern as OpenCode's `export`).
- **Format:** each message has a `role` (`user` | `assistant` | `tool`) and
  ordered `parts`. Part types: `text`, `reasoning`, `tool_call`
  (`name` + `input`, where **`input` is a JSON-encoded string**), `tool_result`,
  `finish`, `binary`, `image_url`. The adapter maps user/assistant messages to
  neutral Turns; it pulls the tool name and edited files (from each `tool_call`
  input's `file_path`/`path`/`filename`) onto the assistant turn. Standalone
  `tool` (tool_result) messages are not emitted as Turns — the assistant turn
  already records the tool/files, matching the Claude/OpenCode adapters.
- **Dir-scoping:** because the store is per-cwd, `crush session list --json` run
  in the agent's worktree is **inherently scoped to that worktree** — no
  global-list directory filter is needed (simpler than OpenCode). Each warden
  agent runs in its own git worktree, so it gets its own `.crush/crush.db`.

## Capability table

| Capability             | Status | Detail |
|------------------------|--------|--------|
| Headless one-shot      | ✅      | `crush run` |
| Resume                 | ✅      | dir-scoped `--continue` today; exact-id `--session <id>` when a 16-hex id is pinned |
| Structured transcript  | ✅ Tier A | sourced via `session show --json` |
| Model selection        | ⚠️ partial | `crush run -m provider/model` and config support it; **the interactive TUI has no `-m` flag** (config-driven), so `LaunchCmd` omits the model |
| Session-id control     | ❌      | Crush mints its own 16-hex id; warden cannot assign one (`SessionIDControl=false`) |
| Permission modes       | default / `yolo` | TUI prompt vs. `--yolo` auto-accept |
| Pricing / spend $      | ❌ (deferred) | Crush **does** track cost/tokens natively in session meta; warden's usage reader is Claude-JSONL-specific and doesn't read it yet |
| System-prompt inject   | ❌      | no launch-time `--append-system-prompt`; customization is config / `CRUSH.md` context files |
| Initial-prompt seeding | ❌ (gap) | the interactive TUI takes no positional/flag prompt |

## What works vs. what warden can't do yet

**Works today:**
- Launch the interactive TUI in a tmux pane (core), with optional `--yolo`.
- Headless classify/summarize offload via `crush run`.
- Resume the worktree's session (`--continue`) or an exact pinned id.
- Tier-A digests / "what changed" from the SQLite-sourced transcript (user &
  assistant turns, tool names, edited files).

**Gaps (warden can't do these yet for Crush):**
1. **No initial-prompt seeding into the TUI.** The interactive `crush` rejects a
   positional prompt as an unknown command and has no prompt flag — only
   `crush run` takes one. `LaunchPromptArg` returns `""`; the operator types the
   first task after attaching, or the headless path is used. *Largest gap.*
2. **No per-agent model on the TUI launch.** The TUI has no `-m`; the model is
   config-driven (`models.large`/`small`) or switched in-TUI. `LaunchCmd` omits
   the resolved model.
3. **No interactive approval automation.** Crush's permission prompts live in
   its Bubble Tea TUI with no stable captured-pane marker, so `DetectState` and
   `ParseApproval` degrade (warden infers idle from staleness; `--yolo` and
   headless `run` raise no prompts).
4. **No warden-side dollar spend.** Multi-provider BYO; pricing degrades to
   tokens-only (Crush's native `meta.cost`/token counts are not yet wired in).
5. **No system-prompt injection** of warden's collab/git/pipeline hints.

## Crush superpowers worth preserving / wiring later

Per the "add on top, never strip" principle, these Crush features remain fully
available to the agent and are candidates for deeper warden integration:

- **Multi-provider, switch-mid-session** — any of dozens of providers (and local
  Ollama) in one session.
- **LSP integration** — language-server-aware code context.
- **MCP client** — Crush can itself consume MCP servers.
- **Native cost & token accounting** — `session show --json` `meta` carries
  `cost`, `prompt_tokens`, `completion_tokens`, `total_tokens` per session; the
  natural source for a future first-class warden spend wiring.
- **`crush server` / host socket** — a server mode (`-H/--host`) that could back
  a richer programmatic integration than the CLI shell-out.
- **`CRUSH.md` context files & config agents** — the future home for injecting
  warden's conventions without a launch flag.

## Deferred (FUTURE_ENHANCEMENTS #52)

- Discover-then-pin the minted 16-hex session id (enables exact-id resume/show
  without the dir-scoped fallback; the adapter already prefers a pinned id).
- Wire warden spend/savings to Crush's native `meta` cost/tokens.
- Map interactive TUI approvals (`DetectState`/`ParseApproval`).
- Seed the initial task prompt (needs a Crush TUI prompt flag or the server API).
- Inject warden's system-prompt hints via `CRUSH.md` / config.

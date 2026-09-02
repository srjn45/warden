# OpenCode backend (experimental)

Adapter: `internal/agentbackend/backends/opencode.go` · Backend ID: `opencode` · #52

[OpenCode](https://opencode.ai) is a multi-provider, bring-your-own-model terminal
coding agent with a SQLite session store. This adapter launches OpenCode's
interactive TUI in a warden-managed tmux session and layers warden's
orchestration / digest / transcript features **on top** — warden never strips
OpenCode's own capabilities. This document records what works today and the honest
gaps.

It is warden's first **SQLite-backed, agent-minted-session-id** backend; the
transcript is sourced through `opencode export <id>` (one command that emits the
whole session as clean `{info, messages[]}` JSON), keeping the adapter decoupled
from the DB schema.

## CLI → warden interface mapping

| warden method        | OpenCode invocation                                   | Notes |
|----------------------|-------------------------------------------------------|-------|
| `LaunchCmd` (TUI)    | `opencode [-m provider/model]`                        | Interactive TUI; auto-approve modes prepend `OPENCODE_CONFIG_CONTENT` (the TUI has no skip-permissions flag). |
| `LaunchPromptArg`    | ` --prompt "$(cat <file>)"`                           | OpenCode runs the seeded prompt on launch and stays interactive. |
| `ResumeCmd`          | `opencode -s <ses_…>` / `opencode -c`                 | exact-id when a `ses_…` id is pinned; else dir-scoped `-c` continue. |
| `HeadlessCmd`        | `opencode run --dangerously-skip-permissions <prompt>`| One-shot for warden's classify/summarize offload. |
| `TranscriptPath`     | `opencode session list --format json` → `opencode export <id>` | SQLite-sourced via the CLI; materialized to a temp JSON file. |
| `ParseTranscript`    | parses `export` JSON `messages[]`                     | `text` / `tool` / `patch` parts → neutral Turns. |
| `SystemPromptFlag`   | — (unsupported)                                       | OpenCode has no `--append-system-prompt` flag. |
| `InjectContext`      | writes `<workdir>/AGENTS.md`                          | warden's collab/git/pipeline addendum is delivered via the AGENTS.md rules file OpenCode reads on startup (the no-flag fallback). |
| `Pricing`            | — (unsupported)                                       | multi-provider BYO; tokens may be exposed, dollars not wired. |
| `DetectState` / `ParseApproval` | — (degraded)                              | TUI prompts not captured this phase. |

Permission-mode folding: warden's `dangerously-skip-permissions` / `yes-always` /
`auto` / `acceptEdits` / `bypassPermissions` / `dontAsk` → the
`OPENCODE_CONFIG_CONTENT` auto-approve env (the interactive TUI has no
`--dangerously-skip-permissions` flag — that flag is `opencode run`-only). Cautious
modes stay interactive.

## Transcript / session storage

- **Store:** SQLite (agent-minted `ses_…` session ids).
- **Read path:** `opencode export <id>` emits the whole session as clean
  `{info, messages[]}` JSON; the adapter sources the transcript through this command
  (querying the agent, **not** the DB schema).
- **Dir-scoping:** the session list is global, so the adapter filters
  `opencode session list --format json` by the agent's working directory (every
  warden agent runs in its own git worktree). When a real `ses_…` id is pinned
  (future discover-then-pin), the adapter automatically prefers exact-id
  `export <id>` / `-s <id>` resume.

## Capability table

| Capability             | Caps flag              | Status | Detail |
|------------------------|------------------------|--------|--------|
| Headless one-shot      | `Headless`             | ✅    | `opencode run`. |
| Resume                 | `Resume`               | ✅    | dir-scoped `-c` today; exact-id `-s <ses_…>` when a real id is pinned. |
| Structured transcript  | `StructuredTranscript` | ✅    | **Tier A** — `export` JSON → neutral Turns. |
| Model selection        | `ModelSelection`       | ✅    | `-m provider/model` (omitted when empty so the BYO config default applies). |
| Session-id control     | `SessionIDControl`     | ❌    | OpenCode mints its own `ses_…` id; warden cannot assign one at launch. |
| Permission modes       | `PermissionModes`      | default / `dangerously-skip-permissions` | TUI prompt vs. config-injected auto-approve. |
| System-prompt inject   | `SystemPromptInject`   | ✅ via rules file | no launch-time `--append-system-prompt`, but warden delivers the same addendum out-of-band via the `AGENTS.md` rules file OpenCode reads on startup (`InjectContext`). The Caps flag stays `false` — it tracks the *launch flag* specifically. |
| Pricing / spend $      | `Pricing`              | ❌    | multi-provider BYO; no warden-side rate table. |
| State / approval detect| —                      | ❌    | degraded (Unknown / false); not yet mapped. |

## What works vs. what warden can't do yet

**Works today:**
- Launch the interactive TUI in a tmux pane (core), with the initial task seeded
  via `--prompt` and optional config-injected auto-approve.
- Headless classify/summarize offload via `opencode run`.
- Resume the worktree's session (`-c`) or an exact pinned `ses_…` id.
- Tier-A digests / "what changed" from the SQLite-sourced transcript.
- Runs $0-local against a BYO Ollama provider.

**Gaps (honest, breadth-first):**
1. **No session-id pinning.** OpenCode mints its own `ses_…` id; warden resolves
   transcript + resume **dir-scoped** until discover-then-pin lands
   (FUTURE_ENHANCEMENTS #52). The adapter already prefers a pinned id when present.
2. **No interactive approval automation.** OpenCode's permission prompts live in
   its TUI with no captured pane marker, so `DetectState` / `ParseApproval` degrade
   (warden infers idle from staleness; the config-injected auto-approve and headless
   `run` raise no prompts).
3. **No warden-side dollar spend.** Multi-provider BYO; pricing degrades to
   tokens-only.
4. ~~**No system-prompt injection** of warden's collab/git/pipeline hints.~~
   **Resolved** — warden delivers its collab/git/pipeline hints by writing them into
   the `AGENTS.md` rules file OpenCode auto-loads from the working directory on
   startup (`InjectContext`, the shared rules-file injector in `inject.go`; same
   no-clobber / idempotent / git-`info/exclude` semantics as Codex). OpenCode follows
   the AGENTS.md standard (loads the nearest `AGENTS.md` by traversing up from the
   working directory). The `SystemPromptInject` Caps flag stays `false` because it
   tracks a *launch-time* flag specifically, which OpenCode still lacks.

## Deferred (FUTURE_ENHANCEMENTS #52)

- Discover-then-pin the minted `ses_…` session id (enables exact-id resume/export
  without the dir-scoped fallback; the adapter already prefers a pinned id).
- Wire warden usage spend/savings to OpenCode's native cost/tokens.
- Map interactive TUI approvals (`DetectState` / `ParseApproval`).

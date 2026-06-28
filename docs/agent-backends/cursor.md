# Cursor CLI backend (experimental)

warden's adapter for **Cursor's CLI agent** (the `cursor-agent` binary), added
breadth-first under #52. This is a thin, honest adapter: warden opens a tmux session
and launches cursor-agent, and **documents the gaps** rather than pretending Cursor is
a perfect drop-in for Claude Code. warden adds capability *on top of* Cursor; it never
strips Cursor's features down to a lowest common denominator.

- **Adapter:** `internal/agentbackend/backends/cursor.go`
- **Tests / fixtures:** `cursor_test.go`, `testdata/cursor/`
- **Tier:** **C** (transcript degraded — interactive sessions persist to an
  unreadable SQLite store; digests do not run on Cursor data yet)
- **Verified against:** `cursor-agent` 2026.06.26-7079533, hosted plan (logged in via
  `cursor-agent login`), default model `composer-2.5-fast`

> **Hosted, not $0-local.** Unlike the Codex/OpenCode/Ollama rigs, Cursor is billed
> against the user's Cursor subscription. This adapter was verified with the **minimum**
> live invocations: one headless transcript capture and one resume check. There is no
> free local rig for Cursor.

---

## CLI → warden interface mapping

| warden method        | Cursor invocation                                  | Notes |
|----------------------|----------------------------------------------------|-------|
| `LaunchCmd` (TUI)    | `cursor-agent [--model <m>] [mode flag]`           | Interactive TUI; prompt seeded as a trailing positional arg. |
| `LaunchPromptArg`    | ` "$(cat <file>)"`                                 | cursor-agent takes the first task as a positional `[prompt...]` (like Claude/Codex). |
| `ResumeCmd`          | `cursor-agent --continue [--model <m>]`            | Dir/workspace-scoped; Cursor continues the latest session for the workspace. |
| `HeadlessCmd`        | `cursor-agent -p --force --trust <prompt>`         | One-shot for warden's classify/summarize offload. |
| `TranscriptPath`     | — (degraded, returns false)                        | Interactive transcript is an unreadable SQLite `store.db` (see below). |
| `ParseTranscript`    | parses `--output-format stream-json` NDLJSON       | Real + tested, but **not wired** today (no on-disk NDJSON for the TUI). |
| `SystemPromptFlag`   | — (unsupported)                                    | Cursor has no `--append-system-prompt` flag. |
| `Pricing`            | — (unsupported)                                    | Hosted plan; tokens surfaced, dollars never are. |
| `DetectState` / `ParseApproval` | — (degraded)                            | TUI approval / trust prompts not yet mapped. |

### Permission / execution modes

Cursor exposes a richer approval surface than a simple prompt/auto toggle, and warden
surfaces it honestly in `PermissionModes` = `default | plan | ask | auto-review |
force`:

| warden mode   | Cursor flag        | Meaning |
|---------------|--------------------|---------|
| `default`     | *(none)*           | Cursor's own interactive approval posture (allowlist) is kept. |
| `plan`        | `--mode plan`      | Read-only planning: analyze and propose, no edits. |
| `ask`         | `--mode ask`       | Read-only Q&A for explanations. |
| `auto-review` | `--auto-review`    | "Smart Auto": a server classifier auto-runs safe tool calls, prompts for the rest. |
| `force`       | `-f` / `--yolo`    | Run everything unless explicitly denied. |

warden's Claude-flavored "just do it" aliases (`dangerously-skip-permissions`,
`bypassPermissions`, `yes-always`, `auto`, `acceptEdits`, `dontAsk`, `yolo`) fold onto
`-f`. Cursor also has `--sandbox enabled|disabled`, `--approve-mcps`, and `--trust`;
these are not mapped into `PermissionModes` yet (see gaps).

---

## ⚠️ Double-worktree hazard (Cursor's own `-w/--worktree`)

cursor-agent ships its **own** worktree feature: `-w/--worktree [name]` creates an
isolated git worktree at `~/.cursor/worktrees/<reponame>/<name>` (with
`--worktree-base` and `.cursor/worktrees.json` setup scripts).

**warden already manages the git worktree.** This adapter therefore **never** passes
`-w/--worktree`; it launches cursor-agent directly in warden's worktree dir (the tmux
pane is already `cd`'d there). Using both would nest a Cursor worktree inside warden's
worktree — duplicated checkouts, a branch warden doesn't track, and commits landing
somewhere warden can't see. A regression test (`TestCursorLaunchNoOwnWorktree`) asserts
the launch command contains no `-w`/`--worktree`.

If a per-agent path override is ever needed, the safe flag is `--workspace <path>`
(point it at warden's worktree) — **not** `--worktree`.

---

## Transcript storage & format

### What's on disk (the interactive path — unreadable, degraded)

A Cursor session persists to a **per-chat SQLite database**:

```
~/.cursor/chats/<md5(workspacePath)>/<chatId>/store.db   # the conversation (SQLite)
~/.cursor/chats/<md5(workspacePath)>/<chatId>/meta.json  # {schemaVersion, hasConversation, …}
```

- The top dir is `md5(workspacePath)` (verified exactly — e.g.
  `md5("/home/srjn45/dev/warden") = f68271cb…`).
- `<chatId>` **is** the minted session id (the same UUID returned as `session_id` in
  headless output).
- The conversation itself lives in `store.db` — an **undocumented binary SQLite
  schema**.

warden **cannot source this minimally**: there is **no `export` command** in
cursor-agent (cf. `opencode export <id>`, which is how the OpenCode adapter avoids
touching SQLite), there is **no `sqlite3` binary** on the rig, and warden carries **no
SQLite dependency**. So `TranscriptPath` returns `false` and `StructuredTranscript` is
**off** — `wd digest` shows "no transcript" for Cursor agents rather than guessing.

### What's parseable (the headless path — implemented, not wired)

The richest *structured* surface is `cursor-agent -p --output-format stream-json`, an
**NDJSON event stream**, one record per line with a top-level `type`:

- `system`/`init` — `cwd`, `session_id`, `model`, `permissionMode`.
- `user` — `message.role`/`message.content[].text` (the human prompt).
- `tool_call` (`subtype: started | completed`) — the call is keyed by a single
  `<name>ToolCall` field (e.g. `editToolCall`, `shellToolCall`); `<name>` is the tool,
  and `.args.path` is the touched file. Carries `timestamp_ms`.
- `assistant` — `message.content[].text` (the model's reply).
- `result` — final `result` text + a `usage` block
  (`inputTokens`/`outputTokens`/`cacheReadTokens`/`cacheWriteTokens`).

warden's `ParseTranscript` implements this format and is **tested against a real
captured fixture** (`testdata/cursor/stream-toolcall.jsonl`) — user prompt, edit tool
call with its file, and the closing reply all normalize into neutral Turns. **But it
is not wired into the live digest path today**, because warden launches Cursor's
*interactive* TUI, which writes only `store.db`, never this NDJSON. The parser is
forward-compat: the day warden gains a `store.db` reader (or captures the headless
stream-json) the backend flips to **Tier A** with no parsing work left.

---

## Capability table

| Capability             | Value | Detail |
|------------------------|-------|--------|
| `Headless`             | ✅    | `cursor-agent -p` (`--output-format text\|json\|stream-json`). |
| `Resume`               | ✅    | `cursor-agent --continue` (dir/workspace-scoped); exact-id once discover-then-pin lands. |
| `ModelSelection`       | ✅    | `--model <m>` (see `--list-models`). |
| `StructuredTranscript` | ❌    | interactive transcript is an unreadable SQLite `store.db` (**Tier C**). |
| `PermissionModes`      | ✅    | `default`, `plan`, `ask`, `auto-review`, `force` (Cursor's native vocabulary). |
| `SessionIDControl`     | ❌    | Cursor mints its own UUID chatId; no flag to assign one at launch. |
| `SystemPromptInject`   | ❌    | no `--append-system-prompt` equivalent on the launch command. |
| `Pricing`              | ❌    | hosted plan; tokens are in the stream, dollars are never surfaced. |

---

## What works vs. what warden can't do yet

**Works today**

- Launch the Cursor TUI in a warden-managed tmux session.
- Seed the agent's first task as the launch prompt (positional).
- Resume the agent (workspace-scoped `--continue`) for rotate/handoff — **verified**:
  `--continue` resumed the same chatId and recalled the prior turn's context.
- Headless one-shots via `cursor-agent -p --force --trust`.
- Map Cursor's plan / ask / auto-review / force approval modes into warden modes.

**Gaps (degraded, documented — not mis-handled)**

- **No structured transcript / digests.** The interactive transcript is an
  undocumented SQLite `store.db` with no export command and no SQLite dep in warden, so
  `TranscriptPath` degrades and `StructuredTranscript` is off. The stream-json parser
  is implemented and tested but unwired (no on-disk NDJSON for the TUI). Forward path:
  a `store.db` reader or a headless-capture launch mode.
- **No session-id pinning.** Cursor assigns its own UUID chatId and exposes no launch
  flag to set one, *and* that id is the same shape as warden's placeholder, so warden
  cannot tell them apart — resume is therefore workspace-scoped `--continue` (every
  warden agent lives in its own worktree, so this is unambiguous). The minted id *is*
  observable (the stream's `system`/init `session_id`, and the on-disk chat dir name);
  the forward path is *discover-then-pin* → exact-id `--resume <chatId>` (#52).
- **No live state / approval detection.** Cursor's run-state, its interactive approval
  UI, *and* its **workspace-trust prompt** live in the TUI; no stable pane marker was
  captured for this phase, so `DetectState` returns `Unknown` and `ParseApproval`
  returns `false` (warden infers idle from staleness, as for Claude/Codex/OpenCode).
- **Workspace-trust prompt at launch.** A fresh warden worktree is an untrusted
  directory, so the *interactive* launch shows a one-time "Do you trust the contents of
  this directory?" prompt. cursor-agent's `--trust` flag **only works in `--print`/
  headless mode**, so it can't pre-clear the TUI prompt; the headless path passes
  `--trust`, but an interactive agent must answer it once (or the workspace be
  pre-trusted via `~/.cursor/projects/<…>/.workspace-trusted`). Pre-seeding trust is
  out of scope for this minimal adapter; tracked as a gap.
- **No warden-side dollar pricing.** The stream-json `result.usage` carries token
  counts, but warden's spend table is Claude-specific and Cursor never surfaces a
  per-call dollar figure (billing is the user's subscription); spend shows tokens,
  savings omits the agent (design §5).
- **No system-prompt injection.** warden's pipeline/collab/git hints aren't appended
  for Cursor agents (no flag). Cursor rules / `AGENTS.md` could carry this later.

**$0-local viability:** ❌ N/A. Cursor is a hosted plan; there is no free local rig.

---

## Cursor-specific superpowers worth preserving (don't lowest-common-denominator)

Cursor ships capabilities Claude Code doesn't, and warden's job is to keep them
reachable, not flatten them away. Future enhancements should surface, not suppress:

- **Rich model catalog with parameterized models** — `--model` accepts bracket
  overrides, e.g. `claude-opus-4-8[context=1m,effort=high,fast=false]`; `--list-models`
  enumerates the account's models (Composer, GPT-5.x/Codex, Claude Opus families).
- **`--auto-review` (Smart Auto)** — a server-side classifier that auto-runs safe tool
  calls and prompts only for the risky ones; a finer-grained approval posture than
  warden's binary prompt/skip. Already mapped into `PermissionModes`.
- **First-class sandboxing** — `--sandbox enabled|disabled` with network-access config;
  could become a richer per-agent posture alongside the approval modes.
- **`create-chat` / `ls` / `resume`** — Cursor can mint an empty chat and return its id
  (`create-chat`), and list/resume sessions; a clean hook for *discover-then-pin* (mint
  the id up front, then pin it for exact-id transcript + resume).
- **`mcp` management & `--approve-mcps`** — Cursor manages MCP servers and can
  auto-approve them; relevant to warden's own MCP surface.
- **Cloud `worker`** — `cursor-agent worker` runs agents in your environment connected
  to Cursor; a different execution topology warden could orchestrate.
- **Plugins & rules** — `--plugin-dir`, `generate-rule`, and `~/.cursor/skills-cursor/`
  skills; a path to per-agent behavior without warden hardcoding it.
- **`--output-format json|stream-json`** — structured headless output (with a `usage`
  token block) useful for warden's classify/summarize offload and a future Tier-A
  transcript capture.

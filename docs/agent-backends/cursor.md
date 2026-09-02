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
- **Verified against:** `cursor-agent` 2026.08.31-4057e58, hosted plan (logged in via
  `cursor-agent login`); live `--list-models` is 214 ids. The committed catalog in
  `internal/backendstore/seed.go` is that live menu (not a boot-time scrape).
  Auto-assign faces: Grok 4.6 Fast + Opus 5 thinking (tier-1), Grok 4.5 + `auto` +
  Sonnet 5 thinking (tier-2), Composer 2.5 Fast + Gemini 3.7 Flash (tier-3).

> **Hosted, not $0-local.** Unlike the Codex/OpenCode/Ollama rigs, Cursor is billed
> against the user's Cursor subscription. This adapter was verified with the **minimum**
> live invocations: one headless transcript capture and one resume check. There is no
> free local rig for Cursor.

---

## CLI → warden interface mapping

| warden method        | Cursor invocation                                  | Notes |
|----------------------|----------------------------------------------------|-------|
| `LaunchCmd` (TUI)    | `cursor-agent [--model <m>] [mode flag]`           | Interactive TUI; task prompt seeded after launch (see `PromptText`). |
| `LaunchPromptArg`    | `""` (empty)                                       | The trailing positional only *populates* cursor-agent's composer — it does not auto-submit — so nothing goes on the launch line. |
| `PromptText`/`ReadyMarker` | types the task + Enter once the composer is ready | `PromptSeeder` (like Aider/Goose): warden types the prompt into the interactive composer and submits it once the fresh-launch placeholder appears. |
| `ResumeCmd`          | `cursor-agent --continue [--model <m>]`            | Dir/workspace-scoped; Cursor continues the latest session for the workspace. |
| `HeadlessCmd`        | `cursor-agent -p --force --trust <prompt>`         | One-shot for warden's classify/summarize offload. |
| `TranscriptPath`     | — (degraded, returns false)                        | Interactive transcript is an unreadable SQLite `store.db` (see below). |
| `ParseTranscript`    | parses `--output-format stream-json` NDLJSON       | Real + tested, but **not wired** today (no on-disk NDJSON for the TUI). |
| `SystemPromptFlag`   | — (unsupported)                                    | Cursor has no `--append-system-prompt` flag. |
| `InjectContext`      | writes `<workdir>/AGENTS.md`                       | warden's collab/git/pipeline addendum is delivered via the AGENTS.md rules file cursor-agent reads on startup (the no-flag fallback). |
| `Pricing`            | — (unsupported)                                    | Hosted plan; tokens surfaced, dollars never are. |
| `DetectState`        | working / idle / needs-input                       | TUI pane markers (`ctrl+c to stop`; composer placeholders; approval/trust menus) — see State & approval detection below. |
| `ParseApproval`      | command-allowlist + workspace-trust prompts        | Both interactive menus normalize into a neutral `Approval`; surfaced in warden's approvals inbox. |

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
**off** — `wd agent digest` shows "no transcript" for Cursor agents rather than guessing.

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

### Transcript-topology decision (resolved — spec §8 Q1)

The perfecting-phase design (`docs/superpowers/specs/2026-06-28-t1-backend-perfecting-design.md`
§5.4 / §8 Q1) named an open choice for closing cursor's transcript gap:
**interactive-but-untranscribed** vs **headless-but-transcribed** vs a dual mode. The
`stream-json` NDJSON above is emitted only by the headless `-p` path, *not* by the
interactive TUI — so "headless-capture" is a genuinely different launch topology, not a
free upgrade.

**Decision: keep cursor interactive (attachable), Tier C, and defer the
headless-transcribed mode.** An attachable interactive cursor session is exactly the
capability an operator wants, and a headless-only transcribed launch would *strip* it —
which violates warden's core principle (**add capability on top of the agent, never
restrict it to a lowest common denominator**). So cursor stays interactive. Live
**state + approval/trust detection** (below) already give warden eyes on the agent
without a transcript, so Tier C is no longer "blind." The already-built `stream-json`
parser remains in place, waiting for an *additive* unlock — a future `store.db` reader
or an opt-in headless-capture launch mode — at which point cursor flips to Tier A with
**no regression** to the interactive session operators rely on today.

---

## State & approval detection (live pane markers)

warden classifies a Cursor agent from its captured tmux pane (`DetectState`) and
normalizes its blocking prompts into a neutral `Approval` (`ParseApproval`). The
markers below were captured live against `cursor-agent 2026.06.26-7079533`
(interactive TUI, default approval posture) and are pinned as fixtures under
`testdata/cursor/` (`state-working.txt`, `state-idle.txt`, `state-idle-after-turn.txt`,
`approval.txt`, `trust-prompt.txt`).

**State markers**

- **Working** — while a turn streams, the composer prompt line carries a right-aligned
  `ctrl+c to stop` hint (the spinner text above it varies: `⠘⠤ Composing`,
  `⠠⠛ Running  N tokens`). That hint is the reliable marker.
- **Idle** — an empty composer shows one of Cursor's placeholders: `Plan, search,
  build anything` (fresh launch) or `Add a follow-up` (after a turn). Because the
  follow-up placeholder is also shown mid-turn, `DetectState` tests `ctrl+c to stop`
  (working) *before* idle.
- **Needs-input** — an open command-allowlist menu or the workspace-trust prompt
  (`ParseApproval` returns a match).
- Anything else ⇒ `Unknown` (no false positives; warden falls back to staleness).

**Command-allowlist approval.** When a shell command isn't on the allowlist Cursor
blocks with:

```
 $  echo hello-from-cursor in .

 Run this command?
 Not in allowlist: echo
  → Run (once) (y)
    Add Shell(echo) to allowlist? (tab)
    Run Everything (shift+tab)
    Skip (esc or n)
```

`ParseApproval` reads the `$ <command>` Action (stripping Cursor's trailing
` in <cwd>` hint), the `Run this command?` Question, and the four options top-down
(1-indexed, key hints kept, the `→` cursor → `SelectedIdx`). The least-privilege
affirmative is `Run (once)` (non-sticky); `Add … to allowlist` / `Run Everything` are
standing grants (sticky). The Question gate keeps a lone parenthesized composer prompt
(e.g. `Reason for rejection (…)`) from being mis-read as a menu. *(File edits inside
the workspace are auto-applied in the default posture and do not raise this menu, so
the command case is the representative approval shape.)*

**Workspace-trust prompt — a 1-time manual step, not a launch blocker.** A fresh
warden workspace is an untrusted directory, so the *interactive* launch shows a one-time
box:

```
│  ⚠ Workspace Trust Required                    │
│  Do you trust the contents of this directory?  │
│    /path/to/workdir                            │
│  ▶ [a] Trust this workspace                    │
│    [q] Quit                                    │
```

`cursor-agent`'s `--trust` flag only works in `--print`/headless mode, so it can't
pre-clear the *TUI* prompt. Per the maintainer's ruling this is **not a blocker**:
every agent asks for trust at least once, and real warden users already have trust
granted for their projects — if warden can't auto-clear it, the operator answers it
once and it's done. warden makes that one answer easy: `ParseApproval` recognizes the
trust box and surfaces it as an `Approval` (Question `Do you trust the contents of this
directory?`, options `Trust this workspace` / `Quit`, the trusted directory as the
Action), so the operator clears it from warden's approvals inbox instead of attaching
to the pane. The affirmative is marked sticky because trusting persists to
`~/.cursor/projects/<…>/.workspace-trusted`. (The workspace can also be pre-trusted by
writing that file.)

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
| `SystemPromptInject`   | ✅ via rules file | no `--append-system-prompt` equivalent on the launch command, but warden delivers the same addendum out-of-band via the `AGENTS.md` rules file cursor-agent reads on startup (`InjectContext`). The Caps flag stays `false` — it tracks the *launch flag* specifically. |
| `Pricing`              | ❌    | hosted plan; tokens are in the stream, dollars are never surfaced. |
| `Usage limits`         | ✅    | `warden usage` maps Included / Auto / API bars from dashboard `GetCurrentPeriodUsage` (Bearer from `~/.config/cursor/auth.json`). Not a `cursor-agent usage` verb; the TUI `/usage` pager is not scraped. |

---

## What works vs. what warden can't do yet

**Works today**

- Launch the Cursor TUI in a warden-managed tmux session.
- Seed the agent's first task as the launch prompt (positional).
- Resume the agent (workspace-scoped `--continue`) for rotate/handoff — **verified**:
  `--continue` resumed the same chatId and recalled the prior turn's context.
- Headless one-shots via `cursor-agent -p --force --trust`.
- Map Cursor's plan / ask / auto-review / force approval modes into warden modes.
- **Three usage windows** on `warden usage` (Included / Auto / API) from the same
  dashboard RPC the CLI's feature-flagged `/usage` pager uses. Composer and
  `cursor-grok-*` share Included; `auto` is its own bar; everything else is API.
  A 100% API bar does not hide Grok/Composer. HTTP failure after a successful
  login still emits the three rows with null percents (not `unsupported`).
- **Live state detection** (`DetectState`) — classify the TUI pane as working / idle /
  needs-input from stable pane markers (see below). Wired into the poller (#52 core
  seam #1), so warden no longer infers idle purely from staleness for Cursor agents.
- **Approval + workspace-trust detection** (`ParseApproval`) — Cursor's interactive
  command-allowlist menu *and* its one-time workspace-trust prompt normalize into a
  neutral `Approval`, so both surface in warden's approvals inbox and can be answered
  without attaching to the pane (auto-approve keys off `Fingerprint(Options)`).

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
- **No warden-side dollar pricing.** The stream-json `result.usage` carries token
  counts, but warden's spend table is Claude-specific and Cursor never surfaces a
  per-call dollar figure (billing is the user's subscription); spend shows tokens,
  savings omits the agent (design §5).
- ~~**No system-prompt injection.**~~ **Resolved** — warden delivers its
  pipeline/collab/git hints by writing them into the `AGENTS.md` rules file
  cursor-agent reads at the project root on startup (`InjectContext`, the shared
  rules-file injector in `inject.go`; same no-clobber / idempotent /
  git-`info/exclude` semantics as Codex). The `SystemPromptInject` Caps flag stays
  `false` because it tracks a *launch-time* flag specifically, which cursor-agent
  still lacks.

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

---

## Superpowers surfaced (#52, step 6)

Two of the superpowers above are now actually wired through warden, not just noted:

- **Live model menu via `wd backend model`** — Cursor implements `agentbackend.ModelLister`
  (`ListModels` runs `cursor-agent --list-models` and parses the `<id> - <Display
  Name>` lines down to ids), so `wd backend model --backend cursor` prints the account's live
  catalog — Composer, GPT-5.x/Codex, Claude Opus/Sonnet, Gemini, Grok, … — one id per
  line (`--json` for an array). These are the exact ids `--model` accepts (including
  parameterized forms like `claude-opus-4-8[context=1m,effort=high,fast=false]`). The
  verb is generic: it type-asserts `ModelLister`, so cursor lit up with no CLI change.
  `--list-models` is a metadata read ("List available models and exit") — it starts no
  chat and spends none of the hosted plan's generation allowance.
- **`--auto-review` (Smart Auto) as a permission mode** — already mapped: warden's
  `auto-review` permission mode emits `--auto-review` (see the modes table above and
  `Capabilities().PermissionModes`), so a server-side classifier auto-runs safe tool
  calls and prompts only for the risky ones — a finer-grained posture than warden's
  binary prompt/skip.

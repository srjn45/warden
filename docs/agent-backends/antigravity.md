# Antigravity CLI backend (experimental)

warden's adapter for **Google's Antigravity CLI** (the `agy` binary), added
breadth-first under #52. This is a thin, honest adapter: warden opens a tmux session
and launches `agy`, sources `agy`'s plaintext trajectory log, and **documents the
gaps** rather than pretending Antigravity is a perfect drop-in for Claude Code.
warden adds capability *on top of* Antigravity; it never strips its features down to
a lowest common denominator.

- **Adapter:** `internal/agentbackend/backends/antigravity.go`
- **Tests / fixtures:** `antigravity_test.go`, `testdata/antigravity/`
- **Tier:** **A** (structured transcript → digests run on real data)
- **Backend id:** `antigravity` (the `--backend` value; the binary stays `agy`)
- **Verified against:** `agy` v1.0.13, hosted free tier (Gemini 3.5 Flash), Linux

> **Hosted, quota-limited.** Antigravity is a Google-hosted agent on the user's free
> tier (a daily-ish cap), **not** a $0-local backend. This adapter was verified with
> the *minimum* live spend: one `agy -p` (capture a transcript fixture) and one
> `agy -c -p` (verify resume) for the transcript work, plus two interactive turns on
> the cheapest model (`Gemini 3.5 Flash (Low)`) to capture the idle / working /
> approval pane fixtures for `DetectState` + `ParseApproval`. Everything else was
> learned from `agy --help`, `agy models`, the bundled `antigravity_guide` skill docs,
> and on-disk inspection.

---

## CLI → warden interface mapping

| warden method        | `agy` invocation                                              | Notes |
|----------------------|--------------------------------------------------------------|-------|
| `LaunchCmd` (TUI)    | `agy [--model <m>] [--sandbox \| --dangerously-skip-permissions]` | Interactive TUI; prompt seeded via `-i`. |
| `LaunchPromptArg`    | ` -i "$(cat <file>)"`                                         | `-i`/`--prompt-interactive`: run the first task, then stay interactive (persistent loop, like Claude). |
| `ResumeCmd`          | `agy -c [--model <m>] [posture flag]`                         | Dir-scoped; `-c` continues the most recent conversation for the workspace. |
| `HeadlessCmd`        | `agy --dangerously-skip-permissions -p <prompt>`             | One-shot print for warden's classify/summarize offload. |
| `TranscriptPath`     | reads `~/.gemini/antigravity-cli/brain/<conv-id>/.system_generated/logs/transcript.jsonl` | conv-id resolved dir-scoped via `cache/last_conversations.json`. |
| `ParseTranscript`    | parses the trajectory JSONL `USER_INPUT` / `PLANNER_RESPONSE` records | → neutral Turns. |
| `SystemPromptFlag`   | — (unsupported)                                              | `agy` has no `--append-system-prompt` flag. |
| `Pricing`            | — (unsupported)                                              | Google-hosted free tier; tokens shown in `/usage` TUI only, dollars not surfaced. |
| `DetectState`        | classify the TUI status bar                                  | `? for shortcuts` ⇒ idle, `esc to cancel` / `Generating...` ⇒ working, a permission menu ⇒ needs-input. |
| `ParseApproval`      | parse the `Do you want to proceed?` permission menu          | numbered options (`Yes` / `Yes, and always allow …` / `No`) → neutral `Approval`. |

### Models & permissions

`agy models` lists the available models (`Gemini 3.5 Flash (Low/Medium/High)`,
`Gemini 3.1 Pro (Low/High)`, `Claude Sonnet 4.6 (Thinking)`, `Claude Opus 4.6
(Thinking)`, `GPT-OSS 120B (Medium)`, …); the config default is `gemini-3.5-flash`.
warden passes the model verbatim via `--model` and omits it when empty so `agy`'s own
default applies.

`agy` exposes two boolean posture flags on the launch command — `--sandbox`
(restricted terminal) and `--dangerously-skip-permissions` (auto-approve every tool).
warden surfaces these as `PermissionModes = [default, sandbox,
dangerously-skip-permissions]`; the richer Claude-flavored modes
(`bypassPermissions`, `yes-always`, `acceptEdits`, …) fold onto
`--dangerously-skip-permissions`. The finer-grained `toolPermission` settings
(`always-proceed` / `request-review` / `strict` / `proceed-in-sandbox`) live in
`settings.json`, not on the CLI, so they are not mapped to launch flags here.

---

## Transcript storage & format

This was the central investigation. `agy` keeps a conversation's **durable** store in
forms warden can't read:

- `~/.gemini/antigravity-cli/implicit/<uuid>.pb` — a high-entropy proto blob; an
  entropy check (`gzip` larger than the original) confirms it is **encrypted**.
- `~/.gemini/antigravity-cli/conversations/<conv-id>.db` — a per-conversation SQLite
  DB. The schema is readable (`steps`, `trajectory_meta`, `gen_metadata`, …) but the
  `steps.step_payload` column is an opaque **proto blob**.
- There is **no `export` / `dump` CLI verb** (subcommands are only `changelog`,
  `help`, `install`, `models`, `plugin`, `update`), and `/copy` / `/diff` are
  **TUI-only**.

…but `agy` *also* writes a **plaintext JSONL trajectory log** that warden parses:

```
~/.gemini/antigravity-cli/brain/<conv-id>/.system_generated/logs/transcript.jsonl
```

(There is a sibling `transcript_full.jsonl`; warden uses `transcript.jsonl`.)

It is **JSONL**, one record per line:

```json
{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-06-28T10:04:18Z","content":"<USER_REQUEST>\n…\n</USER_REQUEST>…"}
{"step_index":2,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-06-28T10:04:18Z","content":"WARDEN_FIXTURE_OK"}
```

Fields: `step_index`, `source` (`USER_EXPLICIT` / `MODEL` / `SYSTEM`), `type`,
`status`, `created_at` (RFC3339), `content`.

warden maps:

- `USER_INPUT` (source `USER_EXPLICIT`) → a **user** Turn. `agy` wraps the human
  prompt as `<USER_REQUEST>…</USER_REQUEST>` and appends `<ADDITIONAL_METADATA>` /
  `<USER_SETTINGS_CHANGE>` blocks; the adapter unwraps the request body and drops the
  metadata.
- `PLANNER_RESPONSE` (source `MODEL`) → an **assistant** Turn.
- `CONVERSATION_HISTORY`, `CHECKPOINT`, `SYSTEM_MESSAGE` (source `SYSTEM`) — context,
  truncation summaries, and injected system notes → **ignored** (control metadata).

### Locating the conv-id (dir-scoped)

`agy` mints its own UUID conversation id and maintains a `{workspace -> conv-id}` map
at `~/.gemini/antigravity-cli/cache/last_conversations.json`. warden cannot assign the
id, so `TranscriptPath` reads that map to find the conv-id for the agent's worktree,
then opens that conversation's `transcript.jsonl`. Because every warden agent runs in
its own git worktree, this resolution is unambiguous.

---

## Capability table

| Capability             | Value | Detail |
|------------------------|-------|--------|
| `Headless`             | ✅    | `agy -p` (one-shot print). |
| `Resume`               | ✅    | `agy -c` (dir-scoped, workspace-scoped); exact-id once discover-then-pin lands. |
| `ModelSelection`       | ✅    | `--model <m>` (`agy models` to list). |
| `StructuredTranscript` | ✅    | plaintext trajectory JSONL → neutral Turns (**Tier A**). |
| `PermissionModes`      | ✅    | `default`, `sandbox`, `dangerously-skip-permissions` (`agy`'s native posture flags). |
| `SessionIDControl`     | ❌    | `agy` mints its own UUID conv-id; no flag to assign one at launch. |
| `SystemPromptInject`   | ❌    | no `--append-system-prompt` equivalent on the launch command. |
| `Pricing`              | ❌    | Google-hosted free tier; tokens in `/usage` TUI, dollars not wired into warden spend. |

---

## What works vs. what warden can't do yet

**Works today**

- Launch the Antigravity TUI in a warden-managed tmux session.
- Seed the agent's first task as the launch prompt (`-i`, stays interactive).
- Resume the agent (dir-scoped `agy -c`) for rotate/handoff — **verified**: `-c`
  reused the same conv-id and the model recalled the prior turn's context.
- Headless one-shots via `agy -p`.
- **Digests** — the trajectory parses into structured Turns (Tier A): warden sees the
  human prompts, the model replies, and their timestamps.
- **Live state detection** — `DetectState` classifies `agy`'s TUI status bar (captured
  live against `agy` v1.0.13, Gemini 3.5 Flash): the `? for shortcuts` footer at rest
  ⇒ **idle**, the `esc to cancel` footer / `Generating...` spinner during a turn ⇒
  **working**, and an open permission menu ⇒ **needs-input**. Anything unrecognized
  stays `Unknown` so warden falls back to staleness (no false positives).
- **Approval detection + auto-approve** — `ParseApproval` maps `agy`'s
  `Do you want to proceed?` permission menu into warden's neutral `Approval` (the
  proposed command, the numbered `Yes` / `Yes, and always allow …` / `No` options, the
  highlighted option, and the least-privilege affirmative). This lights up the
  approvals inbox and auto-approve for Antigravity agents. Captured under the **default**
  permission posture — `--dangerously-skip-permissions` raises no prompt, and the
  `agy -p` headless path is non-interactive.

**Gaps (degraded, documented — not mis-handled)**

- **No tool-call / files-changed extraction.** The captured fixture was a text-only
  conversation, so `agy`'s tool-step record format is **unverified**. Rather than
  guess field names, `ParseTranscript` leaves `ToolName`/`Files` empty — the digest's
  "what changed" column degrades for Antigravity agents. Capturing a tool-using
  transcript and wiring tool/file extraction is the obvious next step (it would cost
  live quota, deferred from this frugal phase).
- **No session-id pinning.** `agy` assigns its own UUID and exposes no launch flag to
  set one. Worse than the other backends, warden's *own* placeholder session id is
  also a UUID, so it is **indistinguishable** from a real `agy` conv-id (unlike
  OpenCode's `ses_` prefix) — so the adapter never trusts the passed id and resolves
  transcript + resume **dir-scoped**. The forward path is *discover-then-pin*: read
  the minted conv-id from `cache/last_conversations.json` after first launch and use
  exact-id `agy --conversation <uuid>` / direct `brain/<conv-id>/…` lookup
  (FUTURE_ENHANCEMENTS #52).
- **Approval coverage is shell-command-only (so far).** `DetectState` + `ParseApproval`
  are wired (see "Works today"), but the captured permission fixture was a **shell
  command** (`Bash(echo …)`). `agy`'s file-edit / MCP-tool prompts almost certainly
  reuse the same `Do you want to proceed?` + numbered-menu shape the parser keys on,
  but that was not captured this frugal phase (kept within the free-tier quota), so it
  is unverified. The header-gated, sequential-1..N parser degrades to `(nil,false)`
  rather than mis-parsing any prompt variant it has not seen.
- **No warden-side dollar pricing.** Antigravity is hosted on a Google free tier;
  `agy` shows token usage / session cost only in its `/usage` TUI panel and surfaces
  no per-call dollar figure on the CLI, and warden's spend table is Claude-specific.
  Spend shows tokens, savings omits the agent (design §5).
- **No system-prompt injection.** warden's pipeline/collab/git hints aren't appended
  for Antigravity agents (no flag). `agy`'s customization is skills/rules/AGENTS.md
  based; one of those could carry this later.

**Encrypted durable store.** Note the transcript warden reads is the *plaintext
trajectory log*; the canonical conversation store (`implicit/*.pb`,
`conversations/*.db` `step_payload`) is encrypted/proto and intentionally not relied
on. If a future `agy` build stops emitting the plaintext `transcript.jsonl`, this
adapter would need an `export`-style verb (none exists today) and would degrade to
"no transcript" until then.

---

## Antigravity-specific superpowers worth preserving (don't lowest-common-denominator)

Antigravity ships capabilities Claude Code doesn't; warden's job is to keep them
reachable, not flatten them away. Future enhancements should surface, not suppress:

- **Multi-vendor model menu** — one agent can run Gemini, Claude, *and* GPT-OSS models
  (`agy models`) under a single hosted login; a natural fit for per-agent model
  routing.
- **Projects & multi-dir workspaces** — `--project <id>` / `--new-project` /
  `--add-dir` group conversations and span multiple directories.
- **First-class skills / rules / hooks / MCP / sidecars / plugins** — `agy plugin`,
  the bundled `antigravity_guide` skill, `/skills`, `/hooks`, `/mcp`; warden could
  drive these for richer per-agent setup.
- **Conversation lifecycle in the TUI** — `/fork` (branch a thread preserving
  history), `/rewind` (checkpoint undo), `/rename`, `/resume` by id *or name*; `/fork`
  is a stronger "what-if" primitive than warden's snapshot.
- **Sandbox + terminal restrictions** — `--sandbox` / `enableTerminalSandbox`; richer
  per-agent posture could be exposed beyond the two booleans mapped today.
- **Python SDK** — `pip install google-antigravity` exposes programmatic agent
  spawning with strongly-typed `tool_calls` / `thoughts` streams; a cleaner structured
  surface than scraping the TUI, and a candidate source for the tool/file extraction
  gap above.

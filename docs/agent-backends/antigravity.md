# Antigravity CLI backend (beta)

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
- **Verified against:** `agy` v1.0.13, hosted free tier (Gemini 3.5 Flash), Linux;
  the workspace-trust prompt and the tool-call transcript re-captured against
  `agy` v1.0.16

> **Hosted, quota-limited.** Antigravity is a Google-hosted agent on the user's free
> tier (a daily-ish cap), **not** a $0-local backend. This adapter was verified with
> the *minimum* live spend: one `agy -p` (capture a transcript fixture) and one
> `agy -c -p` (verify resume) for the transcript work, plus two interactive turns on
> the cheapest model (`Gemini 3.5 Flash (Low)`) to capture the idle / working /
> approval pane fixtures for `DetectState` + `ParseApproval`. A follow-up pass
> (v1.0.16) captured the workspace-trust prompt (free — it appears before any model
> call) and spent one more request on a file-edit session to capture a tool-using
> transcript. Everything else was learned from `agy --help`, `agy models`, the
> bundled `antigravity_guide` skill docs, and on-disk inspection.

---

## CLI → warden interface mapping

| warden method        | `agy` invocation                                              | Notes |
|----------------------|--------------------------------------------------------------|-------|
| `LaunchCmd` (TUI)    | `agy [--model <m>] [--sandbox \| --dangerously-skip-permissions]` | Interactive TUI; prompt seeded via `-i`. |
| `LaunchPromptArg`    | ` -i "$(cat <file>)"`                                         | `-i`/`--prompt-interactive`: run the first task, then stay interactive (persistent loop, like Claude). |
| `ResumeCmd`          | `agy -c [--model <m>] [posture flag]`                         | Dir-scoped; `-c` continues the most recent conversation for the workspace. |
| `HeadlessCmd`        | `agy --dangerously-skip-permissions -p <prompt>`             | One-shot print for warden's classify/summarize offload. |
| `TranscriptPath`     | reads `~/.gemini/antigravity-cli/brain/<conv-id>/.system_generated/logs/transcript.jsonl` | conv-id resolved dir-scoped via `cache/last_conversations.json`. |
| `ParseTranscript`    | parses the trajectory JSONL `USER_INPUT` / `PLANNER_RESPONSE` records (incl. `tool_calls`) | → neutral Turns with tool names + files changed. |
| `SystemPromptFlag`   | — (unsupported)                                              | `agy` has no `--append-system-prompt` flag. |
| `InjectContext`      | writes `<workdir>/AGENTS.md`                                 | warden's collab/git/pipeline addendum is delivered via the AGENTS.md rules file `agy` reads on startup (the no-flag fallback). |
| `Pricing`            | — (unsupported)                                              | Google-hosted free tier; tokens shown in `/usage` TUI only, dollars not surfaced. |
| `DetectState`        | classify the TUI status bar                                  | `? for shortcuts` ⇒ idle, `esc to cancel` / `Generating...` ⇒ working, a permission menu ⇒ needs-input. |
| `ParseApproval`      | parse the `Do you want to proceed?` permission menu **and** the launch-time workspace-trust prompt | numbered options (`Yes` / `Yes, and always allow …` / `No`), or the trust prompt's `Yes, I trust this folder` / `No, exit` → neutral `Approval`. |

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
`status`, `created_at` (RFC3339), `content`. A `PLANNER_RESPONSE` that invokes a tool
carries the calls under `tool_calls` instead of content (captured live, v1.0.16):

```json
{"step_index":2,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-07-05T09:14:21Z","tool_calls":[{"name":"write_to_file","args":{"CodeContent":"\"hello from agy\\n\"","TargetFile":"\"/abs/path/hello.txt\"","Overwrite":"true","…":"…"}}]}
```

Note every `args` value is itself **JSON-encoded into the string** (a path arrives
double-quoted); the adapter decodes through that.

warden maps:

- `USER_INPUT` (source `USER_EXPLICIT`) → a **user** Turn. `agy` wraps the human
  prompt as `<USER_REQUEST>…</USER_REQUEST>` and appends `<ADDITIONAL_METADATA>` /
  `<USER_SETTINGS_CHANGE>` blocks; the adapter unwraps the request body and drops the
  metadata.
- `PLANNER_RESPONSE` (source `MODEL`) → an **assistant** Turn: its prose content when
  present, and/or its `tool_calls` — each call's `name` (`write_to_file`,
  `run_command`, …) becomes the Turn's tool, and a file-bearing call's decoded
  `TargetFile` lands in `Files` (the digest's "what changed"). Verified against the
  captured tool-using fixture: `write_to_file` extracts name + file, `run_command`
  extracts name (no file args ⇒ no Files).
- `CONVERSATION_HISTORY`, `CHECKPOINT`, `SYSTEM_MESSAGE` (source `SYSTEM`) — context,
  truncation summaries, and injected system notes → **ignored** (control metadata).
- `CODE_ACTION`, `RUN_COMMAND` (source `MODEL`) — boilerplate execution-result
  confirmations of a tool step already captured from `tool_calls` → **ignored**.

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
| `SystemPromptInject`   | ✅ via rules file | no `--append-system-prompt` equivalent on the launch command, but warden delivers the same addendum out-of-band via the `AGENTS.md` rules file `agy` reads on startup (`InjectContext`). The Caps flag stays `false` — it tracks the *launch flag* specifically. |
| `Pricing`              | ❌    | Google-hosted free tier; tokens in `/usage` TUI, dollars not wired into warden usage spend. |

---

## What works vs. what warden can't do yet

**Works today**

- Launch the Antigravity TUI in a warden-managed tmux session.
- Seed the agent's first task as the launch prompt (`-i`, stays interactive).
- Resume the agent (dir-scoped `agy -c`) for rotate/handoff — **verified**: `-c`
  reused the same conv-id and the model recalled the prior turn's context.
- Headless one-shots via `agy -p`.
- **Digests** — the trajectory parses into structured Turns (Tier A): warden sees the
  human prompts, the model replies, their timestamps, **and the tool calls / files
  changed** (`tool_calls` on `PLANNER_RESPONSE`; verified against a captured
  tool-using fixture, v1.0.16).
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
- **Workspace-trust prompt surfaced as an approval** — when `agy` launches in a
  directory it has not trusted (every fresh warden workspace), it blocks on a
  `Do you trust the contents of this project?` prompt **before any model call**.
  `ParseApproval` normalizes it (the directory under question as the Action, the
  `Yes, I trust this folder` / `No, exit` options, sticky affirmative — trusting
  persists for the folder) so it reaches the approvals inbox instead of silently
  stalling the agent. Captured live (v1.0.16).

**Gaps (degraded, documented — not mis-handled)**

- ~~**No tool-call / files-changed extraction.**~~ **Resolved** (v1.0.16) — a live
  file-edit session was captured as a fixture, revealing the tool-step format:
  `tool_calls` on `PLANNER_RESPONSE` records, each `{name, args}` with JSON-encoded
  arg values. `ParseTranscript` now extracts the tool name and the decoded
  `TargetFile` into `ToolName`/`Files` (fixture-locked tests cover `write_to_file`
  and `run_command`). Only `TargetFile` is the live-verified file-bearing key —
  `AbsolutePath` (the same tool family's read/edit key) is accepted defensively but
  not fixture-proven.
- **No session-id pinning.** `agy` assigns its own UUID and exposes no launch flag to
  set one. Worse than the other backends, warden's *own* placeholder session id is
  also a UUID, so it is **indistinguishable** from a real `agy` conv-id (unlike
  OpenCode's `ses_` prefix) — so the adapter never trusts the passed id and resolves
  transcript + resume **dir-scoped**. The forward path is *discover-then-pin*: read
  the minted conv-id from `cache/last_conversations.json` after first launch and use
  exact-id `agy --conversation <uuid>` / direct `brain/<conv-id>/…` lookup
  (FUTURE_ENHANCEMENTS #52).
- **Approval coverage is shell-command + workspace-trust (so far).** `DetectState` +
  `ParseApproval` are wired (see "Works today") for the two captured prompts: the
  **shell-command** permission menu (`Bash(echo …)`) and the launch-time
  **workspace-trust** prompt. `agy`'s file-edit / MCP-tool prompts almost certainly
  reuse the same `Do you want to proceed?` + numbered-menu shape the parser keys on,
  but they were not captured (kept within the free-tier quota), so they are
  unverified. The header-gated parsers degrade to `(nil,false)` rather than
  mis-parsing any prompt variant they have not seen.
- **No warden-side dollar pricing.** Antigravity is hosted on a Google free tier;
  `agy` shows token usage / session cost only in its `/usage` TUI panel and surfaces
  no per-call dollar figure on the CLI, and warden's spend table is Claude-specific.
  Spend shows tokens, savings omits the agent (design §5).
- ~~**No system-prompt injection.**~~ **Resolved** — warden delivers its
  pipeline/collab/git hints by writing them into the `AGENTS.md` rules file `agy`
  reads from the active directory on startup (`InjectContext`, the shared
  rules-file injector in `inject.go`; same no-clobber / idempotent /
  git-`info/exclude` semantics as Codex). The `SystemPromptInject` Caps flag stays
  `false` because it tracks a *launch-time* flag specifically, which `agy` still
  lacks.

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
  routing. **Surfaced (step 6 PR-C):** `wd backend model` exposes this live menu via the
  additive `agentbackend.ModelLister` interface — the adapter runs `agy models` and
  normalizes its one-id-per-line stdout into the ids you pass to `--model` (e.g.
  `Gemini 3.5 Flash (Low)`, `Claude Opus 4.6 (Thinking)`). Listing is a metadata read,
  not a generation request, so it spends no hosted-tier quota; on a command error
  (binary missing / not signed in) `ListModels` returns `ok=false` and the verb
  degrades with a clear message. Backends with a static model set (Claude) don't
  implement the interface and aren't offered the verb.
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

# Codex CLI backend (experimental)

warden's adapter for **OpenAI's Codex CLI** (the `codex` binary), added breadth-first
under #52. This is a thin, honest adapter: warden opens a tmux session and launches
Codex, sources Codex's session transcript, and **documents the gaps** rather than
pretending Codex is a perfect drop-in for Claude Code. warden adds capability *on
top of* Codex; it never strips Codex's features down to a lowest common denominator.

- **Adapter:** `internal/agentbackend/backends/codex.go`
- **Tests / fixtures:** `codex_test.go`, `testdata/codex/`
- **Tier:** **A** (structured transcript → digests run on real data)
- **Verified against:** `codex` v0.142.3, $0-local (Ollama `qwen2.5-coder:3b`)

---

## CLI → warden interface mapping

| warden method        | Codex invocation                                                            | Notes |
|----------------------|------------------------------------------------------------------------------|-------|
| `LaunchCmd` (TUI)    | `codex [-m <model>] [-s <sandbox>] [-a never]`                               | Interactive TUI; prompt seeded as a trailing positional arg. |
| `LaunchPromptArg`    | ` "$(cat <file>)"`                                                            | Codex takes the first task as a positional PROMPT (like Claude). |
| `ResumeCmd`          | `codex resume --last`                                                         | Dir-scoped; Codex filters resume by cwd by default. |
| `HeadlessCmd`        | `codex exec --skip-git-repo-check --dangerously-bypass-approvals-and-sandbox <prompt>` | One-shot for warden's classify/summarize offload. |
| `TranscriptPath`     | reads `$CODEX_HOME/sessions/**/rollout-*.jsonl`                               | Resolved dir-scoped (match `session_meta.cwd`). |
| `ParseTranscript`    | parses rollout JSONL `response_item` records                                 | message + function_call → neutral Turns. |
| `SystemPromptFlag`   | — (unsupported)                                                              | Codex has no `--append-system-prompt` flag. |
| `Pricing`            | — (unsupported)                                                              | OSS/BYO; tokens exposed, dollars not wired. |
| `DetectState` / `ParseApproval` | — (degraded)                                                     | TUI approval prompts not yet mapped. |

### $0-local launch (Ollama)

Codex has native OSS support. The provider is selected by Codex's config, **not** by
warden, so warden's adapter passes only `-m <model>`. For the free local rig, point
Codex at Ollama once in `~/.codex/config.toml` (or a `-p <profile>`):

```toml
model_provider = "ollama"
model = "qwen2.5-coder:3b"

[model_providers.ollama]
name = "ollama"
base_url = "http://127.0.0.1:11434/v1"
```

…then `codex` (and warden's launch) runs fully local at $0. The exact rig used to
verify this adapter:

```sh
codex exec --oss --local-provider ollama -m qwen2.5-coder:3b \
  --skip-git-repo-check --dangerously-bypass-approvals-and-sandbox --json "<prompt>"
```

> Keeping provider selection in Codex config (the "BYO config" stance shared with the
> OpenCode adapter) means the *same* adapter serves the free Ollama rig and a paid
> OpenAI-auth setup without warden picking sides.

---

## Transcript storage & format

Codex persists every session as a **rollout file**:

```
$CODEX_HOME/sessions/<YYYY>/<MM>/<DD>/rollout-<timestamp>-<uuid>.jsonl
```

(`$CODEX_HOME` defaults to `~/.codex`; `--ephemeral` skips persistence.)

It is **JSONL**, one record per line, each with a top-level `type`:

- `session_meta` — header: `session_id`, `cwd`, `model_provider`, `cli_version`, …
- `response_item` — the canonical conversation items. `payload.type` is one of
  `message` (with `role` = `user` / `assistant` / `developer`), `function_call`,
  `function_call_output`, `reasoning`, …
- `event_msg` — UI/stream events (`task_started`, `user_message`, `agent_message`,
  `token_count`, `task_complete`, `patch_apply_begin/end`, …)
- `turn_context` — per-turn settings (model, sandbox/approval policy, cwd)

warden parses the **`response_item`** records (the durable conversation log):

- `message`/`user` → a user Turn. Codex injects synthetic `<environment_context>`,
  `<permissions instructions>`, and `<skills_instructions>` blocks as user/developer
  messages; the adapter drops those so only real human prompts surface.
- `message`/`assistant` → an assistant Turn.
- `function_call` → the tool name + any touched files (parsed from an `apply_patch`
  envelope's `*** Add|Update|Delete File:` headers, or a `shell` command running
  `apply_patch`) fold onto the assistant Turn.

The `--json` event stream from `codex exec` is a *separate, also-clean* surface
(`thread.started`, `turn.completed` with a `usage` block, `item.completed`); warden
sources the rollout file rather than the live stream so it works the same for the TUI
and `exec`, and after the process exits.

---

## Capability table

| Capability             | Value | Detail |
|------------------------|-------|--------|
| `Headless`             | ✅    | `codex exec` (`--json` for JSONL events). |
| `Resume`               | ✅    | `codex resume --last` (dir-scoped); exact-id once discover-then-pin lands. |
| `ModelSelection`       | ✅    | `-m <model>`. |
| `StructuredTranscript` | ✅    | rollout JSONL → neutral Turns (**Tier A**). |
| `PermissionModes`      | ✅    | `read-only`, `workspace-write`, `danger-full-access` (Codex's native sandbox). |
| `SessionIDControl`     | ❌    | Codex mints its own UUID; no flag to assign one at launch. |
| `SystemPromptInject`   | ❌    | no `--append-system-prompt` equivalent on the launch command. |
| `Pricing`              | ❌    | OSS/BYO; tokens are in the rollout, dollars not wired into warden spend. |

---

## What works vs. what warden can't do yet

**Works today**

- Launch the Codex TUI in a warden-managed tmux session ($0-local or paid).
- Seed the agent's first task as the launch prompt.
- Resume the agent (dir-scoped `--last`) for rotate/handoff.
- Headless one-shots via `codex exec`.
- **Digests** — the rollout parses into structured Turns (Tier A): warden sees the
  prompts, the model replies, the tools called, and the files patched.

**Gaps (degraded, documented — not mis-handled)**

- **No session-id pinning.** Codex assigns its own UUID and exposes no launch flag to
  set one, so warden resolves transcript + resume **dir-scoped** (every warden agent
  lives in its own worktree, so this is unambiguous). The forward path is
  *discover-then-pin*: read the minted `session_id` from `session_meta` after first
  launch and use exact-id `codex resume <uuid>` / direct rollout lookup
  (FUTURE_ENHANCEMENTS #52).
- **No live state / approval detection.** Codex's run-state and approval prompts live
  in its TUI; no stable pane marker was captured for this phase, so `DetectState`
  returns `Unknown` and `ParseApproval` returns `false` (warden infers idle from
  staleness, as it does for Claude/Aider/OpenCode). The faithful non-interactive
  surface is `codex exec`, which raises no prompts.
- **No warden-side dollar pricing.** The rollout carries token counts
  (`token_count` events, `turn.completed.usage`), but warden's spend table is
  Claude-specific; spend shows tokens, savings omits the agent (design §5).
- **No system-prompt injection.** warden's pipeline/collab/git hints aren't appended
  for Codex agents (no flag). A `-c <key=value>` override or `AGENTS.md` could carry
  this later.

**$0-local viability:** ✅ Confirmed. Codex's native `--oss --local-provider ollama`
runs `qwen2.5-coder:3b` against a local Ollama at zero cost. Caveat observed on the
rig: 3B-class local models are too weak to reliably emit tool calls — they tend to
print a JSON blob *describing* a call instead of invoking `apply_patch`. The rollout
format is unaffected (the tool-call parser is covered by a schema-faithful fixture,
mirroring OpenCode's approach); only the model's *behavior* is limited, which is a
property of the tiny free model, not of Codex or this adapter.

---

## Codex-specific superpowers worth preserving (don't lowest-common-denominator)

Codex ships capabilities Claude Code doesn't, and warden's job is to keep them
reachable, not flatten them away. Future enhancements should surface, not suppress:

- **First-class sandboxing** — `read-only | workspace-write | danger-full-access`
  with a separate approval policy (`untrusted | on-request | never`). Already mapped
  into `PermissionModes`; richer per-agent posture could be exposed.
- **`codex apply`** — apply the agent's last produced diff to the working tree as a
  `git apply`. A natural fit for warden's review-then-land flow.
- **`codex review`** (`codex exec review`) — run a code review against the repo. Could
  back a warden review step directly.
- **`codex mcp-server` / `codex mcp`** — Codex can *be* an MCP server and consume
  external MCP servers; relevant to warden's own MCP surface.
- **`codex fork`** — branch a session to explore alternatives without losing the
  original — a stronger primitive than warden's snapshot for "what-if" runs.
- **Sessions lifecycle** — `archive` / `unarchive` / `delete` verbs for housekeeping.
- **`--output-schema` / `-o`** — structured final-output capture (JSON Schema), useful
  for warden's classify/summarize offload beyond plain text.
- **Profiles & `-c` overrides** — `~/.codex/config.toml`, `-p <profile>`,
  `-c key=value`; a clean path to per-agent provider/model/policy without warden
  hardcoding any of it.

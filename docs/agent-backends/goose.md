# Goose backend (experimental)

warden's adapter for **Goose** — Block's open-source terminal AI agent
(<https://github.com/block/goose>). This is a **breadth-first, experimental**
integration (#52): the adapter launches Goose in a tmux pane, sources its
session transcript, and **honestly documents the gaps** rather than papering
over them. warden adds capability _on top_ of Goose; it does not restrict any of
Goose's own features.

Adapter: [`internal/agentbackend/backends/goose.go`](../../internal/agentbackend/backends/goose.go)
· Backend id: `goose` · select with `--backend goose`.

---

## Install method that worked

```bash
curl -fsSL https://raw.githubusercontent.com/block/goose/main/download_cli.sh \
  | CONFIGURE=false GOOSE_BIN_DIR="$HOME/.local/bin" bash
```

- `CONFIGURE=false` skips Goose's interactive provider-setup wizard (otherwise
  the installer drops you into `goose configure`).
- The binary lands at `~/.local/bin/goose`. Verified version: **goose 1.39.0**.

### $0-local provider (Ollama)

Goose supports Ollama natively. Drive it via env (or `~/.config/goose/config.yaml`):

```bash
export GOOSE_PROVIDER=ollama
export GOOSE_MODEL=qwen2.5-coder:3b      # or llama3.2:3b
export OLLAMA_HOST=http://127.0.0.1:11434
```

**Confirmed running fully local at $0** against Ollama 0.30.10 with
`qwen2.5-coder:3b` and `llama3.2:3b` — both a headless `goose run -t` and an
interactive `goose session` completed with no paid API calls.

> **First-run gate:** a fresh Goose install shows a one-time interactive
> telemetry-consent prompt ("Share anonymous usage data?") that **blocks the
> interactive session until answered**. Answering it writes
> `GOOSE_TELEMETRY_ENABLED: <bool>` to `~/.config/goose/config.yaml`. Pre-seed
> that key (or answer once) so a warden-spawned `goose session` does not stall on
> the consent prompt. This is a setup gap, not a per-session one.

---

## CLI → warden interface mapping

| warden interface       | Goose command                                                        | Notes |
|------------------------|----------------------------------------------------------------------|-------|
| `LaunchCmd` (interactive) | `goose session --name <warden-agent-id>`                          | warden pins its own agent id as the session **name**. |
| `ResumeCmd`            | `goose session -r --name <warden-agent-id>`                          | Name-deterministic resume (falls back to bare `-r` = most-recent). |
| `HeadlessCmd`          | `goose run --no-session --quiet -t <prompt>`                         | One-shot; `--no-session` keeps it out of the store, `--quiet` prints only the model reply. |
| `TranscriptPath`       | `goose session list --format json --working_dir <dir>` → `goose session export --session-id <id> --format json` | Dir-scoped resolve, then export to JSON. |
| `ParseTranscript`      | parses the export's `conversation[]`                                 | Tier A. |
| `LaunchPromptArg`      | _(none)_                                                             | **Gap** — see below. |
| `SystemPromptFlag`     | _(none on `session`)_                                                | `goose run --system` exists; interactive `session` has no equivalent. |
| `InjectContext`        | writes `<workdir>/.goosehints`                                        | warden's collab/git/pipeline addendum is delivered via the `.goosehints` file Goose auto-loads on startup (the no-flag fallback). |

### Session identity & resume

Goose mints its own date-stamped session id (e.g. `20260628_1`) — warden cannot
assign it up front. But Goose accepts a free-form **`--name`**, which warden pins
to its own stable agent id (`sess.ID`). That makes **resume name-deterministic**
(`goose session -r --name <id>`) — no discovery step, strictly richer than
OpenCode's dir-scoped `-c` resume. Verified: a `goose run --name X -r` correctly
continued the prior session's context.

(`--session-id` and `--name` are mutually exclusive on `run`, and `--session-id`
is **resume-only**; that's why warden pins the **name** for fresh launches.)

---

## Transcript / session storage

- **Location:** `~/.local/share/goose/sessions/sessions.db` — a **SQLite**
  database (with `-wal`/`-shm`), not flat JSONL files.
- **Sourcing:** because there is no on-disk transcript file to point at, the
  adapter materializes one with `goose session export --format json` (the
  "DB query, not file read" variant of the interface) and writes it to a temp
  file for `ParseTranscript`. This keeps warden decoupled from the DB schema.
- **Format** (`--format json`): a single JSON document. Top level carries
  `id`, `working_dir`, `name`, `created_at`/`updated_at`, `provider_name`,
  `model_config`, `goose_mode`, `usage`/`accumulated_usage`/`accumulated_cost`,
  and the ordered `conversation[]`. Each message:

  ```jsonc
  { "role": "user" | "assistant",
    "created": 1782638988,                 // epoch SECONDS
    "content": [ /* parts */ ] }
  ```

  Part types the adapter reads:
  - `{"type":"text","text":"…"}` — message body.
  - `{"type":"toolRequest","id":"…","toolCall":{"value":{"name":"write","arguments":{…}}}}`
    — a tool call; the adapter takes the tool **name** and the file path from
    `arguments` (`path`/`filePath`/`filename`).
  - `{"type":"toolResponse","id":"…","toolResult":{…}}` — a tool result, which
    Goose attaches to a **`role: user`** message; carries no prompt text and is
    skipped (matching the Claude/OpenCode parsers).

Fixtures captured under
[`testdata/goose/`](../../internal/agentbackend/backends/testdata/goose/):
a real `llama3.2:3b` run with a genuine `toolRequest`/`toolResponse`
(`export-session.json`), a real `qwen2.5-coder:3b` run (`export-text.json`), and
a dir-scoped `session-list.json`.

---

## Capability table

| Capability               | Cap field              | Goose | Notes |
|--------------------------|------------------------|:-----:|-------|
| Headless one-shot        | `Headless`             | ✅    | `goose run -t`. |
| Resume by session        | `Resume`               | ✅    | Name-deterministic (`-r --name`). |
| Structured transcript    | `StructuredTranscript` | ✅    | **Tier A** — JSON export → neutral Turns. |
| Model selection (warden-driven) | `ModelSelection` | ❌    | Interactive `session` has no `--model`; config/env-driven. `goose run` does take `--model`/`--provider`. |
| Permission-mode select   | `PermissionModes`      | ⚠️    | Native modes `auto`/`approve`/`chat`/`smart_approve` are listed for reference, but they're `GOOSE_MODE` env/config — the launch command can't select one. |
| Session-id control       | `SessionIDControl`     | ❌    | Goose mints its own id; warden pins a **name** instead. |
| System-prompt inject     | `SystemPromptInject`   | ✅ via rules file | `goose session` has no launch flag (only headless `goose run --system`), but warden delivers the same addendum out-of-band via the `.goosehints` file Goose reads on startup (`InjectContext`). `SystemPromptInject` Caps stays `false` — it tracks the *launch flag* specifically. |
| Pricing / spend          | `Pricing`              | ❌    | BYO multi-provider; no warden-side rate table (Goose does track tokens/cost natively — see deferred). |
| State / approval detect  | —                      | ❌    | Degraded (Unknown / false); not yet mapped. |

---

## What works vs. what warden can't do yet

**Works today**
- Launch an interactive Goose session in a tmux pane, with a warden-owned `--name`.
- Resume that exact session deterministically by warden's agent id.
- Headless one-shot for warden's classify/summarize offload.
- Full digests: the JSON export parses into neutral Turns (text, tool names,
  edited files, timestamps) — Tier A.
- Runs $0-local against Ollama.

**Gaps (honest, breadth-first)**
- **No initial-task seeding into the interactive session.** `goose session`
  accepts no initial-prompt argument (a positional is parsed as a subcommand; it
  has no `-t`/`--message`). The only prompt-capable entry point is the headless
  `goose run -t`, which is one-shot. So a warden-spawned _managed_ Goose agent
  opens interactively but its task prompt is **not auto-typed** (it is still
  written to warden's prompt file; an operator pastes it). `LaunchPromptArg`
  returns `""` rather than break the `goose session` command.
- **No warden-driven model/provider on launch.** Interactive `session` has no
  `--model`/`--provider`; warden relies on `GOOSE_PROVIDER`/`GOOSE_MODEL`.
- **No warden-driven permission mode on launch.** `GOOSE_MODE` env/config only.
- ~~**No system-prompt injection** on the interactive launch.~~ **Resolved** —
  warden delivers its pipeline/collab/git hints by writing them into the
  `.goosehints` file Goose auto-loads from the working directory on startup
  (`InjectContext`, the shared rules-file injector in `inject.go`; same
  no-clobber / idempotent / git-`info/exclude` semantics as Codex). The
  `SystemPromptInject` Caps flag stays `false` because it tracks a *launch-time*
  flag specifically, which `goose session` still lacks.
- **State/approval detection degraded.** Goose's run-state and `approve`-mode
  prompts live in its TUI; no stable pane marker was captured this phase, so
  `DetectState` returns Unknown and `ParseApproval` returns false (idle is
  inferred from staleness — same stance as the Aider/OpenCode adapters).
- **First-run telemetry consent** can stall a fresh interactive session (see
  install note).
- **Weak $0 models emit tool calls as text.** `qwen2.5-coder:3b` frequently
  printed a tool call as fenced JSON _text_ instead of a real `toolRequest`
  (captured in `export-text.json`); `llama3.2:3b` produced genuine tool calls.
  The parser handles both — it never mis-attributes fenced text as a real tool.

---

## Goose superpowers worth preserving / wiring later

Goose is feature-rich; warden currently launches it thinly and stays out of its
way. Things to integrate _on top_ later (none stripped today):

- **Extensions / MCP.** Goose's tools are pluggable extensions (the bundled
  `developer`, `todo`, `summon`, `analyze`, …) and arbitrary MCP servers
  (`--with-extension`, `--with-streamable-http-extension`). A first-class
  capability surface worth surfacing in warden.
- **Recipes & sub-recipes.** `goose run --recipe` / `--sub-recipe` define
  reusable, parameterized agent configurations — a natural fit for warden
  pipelines.
- **Multi-provider, including $0-local.** openai / anthropic / ollama /
  databricks / gemini / claude-code and more, switchable per `goose run`.
- **Native cost & token accounting.** The export already carries
  `usage`/`accumulated_usage`/`accumulated_cost` and `model_config`; wiring
  warden's `spend`/`savings` to Goose's own numbers would lift the Pricing gap
  without a warden-side rate table.
- **`--name` / `--session-id` session control + fork.** Beyond resume, Goose can
  `--fork` a session (copy-on-resume) — useful for warden snapshots/handoff.
- **Structured output & ACP.** `goose run --output-format json|stream-json` and
  `goose acp`/`goose serve` (Agent-Client-Protocol over stdio / HTTP+WS) offer a
  programmatic drive path that could replace tmux-pane scraping entirely.
- **Containers, max-turns, max-tool-repetitions** — built-in guardrails warden
  could expose.

---

## Tier decision

**Tier A** (transcripts). Goose's SQLite store is sourced via
`goose session export --format json` into clean, parseable `conversation[]` JSON,
yielding high-fidelity neutral Turns. Resume is supported and name-deterministic.
Pricing, model/mode-on-launch, and state detection are the honest degradations
above; warden's system-prompt addendum now reaches Goose out-of-band via the
`.goosehints` file (`InjectContext`).

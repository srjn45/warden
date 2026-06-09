# agentctl Completion Digest — Design

Date: 2026-06-05

## Problem

agentctl is strong at the *front half* of the agent lifecycle — launch, steer, monitor,
talk to, pipeline, approve. The thin side is **outcomes**. You can spawn six background
agents, but learning "what did this one actually accomplish?" still means attaching to
each tmux session by hand. The existing status (`working` / `waiting` / `idle`) is coarse:
it tells you an agent is *at the prompt*, not *what it did to get there*.

A **completion digest** closes that gap: an on-demand, structured summary of what an agent
accomplished — a short narrative plus the concrete facts (files touched, branch, turns,
status) — pulled from data agentctl already has (the deterministic transcript + the agent's
cwd/worktree).

### Non-goals

- **No auto-detection of "done".** Claude Code emits no hard completion signal; an idle
  agent may be done, paused for input, or blocked. The digest is **on-demand only** — it
  never asserts an agent finished. It summarizes *where the agent is right now*.
- **No token/cost accounting.** Deliberately shelved (see the prior brainstorm) as a vanity
  metric for this setup.
- **No caching / background precomputation.** On-demand means we parse fresh each call.
  Transcripts are small; if this ever gets slow, a poller cache is a clean later addition.

## Decisions (locked during brainstorming)

| Question | Decision |
|---|---|
| When is a digest generated? | **On-demand only** (`agentctl digest <id>`, web button, TUI key). |
| How is content produced? | **Hybrid** — deterministic facts + a best-effort LLM narrative. |
| Where does it surface? | **CLI + TUI + Web** (no MCP tool in v1). |
| How are files-touched determined? | **Both, merged** — transcript tool-call targets (authoritative) annotated with `git diff --numstat` (+/− lines) when the cwd is a repo. |

## Architecture

On-demand, synchronous. A request walks: resolve transcript → parse facts (pure) →
annotate with git → enrich with LLM narrator (best-effort) → return `Digest`.

```
CLI / Web / TUI
      │  GET /sessions/{id}/digest
      ▼
  daemon handler
      │  transcriptPath(sess)        (reuse internal/lifecycle)
      ├─ digest.ParseTranscript(r)   PURE: files, turns, task, last-msg
      ├─ git -C <cwd> diff --numstat / rev-parse   (annotate +/-, branch)
      └─ Narrator.Summarize(facts)   best-effort claude -p; fallback = last msg
      ▼
   Digest (JSON)
```

### Component 1 — `internal/digest` (pure core + helpers)

The transcript parser is **pure** (`io.Reader` in, structs out) — no filesystem, no
subprocess, no store dependency — so it is trivially unit-testable with fixture transcripts.

```go
type Facts struct {
    EditedFiles []string // unique Write/Edit/MultiEdit/NotebookEdit targets
    Turns       int      // assistant message count
    Task        string   // first user prompt (for narrator context)
    LastMessage string   // last assistant text (deterministic summary fallback)
}

type FileChange struct {
    Path           string
    Added, Removed int  // from git --numstat; 0 when cwd isn't a repo
    Edited         bool // appeared as an edit-tool target in the transcript
}

type Digest struct {
    Summary string       // LLM narrative, or LastMessage on fallback
    Files   []FileChange // union: transcript edits ∪ git-changed files
    Branch  string       // "" when cwd isn't a git repo
    Turns   int
    Status  string       // current agentctl status, passed in by the daemon
    Task    string
}

// PURE — transcript JSONL → deterministic facts.
func ParseTranscript(r io.Reader) (Facts, error)
```

**Transcript shape (grounded in a real `.jsonl`):** each line is one record with a top-level
`type` discriminator. Most records are not conversation turns — `last-prompt`, `custom-title`,
`agent-name`, `mode`, `permission-mode`, `attachment`, `system`, `file-history-snapshot`, etc.
The parser keys on `type` ∈ {`user`, `assistant`} and reads the nested `message` object; every
other record type is skipped. A message's `content` is **either a plain string or a list of
blocks** (`text`, `tool_use`, `tool_result`); both forms must be handled. Edit-tool targets
live in a `tool_use` block's `input` map (`input.file_path` for Write/Edit/MultiEdit,
`input.notebook_path` for NotebookEdit).

**Parsing rules:**
- Iterate JSONL lines; tolerate malformed lines (skip, not fatal — transcripts mix record
  types and a record may be truncated).
- **Files:** collect targets from every `assistant` record's `tool_use` blocks whose name is
  `Write`, `Edit`, `MultiEdit`, or `NotebookEdit` (`input.file_path`, falling back to
  `input.notebook_path`). Dedupe, preserve first-seen order. Skip blocks with an empty/missing
  target.
- **`Turns`** = count of `assistant` records (every assistant record counts, including
  tool_use-only records with no text).
- **`Task`** = text of the first `user` record that is an actual prompt — i.e. whose content
  is a string or contains a `text` block. `user` records whose content is only `tool_result`
  blocks are **not** prompts and are skipped for this purpose.
- **`LastMessage`** = text of the last `assistant` record that contains a `text` block
  (assistant records may be tool_use-only with no text; those are skipped here). Concatenate
  the record's `text` blocks.

### Component 2 — git annotation (I/O)

Given the session cwd:
- `git -C <cwd> rev-parse --abbrev-ref HEAD` → `Branch` (empty on error / non-repo).
- `git -C <cwd> diff --numstat` → parse `added\tremoved\tpath` rows into a
  `path → (added, removed)` map.

**Merge rule** — the file list is the **union** of transcript-edited files and
git-changed files:
- Transcript is authoritative for *which* files the agent touched (so a file edited then
  reverted still appears, with `Edited=true`, `Added=Removed=0`).
- git rows annotate +/− and surface files changed by side effects (e.g. an agent's `Bash`
  formatter run) with `Edited=false`.
- Non-repo cwd → no git rows; file list = transcript edits only, no +/− numbers.

### Component 3 — Narrator (the LLM half, behind an interface)

```go
type Narrator interface {
    Summarize(ctx context.Context, f Facts) (string, error)
}
```

- **Real impl** shells `claude -p` with a compact prompt built from `Task`, `EditedFiles`,
  and `LastMessage`, asking for a 1–2 sentence "what this agent did" summary. Reuses the
  **same plumbing `internal/lifecycle` already uses for agent-title generation** (`claude -p`
  invocation + bounded timeout).
- **Test impl** returns canned text → daemon/CLI tests stay hermetic and offline.
- **Best-effort with graceful degradation:** if `Summarize` errors or times out, the daemon
  sets `Summary = Facts.LastMessage`. Deterministic facts are **always** present; the LLM
  only enriches. This bounds the (ironic, user-triggered, one-shot) token spend and keeps the
  feature robust offline.

## Surfaces

- **Daemon:** `GET /sessions/{id}/digest`. Resolves the transcript via the existing
  `transcriptPath(sess)`; on missing transcript returns a digest with `Status` only and a
  summary like `"no transcript available"`. Synchronous — the narrator call dominates latency
  (a few seconds), acceptable for on-demand. Narrator timeout bounded; failure degrades to
  the deterministic fallback rather than erroring the request.
- **CLI:** `agentctl digest <id>` — human layout (summary paragraph → files table with +/− →
  branch / turns / status) and `--json` mirroring the payload, per the existing `--json`
  convention.
- **Web:** a **Digest** button in the agent tab → calls the endpoint, shows a spinner while
  the narrator runs, renders the summary + file table.
- **TUI:** a keybinding (`d`) on the selected agent → async fetch (detail pane shows
  "generating…") → renders the digest into the detail pane.

## Error handling

| Condition | Behavior |
|---|---|
| No transcript for the session | Digest with `Status` only; summary `"no transcript available"`. |
| `claude -p` fails / times out | `Summary = LastMessage`; facts intact. Request still 200. |
| cwd is not a git repo | No +/− annotation, no branch; transcript file list still returned. |
| Malformed transcript lines | Skipped; parse continues. |
| Unknown session id | 404 (matches existing session-route convention). |

## Testing

- `internal/digest` (pure): table-driven on fixture transcripts in `testdata/` — file
  extraction across all four edit-tool variants (incl. `notebook_path`), dedup + order, turn
  count, `Task` extraction skipping `tool_result`-only user records, `LastMessage` skipping
  tool_use-only assistant records, string-vs-block `content` forms, and malformed-line
  tolerance. Skip non-message record types (`attachment`, `system`, `file-history-snapshot`).
- numstat parser + merge: sample `git diff --numstat` output → assert parse and the
  union/annotation merge (including edited-then-reverted and non-`Edited` git-only files).
- daemon handler: temp transcript dir + temp git repo + **fake Narrator** → assert payload
  shape, the missing-transcript case, and narrator-failure degradation.
- CLI: golden format test for the human table and `--json`.

Implementation via subagent-driven TDD in a worktree (sibling `agentctl-<topic>` or native
`.claude/worktrees/<topic>`, baseRef=head), per repo convention.

## Future seams (not in v1)

- **MCP `digest` tool** — let the lead Claude pull a structured summary of an agent it
  spawned, without attaching. Strong fit for the orchestration loop; deferred to keep v1 tight.
- **Poller-cached digests** — if on-demand parsing ever feels slow at fleet scale, cache in
  the store keyed by transcript mtime.
- **Active no-progress guard** — the separately-brainstormed successor to stuck-detection;
  unrelated code path, mentioned only to mark the boundary.

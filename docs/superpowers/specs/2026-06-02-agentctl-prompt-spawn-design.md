# agentctl Prompt-Spawn + Auto-Type — Design

**Date:** 2026-06-02
**Status:** Approved design (pre-implementation)
**Owner:** Srajan Pathak (personal project)
**Extends:** `docs/specs/2026-06-01-agentctl-design.md` and the GUI design.

---

## 1. Goal

Simplify creating an agent to **a single initial prompt** — no type, repo, ticket,
or any other field. An agent may have **no repo at all** (e.g. personal research);
nothing about a repo is assumed. The agent's **type label is assigned automatically**
by classifying the prompt with the same `claude`.

## 2. Key decisions

| Decision | Choice |
|---|---|
| Creation input | Just `{ "prompt": "…" }`. GUI is a single textarea. |
| Repo/worktree | **None assumed.** Prompt-spawned agents get NO git worktree; the prompt carries all context. |
| Working directory | `AGENTCTL_WORKDIR`, default `~/agentctl-agents` (created at daemon start). All prompt-spawned agents run there. |
| Prompt delivery | Launch `claude --dangerously-skip-permissions '<prompt>'` (prompt as Claude's first message, shell-quoted). |
| Type classification | **Async**, via the same Claude headless: `claude -p "<classify…>"`. Normalized to the existing enum; fallback `other`. Pushed live via SSE. |
| Existing typed flow | **Kept** — CLI `start --type … --repo …` (managed worktree) is unchanged. This is additive. |

## 3. Behavior

### Create (prompt-only)
`POST /spawn` with `{ "prompt": "..." }`:
1. Resolve id `agent-<shortid>` (no ticket/type yet).
2. `tmux new-session -d -s <id> -c <AGENTCTL_WORKDIR>`.
3. Launch `claude --dangerously-skip-permissions '<prompt>'` in the session (prompt shell-quoted so multi-line/special chars survive).
4. Insert the session doc: `prompt` set, `type: ""` (empty = classifying), `worktree`/`branch`/`repo` empty, `status: spawning`.
5. Return 201 immediately.
6. **Background goroutine** (detached context): `type = Classify(prompt)`, `store.UpdateType(id, type)`, `notify()` → the new label streams to the UI over SSE.

### Classify
The daemon runs the same Claude headless:
`claude -p "You are a classifier. Classify the following agent task into exactly one of: development | analysis | spike | pr-review | buildkite-debug | test-run | env-test | other. Reply with ONLY the label.\n\nTask: <prompt>"`
- Output parsed (`parseType`): scan whitespace tokens, return the first that maps to a known type via `store.NormalizeType`; else `other`.
- If `claude -p` errors or is unavailable → `other` (never blocks the agent).
- Classified once from the initial prompt (no re-classification).

### Typed flow (unchanged)
CLI `start --type development --repo … [--branch/--pr/--worktree]` still creates a managed worktree per the existing per-type policy. The prompt-only path and the typed path coexist; `/spawn` distinguishes by whether `prompt` is present.

## 4. Changes by component

- **store** (`internal/store`): add `Prompt string` (`bson:"prompt" json:"prompt"`) to `Session`; add `UpdateType(ctx, id string, t Type) error` (sets type + bumps `updated_at`).
- **config** (`internal/config`): add `Workdir` (`AGENTCTL_WORKDIR`, default `<home>/agentctl-agents`).
- **lifecycle** (`internal/lifecycle`):
  - `SpawnRequest` gains `Prompt string` and `Workdir string`.
  - `Spawn`: when `Prompt != "" && Type == ""` → **prompt mode**: id `agent-<shortid>`, no worktree, `tmux new-session -c <Workdir>`, launch `claude --dangerously-skip-permissions '<shell-quoted prompt>'`, set `sess.Prompt`. Otherwise the existing typed/worktree path (unchanged). `resolveID` uses prefix `agent` when type is empty.
  - `shellQuoteArg(s)` helper (single-quote wrap, escape internal `'`).
  - `Classify(ctx, prompt) (store.Type, error)` — runs `claude -p <metaprompt>` via the mockable `Runner`; `parseType(out)` pure helper; `classifyArg(prompt)` builds the meta string (shared with tests).
- **daemon** (`internal/daemon`):
  - `SpawnRequest` DTO gains `Prompt string` (`json:"prompt"`) and an internal `Workdir string`.
  - `Server` gains `workdir string`; `NewServer(st, life, p, interval, workdir)` (one extra param; update `cli/daemon.go`).
  - `Lifecycle` interface gains `Classify(ctx, prompt) (store.Type, error)`; the adapter delegates to `lifecycle.Lifecycle`.
  - `handleSpawn`: accept EITHER `prompt` OR (`type` + `repo`); 400 if neither. Prompt mode: set `req.Workdir = s.workdir`, `Spawn`, `Insert`, return 201, then `go s.classifyAndUpdate(context.Background(), id, prompt)` → `Classify` → `store.UpdateType` → `notify()`. (Existing dup-ticket 409 only when a ticket is given.)
  - `cli/daemon.go`: ensure `os.MkdirAll(cfg.Workdir, 0o755)` at startup; pass `cfg.Workdir` to `NewServer`.
- **client** (`internal/client`): `SpawnParams` gains `Prompt string`; `spawn` includes `prompt` in the body.
- **GUI** (`web/`): `NewAgentModal` becomes a single prompt `<textarea>` + Create (validation: non-empty). `AgentList`/`AgentDetail` render `type === '' → "classifying…"`; detail shows the initial `prompt`. `types.ts` `Session` gains `prompt: string`.
- **CLI (optional convenience):** `agentctl start "<prompt>"` (no `--type`) routes to the prompt path. (Primary prompt UX is the GUI; this keeps parity.)

## 5. Error handling
- `claude -p` failure/timeout → type falls back to `other`; logged, never blocks.
- Empty prompt → 400 (GUI also disables Create when empty).
- Workdir missing → created at daemon start (`MkdirAll`); spawn does not assume a repo.
- Cleanup of a prompt agent: it has no worktree, so the existing `Cleanup` path just kills tmux + archives (no git guard) — no change needed.

## 6. Testing
- **store:** `UpdateType` round-trip (testcontainers); `Prompt` persists (BSON).
- **lifecycle:** `parseType` table (each label, sentence-with-label, junk→other); `Classify` issues `claude -p <arg-containing-prompt>` and returns the parsed canned response (FakeRunner keyed via shared `classifyArg`); prompt-mode `Spawn` asserts NO `git` calls, `tmux new-session -c <workdir>`, and the launch keystroke includes the shell-quoted prompt; `shellQuoteArg` escaping.
- **daemon:** `/spawn {prompt}` → 201 with empty type + prompt stored; background classify (fake `Classify`) sets the type + notifies a subscriber; validation 400 when neither prompt nor type+repo.
- **client/GUI:** `spawn` body includes `prompt`; `status.ts`/badge unaffected; GUI builds (`tsc`/`vitest`/`astro build`).

## 7. Out of scope
- Re-classifying as work evolves (classify once).
- Inferring a repo from the prompt.
- `claude` auth setup (assumes the daemon's user can run `claude` / `claude -p`).

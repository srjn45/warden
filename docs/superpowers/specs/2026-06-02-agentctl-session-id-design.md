# agentctl Deterministic Claude Session ID — Design

**Date:** 2026-06-02
**Status:** Approved design (pre-implementation)
**Owner:** Srajan Pathak (personal project)
**Sub-project 2 of 5** in the refinement direction (1 storage ✅ → **2 deterministic claude session-id** → 4 restore → 3 layered teardown → 5 monitoring/notify).

---

## 1. Goal

Pin each agent's Claude conversation to a stable, agent-owned **UUID** so the transcript file is deterministic (`<uuid>.jsonl`) and the session can later be resumed by id. This removes today's "newest `.jsonl`" guess and lays the groundwork for session restore (sub-project 4).

## 2. Verified facts (empirical, 2026-06-02)

- `claude --session-id <uuid>` writes the transcript to exactly `~/.claude/projects/<encoded-cwd>/<uuid>.jsonl`, and the `sessionId` field inside matches. The id must be a valid UUID.
- The project directory is still derived from the cwd, and the encoding resolves symlinks (`/tmp`→`/private/tmp`) and maps non-alphanumerics to `-` — so cwd-based dir derivation is fragile, but the UUID **filename** is globally unique, making a `*/<uuid>.jsonl` glob an unambiguous fallback.
- `claude --resume <uuid>` resumes by id (used by sub-project 4, not here). `-n, --name <name>` sets a human display name on the session.

## 3. Key decisions

| Decision | Choice |
|---|---|
| ID model | Store a UUID per agent (`Session.ClaudeSessionID`); the friendly `ID`/`TmuxSession` are unchanged. The two are 1:1 linked via the stored mapping + the `<uuid>.jsonl` path. |
| "Same name" intent | Realized via `claude --name <agent-id>` so the session's **display name = tmux name = agent id**; the UUID is the under-the-hood handle. |
| UUID source | `crypto/rand` v4 generator in the `store` package — no new dependency. |
| Transcript lookup | Locate `<uuid>.jsonl` (encoded-cwd dir, then `*/<uuid>.jsonl` glob fallback); fall back to the existing newest-`.jsonl` heuristic only when `ClaudeSessionID` is empty (back-compat). |
| Migration | None — the file store is fresh/empty; the empty-field fallback covers any pre-existing record. |
| Restore | Out of scope (sub-project 4 uses `--resume <uuid>`). |

## 4. Data model

`internal/store/types.go` — add one field to `Session`:
```go
ClaudeSessionID string `json:"claude_session_id"` // pinned claude --session-id (UUID); deterministic transcript + future --resume
```
(Place it near `TmuxSession`/`Workdir`. JSON-only tag, consistent with the file store.)

`internal/store/` — a new UUID helper (new file `id.go` or appended to an existing store file):
```go
// NewSessionID returns a random RFC-4122 v4 UUID string for use as a claude
// --session-id. Uses crypto/rand; panics only if the OS RNG fails.
func NewSessionID() string
```
Implementation: read 16 bytes from `crypto/rand`, set version (`0x40`) and variant (`0x80`) bits, format `8-4-4-4-12` hex.

## 5. Spawn — pin the id and the display name

`internal/lifecycle/lifecycle.go`, `Spawn`:
- After `id` is resolved and `sess` is built, generate `sess.ClaudeSessionID = store.NewSessionID()`.
- Build the claude invocation with the pinned id + display name. Today `claudeCmd = "claude --dangerously-skip-permissions"`. Introduce a small builder so both paths share it, e.g.:
  ```go
  func claudeLaunch(sessionID, name string) string {
      return claudeCmd + " --session-id " + sessionID + " --name " + shellQuoteArg(name)
  }
  ```
  (`sessionID` is a generated UUID — safe charset — but `name` is the agent id which can contain a Jira key; quote it.)
- **Prompt-mode path:** `launch := claudeLaunch(sess.ClaudeSessionID, id) + ` "$(cat ` + shellQuoteArg(promptFile) + `)"``.
- **Typed/managed path:** send `claudeLaunch(sess.ClaudeSessionID, id)` instead of bare `claudeCmd`.
- Both paths already `tmux send-keys … Enter`; only the command string changes.

## 6. Deterministic transcript lookup

`internal/lifecycle/lifecycle.go`, `recentActivity` (and helpers `claudeProjectDir`/`newestTranscriptTail`):
- New resolver `transcriptPath(projectsDir, sess) string`:
  1. If `sess.ClaudeSessionID != ""`:
     - candidate = `<claudeProjectDir(projectsDir, sess.Workdir)>/<ClaudeSessionID>.jsonl`; if it exists, use it.
     - else glob `<projectsDir>/*/<ClaudeSessionID>.jsonl`; if exactly one match, use it.
  2. Else (empty id → legacy): fall back to `newestTranscriptTail` over the encoded dir (current behavior).
- `recentActivity` reads the tail of the resolved file (reusing the existing tail-read), else the tmux pane (unchanged final fallback).
- Keep `newestTranscriptTail` for the legacy branch.

## 7. Surfacing

- `ClaudeSessionID` is serialized on `Session` (so it flows through the daemon API, `internal/client`, MCP `get_agent`, and the web GUI automatically).
- `agentctl status <id>` (CLI detail view) prints it as a "claude session" line.
- **Not** added to the `ls` table or the TUI list (internal handle; friendly id + subject remain the face). The TUI detail pane may show it later (not required here).

## 8. Components & files

- `internal/store/types.go` — `+ClaudeSessionID` field.
- `internal/store/id.go` (new) — `NewSessionID()` + test `id_test.go`.
- `internal/lifecycle/lifecycle.go` — generate id in `Spawn`; `claudeLaunch` builder; both launch paths; `transcriptPath` resolver in `recentActivity`.
- `internal/lifecycle/lifecycle_test.go` — spawn-command + spawned-session assertions; transcript-lookup tests.
- `internal/cli/sessions.go` (or wherever `status` renders) — print the claude session line.

## 9. Testing

- **store:** `NewSessionID` returns a well-formed v4 UUID (regex `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`) and two calls differ; `Session` JSON round-trip includes `claude_session_id`.
- **lifecycle (mock `Runner`, asserting captured commands):**
  - prompt-mode spawn launches a command containing `--session-id <uuid>` and `--name <id>`, and the returned `Session.ClaudeSessionID` is that same non-empty uuid.
  - typed-mode spawn likewise.
- **lifecycle transcript lookup (temp-dir fixtures):**
  - finds `<uuid>.jsonl` in the encoded project dir;
  - finds it via the `*/<uuid>.jsonl` glob when the dir encoding doesn't match;
  - falls back to newest-`.jsonl` when `ClaudeSessionID` is empty.

## 10. Out of scope

- Session restore / `--resume` (sub-project 4).
- Layered teardown (3), monitoring/notify (5).
- Backfilling ids onto already-running agents spawned by the old binary (they use the legacy fallback; re-spawn to get a pinned id).

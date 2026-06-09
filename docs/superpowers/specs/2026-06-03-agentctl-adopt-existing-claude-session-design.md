# `agentctl adopt` — bring an existing Claude Code session under management

**Date:** 2026-06-03
**Status:** Approved (design); pending implementation plan

## Goal

Let a user register a Claude Code session that agentctl did **not** spawn — one
they started by hand in a plain terminal, or one already running inside a tmux
session — so it appears in the fleet and is monitored, sendable, and
restorable like any spawned agent.

A single command, `agentctl adopt`, detects the situation and does the right
thing:

- **Plain shell** (no tmux): agentctl re-opens the *same* conversation inside a
  fresh tmux session via `claude --resume <uuid>` and registers it. The
  original (exited) terminal is left alone; the command prints how to attach.
- **Already in tmux**: agentctl registers the existing tmux session by name and
  starts monitoring it. No relaunch, zero disruption.

## Non-goals / physical constraint

- **You cannot move a live process into tmux.** A running `claude` is bound to
  its controlling pty; tmux only ever creates a *new* pty and runs a *new*
  command. So for the plain-shell case the conversation is **resumed** (same
  transcript, continues where it left off), not the literal live process
  carried across. The user is expected to have exited the original first.
- No `exec`-replace-the-current-shell handoff. `adopt` registers detached and
  prints an attach hint (`agentctl attach <id>`). (Chosen for simplicity.)
- No launch wrapper (`agentctl run`) in this work — possible fast-follow.
- No transcript-based subject classification — the existing poller summarizer
  fills `Subject` from pane content.
- **No TUI changes.** Adopted sessions appear in the list automatically because
  the poller monitors every stored session by its tmux name.

## How adoption maps onto existing machinery

Most of the work already exists; adopt is mostly glue.

| Need | Existing piece (reused) |
|---|---|
| Resume a conversation under tmux | `Lifecycle.Restore` body (`internal/lifecycle/lifecycle.go`): `tmux new-session -d -s <id> -c <cwd>` then `send-keys` `claude --resume <uuid> --name <id>`. |
| Find the claude session for a dir | `claudeProjectDir` + `newestTranscriptPath` + `transcriptPath`. |
| Monitor a session | Poller reads any tmux session by name (`tmux has-session`, `tmux capture-pane`) — it does **not** own the session. No changes. |
| Subject auto-summary | Poller's existing summarizer dispatch. |
| Session record | `store.Session` already has every field (`ID`, `TmuxSession`, `Workdir`, `ClaudeSessionID`, `Status`, `Type`, `Subject`, timestamps). `store.Insert` validates the id via `safeID`. |

New code is limited to: a discovery helper, a shared resume helper extracted
from `Restore`, the `/adopt` daemon handler, the `adopt` CLI command, and
(stretch) an MCP tool.

## A. CLI surface

```
agentctl adopt [--session-id <uuid>] [--dir <path>]
```

- **cwd**: `--dir` (resolved to an absolute path against the *caller's* cwd, not
  the daemon's) else `os.Getwd()`.
- **tmux detection**: if `$TMUX` is non-empty the command is running inside
  tmux ⇒ **live** mode; the current session name comes from
  `tmux display-message -p '#S'`. Empty ⇒ **resume** mode.
- Builds the request and `POST`s to `/adopt`, then prints, e.g.:
  - `adopted as agent-7f3c (resumed) — attach with: agentctl attach agent-7f3c`
  - `adopted as work (live) — attach with: agentctl attach work`
- Surfaces any `warning` the daemon returns (e.g. live-registered without a
  resolvable claude session id).

## B. Daemon endpoint `POST /adopt`

```go
type AdoptRequest struct {
    Cwd         string `json:"cwd"`          // required; must be an existing dir
    SessionID   string `json:"session_id"`   // optional; claude uuid override
    TmuxSession string `json:"tmux_session"` // non-empty ⇒ live-register an existing tmux session
}
```

Handler flow:

1. **Validate cwd** is an existing directory (same check as `/spawn`).
2. **Resolve claude session id**:
   - if `SessionID != ""` use it (and verify a transcript exists for it; 400 if
     not).
   - else discover the newest transcript for `Cwd` via the new
     `NewestClaudeSession(cwd)` helper.
3. **Two-heads guard**: if any existing session already has this
   `ClaudeSessionID`, reject with 409 (prevents adopting the same conversation
   twice / racing two claudes on one transcript). This replaces mtime-sniffing,
   which is unreliable for an idle claude.
4. **Branch** (see C).
5. `store.Insert` the record. On insert failure, tear down anything created
   (mirror the `/spawn` rollback: kill the tmux session we made in resume mode).
6. `notify()` SSE subscribers; return the `Session` JSON with `201 Created` and
   an optional `warning`.

## C. The two branches

**Resume** (`TmuxSession == ""`):
- Require a resolved claude session id; else `400 "nothing to resume in <cwd>"`.
- Generate `id = agent-<short>` (reuse the prompt-spawn id scheme).
- Call the shared `resumeInTmux(ctx, id, cwd, claudeID)` (see E).
- Initial `Status = spawning`.

**Live** (`TmuxSession != ""`):
- Validate `tmux has-session -t <TmuxSession>`; 404 if it's gone.
- **No relaunch.**
- Claude session id is best-effort: monitoring works without it; only
  later `restore` needs it. If empty, return a `warning`.
- Initial `Status = working` (poller reclassifies within ~1s).
- Choose the id per section D, pointing `TmuxSession` at the real tmux session.

Both branches set `Type = other`, `Workdir = cwd`, `ClaudeSessionID =` the
resolved id (possibly empty in live mode), and leave `Subject` empty for the
poller to fill.

## D. ID ↔ tmux name (live case)

Attach paths target tmux by the **id** (`agentctl attach <id>` → `tmux attach -t
<id>`; cockpit `switch-client -t <id>`), so the `ID == TmuxSession` invariant
must hold. For a live adopt:

- If the existing tmux session name passes `safeID` **and** is not already a
  registered session id → use it as the id directly (no rename; least
  surprise).
- Otherwise generate `agent-<short>` and `tmux rename-session -t <old> <new>`
  so the invariant holds and the agent looks uniform in the list.

## E. Shared resume helper

Extract the resume body of `Lifecycle.Restore` into:

```go
func (l *Lifecycle) resumeInTmux(ctx context.Context, id, cwd, claudeID string) error
```

which does the workdir check, `tmux new-session -d -s <id> -c <cwd>`, and
`send-keys` of `claudeResume(claudeID, id)`. `Restore` (resumes an existing
record) and `adopt` (creates a new record, then resumes) both call it. Add:

```go
func (l *Lifecycle) NewestClaudeSession(cwd string) (string, error)
```

returning the uuid of the newest `*.jsonl` transcript under
`claudeProjectDir(ProjectsDir, cwd)` (uses `newestTranscriptPath`, strips the
`.jsonl` suffix). Returns `ErrNoTranscript` when none exist.

## F. MCP tool (stretch, same PR)

Add an `adopt_agent` MCP tool mirroring the endpoint: args `cwd` (default the
MCP server's cwd), optional `session_id`, optional `tmux_session`. Cheap because
the logic is daemon-side. May be deferred without affecting the CLI.

## G. Error handling summary

| Condition | Result |
|---|---|
| cwd missing / not a dir | 400 |
| resume mode, no claude id resolvable | 400 "nothing to resume" |
| `--session-id` given but no transcript for it | 400 |
| claude id already adopted by another session | 409 |
| live mode, tmux session gone | 404 |
| live mode, no claude id found | 201 + `warning` (monitoring only; no restore) |
| store insert fails | 500 + rollback (kill the tmux we created in resume mode) |

## H. Testing

- **lifecycle**: `NewestClaudeSession` discovery against a temp projects dir
  (newest-by-mtime, none-found); `resumeInTmux` via the fake runner (correct
  tmux args + resume command); live id-selection (safe-and-unused name kept,
  unsafe/duplicate name → generate + rename); the dup-`ClaudeSessionID` guard.
- **daemon**: `/adopt` resume branch and live branch with fake lifecycle+store
  (mirror `internal/daemon/api_test.go` fakes); error rows from section G.
- **cli**: request building — `$TMUX` set vs unset, `--dir` vs `os.Getwd()`,
  `--session-id` passthrough.

## I. Risks / notes

- **Shared-repo collision**: a concurrent session is actively editing
  `internal/tui/*` in this checkout. Adopt deliberately touches none of the TUI;
  its footprint is `internal/cli`, `internal/daemon`, `internal/lifecycle`,
  `internal/client`, and (stretch) `internal/mcp`. Check tmux/worktrees before
  any git ops.
- **Resume double-head**: the dup-`ClaudeSessionID` guard plus the documented
  "exit the original first" expectation is the v1 mitigation. No process-level
  detection is attempted.
- **cwd vs claude dir mismatch** (live mode, adopt run from a different pane/dir
  than claude): default to caller cwd, allow `--dir`; with an explicit
  `--session-id` the glob fallback in `transcriptPath` finds the transcript
  regardless of cwd.

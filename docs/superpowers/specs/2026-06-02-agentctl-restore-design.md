# agentctl Session Restore — Design

**Date:** 2026-06-02
**Status:** Approved design (pre-implementation)
**Owner:** Srajan Pathak (personal project)
**Sub-project 4 of 5** (1 storage ✅ → 2 session-id ✅ → **4 restore** → 3 layered teardown → 5 monitoring/notify).

---

## 1. Goal

When an agent's tmux/claude session is lost (reboot, tmux server killed, crash) but its record survives in the file store, **restore** it: recreate the tmux session in its original directory and resume the *same* Claude conversation via `claude --resume <ClaudeSessionID>`. Detection of "lost" is *manual-trigger*: agentctl marks lost sessions `orphaned`, and the user/orchestrator restores explicitly — no surprise auto-relaunches.

## 2. Verified facts (empirical, 2026-06-02)

- `claude --resume <uuid>` continues the **same** conversation (recalled a fact seeded in a separate invocation) and keeps the **same** transcript id (`<uuid>.jsonl`, not forked). Restore therefore needs only the pinned `ClaudeSessionID` from sub-project 2 + the original cwd.

## 3. Detection — already implemented (no work in this sub-project)

The poller already does this. `poller.tick` calls `deps.SessionAlive(ctx, TmuxSession)`; when false, `poller.classify` returns `store.StatusOrphaned`, persisted via the `UpdateStatusIf` CAS and pushed over SSE. This also **reconciles after a reboot**: launchd restarts the daemon, the file store persists the records, and the first poll marks dead sessions `orphaned`. `ls`/`status`/TUI/GUI render status already, so orphaned surfaces with no change. **This sub-project adds only the restore action.** (A distinct `orphaned` badge/labelling is optional polish, not required here.)

## 4. Restore action

New `lifecycle.Restore(ctx, sess *store.Session) error` (takes `*store.Session`, consistent with `Summarize`):
- Recreate tmux: `tmux new-session -d -s <ID> -c <Workdir>`.
- Relaunch, resuming (no prompt re-injection — `--resume` carries the history):
  ```
  claude --dangerously-skip-permissions --resume <ClaudeSessionID> --name <ID>
  ```
  via a small `claudeResume(sessionID, name)` builder (sibling of `claudeLaunch`), sent with `tmux send-keys -t <ID> <cmd> Enter`.

The daemon handler sets status → `store.StatusSpawning` after a successful `Restore` (the poller then advances it to working/waiting/idle) and calls `notify()`.

## 5. Preconditions — resume-only, refuse-if-alive

`Restore` validates first and returns a specific error (no silent fresh start):
- **tmux already alive** (`tmux has-session` succeeds) → `ErrAlreadyRunning` ("already running; use send/attach"). Prevents a double-launch. (A `--force` variant is out of scope.)
- **Empty `ClaudeSessionID`** (legacy pre-#2 agent) → error "no pinned session id; re-spawn instead".
- **Workdir missing** (`os.Stat(sess.Workdir)` fails — e.g., a worktree removed by teardown) → error "workdir gone; re-spawn". Restore does **not** recreate worktrees (teardown/#3 territory).
- **Transcript missing** (`transcriptPath(sess) == ""` — reuses the sub-project-2 resolver) → error "no transcript to resume".

Errors are sentinels (`ErrAlreadyRunning`, `ErrNoSessionID`, `ErrWorkdirMissing`, `ErrNoTranscript`) so the daemon can map them to clear HTTP responses.

## 6. Wiring

- **Daemon:** `POST /sessions/{id}/restore` → `handleRestore` (loads the session via the store, calls `life.Restore(ctx, sess)`, on success `UpdateStatus(id, spawning)` + `notify()`; maps precondition errors to 4xx/409). Add `Restore(ctx, *store.Session) error` to the daemon's `Lifecycle` interface + the lifecycle adapter.
- **Client:** `internal/client` gains `Restore(id string) error`.
- **CLI:** `agentctl restore <id>` (in `internal/cli/lifecycle.go`, beside `done`).
- **MCP:** new tool `restore_agent` (id) → daemon restore endpoint, described as "recreate and resume a lost/orphaned agent."
- **Skill:** add a row to `SKILL.md`'s intent→tool table ("restore / bring back a lost agent → `restore_agent`"); guardrail: only for `orphaned`/dead sessions.

## 7. Trust gate (free win)

Restore reuses the **original** `Workdir`, which claude trusted on the first run — so the folder-trust prompt that blocks claude in a *fresh* dir does not reappear. Restored agents land at the composer ready for input.

## 8. Testing

- **lifecycle.Restore (mock `Runner`):**
  - happy path: `tmux has-session` reports dead → recreates `tmux new-session … -c <workdir>` and sends `claude … --resume <uuid> --name <id>`.
  - each precondition error: alive → `ErrAlreadyRunning`; empty id → `ErrNoSessionID`; missing workdir → `ErrWorkdirMissing`; missing transcript → `ErrNoTranscript` (temp-dir fixtures for workdir/transcript; `transcriptPath` already tested in #2).
- **daemon/client:** `POST /sessions/{id}/restore` round-trips; precondition errors map to the right status codes; `handleRestore` sets status `spawning` on success (fake lifecycle/store).
- **mcp:** `restore_agent` calls the daemon restore endpoint (in-memory client + fake daemon).
- **live smoke:** spawn an agent (in a trusted dir) → `tmux kill-session` it → `agentctl ls` shows `orphaned` → `agentctl restore <id>` → the conversation resumes (recall check) and status returns to working.

## 9. Out of scope

- Auto-restore (the chosen model is detect + manual).
- Recreating a removed worktree (re-spawn instead; teardown is #3).
- A fresh re-spawn fallback when not resumable (restore is resume-only; errors and suggests `agentctl start`).
- `--force` restore of a still-alive session.
- Monitoring/notification on `orphaned` (sub-project 5).

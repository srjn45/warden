# Crash Detection — Design

**Date:** 2026-06-09
**Status:** Approved (brainstorming → spec)

## Problem

When an agent's Claude process ends *without* firing a clean `SessionEnd` hook, warden cannot tell what happened. Today the outcomes collapse confusingly:

- **Clean finish** — Claude fires `SessionEnd` → status `done`. The poller treats `done` as terminal, so when the tmux window later dies it stays `done`. ✅ (correct)
- **Claude crashes but its shell survives** (OOM-kill / SIGKILL / segfault of the `claude` process) — `claude` is *typed into an interactive shell* via `send-keys`, so when it dies the shell returns to a prompt and `SessionAlive` (tmux `has-session`) stays **true**. The poller sees no `esc to interrupt` / `❯` box, and after `stuckAfter` downgrades `working` → **`idle`**. A genuine crash masquerades as idle.
- **Whole window/shell vanishes** (reboot, OOM-killed shell, `tmux kill-session`, daemon teardown) — `SessionAlive` false → poller sets **`orphaned`**.

So `orphaned` is a coarse catch-all and the most common crash case (process dies, shell lives) isn't detected at all. There is already an unused `StatusErrored` in the enum (`internal/store/types.go:13`) that the poller never sets — the natural home for a real crash.

The daemon never `wait()`s on Claude (tmux owns it), so once the process is gone its exit status is lost unless we capture it at exit time.

## Goal

Classify a dead-without-clean-hook agent into a precise 3-way outcome, recording the exit code on a real crash:

| Condition | Status |
|---|---|
| exit `0` (clean exit, `SessionEnd` hook missed) | `done` |
| exit non-zero or killed-by-signal | `errored` (records exit code) |
| window/shell vanished, no exit info recoverable | `orphaned` |

Non-goals (separate backlog items): **auto-restart** of an `errored` agent (`errored` becomes its clean trigger), and **gauge-triggered rotation**.

## Approach: exit-code sentinel file

`claude` is launched by typing a single shell line into the agent's tmux pane (`internal/lifecycle/lifecycle.go` — `spawnFreeForm` line ~593, `resumeInTmux`, and the pipeline job spawn). Append an exit-capture suffix to that line so the surviving shell records Claude's exit status:

```
<claude … invocation> ; printf '%s' "$?" > "<exitfile>"
```

The poller, each tick, for **non-terminal** sessions, reads the exit-file and decides:

- file present, contents `0` → `done`
- file present, contents non-zero → `errored` (capture the code)
- file **absent** and `SessionAlive` false (window gone) → `orphaned`
- file absent and `SessionAlive` true → unchanged (still running)

This catches the common "Claude died, shell survived" case the current logic misses. It needs no tmux behavior change and leaves no lingering dead panes.

### Rejected alternatives

- **tmux `remain-on-exit` + `pane_dead_status`.** Claude is a child of the interactive shell, so the pane only dies when the *shell* dies — it never observes Claude's own exit. Would force launching Claude as the pane command, breaking attach/interact ergonomics.
- **PID liveness via `/proc`.** Tells you Claude is gone but not *why* — an exit code can't be recovered from a non-child process, so `done` and `errored` can't be split.

## Components

### 1. Status semantics (`internal/poller/poller.go`)

The `SessionEnd` hook remains the **primary, fast** path to `done` (`statusForHook`, `internal/daemon/api.go:309`). The exit-file is a backstop + disambiguator that runs in the poller tick.

- `classify` (or a new sibling check in `tick`) consults the exit-file *before* the existing liveness/pane logic, only for non-terminal sessions.
- Status swaps go through the existing CAS (`UpdateStatusIf`) so a hook that already set `done` between `List` and the write wins — the exit-file read never clobbers a newer hook status.
- The `orphaned` branch (`!sessionAlive`) is now gated on the exit-file being absent; an exit-file present at the moment the window is gone still yields `done`/`errored` from its contents.

### 2. Exit-file plumbing (`internal/lifecycle/lifecycle.go`)

- One helper builds the suffix (` ; printf '%s' "$?" > <quoted exitfile>`), keeping it DRY and correctly shell-quoted. Threaded through the three send-keys launch sites: `claudeLaunch`/`spawnFreeForm`, `claudeResume`/`resumeInTmux`, and the pipeline job spawn (`JobSpawnRequest`, line ~1045).
- Exit-files live in a dedicated per-id state dir alongside the existing data dirs (e.g. `~/.warden/exits/<id>`), created `0o700` consistent with the Track-1 hardening of the other data dirs.
- **Lifecycle of the file:** deleted on spawn for that id (stale-guard against a prior run), and deleted on consumption by the poller once it has driven the terminal transition. Restore/rotate get fresh files (rotate already mints a new id; restore reuses the Claude session under the same agent id, so its exit-file is cleared on resume).
- The path is passed into the session environment or interpolated into the suffix at launch; it must not depend on the agent's cwd (the file belongs to warden, not the project).

### 3. Data model (`internal/store/types.go`)

- Add `ExitCode *int` to `Session` (pointer so the three states are distinct: `nil` = no exit info recovered (orphaned / pre-feature agent), `0` = clean exit, non-zero = crash; `omitempty` in JSON). **Always populated** when the poller consumes an exit-file — on the clean `0` path as well as the `errored` path — so the field is a complete record of how every agent that ran the exit-suffix terminated. This is what a future auto-restart ("restart iff `ExitCode` non-zero") and standup rollup ("N clean, M crashed") want; the cost is the `done` path also writing a `0`, with no behavior change.
- Append a `store.Event` on the terminal transition: `session exited: code N (<signal name if 128<N<=165>)`. This gives a human-readable trail in the events list.
- `errored`/`orphaned` transitions already flow through `OnTransition` (`poller.go:65`) → the daemon's notification wiring fires automatically. This **is** the "no longer silently stale" win; no new notification plumbing needed.

### 4. Surfacing (web + TUI)

- Web mission-control: show the exit code as a badge/reason on `errored` agents (the attention queue / agent tab already render status; extend the status presentation).
- TUI: show the code next to an `errored` agent in the list.
- `StatusErrored` already exists in the enum, so list/filter consumers need only handle the now-reachable value — minimal churn.

## Data flow

```
launch  → lifecycle appends `; printf '%s' "$?" > exits/<id>`; clears any stale exits/<id>
run     → claude runs as child of the pane's interactive shell
exit    → shell evaluates $? and writes exits/<id> (unless the shell itself was killed)
poll    → for each non-terminal session:
            read exits/<id>
              "0"        → done    (ExitCode=0)                  ─┐ CAS swap;
              non-zero   → errored (ExitCode=N, append event)     │ hook-set done wins
              (absent)   → if !SessionAlive → orphaned (nil)      │
                           else unchanged                        ─┘
            on terminal swap: delete exits/<id>
notify  → OnTransition fires existing user notification
```

## Error handling

- **Unreadable / partially-written exit-file:** treat as absent this tick (retry next tick). The file is written by a single `printf` so a partial read is unlikely, but a malformed (non-integer) body is treated as absent rather than crashing classification.
- **Exit-file write never happens** (shell itself SIGKILLed/OOM with the window): correctly falls to the `orphaned` branch via `!SessionAlive`.
- **Stale exit-file from a prior id reuse:** prevented by deleting on spawn; the poller also only reads exit-files for sessions it is actively polling (present in the store, non-terminal).
- **State-dir creation failure at launch:** non-fatal to the spawn (matches the existing best-effort posture for tmux option-setting); crash detection degrades to the current `orphaned`-only behavior for that agent rather than blocking the launch.

## Testing

- **Poller unit tests** (table-driven over the four file/liveness combinations): `"0"`+alive → done; `"137"`+alive → errored w/ ExitCode; absent+dead → orphaned; absent+alive → unchanged. Plus a CAS-loses-to-hook case (status already `done` → exit-file read is a no-op).
- **Malformed/empty exit-file** → treated as absent (no transition, no panic).
- **Lifecycle test** asserting the exit-capture suffix is appended (and the exit-file path is correctly quoted) at each of the three launch sites, and that a stale exit-file is cleared on spawn.
- **Event/ExitCode assertion**: an `errored` transition appends the `session exited: code N` event and sets `ExitCode`.

## Rollout

- Daemon-side change (poller + lifecycle + store + daemon wiring) → requires `make install` + **daemon restart** to take effect. Web UI change requires the embedded `web/dist` rebuild (release/install).
- Backward compatible: agents spawned before the change have no exit-file → they fall back to today's `orphaned`/`idle` behavior; no migration needed.

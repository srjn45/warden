# Auto-restart of errored agents — Design

**Date:** 2026-06-10
**Status:** Approved (brainstorming → spec)
**Builds on:** crash detection (`docs/superpowers/specs/2026-06-09-warden-crash-detection-design.md`, merged `33b5493`) — which made `errored` a precise, recoverable terminal state.

## Problem

Crash detection now flags a genuinely-crashed agent as `errored` (records the exit code + a `session exited` event). But the agent stays dead — recovery is fully manual (`warden restore`). For an agent the user trusts to run unattended, a transient crash (OOM-kill, SIGKILL, a flaky tool) ends the work even though the conversation transcript is intact and resumable.

`errored` is the clean trigger crash detection was built to enable. This feature reactively resumes such an agent — bounded so a persistent crash can't loop forever.

## Goal

When an agent the user opted in to auto-restart reaches `errored`, automatically resume its Claude conversation (preserving context) in a fresh tmux session — up to a small retry cap, after which it gives up and stays `errored` for the human. Conservative and opt-in by default, consistent with warden's posture (the proactive `self-rotate` was deliberately human-triggered, not automatic).

Non-goals (v1): pipeline-job auto-restart (the pipeline executor owns job retry/failure), exit-code filtering (restart every `errored`, not just signal codes), `orphaned`-resurrection (an orphaned window may be a deliberate `kill-session`), and a web checkbox (CLI flag only in v1).

## Approach

Reuse the existing `Lifecycle.Restore` (recreate the tmux session in the original workdir and `claude --resume <ClaudeSessionID>` — preserving the transcript). Trigger it from the daemon's existing `OnTransition` hook (`internal/cli/daemon.go:90`), which already fires once per status swap and is where notifications and pipeline reconciliation hang.

### Why kill-then-Restore (the restart mechanic)

The two crash types crash detection distinguishes need different handling:
- **`errored`** (claude process killed, shell survived): the tmux session is **still alive** at a shell prompt, so `Restore` refuses with `ErrAlreadyRunning` (its `has-session` guard).
- (`orphaned` is out of scope, but for completeness its window is gone, so `Restore` would work directly.)

Rather than special-case, the restarter does a uniform **best-effort `tmux kill-session` then `Restore`**: kill any surviving shell, then let `Restore` create a fresh tmux session and resume. This reuses `Restore` unchanged and yields one code path. (Rejected alternative: re-launch `claude --resume` into the surviving shell — avoids a teardown but forks the logic across the two crash types and duplicates `resumeInTmux`'s send-keys.)

The relaunch path (`resumeInTmux`) already appends the crash-detection exit-capture suffix, so a restarted agent is itself crash-detectable and re-restartable — the cap is what stops an infinite loop.

### Crash-loop bound

Two new `Session` fields: `RestartCount int` and `LastRestartAt time.Time`.

On an `errored` transition for an auto-restart-enabled, non-pipeline session, a **pure decision function** decides the action from `(RestartCount, LastRestartAt, now, max, resetAfter)`:

1. **Reset:** if `LastRestartAt` is zero, or `now - LastRestartAt >= resetAfter` (default 5m), treat this as a fresh incident → effective count = 0. This realises "a successful run resets the counter," defined as *sustained* health (≥ resetAfter since the last restart) so a resume→immediate-crash loop cannot evade the cap by briefly reaching `working`.
2. **Give up:** if effective count `>= max` (default 3) → do nothing further: stay `errored`, append event `auto-restart: giving up after N attempts`. No extra notification is needed — `notifyHook` already fired for this `errored` transition earlier in the same `OnTransition`. (Suppressing the `errored` notification on the *restart* path, so the user is only alerted on give-up, is a deferred refinement — v1 keeps notifications exactly as today.)
3. **Restart:** else → set `RestartCount = effective count + 1`, `LastRestartAt = now`, append event `auto-restart: attempt N/max`, then kill-then-Restore. `Restore` returns the session to a live status; the resumed Claude's `SessionStart` hook drives it to `working`.

A lifetime that interleaves healthy runs and occasional crashes therefore keeps recovering (each ≥5-min-healthy crash resets to attempt 1/3); a tight crash-loop burns 3 attempts within ~`3 × resetAfter`-bounded windows and then stops.

## Components

### `internal/daemon/autorestart.go` (new)
- A `Restarter` with the lifecycle dependency it needs (`Restore`, `tmux kill-session`) and the store (to read/update `RestartCount`/`LastRestartAt`, append events, set status). It exposes one `OnTransition(sess, from, to)` method matching the existing hook signature.
- A **pure** helper `decideRestart(count int, lastRestartAt, now time.Time, max int, resetAfter time.Duration) (action restartAction, nextCount int)` returning one of `actionRestart` / `actionGiveUp` (the gating predicates — flag/pipeline/status — live in the method, but the count/reset/cap arithmetic is pure and unit-tested in isolation).
- Config knobs read at construction: `WARDEN_AUTO_RESTART_MAX` (default 3), `WARDEN_AUTO_RESTART_RESET` (default 5m). The *feature* is enabled per-agent (the `AutoRestart` flag), not by a global env switch.

### `internal/cli/daemon.go`
- Construct the `Restarter` and add it as a third callback in the `OnTransition` closure, after `notifyHook` and `exec.OnTransition`:
  ```go
  pl.OnTransition = func(sess, from, to) {
      notifyHook(sess, from, to)
      exec.OnTransition(sess, from, to)
      restarter.OnTransition(sess, from, to)
  }
  ```
  Ordering: notify and pipeline-reconcile first (they only read/branch on the terminal status); the restarter acts last so its kill-then-Restore (which mutates tmux + status) happens after the others have observed the `errored` edge.

### `internal/store` (model + setter)
- `Session` gains `RestartCount int json:"restart_count,omitempty"` and `LastRestartAt time.Time json:"last_restart_at,omitempty"`.
- A store method to record a restart attempt (set count + timestamp) and to append the attempt/give-up event — reuse `AppendEvent` + a small `mutate`-based setter, mirroring existing setters (e.g. `UpdatePane`).

### `internal/lifecycle` (flag plumbing + persistence)
- `SpawnRequest` gains `AutoRestart bool`; persisted onto the `Session` at spawn (alongside `Supervised`). `Restore` already reuses the persisted session, so a restarted agent keeps `AutoRestart` (and `Supervised`).

### CLI / client / daemon API
- `warden start --auto-restart` flag → threaded through the client spawn request → daemon spawn handler → `SpawnRequest.AutoRestart`, exactly mirroring how `--supervised` is plumbed.

## Data flow

```
agent crashes → poller finalizes → errored (crash detection)
   │
   └─ daemon OnTransition(sess, working, errored)
        notifyHook                      (unchanged)
        exec.OnTransition               (unchanged; pipeline jobs)
        restarter.OnTransition:
          guard: to==errored AND sess.AutoRestart AND sess.PipelineID==""   else return
          decideRestart(count, lastRestartAt, now, max, resetAfter):
             stale/zero lastRestartAt  → count:=0
             count >= max              → GIVE UP: event "giving up after N", stay errored
                                          (errored notify already fired via notifyHook above)
             else                      → RESTART:
                                          persist count+1, lastRestartAt=now, event "attempt N/max"
                                          tmux kill-session (best-effort)
                                          Restore(sess)  → fresh tmux + claude --resume
   resumed claude SessionStart hook → working   (≥ resetAfter healthy → next crash resets the counter)
```

## Error handling

- **`Restore` fails** (e.g. `ErrWorkdirMissing`, `ErrNoTranscript`, tmux error): the attempt has already been counted and evented; append a `auto-restart: restore failed: <err>` event and leave the session `errored`. The next manual `restore`/intervention is the recovery. Do **not** retry within the same tick (avoids a hot loop on a permanently-unrestorable agent).
- **Best-effort `kill-session`**: a failure (already-dead session for the `orphaned`-shaped case, were it ever in scope, or a tmux hiccup) is logged, not fatal — `Restore`'s own `has-session` guard still protects against a double-launch.
- **Concurrency:** `OnTransition` is edge-triggered once per swap by the poller's CAS, so a single `errored` transition yields exactly one restart decision. The restarter's store writes (count/timestamp) use the existing atomic `mutate` path.
- **Daemon restart mid-incident:** `RestartCount`/`LastRestartAt` persist, so the cap is honoured across a daemon restart.

## Testing

- **Pure `decideRestart`** table tests: zero `LastRestartAt` → restart (count 1); count below max, recent restart → restart (count+1); count at max, recent → give up; count at max but `now-last >= resetAfter` → reset → restart (count 1); just-below-threshold vs just-over-threshold boundary.
- **`Restarter.OnTransition` gating** (fake lifecycle + store): no-op when `to != errored`; no-op when `AutoRestart` false; no-op when `PipelineID != ""`; on a qualifying errored edge → calls `kill-session` then `Restore`, persists count/timestamp, appends the attempt event; at cap → appends give-up event, does **not** call `Restore`.
- **`Restore`-fails path** → attempt counted, `restore failed` event, status stays `errored`, no panic.
- **CLI plumbing**: `warden start --auto-restart` sets `SpawnRequest.AutoRestart`; persisted on the session; survives `Restore`.

## Rollout

Daemon-side change → `make install` + daemon restart. Backward compatible: existing/old agents have `AutoRestart=false` (zero value) → behaviour unchanged. Live smoke: `warden start --auto-restart --dir <d>`; `kill -9` its claude → expect an `auto-restart: attempt 1/3` event and the agent back to `working` with the same conversation; `kill -9` repeatedly to confirm it gives up at 3 and stays `errored`.

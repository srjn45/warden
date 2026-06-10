# Warden Scheduled / Cron Pipelines — Decision Record

**Date:** 2026-06-10  
**Status:** Deferred — not building a native scheduler. Decision recorded here for future reference.

---

## Decision

Do not add a native cron/scheduled pipeline runner to the warden daemon. Use OS-native scheduling (cron / launchd / systemd timers) + the existing `warden pipeline create` CLI instead.

---

## Rationale

**Against native scheduling:**

1. **Warden is interactive-first.** The approvals inbox, supervised mode, and digest all exist because agents need human oversight. Unattended cron jobs conflict with this model — a failed job at 3am is invisible, and running jobs in `--dangerously-skip-permissions` mode defeats supervised spawn.

2. **Solved by the OS.** cron, launchd, and systemd timers are mature, reliable, and already on every target platform. Adding a scheduler inside the daemon duplicates this infrastructure without adding value.

3. **Complexity vs. clarity.** A scheduler needs persistence (survive daemon restart), error handling (retry policy, failure notification), and a management UI. Each is non-trivial. The daemon's job is orchestration, not time-keeping.

4. **Use cases aren't well-defined.** At the time of this decision, no concrete recurring pipeline workflows had been identified that couldn't be served by an OS cron + `warden pipeline create`.

**For native scheduling (the arguments we're not taking):**

- Single-pane discoverability: schedules visible alongside running agents in the web UI
- In-app notification when a scheduled run completes or fails
- No need to learn crontab syntax

These are real UX benefits, but they don't outweigh the complexity cost at this stage.

---

## Recommended Pattern (document in USAGE.md)

Run a pipeline on a schedule using OS cron:

```cron
# Every day at 9am: run the "morning-digest" pipeline spec
0 9 * * * /usr/local/bin/warden pipeline create --spec ~/.warden/specs/morning-digest.yaml
```

Or launchd on macOS / systemd timer on Linux with equivalent semantics.

The `warden pipeline create` CLI exit code indicates whether the pipeline was accepted (0) or failed to start (non-zero), so the OS scheduler's built-in failure notification works.

---

## One-Shot Deferred Spawn (`--at`)

A lighter-weight addition worth considering independently of a full scheduler:

```
warden spawn --at "2026-06-11T09:00:00" "run the morning audit"
```

Spawns a timer goroutine in the daemon that fires at the specified time. Single-shot, no recurrence. Survives daemon restart if persisted to store. Lower complexity than a full scheduler; addresses "I want to queue this for later" without crontab.

This is a separate decision — not blocked by or blocking the scheduler decision.

---

## Revisit Criteria

Reconsider native scheduling if:
- A concrete set of recurring pipeline use cases emerges that genuinely can't be served by OS cron
- A "read-only scheduled monitoring" use case appears (no approvals needed → conflict with supervised model goes away)
- The warden user base grows to include teams that want in-app schedule management

---
title: Backend hard-limit recovery
description: When an agent hits a confirmed provider hard limit, warden automatically tries the next eligible backend so the agent keeps working without operator intervention.
---

When a coding agent hits a provider hard limit — Claude's 5-hour session clock, a weekly cap, or a monthly spend cap — warden can do more than pause and wait. The **reactive backend recovery coordinator** picks the next eligible subscription backend from the [backend registry](/warden/guides/backend-registry/), switches the agent over through the existing hot-swap lifecycle, and only clears the recovery once the new backend demonstrably stabilizes.

No operator action is required unless every known backend is exhausted.

## How it works

### Detection

A confirmed `rate_limited` status transition (from the poller detecting a provider hard-limit banner) starts at most one recovery **generation** per agent. A duplicate transition while a generation is already running is a no-op — the coordinator owns the session exclusively.

### Usage refresh

The coordinator reads authoritative backend-usage windows from `internal/backendusage`. Each window carries:

- A stable **scope** key (the provider's constraint identity)
- **Model selectors** (which models the window applies to)
- An optional `used_percent` (nil = unknown, never fabricated as zero)
- An optional `resets_at` (nil when the provider supplies no reset time)

### Candidate ranking

Eligible candidates are `(backend, model)` pairs from the backend registry where:

- The backend is **installed**, **enabled**, not local-only, and on a **subscription or free** tier
- The model is **enabled** and has **auto-assign** set
- Existing tier and role rules are respected (a tier-1 role gets tier-1 candidates first)

Candidates are ranked by **minimum known headroom** across all applicable usage windows:

- `headroom = clamp(100 - used_percent, 0, 100)` per window
- The tightest overlapping constraint is authoritative
- **Unknown headroom** (no data or refresh failure) ranks after known-positive headroom — never fabricated as zero or 100%

### Switching

- If the top candidate is the **original pool**, the coordinator uses the same-backend `Restore` path (same process, same key/prompt).
- Any other candidate uses the existing `HotSwap` lifecycle (hot-swap handoff, new process, same session identity).

Both paths preserve the session ID, worktree, branch, pipeline job, Autopilot run/task, role, parentage, and all tags.

### Stabilization

A candidate is declared stable only after the agent stays in a **live non-rate-limited status** (working, idle, or waiting-for-input) for at least `rate_limit.recovery.stabilization_window` (default 10 seconds). A process launch alone is insufficient.

If the candidate is **immediately rate-limited** after switching, the attempt is recorded as `immediate_hard_limit`, and the coordinator advances to the next candidate in the same generation.

### Exhaustion and waiting

When no unattempted candidate with positive known headroom remains, the agent transitions to `waiting_for_capacity`. The coordinator:

1. Persists the full attempt list, generation, and known reset times
2. Schedules a retry at the earliest known `resets_at` across all limited pools — or falls back to `rate_limit.retry_interval` (default 30 m) when no reset time is available
3. Fires the retry automatically when the timer fires

If the original pool is restored and eligible, it resumes in place. If a different candidate wins the re-ranking, `HotSwap` is used.

### Manual actions always win

A manual `warden switch`, stop, or delete **supersedes** automatic recovery immediately:

1. The recovery record is cleared
2. The active timer is cancelled
3. A `backend_recovery_superseded` event is emitted
4. Stale timers and late callbacks are generation-guarded and become no-ops

## Recovery state in the UI

The agent's status shows the current phase wherever status is shown (TUI, web, CLI `warden status <id>`, API, MCP):

| Phase | Displayed as |
|---|---|
| `refreshing_usage` | `recovering: refreshing usage` |
| `switching` | `recovering: trying <backend>/<model>` |
| `stabilizing` | `recovering: stabilizing <backend>/<model>` |
| `waiting_for_capacity` | `waiting for capacity; retry <time>; attempted N` |

The `backend_recovery` field on every session object is non-null while recovery is active. It contains `phase`, the current candidate, the full attempt list, known reset times per scope, and `next_retry_at`. Null means no recovery is active.

SSE subscribers see a notification on every phase transition so the web UI and any connected client stay current without polling.

## Event log

Recovery emits these events to the agent's event log (visible in `warden status <id>` / TUI `i` pane / `get_agent` MCP):

| Event | When |
|---|---|
| `backend_recovery_started` | First hard-limit detection |
| `backend_usage_refreshed` | After each usage read (ok or unavailable) |
| `backend_recovery_candidate_selected` | A candidate is chosen |
| `backend_recovery_switch_started` | HotSwap or Restore begins |
| `backend_recovery_stabilizing` | Switch succeeded; stabilization window started |
| `backend_recovery_attempt_failed` | Attempt outcome: `launch_failed` or `immediate_hard_limit` |
| `backend_recovery_waiting_for_capacity` | Exhaustion; retry scheduled |
| `backend_recovery_stabilized` | Recovery complete; candidate confirmed stable |
| `backend_recovery_superseded` | Manual action cancelled the recovery |

Events never contain provider credentials, account identifiers, raw terminal banners, prompts, or transcripts. Usage percentages appear only on local authenticated surfaces; notifications say "capacity known/unknown" and the reset time.

## Configuration

```yaml
rate_limit:
  recovery:
    enabled: true              # set false to fall back to same-backend auto-resume only
    stabilization_window: 10s  # Go duration; how long candidate must stay live before clearing
```

The defaults are safe for most users. Lower `stabilization_window` to clear recovery faster in tests; raise it if false-positive stabilizations appear in a flaky environment.

## Deprecated fields

`handover.threshold_percent` and `handover.rolling_quota_threshold` are **deprecated** as of this release. They are decoded for one compatibility window but have no effect on routing — reactive recovery replaced proactive quota prediction. `context_fill_threshold` (the context handoff trigger) is unchanged and still active.

## Observing recovery state from an agent or MCP

```bash
# CLI
warden status <id> --json | jq .backend_recovery

# MCP
get_agent {ticket: "<id>"}
# → session.backend_recovery.phase, .current, .attempts, .next_retry_at

# list_agents includes backend_recovery on every session row
list_agents {}
```

## Interaction with pipelines and Autopilot

A recovering pipeline job stays `running/recovering` — never falsely `failed`. The Autopilot guardian observes recovery state but does not intervene; the coordinator is the sole recovery owner. Run/task ownership, slot IDs, and plan parentage are preserved end to end.

## Restart safety

If the daemon restarts while an agent is `waiting_for_capacity`, the coordinator reconstructs exactly one retry timer from persisted state on startup — no retry is lost or duplicated.

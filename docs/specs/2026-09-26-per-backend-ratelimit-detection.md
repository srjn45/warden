# Per-Backend Rate-Limit Detection — Design Spec

**Date:** 2026-09-26  
**Status:** Frozen (Phase 0)  
**Integration branch:** `autopilot/per-backend-ratelimit-detection`

---

## Problem

`poller/detect.go detectRateLimit()` matches only Claude's banner text.
`classify()` in `internal/poller/poller.go` calls it unconditionally, so the
rate-limit check is tied to Claude's exact pane wording. Downstream,
`RateLimitScheduler.limitClearsAt()` in `internal/daemon/ratelimit.go` calls
`poller.ParseRestoreTime(sess.LastPaneExcerpt)` and
`poller.SpendLimitBannerPresent(sess.LastPaneExcerpt)` — both Claude-specific.

**Impact for non-Claude backends:** `StatusRateLimited` is never set, so
`BackendRecoveryCoordinator.OnHardLimit` never fires. The agent sits
orphaned or stuck indefinitely with no handover, no timer, no recovery attempt.

---

## D1 — `RateLimitDetector` optional interface

**File:** `internal/agentbackend/backend.go`

Add an optional interface that backends may implement:

```go
// RateLimitDetector is an optional capability a backend may implement to detect
// rate-limit conditions from captured pane text. classify() prefers this over the
// package-level detectRateLimit() when present.
type RateLimitDetector interface {
    // DetectRateLimit inspects the captured pane text and reports whether the
    // agent is currently rate-limited. When limited, resetAt is the expected
    // clear time (zero when unknown); resetKnown is false when the pane carries
    // no parseable reset time (e.g. a spend cap) and the caller should apply a
    // fallback interval.
    //
    // Implementations MUST fail closed: only return limited=true when the pane
    // conclusively shows a rate-limit condition (anchored to trailing lines).
    // Returning true on ambiguous output would misclassify a working agent.
    DetectRateLimit(pane string) (limited bool, resetAt time.Time, resetKnown bool)
}
```

**Change to `classify()`** (`internal/poller/poller.go`):

- Check whether the backend implements `RateLimitDetector`.
- If it does, call `b.DetectRateLimit(pane)` instead of the package-level `detectRateLimit(pane)`.
- If it does not (or `b == nil`), fall back to `detectRateLimit(pane)` as today.
- `detectRateLimit()` is **NEVER deleted** — backward compatibility requires it
  as the permanent Claude fallback for callers that do not implement the interface.

---

## D2 — `RateLimitResetParser` optional interface and scheduler refactor

**File:** `internal/agentbackend/backend.go`

Add a second optional interface:

```go
// RateLimitResetParser is an optional capability a backend may implement to
// extract a rate-limit reset time from captured pane text.
// RateLimitScheduler.limitClearsAt() prefers this over the Claude-specific
// poller helpers when present.
type RateLimitResetParser interface {
    // ParseRateLimitReset extracts the reset time from the pane. ok is false
    // when no parseable reset time is present and the scheduler should apply
    // its configured fallback.
    ParseRateLimitReset(pane string) (resetAt time.Time, ok bool)
}
```

**Change to `RateLimitScheduler.limitClearsAt()`** (`internal/daemon/ratelimit.go`):

Add a `BackendResolver` field to `RateLimitScheduler`:

```go
// BackendResolver, when set, resolves the backend for a session so
// limitClearsAt() can prefer the backend's RateLimitResetParser over the
// Claude-specific poller helpers.
BackendResolver func(sess *store.Session) agentbackend.Backend
```

Refactor `limitClearsAt()` to try parse sources in order:

1. Session backend's `RateLimitResetParser.ParseRateLimitReset(sess.LastPaneExcerpt)` — if the backend implements the interface and returns `ok=true`.
2. `poller.ParseRestoreTime(sess.LastPaneExcerpt)` — Claude legacy clock-time parser.
3. `poller.SpendLimitBannerPresent(sess.LastPaneExcerpt)` — Claude spend-cap legacy (returns `spendRetryInterval`).
4. `retryInterval` — final fallback.

Sources are tried in this order on every call; the first that yields a usable
reset time wins.

---

## D3 — Per-backend `RateLimitDetector` implementations (Phase 2)

One `RateLimitDetector` (and optionally `RateLimitResetParser`) implementation
per backend adapter. All implementations **fail closed**: only return
`limited=true` when the pane conclusively shows a rate-limit condition, anchored
to the trailing `limitBannerTailLines` lines.

### Claude (`internal/agentbackend/backends/claude/`)
- Wraps `claudeLimitBannerRe` and `claudeSpendLimitRe` from `poller/detect.go`
  (or reproduces them inline).
- Also implements `RateLimitResetParser`: delegates to `poller.ParseRestoreTime`.

### Codex (`internal/agentbackend/backends/codex/`)
- `DetectRateLimit`: keywords "usage limit", "rate limit", or "quota" combined
  with "exceeded", "reached", or "hit"; OR an HTTP 429 / "Too Many Requests"
  literal. Anchored to trailing lines.
- `ParseRateLimitReset`: delegates to `poller.ParseRestoreTime` (reuses its
  clock-time patterns where Codex mirrors Claude's output format).

### Cursor (`internal/agentbackend/backends/cursor/`)
- `DetectRateLimit`: keywords "rate limit", "quota exceeded", or "Usage limit
  reached". Anchored to trailing lines.
- `ParseRateLimitReset`: generic clock-time parser (extracts `HH:MM` / `H:MMam`)
  from the captured line; returns `ok=false` when no time is present.

### Antigravity (`internal/agentbackend/backends/antigravity/`)
- `DetectRateLimit`: keywords "rate limited", "quota", or "limit reached".
  Anchored to trailing lines.
- `ParseRateLimitReset`: extracts "resets at HH:MM" if present; returns
  `ok=false` otherwise.

### Aider (`internal/agentbackend/backends/aider/`)
- `DetectRateLimit`: keywords "rate_limit_error", "RateLimitError", "429", or
  "quota exceeded". Anchored to trailing lines.
- `ParseRateLimitReset`: returns `(time.Time{}, false)` — Aider carries no
  in-band reset time.

### OpenCode, Crush, Goose
These backends use a Tea TUI and emit no reliable pane-readable text markers.
Their `DetectRateLimit` implementations return `(false, time.Time{}, false)` —
explicitly **pane-blind**. Rate-limit detection for these backends is covered by
the Phase 3 usage-API polling fallback (D4).

---

## D4 — Usage-API polling fallback for pane-blind backends (Phase 3)

`runUsageSync` in `daemon/usage_sync.go` already calls
`backendusage.SyncToStore` after every Snapshot, which populates each backend's
usage record in the store. Phase 3 adds one additional step:

**New function:** `func (s *Server) limitSessionsFromSnapshot(ctx context.Context, snap backendusage.Snapshot)`

Logic:
1. For each backend entry in `snap` where `status == "rate_limited"`:
   a. Skip backends that implement `RateLimitDetector` — they are handled by
      `classify()` on the next poll tick; triggering here would double-fire.
   b. Enumerate all active sessions (not already `StatusRateLimited`) on that
      backend.
   c. For each session: call `store.UpdateStatusIf(ctx, sess.ID, currentStatus, store.StatusRateLimited)`.
   d. If the CAS swap succeeds, call `RateLimitScheduler.OnTransition(sess, currentStatus, store.StatusRateLimited)`.
2. When `UsageLimit.ResetsAt` is non-zero, use it as the reset time override in
   `limitClearsAt()` (the `BackendResolver` path already supports this via
   `RateLimitResetParser`; the snapshot-side implementation stores `ResetsAt`
   into the session's `LastPaneExcerpt`-equivalent or passes it through a new
   field — exact mechanism deferred to Phase 3 implementation).

**Invariant:** `limitSessionsFromSnapshot` is the authoritative transition
source for pane-blind backends. `classify()` will not fire `StatusRateLimited`
for them because their `DetectRateLimit` returns false. There is no
double-trigger risk.

---

## D5 — Preserved invariants

The following components are **not changed** in any phase of this feature:

- `BackendRecoveryCoordinator` — already backend-agnostic; wired via `OnHardLimit`.
- `RateLimitScheduler` timer logic — `scheduleResume`, `attemptResume`, `advance`,
  `waitLocked`, `verifyStable` remain untouched.
- `RateLimitScheduler.OnTransition` call sites — only the `limitClearsAt()` parse
  ordering changes (D2); the transition hook itself is unchanged.

The only structural change to `RateLimitScheduler` is adding the
`BackendResolver` field (D2). All existing callers that leave it nil continue to
work exactly as before — `limitClearsAt()` falls through to the Claude legacy
path.

---

## D6 — Conformance test (Phase 2)

**File:** `internal/agentbackend/backends/conformance_test.go`

Add a `RateLimitDetector` conformance test exercised against all registered
backends:

```
For each registered backend b:
  - If b implements RateLimitDetector:
      - synthetic limited pane (contains a known limit phrase for that backend)
        → expect (true, ?, ?)
      - clean pane (ordinary idle output)
        → expect (false, time.Time{}, false)
  - If b does not implement RateLimitDetector:
      - record it as "pane-blind" in the test log — NOT a test failure.
        (Pane-blind backends are covered by the Phase 3 usage-API fallback.)
```

The test uses `t.Run(b.ID(), ...)` for per-backend subtests so a failure in one
backend's detector is isolated and named.

---

## Phasing

| Phase | Scope | Key files |
|---|---|---|
| **Phase 1** | Interface definitions + `classify()` and `limitClearsAt()` refactor to dispatch through the new interfaces. No per-backend impls yet — Claude falls through to existing `detectRateLimit()` / `ParseRestoreTime` unchanged. | `internal/agentbackend/backend.go`, `internal/poller/poller.go`, `internal/daemon/ratelimit.go` |
| **Phase 2** | Per-backend `RateLimitDetector` / `RateLimitResetParser` impls (Claude, Codex, Cursor, Antigravity, Aider; pane-blind stubs for OpenCode/Crush/Goose) + conformance test. | `internal/agentbackend/backends/*/`, `internal/agentbackend/backends/conformance_test.go` |
| **Phase 3** | Usage-API polling fallback for pane-blind backends: `limitSessionsFromSnapshot` in `daemon/usage_sync.go`. | `internal/daemon/usage_sync.go` |
| **Phase 4** | Remove the `handover_settings` gate on `BackendRecoveryCoordinator.OnHardLimit` (universal recovery for all backends) + integration wiring + integration test + DoD docs update (README, docs/FEATURES.md, site, skill). | `internal/daemon/backend_recovery.go`, docs/, site/, skills/warden/ |

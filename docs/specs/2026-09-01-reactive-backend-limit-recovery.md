# Reactive Backend Hard-Limit Recovery — Implementation Plan

**Date:** 2026-09-01  
**Status:** Approved design; implementation pending  
**Scope:** Documentation of the implementation plan only

## 1. Outcome and non-goals

Replace proactive provider-quota threshold prediction with recovery triggered by a confirmed hard usage limit. A poller transition into `store.StatusRateLimited` is the only automatic provider-capacity trigger. Warden will mark the exact backend/model pool limited, refresh authoritative usage windows, rank eligible backend/model candidates, hand off through the existing daemon lifecycle, and keep trying candidates sequentially until one demonstrably stabilizes. If none does, it will persist a restart-safe `waiting_for_capacity` recovery and retry at known reset times.

This change does not alter context-fill detection, context compaction, or their thresholds. It deprecates only `threshold_percent` and `rolling_quota_threshold` as quota-switching controls. It does not predict exhaustion from locally counted tokens, invent missing provider usage, scrape terminal output for usage, execute `warden backend usage`, introduce a second switch implementation, or give the Autopilot guardian an independent recovery loop.

## 2. Verified current behavior and semantic contract

Phase 1 must record this baseline in tests before changing behavior:

- `internal/poller/poller.go` classifies a recognized provider hard-limit banner as `store.StatusRateLimited` and invokes the daemon transition chain.
- `internal/daemon/ratelimit.go` already defines `RateLimitScheduler.OnHardLimit`, calls it before scheduling resume, and `internal/daemon/ratelimit_test.go` covers handled, unhandled, and auto-resume-disabled cases. The verified integration gap to preserve in the baseline inventory is that production does not assign/wire this hook; implementation must make the recovery coordinator its sole production owner rather than add another independent callback path.
- Manual `warden switch` already works through the daemon lifecycle: `internal/cli/switch.go` calls `internal/client/client.go`, the daemon endpoint validates/owns mutation, and `internal/lifecycle/switch.go` writes the handoff, retires the old process, launches the successor in the same worktree, and mutates backend/model on the existing session.
- `internal/router/resolver.go` currently ranks model entries with backend-level quota/headroom and threshold rejection. `internal/backendstore/types.go`, `store.go`, and `quota.go` persist backend catalog, handover settings, and synthetic/backend-level quota data. Those quota records are not the new source of provider usage truth.
- `internal/daemon/strict_models.go`, `internal/client/client.go`, `internal/daemon/apidocs/openapi.yaml`, generated `internal/daemon/oapi/api.gen.go`, MCP surfaces, and TUI currently expose the handover settings and therefore form one migration surface.

The implementation contract is:

1. A confirmed `StatusRateLimited` transition starts at most one recovery generation per agent.
2. The limited identity and every candidate identity are `(backend_id, model_id)`; backend-only exclusion is invalid when a provider has independent model pools.
3. Existing tier, requested role, model `Enabled`/`AutoAssign`, backend installed/enabled, subscription/free/pay-per-use, and local-backend eligibility rules remain authoritative. Recovery may not silently widen them.
4. Launching a successor process is an attempt, not success. Success requires a stabilization observation.
5. Manual switch, stop, or delete wins over automatic work and invalidates its timers/callbacks.
6. A pipeline or Autopilot agent remains the same logical session in the same worktree, branch, ownership domain, tags, role, pipeline job, and Autopilot run/task. Only its driver backend/model changes.

## 3. Backend-usage dependency and data semantics

All provider usage comes from the `internal/backendusage` service delivered by the separate **backend-usage-command** PR. The daemon consumes the Go service directly; it must never execute a CLI command or parse human/JSON CLI output. This plan assumes the dependency exposes or can be adapted behind this narrow contract:

```go
package backendusage

type Window struct {
    Scope          string     // stable provider-defined constraint identity
    Label          string     // display-only; never identity
    ModelSelectors []string   // exact ids and/or documented selector syntax
    UsedPercent    *float64   // nil means unknown, not zero
    ResetsAt       *time.Time // nil means provider supplied no reset
}

type BackendUsage struct {
    BackendID string
    Windows   []Window // zero or more
    RefreshedAt time.Time
}

type Service interface {
    Refresh(ctx context.Context, backendIDs ...string) ([]BackendUsage, error)
}
```

If the merged dependency names these types differently, add a small coordinator-side adapter; do not duplicate its collectors, persistence, cache, parsing, or selectors. `Scope` must be stable across refreshes and daemon restarts. `Label` is safe for operator display but cannot be a key. `UsedPercent` and `ResetsAt` remain nullable end to end through Go types, persisted state, OpenAPI, clients, and UI.

### Applicability and headroom

For candidate `C=(backend, model)`, let `W(C)` be every window for that backend whose selectors match the model. A backend may have zero windows, multiple alternative model pools, and overlapping global/model constraints.

- For each applicable window with known `used_percent`, remaining headroom is `clamp(100-used_percent, 0, 100)`.
- Candidate known headroom is the minimum remaining headroom across **all** applicable known windows. This makes the tightest overlapping constraint authoritative.
- No matching windows, an empty window list, a refresh failure, or only nullable `used_percent` values means usage is unknown. Unknown candidates remain eligible for sequential trial and are ranked after candidates with positive known headroom (with deterministic existing tie-breaking); they are not assigned fake `0%`, `100%`, or average usage.
- A confirmed hard limit overlays a limited marker on the exact `(backend, model)` pool and applicable stable scopes. It must not automatically exclude unrelated models on the backend. Where the provider reports a global applicable window, that real overlapping constraint can exclude all models it covers.
- Reset scheduling uses all exhausted/limited applicable windows with known future `resets_at`; the next retry is the earliest time at which at least one not-yet-attempted or previously limited candidate might be viable. If none supplies a reset, use the configured rate-limit retry fallback and retain null reset data.

Required fixtures include Codex short and weekly windows both applying to a model, and Antigravity windows that independently select Gemini and non-Gemini pools plus a global overlapping constraint.

## 4. Proposed ownership, files, and interfaces

### New coordinator package

Add `internal/recovery` (name may be `internal/daemon/recovery` if repository package boundaries require daemon ownership):

```go
type CandidateID struct { BackendID, ModelID string }

type Phase string
const (
    PhaseDetecting Phase = "detecting"
    PhaseRefreshing Phase = "refreshing_usage"
    PhaseSelecting Phase = "selecting_candidate"
    PhaseSwitching Phase = "switching"
    PhaseStabilizing Phase = "stabilizing"
    PhaseWaiting Phase = "waiting_for_capacity"
)

type Attempt struct {
    Candidate CandidateID
    Generation uint64
    StartedAt time.Time
    Outcome string // immediate_hard_limit|launch_failed|stabilized|superseded
}

type RecoveryState struct {
    SessionID string
    Generation uint64
    Phase Phase
    Limited CandidateID
    Attempted []Attempt
    WindowResets map[string]*time.Time // stable scope key; null is preserved
    NextRetryAt *time.Time
    UpdatedAt time.Time
}

type UsageSource interface {
    Refresh(context.Context, ...string) ([]backendusage.BackendUsage, error)
}

type Switcher interface {
    HotSwap(context.Context, *store.Session, lifecycle.SwapRequest) (*lifecycle.SwapResult, error)
}

type StateStore interface {
    Get(context.Context, string) (RecoveryState, error)
    Put(context.Context, RecoveryState) error
    Delete(context.Context, string, uint64) error // generation-checked
    ListWaiting(context.Context) ([]RecoveryState, error)
}
```

The coordinator owns per-agent locks, generation checks, candidate attempts, stabilization watchers, retry timers, persistence, and audit emission. Inject a clock/timer factory and poll/status observer for deterministic tests. Persist recovery records in a dedicated ScrivaDB collection under the daemon data directory; do not overload transient in-memory `RateLimitScheduler.timers`.

### Existing files to change in implementation PRs

- `internal/daemon/ratelimit.go` and tests: keep detection/resume responsibilities; make `OnHardLimit` enqueue/claim a coordinator generation and return handled only when recovery owns either switching or persisted waiting. Avoid recursive recovery from coordinator-authored status changes.
- Daemon construction (currently near scheduler/poller wiring): construct one coordinator, inject `backendusage.Service`, store, resolver/ranker, lifecycle, and notifier; wire the existing hook exactly once; reconstruct waiting records once at startup.
- `internal/poller/poller.go` and tests: retain confirmed-banner classification and expose enough transition metadata to distinguish a candidate's immediate repeat hard limit during stabilization. Do not add proactive usage polling here.
- `internal/router/resolver.go` and tests: factor catalog eligibility from ranking; add recovery ranking over `CandidateID` and usage windows. Remove quota-threshold rejection while retaining deterministic tier/role/fallback and paid/local rules.
- `internal/lifecycle/switch.go` and tests: reuse `HotSwap`; add explicit candidate selection and recovery generation/reason metadata if needed. Preserve identity fields not owned by the switch. Do not treat `launchSuccessor` returning nil as stabilized.
- `internal/backendstore/types.go`, `store.go`, and `quota.go`: migrate handover settings; retain limited data only where needed for compatibility/audit, and prevent legacy synthetic quota from driving recovery. Exact model-pool hard-limit overlays should live with recovery/backend-usage state rather than backend-only `SetBackendLimited`.
- `internal/daemon/strict_models.go`, `internal/client/client.go`, OpenAPI, generated clients/types, MCP tools, and command help: expose migration-safe settings and recovery/status data. Edit the OpenAPI source first and regenerate; do not hand-edit generated code.
- TUI list/detail/cockpit models and tests: display recovery phase, attempted target, next retry, and known reset labels without leaking raw provider payloads.
- Autopilot controller/runtime and pipeline status projections: observe coordinator state only; preserve ownership/session graph and suppress guardian duplicate recovery.

## 5. State machine and concurrency

```text
normal
  -- confirmed StatusRateLimited --> detecting[g]
detecting[g] --> refreshing_usage[g] --> selecting_candidate[g]
selecting_candidate[g] -- candidate --> switching[g,c] --> stabilizing[g,c]
stabilizing[g,c] -- stable observation window --> normal (clear recovery)
stabilizing[g,c] -- immediate hard limit --> mark c; selecting_candidate[g]
switching[g,c] -- launch/handoff failure --> record failure; selecting_candidate[g]
selecting_candidate[g] -- exhausted --> waiting_for_capacity[g]
waiting_for_capacity[g] -- retry/reset --> refreshing_usage[g]
waiting_for_capacity[g] -- capacity restored --> switching[g,c]
any automatic state -- manual switch/stop/delete --> superseded; generation g+1
```

The first transition atomically increments and claims the session generation under its per-agent lock. Every asynchronous callback carries `(session_id, generation, candidate)` and re-reads state before mutation. A mismatched generation is a no-op. Coordinator-triggered lifecycle/status events carry origin metadata (or an equivalent guarded context) so they cannot recursively start a second recovery.

Stabilization is a bounded observation window defined in tests/config as consecutive poll observations or elapsed time in a non-rate-limited live status, with the successor process/session still present. `spawning` alone and a successful process launch are insufficient. An immediate `StatusRateLimited` for the attempted candidate records the attempt and continues the same generation with the next candidate. Candidate ordering is snapshotted per refresh and deterministic; no candidate is attempted twice in one generation unless a known reset has elapsed and a fresh usage refresh makes it eligible again.

Manual `switch` first invalidates/cancels automatic state, then follows the existing switch path. Manual stop/delete invalidates the generation before lifecycle teardown and removes or tombstones recovery state as appropriate. Stale timers, late poll events, and late switch completions cannot resurrect or overwrite the manual result.

## 6. Exhaustion, restart, and restoration

When ranking yields no unattempted candidate, persist `PhaseWaiting` with the full attempted candidate list, generation, nullable reset times by stable scope, and `next_retry_at` before arming a timer. The session remains operator-visible as `waiting_for_capacity`; do not report it as errored or as a successful switch.

On daemon startup, the coordinator lists waiting records and reconciles each against the live session/store:

1. Acquire the per-agent lock and verify the persisted generation is still current.
2. Skip/tombstone deleted, stopped, manually switched, or otherwise superseded sessions.
3. Register exactly one timer or immediately run one refresh when `next_retry_at` is past; idempotent startup and repeated reconciliation must not double resume.
4. Refresh backend usage directly and re-rank. If the original `(backend, model)` is restored and eligible, resume it through the existing configured prompt/bare-key path. If another candidate wins, use the existing switch/handoff lifecycle.
5. Require stabilization, then atomically clear recovery and timer. A fresh immediate hard limit returns to sequential selection in the same generation.

Only restoration of the exact original `(backend, model)` may resume in place without an unnecessary handoff or session-identity change. Every different candidate must use `Lifecycle.HotSwap`, including a different model pool on the same backend as well as a different backend. Both paths preserve the prompt/key semantics already implemented by `RateLimitScheduler` and both are exactly-once at the coordinator boundary.

## 7. Configuration migration

Keep `handover.enabled`, `context_fill_threshold`, and `cooldown_period` (where the latter still governs context handover) unchanged. Deprecate only:

- `threshold_percent`
- `rolling_quota_threshold`

Migration steps:

1. Stop consulting both fields for provider quota switching/routing in the first behavior-changing release.
2. Continue decoding and returning them for one compatibility window, mark them deprecated in OpenAPI/JSON schema/MCP descriptions, and emit a once-per-load warning that confirmed hard-limit recovery replaced predictive quota thresholds. Do not reinterpret either as a context threshold.
3. Preserve stored unknown fields during rolling upgrade. Existing values require no translation and have no effect on recovery.
4. Update CLI/TUI labels and examples. `context_fill_threshold` remains editable and explicitly labeled as context-only.
5. In a later removal release, delete fields from `HandoverSettings`, backend store defaults/migrations, strict handlers, client DTOs, OpenAPI, regenerated surfaces, tests, and docs after the compatibility policy permits.

New recovery tuning, if configuration is required, belongs under `rate_limit.recovery` (for example `enabled`, `stabilization_window`, and retry bounds), with safe defaults. It must not include a usage-percent trigger.

## 8. Events, audit, privacy, and UI

Use stable event/audit names:

- `backend_recovery_started`
- `backend_pool_limited`
- `backend_usage_refreshed`
- `backend_recovery_candidate_selected`
- `backend_recovery_switch_started`
- `backend_recovery_stabilizing`
- `backend_recovery_attempt_failed`
- `backend_recovery_waiting_for_capacity`
- `backend_recovery_retry_scheduled`
- `backend_recovery_resumed_same_backend`
- `backend_recovery_switched_backend`
- `backend_recovery_stabilized`
- `backend_recovery_superseded`

Audit fields are session id, generation, backend/model ids, stable window scopes, nullable used percent/reset timestamps, attempt ordinal/outcome, reason, and next retry. Never record provider credentials, account/email identifiers, raw CLI/provider responses, full terminal banners, prompts, transcripts, or handoff contents. Treat provider labels as display data: sanitize/control length and do not use them as keys. API/TUI must not turn unknown percentages into zero or “full”; render `unknown`. Exact percentages may be shown only on existing local authenticated surfaces; notifications should say capacity known/unknown and the reset time, not expose account metadata.

CLI, API, TUI, and web status should distinguish:

- `recovering: refreshing usage`
- `recovering: trying <backend>/<model> (n/m)`
- `recovering: stabilizing <backend>/<model>`
- `waiting for capacity; retry <time|fallback>; attempted n`
- `recovery superseded by manual <switch|stop|delete>`

Candidate detail may show each applicable window and the minimum known headroom. Unknown windows remain visibly unknown. SSE/store notifications fire on phase changes, not every stabilization poll.

## 9. Pipeline, Autopilot, and guardian integration

Recovery mutates only backend/model driver fields and recovery metadata. It must preserve session ID/name, parent/child relationships, `PipelineID` and pipeline job identity/state, Autopilot run/task tags and ownership, role, repo/workdir/worktree, branch, all user/system tags, permission mode, and launch provenance. A pipeline job remains running/recovering rather than failed merely because capacity is being tried or awaited.

Autopilot's manager sees the same status projection as any other owner. Its guardian may notify/escalate stalled recovery, but must not mark backends independently, invoke switch, resume, or schedule retry. Remove/disable the guardian's legacy quota-threshold selection and make the coordinator the single recovery owner. Ownership guards still apply to operator-destructive actions; automatic recovery does not bypass them.

## 10. Implementation phases and dependency sequencing

### Phase 1 — Current behavior inventory and semantic contract

Add characterization tests for poller transition, `RateLimitScheduler.OnHardLimit`, production wiring absence, manual daemon switch, lifecycle identity preservation, threshold routing, guardian behavior, and UI/API fields. Record exact seams and define stabilization/manual-override semantics. **This phase can start before backend-usage-command merges.** Avoid importing speculative backendusage types.

### Phase 2 — Usage-window dependency

After backend-usage-command merges to `main`, branch from that updated `main`. Adopt its `internal/backendusage` types/service, add only the minimal adapter required, and test selector applicability, null propagation, refresh errors, alternative pools, overlapping windows, and minimum-headroom calculation.

### Phase 3 — Coordinator

Add persistent recovery state/store, per-agent lock/generation, deterministic ranking, events, injected clock/timers, and startup reconciliation. No lifecycle switch yet; exercise state transitions with fakes.

### Phase 4 — Switch integration and stabilization

Wire the existing `OnHardLimit` exactly once to the coordinator. Reuse explicit-candidate `Lifecycle.HotSwap` and existing same-backend prompt/key resume. Add live-status stabilization; launch is not success.

### Phase 5 — Sequential fallback

On launch failure or immediate hard limit, persist the outcome, mark the exact pool/scopes, refresh/re-rank as needed, and try the next candidate deterministically without recursion.

### Phase 6 — Wait and restart recovery

Persist exhaustion, attempts, generation, reset times, and retry; reconstruct exactly once after restart; support same-backend resume and different-backend handoff after capacity returns.

### Phase 7 — Threshold deprecation

Remove proactive quota threshold behavior, add compatibility decoding/warnings, update config/OpenAPI/MCP/client/generated surfaces, and retain context-fill/compaction settings.

### Phase 8 — UI, pipeline, and Autopilot integration

Add statuses/details/events, preserve all ownership and topology metadata, and make guardian observation-only for recovery.

### Phase 9 — Independent review

Have a reviewer trace every transition and destructive/manual race, inspect privacy and generated-surface changes, run the full configured repository checks, and execute focused race/restart tests. Fix findings before landing.

**Sequencing constraint:** Phase 1 inventory/contract may be developed independently before backend-usage-command lands. Every code change that imports or consumes `internal/backendusage` types/service, including Phase 2 and all later coordinator, lifecycle, persistence, migration, and UI integration, must branch only after the backend-usage-command PR has merged to `main`. Do not stack later work on an unmerged dependency or recreate its API locally.

## 11. Comprehensive test matrix

| Area | Scenario | Required assertion |
|---|---|---|
| Detection | working to confirmed `StatusRateLimited` | One generation starts; exact backend/model recorded |
| Detection | repeated same transition/callback | No duplicate generation, switch, or timer |
| Existing hook | handled/unhandled; auto-resume off | Existing scheduler contract remains covered |
| Codex windows | short + weekly overlap | Candidate headroom is minimum remaining known headroom; both resets persist |
| Codex windows | one known, one unknown | Known minimum used; unknown preserved and displayed, not fabricated |
| Antigravity pools | Gemini pool limited | Non-Gemini model remains independently eligible when selectors permit |
| Antigravity pools | non-Gemini pool limited | Gemini alternative remains independently eligible |
| Overlap | pool window plus global constraint | Global exhaustion constrains every matched pool |
| Zero windows | backend reports none | Candidate remains eligible as unknown |
| Refresh failure | service unavailable/partial | No fabricated usage; unknown eligible; audit records error class only |
| Eligibility | disabled/uninstalled/local/free/paid | Existing rules unchanged; paid only when already authorized |
| Tier/role | recovery candidate ranking | Requested tier/role and fallback policy preserved |
| Identity | two models on same backend | Attempts and limited markers remain distinct `(backend, model)` keys |
| Stabilization | process launches, then immediate limit | Not successful; record attempt and try next candidate |
| Stabilization | live non-limited observations for window | Recovery clears exactly once |
| Sequential | first launch fails, second stabilizes | Ordered attempts recorded; second wins |
| All exhausted | no candidate available | Persist `waiting_for_capacity`, attempts, generation, resets, next retry |
| Restoration | original pool resets | Resume same backend/model via existing key/prompt path |
| Restoration | different pool becomes best | Switch through handoff lifecycle and stabilize |
| Restart | daemon dies while waiting | One reconstructed retry, same generation, no duplicate resume |
| Restart | daemon dies while stabilizing | Reconcile process/status safely; stabilize or continue once |
| Manual switch | during refresh/switch/wait | Manual target wins; automatic callback/timer is stale and harmless |
| Stop during recovery | during switch/stabilize/wait | Stop wins; no relaunch, retry, or state resurrection |
| Delete | stale timer after deletion | No-op and recovery record cleaned/tombstoned |
| Recursive event | attempted candidate immediately limited | Same generation advances once; no nested coordinator invocation |
| Pipeline job | candidate switch and wait | Job/session/worktree/branch/tags/parentage remain intact; not falsely failed |
| Autopilot worker | hard limit | Run/task ownership and tags preserved; guardian does not duplicate recovery |
| Privacy | events/API/TUI | No credential, account id, raw response/banner, prompt, or transcript leakage |
| Migration | old threshold fields present | Decode plus warning; values do not trigger quota switching |
| Context | context threshold reached | Existing context handoff/compaction behavior unchanged |
| API | nullable usage/reset fields | Null round-trips through OpenAPI, generated client, daemon client, and UI |
| Race | `go test -race` focused coordinator tests | Locks/generations prevent duplicate mutation and timer races |

Use table-driven unit tests for selectors/headroom/ranking and coordinator transitions; fake clock/timers and usage service for determinism. Add daemon integration tests around poller-to-coordinator wiring, lifecycle fakes for sequential fallback, file/store reopen tests for restart, strict API/client contract tests, and TUI golden/model tests. Retain existing manual switch and rate-limit resume suites.

## 12. Acceptance criteria

- Automatic provider switching occurs only after a confirmed hard-limit transition; no percentage threshold predicts or initiates it.
- Context-fill and compaction thresholds behave exactly as before; only `threshold_percent` and `rolling_quota_threshold` are deprecated.
- Production wires the existing `RateLimitScheduler.OnHardLimit` seam to one coordinator, with no guardian or poller duplicate owner.
- Usage is read by calling `internal/backendusage.Service` directly. No subprocess or CLI-output parser exists in the recovery path.
- Each candidate is `(backend, model)`. Multiple alternative model pools and overlapping constraints work, and candidate headroom is the minimum across all applicable known windows.
- Nullable/absent usage remains unknown and eligible for trial; no layer fabricates provider data.
- Existing tier, role, enabled, installed, paid, free, and local eligibility rules are preserved.
- Candidates are attempted sequentially. A launch alone never counts as success; stabilization is observed, and an immediate hard limit advances to the next candidate.
- Exhaustion durably records `waiting_for_capacity`, attempts, generation, reset times, and next retry. Restart reconstructs one recovery and neither loses nor duplicates it.
- Capacity restoration can resume the original backend through the existing prompt/key path or switch to a different backend through the existing handoff lifecycle.
- Manual switch/stop/delete always supersedes recovery; stale timers and callbacks cannot undo it or recursively recover.
- Pipeline and Autopilot session identity, ownership, worktree, branch, role, parentage, job/task links, and tags survive recovery; guardian is observation-only.
- Status, event/audit, privacy, config migration, OpenAPI/generated, client, MCP, and TUI behavior match this plan.
- The full matrix above, focused race tests, repository-configured checks, and independent review pass before implementation lands.

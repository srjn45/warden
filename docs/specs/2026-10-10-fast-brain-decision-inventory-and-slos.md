# Fast-Brain decision inventory, SLOs and admission policy

**Date:** 2026-10-10
**Status:** Design + baseline fixtures. No controller behaviour change in this PR.
**Plan:** Fast-Brain admission control and efficiency (`plan-b0a0e236`), task
`decision-inventory-and-slos`.
**Baseline:** `main` at `3c03584e` (includes #775, #785, #858).
**Fixtures:** `internal/fastbrain/admission_baseline_test.go`,
`internal/fastbrain/testdata/incident_2026_10_timeout.json`.

## 1. Purpose

Fast-Brain (`internal/fastbrain`) is the single port (`Engine.Decide`) through
which the daemon makes cheap model-backed micro-decisions. Every decision spawns
a headless native AI CLI process. Admission today is one process-wide counting
semaphore. This document (a) inventories every `DecisionKind`, (b) records what
#775, #785 and #858 actually delivered and what they did not, and (c) defines
the SLOs, deadlines, cancellation, privacy, provider-selection and priority
policy the follow-up implementation tasks must meet, with measurable rollout
thresholds.

Non-goals: changing any controller behaviour, re-enabling activity summaries,
altering approval safety rules (the destructive-action guard stays before any
model call and is not overridable).

## 2. Delivered baseline

| PR | Delivered | Where |
|----|-----------|-------|
| #775 | `recognize_prompt`: thinking-tier read of a menu no backend parser matched; confidence ≥ 0.8; ≥ 2 options; verified against on-screen options; ≤ 2 attempts per distinct menu after an 8 s settle | `fastbrain/recognize.go`, `poller/recognize.go` |
| #785 | Known-prompts store: a recognized shape is persisted so later identical prompts are read with **no model call** (`prompt_known` event); pruned by count/age | `knownprompts`, `poller/recognize.go` |
| #858 | Bounded engine: `fast_timeout` 10 s, `thinking_timeout` 20 s, `max_concurrent` 2; in-flight coalescing of byte-identical requests; cosmetic `summarize_activity` may never take the last slot (`StatusDeferred`); `activity.enabled` kill switch honoured by the poller | `fastbrain/runner.go`, `config`, `poller.go:1220` |

#858 must not regress. Guards: `TestActivitySummaryYieldsReservedCapacity`
(existing) and `TestIncidentCosmeticStampedeLeavesCapacityForOperationalDecisions`,
`TestActivityNeverRunsWhenMaxConcurrentIsOne` (new).

### 2.1 Known gaps (each pinned by a `*Gap` test)

| # | Gap | Pinned by | Consequence |
|---|-----|-----------|-------------|
| G1 | **Semaphore-only admission.** One pool; no priority class, no queue discipline; only `summarize_activity` is treated specially. Any non-cosmetic work can hold every slot. | `TestIncidentSemaphoreOnlyAdmissionStarvesArbitrationGap`, `TestTiersShareOneAdmissionPoolGap` | A permission arbitration queues behind `commit_message`/`classify_task` and burns its caller deadline without reaching a model. |
| G2 | **Exact, in-flight-only coalescing.** Key = sha256(kind, tier, prompt). No semantic grouping, no result cache. | `TestDistinctPromptsAreNotCoalescedGap`, `TestNoCacheAndNoCircuitHealthGap` | N agents asking N slightly different questions cost N calls; a repeat one tick later costs a second call. |
| G3 | **No cache or circuit/health state.** A runner that fails or times out every time is invoked in full on each request; no negative cache, no backoff. | `TestNoCacheAndNoCircuitHealthGap` | During a provider outage every decision pays its full timeout, serially through 2 slots. |
| G4 | **Leader-context coupling.** A coalesced follower returns the leader's `Response`, including the leader's `StatusCanceled`/timeout, even if the follower's own context is live. | `TestCoalescedFollowerInheritsLeaderCancellationGap` | One caller's cancel can fail an unrelated high-priority waiter; the fail-open path then escalates to a human needlessly. |
| G5 | **Claude-only daemon runner wiring.** The daemon builds one `RunnerFunc(lc.RunClaudeP)` and passes it as both fast and thinking runner (`cli/daemon.go:269`). `RunClaudeP` uses the lifecycle's single backend `HeadlessCmd` with a fixed 30 s `claudeCallTimeout`; there is no per-kind model/provider choice and the backend registry/tier ladder is not consulted. The REPL builds its own engine (`cli/repl.go:57`) with fast tier only. | – (wiring) | "Fast" and "thinking" differ only in timeout; cost and latency are those of the default backend CLI cold start. |
| G6 | **No per-agent fairness or priority queue.** One noisy agent can fill the pool; waiters are served in Go channel order. | G1 tests | Fleet-wide head-of-line blocking. |
| G7 | **Limited telemetry.** One `slog.Info` line per call (kind, tier, duration, status, prompt hash). `Response.Duration` is **always zero** (set in a deferred closure after the value return in `run`, so only the log line sees it, and there it is queue wait + run time combined); no queue-wait/run split, no coalesced/deferred/dropped counters, no per-kind histogram, nothing on `/metrics`. | `…StarvesArbitrationGap` (asserts `Duration == 0`) | Cannot measure any SLO below from production. |
| G8 | **Redaction/bounds are per prompt builder, not engine-level.** Crash, stall and CI prompts call `Sanitize` and `clip`; arbiter, recognition and template kinds do not redact, and the arbiter prompt (`Action`/`Question`/`Options`) is unbounded. | `TestPromptRedactionAndBoundBaselineGap` | Secrets visible in a pane/diff/prompt are sent to the provider verbatim for those kinds. |
| G9 | **Cancellation of queued work is caller-only.** A caller that stops caring cancels its own context; coalesced waiters and queued `Decide`s have no engine-side age limit beyond the caller's context. | G1/G4 tests | Stale decisions (an approval already answered by a human) still run a model call. |

## 3. Decision inventory

Effective deadline = min(tier ceiling, `Request.Timeout`, caller context). Tier
ceilings: fast 10 s, thinking 20 s (configurable). "Provider" for every row
today is G5: the daemon's default-backend headless CLI via `lc.RunClaudeP`; no
row selects model, provider or AI CLI individually. "Capacity signal": every
row sees only the shared semaphore (G1); the poller's own gates (below) are the
only eligibility signals.

Classes: **SC** safety-critical, **HI** human-interactive, **EV**
high-value event-driven, **BE** best-effort cosmetic. Priority is the
proposed class from §5.

| Kind | Caller (file) | Trigger / eligibility | Tier | Payload bound | Retry path | Failure consequence | Class / P |
|------|---------------|-----------------------|------|---------------|-----------|---------------------|-----------|
| `arbitrate_approval` | `poller.arbitrate` (`poller/poller.go:898`) via `ArbitrateApproval` | Static approval rules could not answer **and** (`policy.use_fast_brain` or autopilot-owned agent) and engine non-nil; destructive guard + circuit breaker already ran | fast (tool permission) / thinking (strategic question) | **None** (G8): action, question, options verbatim | None per call; poller re-evaluates on a later tick | Fail open → `escalate` → autopilot brain, else human; agent stays blocked until answered. Confidence < 0.8 also escalates | SC / 1 |
| `recognize_prompt` | `poller.recognize` goroutine (`poller/recognize.go:230`) | Backend parsers and known-prompts store both missed; menu unchanged ≥ 8 s; ≤ 2 attempts per distinct menu; `recognize_prompts` on | thinking | Last 40 pane lines; **not redacted** | One re-attempt (attempts ≤ 2) | "Not recognized": prompt stays for a human; agent blocked | SC / 1 |
| `repl_turn` | `FastBrainChatter` (`fastbrain/chatter.go:38`), CLI REPL | One per user turn in `wd repl` | fast | Header + last 32 KB of conversation (`clipTail`) | User re-asks | Turn errors in the REPL | HI / 2 |
| `diagnose_stall` | `autopilot` guardian/overwatch triage goroutines (`guardian_triage.go:105`, `overwatch_triage.go:142`) via `DiagnoseStall` | Heuristics returned `mechanical`; `autopilot.guardian.use_fast_brain` (default true); one in flight per run | fast, then thinking if confidence < 0.6 | Header/facts/pane each ≤ 8 000 B, last 60 pane lines, redacted; nudge text ≤ 600 runes | Up to 2 calls sequentially (fast→thinking); controller re-triages on next watchdog | Fail open → mechanical recovery ladder (no AI) | EV / 2 |
| `diagnose_failure` | `poller` crash triage (`poller/crash.go:115`) | Agent exit with non-zero code, once per exit | fast | Last 40 lines, redacted | None | Deterministic heuristic class; bug draft still staged | EV / 3 |
| `classify_ci_failure` | `daemon/fix_runtime.go:128` | Autopilot CI-fix loop saw a red check | fast | Last 40 log lines, ≤ 8 000 B, redacted | None (loop re-asks next poll) | Fail open → `real` (never ignores a failure) | EV / 3 |
| `summarize_check` | `lifecycle.summarizeCheckOutput` (`lifecycle.go:1462`) | Failed `wd check` output is oversized | fast | ≤ 8 000 B (head), not redacted | None | Deterministic tail truncation | EV / 3 |
| `commit_message` | `lifecycle.commitMessage` (`git.go:319`) | `wd commit` without `-m`, staged diff non-empty | fast | `capDiff` then ≤ 8 000 B, not redacted | None | Deterministic path-derived message | BE / 3 |
| `pr_summary` | `daemon.draftPR` (`pr_routes.go:88`) | `POST` PR-create without caller title/body | fast, then thinking if body empty | Combined input capped, head kept; not redacted | Second tier call (up to 10 s + 20 s serial) | Caller falls back to generated title | BE / 3 |
| `route_tier` | `lifecycle.routeTierByPrompt` (`route_tier.go:63`) | Spawn pins no tier/task/role/model/backend/ai_cli and `router.use_fast_brain` (default **off**) | fast, `Timeout` 1.5 s | ≤ 8 000 B prompt, not redacted | None | Default resolution (spawn is delayed ≤ 1.5 s at most) | BE / 3 |
| `classify_task` | `lifecycle.Classify` (`lifecycle.go:1262`) | Spawn/task classification when the caller asks | fast | ≤ 8 000 B, not redacted | None | `other` | BE / 4 |
| `resolve_agent_name` | `fastbrain.NameRunner` via `lifecycle.SpawnNameRunner`; `lifecycle.GenerateName` (`lifecycle.go:1368`) | Spawn with no explicit name | fast | Prompt-derived, not clipped by the runner | None | Deterministic slug/codename | BE / 4 |
| `summarize_activity` | poller badge (`poller.runSummary` → `lifecycle.Summarize`, `lifecycle.go:1315`; 60 s ctx); `digest.ClaudeNarrator`; `insights.NarrateWithBrain` | Badge: `activity.enabled` **and** pane changed **and** ≥ `activity.interval` since last. Narrators: on demand | fast | ≤ 8 000 B, not redacted | None | Previous badge / deterministic summary; **`StatusDeferred` when last slot** | BE / 4 |
| `curate_extract` | `curate.LLMProposer.Propose` (`curate/propose.go:48`) | Memory curation pass (`memory.curate`, default off) | thinking | `extractionContext` of the batch, ≤ 8 000 B, not redacted | None | Pass skipped | BE / 4 |

**Activity summaries stay disabled.** This document assumes the operator
configuration `activity.enabled: false` (the delivered kill switch). The
repository default is still `true` for backwards compatibility; flipping it is
explicitly out of scope. `summarize_activity` from the digest and insights
narrators is not covered by that switch and remains reservation-gated by #858.

## 4. Latency SLOs, deadlines and cancellation

SLOs are end-to-end from `Decide` entry (including queue wait) to `Response`,
measured per kind over a rolling 15-minute window, healthy provider. "Deadline"
is the engine-enforced hard bound; "SLO" is the p95 target; p99 may use up to
the deadline.

| P | Kinds | p50 | p95 SLO | Hard deadline | Max queue wait | On deadline |
|---|-------|-----|---------|---------------|----------------|-------------|
| 1 | `arbitrate_approval`, `recognize_prompt` | 3 s | 8 s | fast 10 s / thinking 20 s | 2 s, then admit by preemption (§5) | Escalate to human immediately (already the fail-open) |
| 2 | `repl_turn`, `diagnose_stall` | 4 s | 12 s | 20 s (+ one tier escalation only if ≥ 8 s remain) | 3 s | `repl_turn`: error; `diagnose_stall`: mechanical ladder |
| 3 | `diagnose_failure`, `classify_ci_failure`, `summarize_check`, `commit_message`, `pr_summary`, `route_tier` | 4 s | 12 s (`route_tier`: 1.5 s hard) | 10 s fast; `pr_summary` total ≤ 20 s across tiers | 5 s | Deterministic fallback (already present for all) |
| 4 | `classify_task`, `resolve_agent_name`, `summarize_activity`, `curate_extract` | 5 s | best effort | 10 s fast / 20 s thinking | **0 s: shed, never queue** | `StatusDeferred`; keep previous/deterministic value |

Availability SLO (non-deferred, non-canceled calls returning `ok`): P1 ≥ 99 %,
P2 ≥ 97 %, P3 ≥ 90 %, P4 none. Fail-open correctness SLO: 100 % of non-OK
statuses must take the documented fallback in §3 (already true; keep it).

### Cancellation semantics (target)

1. A caller's context is authoritative for **that caller only**. A coalesced
   follower must be detached from the leader's context: the shared model call
   runs on a context owned by the engine, cancelled only when **all** attached
   callers are gone or the deadline fires (fixes G4).
2. A queued request whose caller context ends is removed from the queue at once
   and never starts a model process.
3. A queued request older than its max-queue-wait for its class is shed with
   `StatusDeferred` (P4) or promoted per §5 (P1–P3); it is never silently
   dropped.
4. Superseded work is cancelled by owners: an approval answered by a human, an
   agent terminated, or an autopilot run completed must cancel the owning
   context (the poller/controller already hold the context; the engine only
   needs to honour it while queued — G9).
5. A runner that ignores its context is still bounded by the deadline: the
   engine returns on the deadline and the process is killed by the runner's
   `exec` context (`RunClaudeP` 30 s cap is a backstop, not the SLO).

## 5. Priority and fairness policy

| Class | Members | Policy |
|-------|---------|--------|
| 1 | permission / trust prompts and safety decisions: `arbitrate_approval`, `recognize_prompt` | Always admitted ahead of everything. Owns a **reserved slot** that no lower class may occupy. May preempt (cancel and re-queue) a running P4 call when the pool is full. Never shed |
| 2 | human-interactive and bounded Autopilot recovery: `repl_turn`, `diagnose_stall` | Admitted ahead of P3/P4. Autopilot recovery is bounded: ≤ 1 in-flight per run (already enforced by `triage.inFlight`) and ≤ 2 per run per 10 min |
| 3 | operational summaries / check diagnosis: `diagnose_failure`, `classify_ci_failure`, `summarize_check`, `commit_message`, `pr_summary`, `route_tier` | FIFO within class; queue wait ≤ 5 s then deterministic fallback |
| 4 | badges, naming, narration, curation, insights: `summarize_activity`, `classify_task`, `resolve_agent_name`, `curate_extract` | Only admitted when ≥ 1 slot would remain free for P1–P2 (generalizes the #858 rule); otherwise shed immediately. Collapsible: at most one in flight per kind |

Fairness: within a class, round-robin by `Metadata["agent_id"]` (empty id = own
bucket) so one agent cannot occupy more than ⌈slots/2⌉ concurrent slots in P2–P4.
P1 is exempt from per-agent caps but is bounded by the existing poller circuit
breaker and the 2-attempt recognition limit.

Capacity: `max_concurrent` stays the global bound (default 2). The design
requires `max_concurrent ≥ 2` to give P4 any capacity; at 1, P4 is always
deferred (pinned by `TestActivityNeverRunsWhenMaxConcurrentIsOne`).

## 6. Provider-selection and fallback policy

Today (G5) all kinds use one runner. Target policy, to be implemented behind the
backend registry (`backendstore`, tier ladder) rather than new config:

1. **Selection** per class, cheapest eligible first: P4 and P3 prefer a free or
   local-tier backend with a headless mode; P1 and P2 prefer the lowest-latency
   enabled backend that has recently succeeded, even at higher cost. The
   "fast" tier maps to the backend's fast/small model, "thinking" to its
   reasoning model; where a backend has only one model the two tiers share it.
2. **Eligibility signal**: a backend is eligible iff it has a headless command,
   is not rate-limit-parked, and its circuit is closed (below).
3. **Circuit health** (new, per runner): open after 3 consecutive
   timeout/runner-error results or ≥ 50 % failures over the last 10 calls;
   half-open probe after 30 s then 2 min then 10 min. While open, P4 sheds, P3
   takes the deterministic fallback with no call, P1/P2 try the next eligible
   backend.
4. **Fallback order** for P1/P2 on failure: next eligible backend (once, within
   the remaining deadline) → fail open per §3. Never retry the same backend
   within one decision.
5. **No silent provider change of authority**: provider choice never changes an
   approval's safety rules; the destructive guard and confidence threshold are
   provider-independent.

## 7. Privacy and payload limits

Applies to every prompt before it reaches any runner, enforced once in the
engine (closing G8) rather than per builder:

1. **Redact** with `fastbrain.Sanitize` (credentials, auth headers, provider key
   shapes, home paths) on the whole prompt, idempotently.
2. **Bound** per kind: 8 000 B of untrusted free text per field; whole prompt ≤
   32 KB (the REPL's `clipTail` ceiling); arbiter `Action`/`Question` ≤ 2 000 B
   and options ≤ 20 × 200 B, truncated from the middle with a marker so the
   destructive-marker check (which runs on the unclipped value first) is
   unaffected.
3. **Minimize**: `Request.Input` and `Metadata` are never sent. Metadata/agent
   ids are not logged verbatim; logs carry the prompt hash only (existing).
4. **Never send** the contents of `.env`-style files, file bodies from diffs for
   `commit_message`/`pr_summary` beyond the capped diff, or full transcripts;
   transcript-derived kinds use the existing 4 000-byte tail.
5. **Outputs** are untrusted: sanitized JSON only, rationale redacted before
   storage (already done for crash/stall/CI; extend to arbiter events).
6. **Opt-out**: `activity.enabled=false` removes all badge payloads;
   `router.use_fast_brain`, `memory.curate` and `use_fast_brain` already gate the
   others. No new kind may ship default-on if it sends repo content.

## 8. Telemetry required to measure the SLOs

Per call, labelled by `kind`, `tier`, `class`, `provider`: `queue_wait`,
`run_time`, `status`, `coalesced` (joined/leader), `deferred_reason`,
`preempted`. Per runner: circuit state and consecutive failures. Gauges: slots
in use per class, queue depth per class, oldest queued age. Exposed through the
existing metrics endpoint and one `fastbrain_decision` event per P1 call (the
arbiter already records `fastbrain_arbiter`). Until this exists no threshold in
§9 is measurable; telemetry ships first.

## 9. Rollout thresholds

Each phase is flag-gated (default off) and promoted only when the previous
phase's measured thresholds hold over ≥ 7 days or ≥ 500 P1 decisions, whichever
is later. Any breach of a rollback trigger reverts the flag.

| Phase | Change | Promote when | Roll back when |
|-------|--------|--------------|----------------|
| 0 | Telemetry only (§8) | All kinds report queue_wait/run_time; telemetry overhead < 1 ms/call | – |
| 1 | Class-aware admission + reserved P1 slot + P4 shed | P1 p95 ≤ 8 s; P1 queue wait p99 ≤ 2 s; zero P1 `canceled` due to queue; P4 shed rate ≤ 100 % allowed | P1 p95 regresses > 20 % vs phase 0; any `escalate` increase attributable to starvation > 5 % |
| 2 | Follower detachment + queued-cancel (§4.1–4.2) | Zero cross-caller cancellations in a soak of 10 k coalesced calls | Any follower returns `canceled` with a live context |
| 3 | Engine-level redact + bound (§7) | 0 secret-shaped strings in 1 k sampled outgoing prompts; P1 payload truncation rate < 0.1 % | Arbiter accuracy (human override rate) rises > 2 pts |
| 4 | Per-runner circuit + provider fallback (§6) | During a 10-min injected provider outage P1 success ≥ 95 % via fallback and P3/P4 attempt 0 calls while open | Circuit flaps > 3 times/hour on a healthy provider |
| 5 | Per-agent fairness | No agent > ⌈slots/2⌉ slots in P2–P4 under a 20-agent stampede | P2 p95 > 12 s |

Standing guards for every phase: #858 tests pass; the destructive-action guard
is unchanged; with `activity.enabled=false` there are zero `summarize_activity`
calls from the poller; no P1 decision is ever shed.

## 10. October timeout incident fixtures

`testdata/incident_2026_10_timeout.json` scales the incident to milliseconds and
drives three deterministic tests on the current baseline:

1. **Stampede (post-#858 guard):** 20 distinct cosmetic summaries with
   `max_concurrent=2`; exactly one runs, 19 are `StatusDeferred`, and a
   concurrently issued `arbitrate_approval` starts a runner immediately.
2. **Residual starvation (gap G1):** two non-cosmetic calls
   (`commit_message`, `classify_task`) hold both slots; an
   `arbitrate_approval` with a 150 ms caller deadline ends `StatusCanceled`
   having never reached the runner, and its `Response.Duration` is zero (gap G7).
3. **Leader coupling (gap G4)** and the **no cache / no circuit / no distinct
   coalescing** gaps (G2/G3) are separate fixtures.

When a rollout phase fixes a gap, the corresponding `*Gap` test is rewritten to
assert the new behaviour in the same PR; the stampede guard (1) is never
weakened.

## 11. Open questions

- Should `repl_turn` share the daemon engine (and its admission) or keep the
  CLI-process engine? Currently separate; sharing needs a daemon API.
- Whether P1 preemption may cancel a running P3 (not only P4) call; proposed
  answer: no, P1's reserved slot makes it unnecessary.
- Whether `activity.enabled` should flip to default-off; deliberately left to a
  separate decision.

## 11. Runner health and cancellation (delivered: runner-cancellation-and-health)

- `fastbrain.Health` (per-candidate circuit: 3 consecutive or ≥ 50 %/10 failures
  open it; single half-open probe after 30 s → 2 min → 10 min) and
  `fastbrain.Pool` (policy-ordered candidates, each tried at most once per
  decision, first attempt capped at 60 % of the remaining deadline when a healthy
  fallback exists, `ErrNoCandidate` → `StatusNoRunner` fail-open) close G3 and
  G5 for the daemon runner. Caller cancellation is never a health failure.
- Candidates come from the backend registry (`internal/cli/fastbrain_candidates.go`):
  free → subscription; the registry default leads the thinking tier; pay-per-use,
  unclassified, disabled, uninstalled, rate-limited and headless-less backends
  are rejected with a bounded reason recorded in `Response.Selection.Trail`.
- `Response.Cancel` records `acknowledged` vs `abandoned`; an engine abandons a
  context-ignoring runner after `CancelGrace` (500 ms) so it never pins a slot.
  `ExecRunner` kills the child's whole process group on cancel and bounds output
  drain with `WaitDelay`.
- Coalesced calls are detached from the leader's context and canceled only when
  the last waiter leaves (closes the leader-context-coupling gap).

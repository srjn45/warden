# Autopilot — Run to the Final PR (spec freeze)

**Date:** 2026-10-05
**Status:** Design — frozen for implementation (docs only; no production code in this PR)
**Branch / base:** `autopilot/autopilot-run-to-final-pr`
**Extends:** [autopilot.md](autopilot.md) (§0 principle, §2.1.2 parking, §2.2 task states, §2.3 guardian, §2.4 overwatch, §6 `land`, §9 personas, §11 out of scope), [2026-10-05-plan-restart-and-watchdog.md](2026-10-05-plan-restart-and-watchdog.md), [2026-10-04-intelligent-approval-and-question-arbiter.md](2026-10-04-intelligent-approval-and-question-arbiter.md).
**Restores:** the judgement of the original hourly heartbeat sentinel ([autopilot-implementation.md §2.4](autopilot-implementation.md)) — survey, resume, nudge, respawn — as daemon code plus a bounded Fast-Brain call instead of a scheduled Sonnet agent.

---

## 0. Problem and goal

Autopilot today stops short of "unattended from plan to mergeable result":

1. **Landing depends on an LLM remembering to poll.** A worker opens a PR, merges it itself (worker persona) or the manager calls `land`. If neither does, a green PR sits forever; a red PR is fixed only if the right agent notices.
2. **Failures have no evidence-driven fix path.** `gate_red` carries a one-line summary; nobody collects the failing log, distinguishes flaky from real, caps retries or escalates a repeatedly-red task.
3. **The guardian is mechanical.** It sees "heartbeat stale" or "no progress for 2h" and climbs nudge → restart → rotate. It cannot tell a manager waiting on a prompt from one stuck behind a rate limit from one that lost its brief, and it ends in a human park.
4. **Dead managers are noticed late.** `GuardianRuntime.BrainSession` only reports `SessionMissing` when the store record is gone (`internal/daemon/autopilot_runtime.go`). A manager stopped by `terminate_agent` (record kept) or whose tmux session died reads as `Present`, and is reached only after `heartbeat_timeout` plus a nudge typed into a dead pane.
5. **Worker prompts go to a mailbox.** `internal/daemon/autopilot_approvals.go` forwards an unanswerable worker prompt to the manager's mailbox — pull-only, so an idle manager never reads it.
6. **Parks end at a human** for classes of failure a machine can fix (`no_progress`, repeated `spawn_error`, structural `definition_error`).
7. **The end of a run is thin.** `CompleteAutopilot` opens a one-line-body PR best-effort; nothing verifies the base is current, gates that PR, or repairs it if red.

**Goal.** After enable (§0 of autopilot.md, unchanged), a run proceeds **plan → task PRs landed into the integration branch → integration brought current with the default branch → one gated final PR (integration → default) left open for the human**, with every dead end routed to a machine: the daemon (mechanical, cheap), Fast-Brain (bounded micro-decisions), or a short-lived resolver agent (full tools, fixes the blocker). The human's only post-enable touchpoints are the short list in §D.5 and merging the final PR.

**Non-goals.** Autopilot never merges the final PR (§E.6). No multi-repo, no cross-run scheduling (autopilot.md §11 stands). No new storage engine: new state lives in the run ledger (ctx store) and `RunStatus`.

### Design rules (apply to every section)

- **Frictionless (autopilot.md §0, memory: frictionless-safeguards).** Guards fire only at extremes; every bound below is a generous, hot-reloadable default (§G). Nothing paces normal monorepo work.
- **Daemon-owned, mechanical-first.** Anything decidable from git/gh/store state is code. Fast-Brain is consulted only for classification and triage, always **fail-open** to the mechanical behaviour that exists today. A resolver agent is the last machine rung.
- **Idempotent and restart-safe.** Every action is keyed (PR number + head SHA, task id, blocker fingerprint) and recorded in the ledger/audit log before or atomically with its effect, so a daemon restart or a concurrent `land` never duplicates it (autopilot.md §4 "recovery by construction").
- **Never off `c.mu` for I/O.** `guardianTick` and `overwatchTick` hold `Controller.mu` for the whole pass; that is tolerable for in-memory work and unacceptable for `gh`, `git`, `wd check` or model calls. All new passes follow §A.3.
- **Never a protected target.** The only merge target autopilot ever writes via merge is the integration branch; the default branch is touched solely by the human merging the final PR.

---

## A. Landing loop

### A.1 Pass shape

A new daemon pass, `landingTick`, runs on the **guardian's ticker** (`RunGuardian`, after `guardianTick` and `overwatchTick`; cadence `autopilot.guardian.interval`, default 60s). For every run that is `active` or `finalizing` (§E) it:

1. Lists open PRs whose **base is the run's integration branch**: one `gh pr list --base <integration> --state open --json number,headRefName,headRefOid,mergeable,isDraft,statusCheckRollup,url` per run per tick (one subprocess per run, not per PR).
2. Keeps only PRs whose **head is run-owned** (§A.2). PRs from foreign heads are ignored and never touched; an unmatched PR into the integration branch is audited once as `autopilot.pr_unowned` and left to the human.
3. Skips draft PRs.
4. For each remaining PR, evaluates the **gate for the head SHA** and acts:

| Gate for head SHA | `mergeable` | Action |
|---|---|---|
| green | `MERGEABLE` | `autopilot.Land` (already idempotent, autopilot.md §6) → on success/`already_landed`: `finalizeLanding` (§A.4) |
| green | `CONFLICTING` | fix loop, conflict case (§B.5) |
| green | `UNKNOWN` | nothing; re-evaluate next tick (GitHub still computing) |
| pending | any | nothing |
| red | any | fix loop (§B) |
| `missing` (mode `ci`, no runs) | any | nothing for `ci_grace` (default 10m after PR open/push, CI may not have started), then audited `autopilot.gate_missing` and handled as `gate_pending`; autopilot.md §6.1's `auto` resolution makes this rare |

The gate evaluation is `Land`'s own `checkGate` (CI via `branchtrack.StatusForSHA`, or `GateLocal`); the landing pass does not reimplement it. To avoid calling `Land` just to learn the gate, the pass uses a read-only `Gate(ctx, req, host, pr)` extracted from `checkGate` (same code path, no merge) and calls `Land` only when green + mergeable — `Land` re-checks all five preconditions, so a race between the read and the merge cannot merge red or conflicting code.

**Local gate cost.** `GateLocal` runs the project checks, which is expensive. In `local` mode the result is cached per `(pr, head_sha)` in memory, computed **asynchronously** by a per-run goroutine bounded by `landing.local_gate_timeout` (default 15m); the tick reads the cached state (`pending` while running). If the owning worktree no longer exists, the daemon checks out the head SHA into a detached temporary worktree under `<data_dir>/autopilot/gate/<run>/<sha>` and removes it afterwards.

### A.2 Mapping a PR to a task and a worker

Mapping is deterministic, in this order; the first hit wins:

1. **Owning session.** The run roster (`RunAgents`, tag `run:<run_id>`) contains a session whose `Branch == headRefName`. The task is `session.AutopilotTaskID` (fall back to `session.Task`, exactly as `sessionLandTarget` does today); the worker is that session — even if its tmux session is dead (the record is kept until teardown).
2. **Ledger task branch.** Else a ledger task row with `branch == headRefName` (`LedgerTask.Branch`) gives the task; the worker is `LedgerTask.WorkerID` if that session still exists, else none (fix-up worker path, §B.4).
3. **Fix-up lineage.** Else the branch is a fix-up branch recorded in the fix state (`autopilot.<run>.fix.<task>.branch`, §B.6) → that task.
4. Else the PR is **unowned** (rule 2 above). It is never merged by autopilot.

"Run-owned head" is exactly: the PR matches rules 1–3. This also covers branches pushed by an agent whose session the daemon later archived, via rules 2–3.

### A.3 Staying off `c.mu`

The landing pass is split so `c.mu` is held only for pure in-memory reads/writes:

1. **Snapshot (locked, microseconds).** Under `c.mu`, for each eligible run copy a `landSnapshot{runID, repo, integration, defaultBranch, gate, strategy, deleteBranch, managerID, state}` (the data `LandParams` already exposes) and release.
2. **Work (unlocked).** All `gh`/`git`/check/model I/O runs with no controller lock, under a **per-run mutex** (`landRunMu[runID]`, `sync.Map` of `*sync.Mutex`; `TryLock` — a pass still running from the previous tick makes the new tick skip that run, so a slow local gate never stacks passes). Runs are processed in parallel goroutines capped at 4.
3. **Apply (locked, microseconds).** State changes that must be visible in `RunStatus` (counters, `last_land_at`, fix state summary) are applied under `c.mu` with a re-check that the run still exists and is still `active`/`finalizing` (kill switch: a run paused or stopped between snapshot and apply is left untouched; an in-flight `gh pr merge` is not cancellable, which is acceptable because the pre-snapshot state was active).

`superviseRun`/`overwatchRun` are untouched by this and keep their current locking.

### A.4 `finalizeLanding` — the single post-merge routine

Today the post-merge bookkeeping (ledger `AppendLanding`, `UpdateTaskStatus(done)`, plan-bound evidence events) lives inside the `LandAutopilot` HTTP handler (`internal/daemon/land_routes.go`), and worker teardown is the manager's job. This spec extracts it into one idempotent function `finalizeLanding(ctx, runID, taskID, pr, headSHA, branch)` used by **both** the handler and the landing pass:

1. `ledger.AppendLanding{branch, sha: headSHA, pr}` — skipped if a landing with that head SHA exists.
2. Ledger task state → `landed`; plan task status → `done` with `landed_pr` (`UpdateTaskStatus`).
3. Plan-bound evidence (`recordPlanBoundLandEvents`, `trackPlanBranch`).
4. **Worker teardown**: for the owning session (if any): `Terminate` → `remove_worktree`, in that order (memory: contributor-agent teardown order), skipping any step already done. A fix-up worker for the same task is torn down the same way. Branch deletion stays with `land` (`delete_branch`).
5. Clear the task's fix state (§B.6).
6. Audit `autopilot.landed` (§G).

Because every step checks "already done", `finalizeLanding` is also invoked when `Land` returns `AlreadyLanded` — this heals a crash between `gh pr merge` and the bookkeeping, and the case where an agent or human merged the PR by hand.

### A.5 Coexisting with an agent calling `land`

`land` stays available (MCP `land`, `wd autopilot land`, the REST route) as an escape hatch for operators and for runs where `autopilot.landing.enabled=false`. Concurrency is safe by construction, not by exclusion:

- `Land` is idempotent on merged state and on the recorded head SHA; the second caller gets `already_landed: true`.
- Both entry points take a **per-PR lock** (`landPRMu[runID/pr]`) around "gate → merge → finalizeLanding", so two callers serialise instead of racing `gh pr merge`. The loser blocks briefly, observes `Merged`, returns `already_landed`, and still runs `finalizeLanding` (a no-op if the winner finished it).
- `gh pr merge` on an already-merged PR failing is treated as `already_landed` after a re-read, never as an error.
- The pass never lands a head SHA the ledger already records.

---

## B. Fix loop

### B.1 Trigger and the one-dispatch rule

A fix cycle starts when the landing pass observes, for a run-owned PR, **gate red** or **not mergeable** at head SHA `S`. Fix state per task (ledger key `autopilot.<run>.fix.<task>`, §B.6) records `last_dispatched_sha`. **At most one dispatch per red head SHA**: if `S == last_dispatched_sha` the pass does nothing more (the fixer is working, or finished without pushing — the progress watchdog and §B.3 cap handle that). A push produces a new head SHA, which is a new evaluation.

### B.2 Evidence collection

Collected once per `(pr, S)` and stored (trimmed) in the fix state so a restart does not recollect.

**CI gate (mode `ci`):**
- Failing job names: `gh pr checks <n> --json name,state,link,workflow` filtered to `state=FAILURE|ERROR|CANCELLED`, max 10 names.
- Log excerpt: for up to 3 failing runs, `gh run view <run-id> --log-failed`, then **trimmed**: keep the last 150 lines per run and a total of **8 KiB** (`fix.evidence_max_bytes`), preferring lines matching `FAIL|Error|panic|assert|error:|✗|--- FAIL` with ±3 lines of context, else the tail. ANSI stripped; secrets-looking tokens (`ghp_…`, `Bearer …`, 40+ hex) redacted; the same `fastbrain.Sanitize` used for crash triage.
- PR facts: head SHA, base SHA, `mergeable`, behind-by count vs the base.

**Local gate (mode `local`):** the failed `CheckResult` entries from `life.Check` — per failing check: name, exit code and the last 150 lines / 8 KiB of its output (`summarizeFailedChecks` only keeps one line; the excerpt keeps the rest).

### B.3 Flaky/infrastructure: the one-rerun rule

Before dispatching a fixer for a **CI** red:

1. **Classify** with Fast-Brain `DiagnoseFailure` (`fastbrain.CrashInput{Excerpt: <log excerpt>}`, fast tier ≤1.5s — the existing crash-triage classifier: `transient_error` and `environment_error` ⇒ candidate flaky/infra; `internal_bug` and `task_failure` ⇒ real). No new decision kind is needed; the heuristic fallback already exists and covers 429s, timeouts, runner shutdown, network resets. Acted on at confidence ≥ 0.8 (model) — the heuristic source is acted on as returned.
2. If candidate flaky/infra **and** `fix.rerun_flaky` **and** no rerun yet recorded for head SHA `S`: `gh run rerun <run-id> --failed` for each failing run, record `rerun_done_for_sha = S`, audit `autopilot.gate_rerun`, and stop (the next tick re-reads the gate). **Exactly one rerun per head SHA.**
3. If it fails again at the same `S`, or classification is `real`, or the classifier failed or is unavailable (**fail open to "real failure"**): proceed to dispatch.

A red **local** gate is never rerun (local checks are deterministic at a SHA; an environment failure is classified the same way and routed as real so the fixer or resolver can address the environment).

### B.4 Who receives the fix

Decision order for the recipient of fix evidence for task `T` at head `S`:

1. **Owning worker if its session is alive**: the roster session mapped by §A.2 with tmux alive (`poller.SessionAlive`) and status in {`waiting_for_input`, `idle`}. The dispatch is a `WakeAgent` input turn containing: the evidence block, the instruction "fix the failure on this branch, run `wd check`, push, and end with `wd job done` — do not merge", and the PR/head info. If the worker is `working`/`spawning`, defer: re-evaluate next tick for up to `fix.busy_defer` (default 10m, generous because a working agent is likely already acting on the same failure through its own CI watch); after that, treat as not alive for dispatch purposes and fall through to rule 2 only if the worker is also non-progressing (no pane activity for the window), else keep deferring. `rate_limited` workers are handled by §J, not replaced.
2. **Else a fix-up worker**: `role=worker`, spawned on the **same branch** (the PR head), with tags inherited from the run, `AutopilotTaskID = T`, a brief containing the evidence and the same contract. If the dead owner's worktree still exists and is clean, the fix-up worker takes that worktree as `cwd` (ownership of the worktree moves to the new session); else a fresh worktree is created on the branch. Backend chosen by the tier ladder (autopilot.md §7), same as any worker.

Whichever path, `last_dispatched_sha = S` is written **before** the send/spawn (at-most-once on a crash; the watchdog catches a dispatch that never took effect), and audited `autopilot.fix_dispatched {task, pr, sha, to, kind}`.

### B.5 Conflict case (base moved)

`mergeable == CONFLICTING` at head `S` (or a green gate with a stale base the merge refuses). The evidence is: base branch SHA, `git merge-tree`-style conflicted file list (computed by the daemon in a throwaway worktree, ≤50 paths), and the commits that moved the base. The dispatch is the same as §B.4 with the instruction "merge/rebase `<integration>` into your branch, resolve conflicts preserving both sides' intent, re-run checks, push". A conflict at the same `S` is dispatched once; the new head after the fixer's push is a new evaluation. The conflict case counts toward the red-SHA streak (§B.6) like a red gate.

### B.6 Fix state, the consecutive-red cap, and the resolver

Ledger key `autopilot.<run>.fix.<task>` (JSON, daemon-written, new in `internal/autopilot/ledger.go`):

```
{ task, pr, branch,
  head_sha, kind: "ci_red|local_red|conflict",
  rerun_done_for_sha, last_dispatched_sha,
  red_streak,            // consecutive red head SHAs for this task
  evidence: <trimmed>,   // for the current head_sha
  fixer_session, updated_at }
```

- `red_streak` increments when a **new** head SHA is observed red/conflicting after a dispatch; it **resets to 0** when any head SHA of the task is observed green. (The first red of a fresh PR is streak 1.)
- When `red_streak >= fix.max_red_shas` (default **5**), the pass stops dispatching to workers for that task and calls the **resolver** (§D) with blocker class `red_gate` carrying the whole red history (per-SHA evidence summaries). The streak is *not* reset by the call; it resets only on green.
- After the resolver reports (§D), dispatch resumes only if the resolver outcome is `resolved` (its push produced a green head) or it replaced the approach; `unresolved` after `resolver.max_attempts_per_blocker` ⇒ the §D.5 human stop, wording `red_gate_exhausted`.

---

## C. Guardian triage

### C.1 Where it runs

Today `superviseRun` picks the next rung purely from time (`healNextAt`) and `healStage`. Triage inserts a decision **before** a rung is executed, for a manager stall (stale heartbeat or the progress watchdog due). It is run **off `c.mu`** with the same snapshot/apply split as §A.3: the tick snapshots the stalled run, a goroutine builds the evidence bundle and calls Fast-Brain, then re-locks to apply the chosen action to `healStage`/`healNextAt`.

Triage replaces only the choice "which rung now"; the mechanical ladder (nudge → restart → rotate → backoff) remains the floor and the fallback.

### C.2 Closed action set

Fast-Brain (thinking tier, ≤10s — the `fastbrain` hard budget) returns JSON `{action, text?, confidence, rationale}` where `action` is exactly one of:

| Action | Effect |
|---|---|
| `wait` | Do nothing this tick; healthy-but-slow (manager mid long tool call, backend slow). Subject to §C.4 bounds. |
| `nudge(text)` | Wake the manager with `text` (validated: ≤600 chars, sanitized) instead of the generic `guardianNudge`. Counts as ladder stage 1. |
| `resolve_prompt` | The manager's pane shows an unanswered prompt: run the **prompt chain** (§I) on the manager itself. |
| `resume_rate_limit` | The manager is parked at a limit menu/banner whose reset has passed or whose resume was missed: run the §J resume path for the manager. |
| `redeliver_prompt` | The manager's input never arrived / it lost its brief (empty pane, no activity after spawn): re-send the digest/last wake. |
| `restart` | Stage-2 in-place restart (same backend, fresh context). Requires confidence ≥ 0.8. |
| `rotate` | Stage-3 rotate to the next selectable backend. Requires confidence ≥ 0.8. |
| `call_resolver` | The stall is a blocker the manager cannot clear (e.g., a broken environment): hand to the resolver (§D) with blocker class `manager_stall`. |

Any other `action`, malformed JSON, `StatusInvalidJSON`/`Timeout`/`RunnerError`/`NoRunner`, or an `action` that violates a bound ⇒ **fail open**: execute the mechanical rung exactly as `escalate` does today. Triage can only ever be *more precise*, never *more stuck*, than the ladder.

### C.3 Evidence bundle and size caps

Built by the daemon (all text passed through `fastbrain.Sanitize`); **hard cap 24 KiB** total, truncated in the reverse order of this list:

| Section | Cap |
|---|---|
| Header: run id, state, heal stage, minutes since last activity, minutes since last progress, triage history for this stall (last 5 actions + outcomes) | ~1 KiB |
| Manager pane tail (`Output`, last 60 lines) | 6 KiB |
| Manager facts: backend, tier, model, status, context level, rate-limit state/reset | 0.5 KiB |
| Roster: ≤20 agents × {id, role, state, branch, age} | 2 KiB |
| Ledger tasks: ≤30 rows × {id, state, pr, note≤80} | 3 KiB |
| Open run PRs: ≤10 × {pr, head, gate, mergeable} | 1 KiB |
| Last 15 run audit entries (`RecentAudit`) | 3 KiB |
| Pending approvals for run agents: ≤5 × {agent, question≤200} | 1.5 KiB |
| Plan: goal (≤300), `done_when` (≤5 × 150) | 1.5 KiB |

### C.4 Bounds

- `wait` at most **3 times in a row** and **30 minutes total** per stall episode (an episode ends when progress or a heartbeat is observed). The 4th consecutive `wait`, or any `wait` past 30m, is rewritten to `nudge` with the generic text. Config: `guardian.triage_max_waits` (3), `guardian.triage_max_wait_total` (30m).
- `restart` and `rotate` need `confidence >= 0.8` (`guardian.triage_min_confidence`); below it the action downgrades to the mechanical rung (which may itself be the same restart/rotate when the ladder reaches it by time).
- At most one triage call per run per tick; a run's triage is skipped while a previous call for it is in flight.
- Triage never bypasses `needsAttention` parking or the per-repo kill switch; a parked/paused/stopped run is not triaged.

### C.5 Default and off switch

On by default for autopilot runs: `autopilot.guardian.use_fast_brain` (default **true**, hot-reloaded). When false, `superviseRun` behaves exactly as today. A run with no Fast-Brain runner configured (`StatusNoRunner`) behaves as false per call (fail open) and audits `autopilot.triage_failopen` at most once per hour per run.

---

## D. Resolver agent

### D.1 What it is

A short-lived `brain`-role agent (`roles/brain.yaml`, `permission_mode: bypassPermissions`, headless, tags `autopilot`, `run:<id>`, `system:true`), spawned through `internal/brainconsult` / `SpawnConsultBrain`. Unlike today's `brain_consult` (closed action enum, recommend-only, 8 KiB evidence, 10m timeout) the resolver **must fix the blocker itself**, then report. `brainconsult` gains a second mode, `Mode: resolve`, with its own prompt template and reply schema; the existing consult path is unchanged.

### D.2 Brief and reply

The brief carries: the plan goal and constraints; the blocker class (`red_gate | manager_stall | no_progress | spawn_error | definition_error | base_merge | done_when | prompt`); the evidence for that class (same builders as §B.2/§C.3, cap **16 KiB**); the list of previous resolver attempts for this blocker with their reports; the scope limits (§D.3); and the instruction: *diagnose, change what must change, verify, then reply once.*

Reply (single JSON object on a line, like the consult reply):

```
{ "outcome": "resolved|partial|unresolved",
  "summary": "<what was wrong, one paragraph>",
  "actions": ["<each concrete change made, with branch/commit/agent ids>"],
  "verified": "<how it was verified, e.g. `wd check` passed on <sha>>",
  "follow_up": "<what the daemon/manager should do next, or empty>" }
```

An unparseable or missing reply within `resolver.timeout` (default **30m**, generous: it edits code) counts as `unresolved`. The agent is always torn down afterwards (existing `Consult` defer).

### D.3 Scope limits

Allowed: read anything in the repo and warden state; edit/commit/push on **run-owned branches** (the blocked task's branch, fix-up branches, the integration branch only for `base_merge`); run `wd check`; use warden MCP tools against **run-owned** agents (the ownership guard, autopilot.md §8, already 403s anything else because the resolver carries the run tag); send messages/wake run agents; repair plan *structure* (duplicate task ids, bad `after` edges, missing `done_when` wording) through the plan-modification API; append new tasks for work discovered during `done_when` verification.

Forbidden (enforced by the persona brief **and** mechanically where a guard exists): merging any PR (landing is daemon-owned); touching the default branch or opening/merging the final PR; changing the plan goal or constraints; changing warden config, secrets or auth; force-pushing a non-run branch; terminating non-run agents; spawning further resolvers.

### D.4 Attempts, scope of "blocker", and what counts as resolved

- A **blocker** is fingerprinted `class + subject` (`red_gate:<task>`, `manager_stall:<run>`, `no_progress:<run>`, `spawn_error:<normalized error text>`, `definition_error:<plan revision>`, `prompt:<agent>`). Attempts are counted per fingerprint: `resolver.max_attempts_per_blocker` default **2**. A second attempt receives the first's report in `AlreadyTried`.
- Per run per rolling 24h: `resolver.max_per_run_per_day` default **6** across all fingerprints.
- Records live in the ledger (`autopilot.<run>.resolver`: list of `{fingerprint, started, outcome, summary}`), so a restart keeps the counts.
- **Resolved** = the blocker's check passes after the report, never the agent's word alone: for `red_gate` a head SHA observed green or a new head SHA past the streak; for `manager_stall`/`no_progress` the run **progress fingerprint** (`progressFingerprint`, watchdog.go) changes; for `spawn_error` the next spawn succeeds; for `definition_error` the next hydrate/preflight succeeds; for `prompt` the prompt disappears. `outcome:resolved` without the check passing is recorded `unverified` and counts as an attempt.

### D.5 Parks: resolver first, and what still stops for a human

| Today's park | New behaviour |
|---|---|
| `no_progress` (watchdog ladder exhausted) | Before parking: `call_resolver` with class `no_progress` (evidence: bundle §C.3 + the stalled tasks). Park only if attempts are exhausted or the resolver reports `unresolved`. |
| `spawn_error` repeated `max_identical_failures` | Resolver first (class `spawn_error`: it diagnoses git/tmux/backend/disk/permission problems and fixes the environment where it can). Park only after attempts are exhausted. |
| `definition_error` | Resolver may repair **structural** defects only (duplicate ids, dangling/cyclic `after`). An empty or missing goal, or a plan the resolver cannot make valid without inventing intent, parks immediately. |
| `backend_unavailable` / `no_backend_selectable` | Unchanged: transient, capped-exponential backoff, now with a scheduled resume per §J. |

**The short list that still stops for a human.** The run is parked as `needs_attention` (not failed), exactly one notification and one audit event per episode (autopilot.md §2.1.2). Exact wording (the text appended after `autopilot needs attention (<kind>):`):

| `kind` | Exact text |
|---|---|
| `plan_goal_missing` | `The plan has no goal and the resolver cannot invent one. Edit the plan goal, then run "wd plan resume <plan>".` |
| `auth_required` | `<backend> needs an interactive login or trust prompt that only you can complete. Log in (see "wd backends" / the re-auth guide), then run "wd plan resume <plan>".` |
| `repo_access` | `gh or git cannot reach the remote (<detail>). Restore access (e.g. "gh auth login"), then run "wd plan resume <plan>".` |
| `pay_per_use_gate` | `Only pay-per-use backends remain. Set autopilot.brain.allow_pay_per_use to continue, then run "wd plan resume <plan>".` (existing behaviour, new kind name) |
| `resolver_exhausted` | `The resolver tried <n> times and could not clear: <blocker>. Last report: <summary>. Fix it or run "wd plan restart <plan>".` |
| `red_gate_exhausted` | `Task <task> stayed red across <k> pushes and the resolver could not fix it. PR #<pr>: <failing check>. Fix the branch, then run "wd plan resume <plan>".` |
| `final_pr_unfixable` | `The final PR #<pr> is still red after <n> automated fixes. Failing check: <name>. Fix integration, then run "wd plan resume <plan>"; autopilot will not merge the final PR.` |

Each is a park (visible as *waiting* in the project tree), cleared by the existing unpark routes (plan change, `wd plan resume`, restart).

---

## E. Completion and the final PR

### E.1 Trigger

The run enters **completion** when, on a landing-pass tick: every plan task is `landed` in the ledger (and `done` in the plan), the run has **no open run-owned PRs** into the integration branch, and no run agent is `spawning`/`working` other than the manager. A new run state **`finalizing`** (visible in status; a sub-phase of `active`) is entered; the overwatch and watchdog treat it like `active`.

### E.2 Bring integration current with the default branch

If `origin/<default>` is ahead of the integration branch (`git rev-list --count integration..origin/default > 0`), the daemon, in a throwaway worktree: `git fetch`, `git checkout integration`, `git merge --no-edit origin/<default>`, push integration. Clean merge ⇒ done. Conflicts ⇒ the resolver is called (class `base_merge`) in that worktree to resolve and push; this is the one case where the resolver pushes to the integration branch. The merge commit is not individually gated; the **final PR gate** (§E.4) covers it. Disable with `completion.merge_default=false` (then a stale base is reported on the final PR body instead).

### E.3 Verify `done_when`

1. **Manager first.** The daemon wakes the manager: "all tasks are landed — verify every `done_when` criterion against the integration branch and call `autopilot_complete`." Bound: `completion.manager_verify_timeout` (default **30m**).
2. If the manager has not completed within the bound (or is dead and unrecoverable), the **resolver** is called (class `done_when`) to verify each criterion, reply per criterion `{criterion, pass, evidence}`, and **fix** failures where the fix is mechanical (a failing `wd check` on integration) or append tasks for missing work (the run then returns to `active` with the new tasks). If all pass, the daemon performs the completion itself (same effect as `autopilot_complete`).
3. A plan with no `done_when` skips verification (the landing of all tasks is the criterion).

### E.4 Open and gate the single final PR

The daemon (not the manager) opens **one** PR: `base = default branch`, `head = integration branch`. Idempotent: if an open PR for that head/base exists it is adopted (as `CompleteAutopilot` does today); a closed-unmerged one is reopened or recreated. Generated body (deterministic, no model call):

```
## Autopilot run <run_id> — <plan name>
**Goal:** <goal>
**Integration branch:** <name> (<n> commits ahead of <default>)

### Tasks landed
| Task | PR | Worker | Merged at |
### done_when
- [x] <criterion> — <verification evidence or "verified by manager">
### Run summary
Started / completed times; fixes dispatched (<n>), flaky reruns (<n>), resolver calls (<n>), backend switches (<n>).
### Not done / follow-ups
<from ledger notes and resolver follow_ups, or "none">
---
Opened by warden autopilot. Autopilot never merges this PR — review and merge it yourself.
```

The final PR is then **gated like any PR**: CI on the integration head SHA (or the local gate against an integration worktree). Green ⇒ §E.5. Red ⇒ the fix loop, with a synthetic task id `final-fix-<n>`: the fixer (a fix-up worker, escalating to the resolver by the same §B.6 streak cap) works on a **new branch off integration** and opens a normal PR into integration, which the landing pass lands; the final PR head then moves and is re-gated. Conflict (default moved again) ⇒ §E.2 again.

### E.5 Run state while the final PR is red

The run stays in `finalizing` with `final_pr: {number, url, head_sha, gate: pending|red|green, fix_attempts}` in `RunStatus`; the manager is retained (it is the standing supervisor; the guardian keeps healing it); workers for `final-fix-*` tasks are normal run agents. It becomes **`complete`** only when the final PR gate is **green**: then the existing completion actions run (plan marker `status: complete` + `completed_at`, manager torn down, ledger retained) and the human is notified once: `autopilot run <id> complete — final PR #<n> is green and waiting for your review (autopilot will not merge it).` If the final PR cannot be made green within the §B.6/§D.4 bounds ⇒ the `final_pr_unfixable` park (§D.5).

### E.6 Autopilot never merges the final PR

Stated normatively: **no autopilot code path merges, auto-merges (`gh pr merge --auto`), approves, enables auto-merge on, or closes the final PR**, and none writes to the default branch. `Land` already rejects the default branch as source or target (`ErrWrongBase`); the landing pass additionally filters out any PR whose base is the default branch, and the resolver brief forbids it (§D.3). Merging the final PR is the owner's act (autopilot.md §11, unchanged: "owner fast-forwards"). If the final PR is merged or closed by a human while the run is `finalizing`, the run completes immediately.

---

## F. Persona changes

### F.1 Worker (`roles/worker.yaml`)

Current: "Drive the PR to green: watch CI … Land or merge it as soon as it is green." New contract:

- Implement, run `wd check`, commit, self-review, open the PR **based on the branch you were started from**.
- **Do not merge** the PR, do not enable auto-merge, do not watch CI. The daemon gates and lands it.
- Push, confirm the push succeeded and `wd check` is green locally, then end with **`wd job done`**. Report status to the coordinator as before (message on finish, on block, on failure).
- If the daemon later sends fix evidence (red CI / conflict), fix on the same branch, `wd check`, push, and end again with `wd job done`.

The `worker` role is also used outside autopilot (pipelines, standalone). So the persona is written to be brief-driven: "*If your brief says landing is daemon-owned (autopilot always says so), never merge; end with `wd job done` after the push. If it does not, merge once green as before.*" The daemon injects the line `Landing is daemon-owned: do not merge; end with "wd job done" after a green push.` into the brief of every worker spawned inside a run (it already composes autopilot worker context via the hints-file mechanism; keep it file-backed — the 1024-byte launch-line limit applies, memory: launch-line-1024-byte-limit).

### F.2 Manager (`roles/autopilot.yaml`)

Remove from the normal path: polling CI, calling `land`, `gate_red`/`not_mergeable` handling, post-land cleanup (terminate/remove_worktree/mark landed), and answering forwarded approval prompts (the daemon-owned chain, §I, answers them). Keep: decomposition; spawning workers up to `max_parallel_workers` with the integration branch in each brief; steering stuck workers; reading `fix_dispatched`/`landed` events only to keep the journal; verifying `done_when` and calling `autopilot_complete` when woken (§E.3); `brain_consult` for genuine design calls. `land` remains documented as an *escape hatch* "only if the digest says `landing: disabled`". The persona states plainly: *"warden lands PRs, fixes red gates, and answers prompts for you; your job is the plan."* The digest (`ComposeDigest`) gains a **Landing** section (open run PRs with gate state, fix state per task) so a successor sees in-flight fixes.

### F.3 Brain/resolver (`roles/brain.yaml`)

Persona gains the fix-and-report contract of §D.2/§D.3 (consult mode keeps the recommend-only reply). The two modes are distinguished by the prompt template, not by a second role.

---

## G. Config, audit events, status, model-call volume

### G.1 Config (`autopilot` block; all hot-reloaded unless noted; defaults generous)

```yaml
autopilot:
  guardian:
    use_fast_brain: true              # off switch for triage (§C.5)
    triage_max_waits: 3
    triage_max_wait_total: 30m
    triage_min_confidence: 0.8
    manager_loss_debounce: 20s        # §H
  landing:
    enabled: true                     # false => manager/operators land via `land`
    ci_grace: 10m
    local_gate_timeout: 15m
  fix:
    max_red_shas: 5
    rerun_flaky: true
    evidence_max_bytes: 8192
    busy_defer: 10m
  resolver:
    enabled: true
    timeout: 30m
    max_attempts_per_blocker: 2
    max_per_run_per_day: 6
  completion:
    merge_default: true
    manager_verify_timeout: 30m
  prompts:
    brain_enabled: true               # tier-1 brain stage of the chain (§I)
    brain_timeout: 5m
  limits:
    switch_in_place: true             # §J
```

The tick *cadence* of the landing pass is `guardian.interval` (read once at loop start; restart-only, as today). `auto_approve.use_fast_brain` keeps its meaning for non-autopilot agents; for autopilot-owned agents the arbiter stage is unconditional (§I).

### G.2 Audit event names (`internal/audit`, new constants)

`autopilot_landed`, `autopilot_pr_unowned`, `autopilot_gate_missing`, `autopilot_gate_rerun`, `autopilot_fix_dispatched`, `autopilot_fix_capped`, `autopilot_triage` (detail: action, confidence, source), `autopilot_triage_failopen`, `autopilot_resolver_started`, `autopilot_resolver_result`, `autopilot_prompt_resolved` (detail: stage, decision), `autopilot_prompt_escalated`, `autopilot_manager_lost`, `autopilot_manager_respawned`, `autopilot_final_pr_opened`, `autopilot_final_pr_green`, `autopilot_limit_switched`, `autopilot_limit_resume_scheduled`. Existing `autopilot.needs_attention`, `autopilot.unparked`, `autopilot_land`, `autopilot_complete` unchanged.

### G.3 Status fields (`RunStatus`, spec-first via `openapi.yaml` → `make generate`)

`state` gains `finalizing`; new: `landing {open_prs, last_land_at, last_pass_at}`; `fix[]` `{task, pr, kind, red_streak, last_dispatched_sha, rerun_done}`; `resolver {active, attempts_today, last_outcome}`; `triage {last_action, last_at, waits_in_row}`; `final_pr {number, url, head_sha, gate, fix_attempts}`; `manager {liveness, last_loss_at}`; `next_step {action, at}` (§J — never empty for `degraded`/`healing`/`backoff`); `resting_until`. All additive (clients ignore unknown fields; no capability flag change needed beyond an `autopilot-final-pr` capability string for wd-app).

### G.4 Expected model-call volume

Per run, steady state (healthy): **zero** model calls from the landing pass (mechanical). Fast-Brain calls only on exceptions:

- CI red classification: 1 fast call (≤1.5s) per red CI head SHA.
- Guardian triage: 1 thinking call per stalled run per tick during a stall, i.e. ≤ one per minute per stalled run, ≤ ~10 per stall episode before the ladder acts; zero on healthy runs.
- Prompt chain arbiter: 1 fast/thinking call per unanswered worker prompt (replaces a manager turn); tier-1 brain agent only when the arbiter escalates (expected a few per day per active run).
- Resolver: ≤ `max_per_run_per_day` (6) agent sessions per run per day, typically 0–2.
- Completion: 0–1 resolver for `done_when`, 0–1 for `base_merge`.

Net: replaces the manager's CI-polling/`land`/prompt turns (the dominant steady-state cost today) with a handful of bounded calls.

---

## H. Manager loss

### H.1 The liveness answer

`GuardianRuntime.BrainSession` (present/missing/unknown) is replaced by `BrainLiveness(ctx, agentID) Liveness` returning `{Kind, Reason}`:

| Kind | Condition | Action |
|---|---|---|
| `alive` | record exists, status in the live set (`spawning, working, waiting_for_input, idle, rate_limited`) **and** tmux session alive (`poller.SessionAlive`) | none |
| `missing` | no active record (deleted/archived) | immediate respawn |
| `terminal` | record exists, status ∈ {`done`, `errored`, `orphaned`} or the record is marked terminated by `terminate_agent` | immediate respawn |
| `tmux_gone` | record says live but the tmux session no longer exists | immediate respawn |
| `unknown` | the store/tmux probe errored | no action (as today) |

**Immediate respawn** = `managerLost` + `rotateStep` in the **same slot** (`<scope>-autopilot`, `adoptSlotSession` / `clearDeadSlotSession` as today) with a fresh recovery digest (autopilot.md §4), with **no heartbeat wait and no nudge to the dead pane**. Backend: the manager's last backend if still selectable, else the ladder (`selectBrain`). The loss is debounced by `manager_loss_debounce` (20s): the condition must hold on two consecutive observations 20s apart, so a transient tmux hiccup or the middle of a hot-swap is not mistaken for a loss.

### H.2 Accidental termination vs intent vs hot-swap

The default is **accidental ⇒ respawn**. Termination is *intentional* — and the guardian stands down — only in these cases, all expressed as explicit state rather than inferred:

1. **Operator pause/stop/disable.** The run state is `paused`, `stopped`, `disabled` or `registered`; `guardianTick` already skips those runs. `wd plan pause|stop`, `wd autopilot off` are the operator's way to say "stop"; the spec states that **`wd agent terminate <manager>` on a running run is not a stop** — it is treated as accidental and the manager is respawned (documented in the CLI help for `terminate`: "for an autopilot manager use `wd plan pause`").
2. **Warden's own teardown paths**, which set `r.expectedGone[agentID]` under `c.mu` *before* terminating: `teardownBrain` (disable/complete), `RestartRun`, run delete. The flag is consumed by the next observation.
3. **Hot-swap in progress** (guardian rotation, `plannedRotate`, `BackendRecoveryCoordinator`, `usage_recover`): `HotSwap` rewrites the record in place. The Controller's own swaps set `r.swapping=true` for their duration. Swaps initiated *outside* the Controller (recovery coordinator, §J) are visible as a lifecycle swap-in-progress marker on the session (`Session.SwapInFlight`, set/cleared by `Lifecycle.HotSwap`; the liveness probe treats `SwapInFlight` as `alive`). Debounce (§H.1) is the backstop if the marker is missed.

An accidental termination is audited `autopilot_manager_lost {cause: missing|terminal|tmux_gone, actor?}` — `actor` is the `X-Warden-Actor` session if the terminate came through the API, so the operator can see who did it — and notified once.

### H.3 What the successor must verify before acting

The recovery digest is extended with a mandatory **"Before you act" preamble** (and the persona repeats it), and the daemon pre-computes the facts so the successor verifies rather than recomputes:

1. **Ledger** — `tasks`/`landings` as stored; reconcile `pr_open`/`gated` rows with the **Landing** section (open PRs and fix state, §F.2).
2. **Open PRs** — the run's open PRs and their gate state (the landing pass owns landing them; the successor must not re-spawn a worker for a task whose PR is open or being fixed).
3. **Live workers** — `list_agents` filtered to the run: for every non-`landed` task, whether a live worker exists; spawn only for tasks with neither a live worker nor an open PR nor a fix in flight.
4. Pending wakes: any outstanding `fix_dispatched` / prompt chain item addressed to the old manager id is re-addressed to the slot (same id, since the slot id is stable).

Non-idempotent actions (spawn, terminate) are never repeated from memory of the predecessor; the successor derives them from 1–3.

---

## I. Prompt chain

### I.1 Replace the mailbox forward

`autopilotApprovals.Forward` (mailbox to the manager) is removed from the autopilot path. For **every run agent — workers, the manager, and resolvers** (any session tagged `autopilot` + `run:<id>`) an unanswered prompt goes through a **daemon-owned chain**; stages are tried in order and the first that answers wins:

1. **Auto-approve policy** (existing `approval` rules; the generous default policy installed at enable).
2. **Fast-Brain arbiter** (`fastbrain.ArbitrateApproval`): today conditional on `auto_approve.use_fast_brain`; **unconditional for autopilot-owned agents** (a no-runner engine fails open to the next stage). Tool-permission prompts use the fast tier (≤1.5s), strategic/multi-choice questions the thinking tier (≤10s). Acts at confidence ≥ 0.8 (`ArbiterConfidenceThreshold`).
3. **Tier-1 brain agent**: spawned through `internal/brainconsult` (`Mode: answer`), reads the stuck agent's pane (`Output`, last 80 lines), the plan, and the ledger, and returns `{answer: approve|reject|select_option|type, option?, text?, reason}`. The **daemon types the answer in** (the same input path as `approve`/`send_to_agent`); the brain never touches the stuck agent directly.

### I.2 Pinning tier-1

The brain agent is spawned with its **role tier pinned to tier-1** via the existing role-tier mechanism (`set_role_tier` / `GET list_role_tiers`; role `brain` → `tier-1`): the spawn sets the role-tier mapping for `brain` at run start if unset (via the same API) and the resolver of tier→model (tiered-model-routing) picks the tier-1 model/backend. Autopilot's spawn request additionally carries `model_tier: tier-1` explicitly so a user's role-tier override cannot silently route a prompt answer to an expensive model. (Memory: a broken tier-2 hot-swap once killed workers; tier-1 is the proven path.) The §D resolver uses the *same* brain role but its tier is `resolver.tier` (default tier-1, raisable) because it edits code.

### I.3 Timeouts

Stage 1: immediate. Stage 2: ≤1.5s fast / ≤10s thinking (fastbrain hard limits). Stage 3: `prompts.brain_timeout` default **5m**. If stage 3 times out or fails, the chain does **not** go to a human: the prompt is retried at stage 3 once with a fresh brain; a second failure applies the safe default below and audits `autopilot_prompt_escalated`.

### I.4 Destructive prompts

`approval.IsDestructive` is checked **first, before any stage**, and is non-overridable (as in the arbiter today). A destructive prompt skips stages 1–2 and goes to stage 3 with a *deny-by-default* instruction: the brain may approve only if the action is clearly inside the run's own sandbox (its worktree, run-owned branches) and necessary for the task; otherwise it rejects with a reason typed back to the agent ("rejected: destructive action not permitted unattended; choose a non-destructive alternative"). If stage 3 fails, the default is **reject**. Never a silent approve, never a human wait.

### I.5 Audit and the human inbox

Every chain outcome is audited `autopilot_prompt_resolved {agent, stage, decision, confidence, category}`. The human inbox **only mirrors** (the existing mirror to `humanRecipient`), carrying the decision for visibility; nothing blocks on it and a human answering an already-resolved prompt is a no-op. This supersedes autopilot.md §8's brain-mailbox routing for autopilot-owned agents; the §8 ownership guard is unchanged.

---

### I.6 Implementation status

Implemented: `autopilotApprovals` (internal/daemon/autopilot_approvals.go) no longer mailboxes the manager for answers — it sends only a short informational note plus the human-inbox mirror. The poller chain (internal/poller/promptchain.go) runs policy → arbiter (unconditional for run agents) → one async stage-3 consult per prompt via `brainconsult` `Mode: answer`, pinned `tier-1`; a changed or vanished prompt cancels it; two failures reject a destructive prompt, audit `autopilot_prompt_escalated` and hand the agent to the guardian (`NotifyEscalation`). `prompts.brain_timeout` is currently the daemon constant `promptBrainTimeout` (5m).

---

## J. Usage limits and resting states

### J.1 Rate-limited or out-of-quota run agents

For **every run agent** (manager, workers, fix-up workers, resolvers) the daemon handles `rate_limited` / quota exhaustion in this order (reusing the existing hot-swap and usage-recovery path — `BackendRecoveryCoordinator`, `usage_recover`; autopilot adds policy, not a second swap path):

1. **Switch in place** (`limits.switch_in_place`, default true): select another **selectable** backend (autopilot.md §7 ladder + registry availability, honouring `allow_pay_per_use`) and hot-swap the agent into the same session/slot, continuing the **same task** (the handoff carries the transcript summary; the task, branch, PR and worktree are unchanged). Audited `autopilot_limit_switched {agent, from, to}`.
2. **If none is selectable**: record the **earliest reset** across the limited backends (`tierstate.earliestReset()`, plus provider capacity snapshots from usage reconciliation), set the agent's and the run's `resting_until`, schedule a **timed resume** at that instant (+ `rate_limit.buffer`), and at that time resume the agent in place (the existing `resume_prompt` path; or the S0 menu answer). Audited `autopilot_limit_resume_scheduled {agent, at}`. If no reset time is parseable (monthly spend banner), the fallback is `rate_limit.spend_retry_interval` and the status says so.

A resting agent is not "idle": the overwatch and watchdog must not count a run whose agents are all resting-until-a-future-time as stalled (the watchdog's `watchdogDue` gains `!r.resting()`), and the landing pass keeps landing/fixing for other agents.

### J.2 Every run state and heal stage has a scheduled next step

Invariant (status contract): **a run in `degraded`, `healing` or `backoff` always carries `next_step {action, at}`**, written by whoever puts it in that state. The guardian's next action per state:

| Run state / heal stage | Meaning | Guardian next action | Scheduled at |
|---|---|---|---|
| `registered` | durably registered, not started | none until `plan run` | — |
| `starting` | manager spawn in flight | verify manager alive; on spawn failure → `degraded` | spawn timeout (`heartbeat_timeout`) |
| `active` / stage `healthy` | normal | landing pass, overwatch, triage-on-stall | every tick |
| `active` + watchdog `armed` | no progress ≥ window, agents busy | wait | next tick; re-evaluated each tick |
| `active` + watchdog `escalating` | climbing ladder due to no progress | triage → nudge / restart / rotate / resolver | `healNextAt` |
| `healing` / stage `nudged` | nudge sent | await heartbeat; if none → restart | `healNextAt` (= nudge + `heartbeat_timeout`) |
| `healing` / stage `restarted` | restarted same backend | await heartbeat; if none → rotate | `healNextAt` |
| `healing` / stage `rotated` | rotated to next backend | await heartbeat; if none → backoff (or watchdog park → resolver) | `healNextAt` |
| `degraded` / stage `backoff` | all backends limited/gated or spawn failing | retry the ladder from the top; floor = earliest backend reset | `backoffNextRetry` (capped-exponential, ≤ `backoff_max`; floored/woken by earliest reset) |
| `degraded` + needs-attention (parked) | the §D.5 human list only | none until unparked; `next_step` = "waiting for you: `<command>`" with `at` empty by design | unpark event |
| `degraded` + resolver running | resolver attempt in flight | await result, then verify resolved | resolver `timeout` |
| `degraded` + manager lost | §H respawn in flight | respawn; on failure → backoff | `manager_loss_debounce` then immediate |
| resting (all run agents limited, §J.1) | no selectable backend | timed resume | `resting_until` |
| `finalizing` | §E | merge default / verify / open+gate final PR / fix | every tick; `completion.manager_verify_timeout` for verification |
| `paused` | operator paused | none (operator `resume`) | — |
| `stopped` | operator stopped | none (`restart`/`run`) | — |
| `complete` | final PR green or merged by human | none | — |
| `disabled` | kill switch | none | — |

The needs-attention park and `paused`/`stopped`/`complete` are the only states without an automatic next step, and each says what operator command moves it.

---

## K. Verification plan (contract level, for the implementation PRs)

- **Landing:** green+mergeable lands once and finalizes; pending does nothing; red dispatches exactly once per head SHA; `land` and the pass racing on the same PR merge once; a crash between `gh pr merge` and bookkeeping is healed by the next pass; an unowned PR into integration is never touched; a paused run between snapshot and apply is left alone.
- **Fix loop:** flaky CI gets exactly one rerun then a dispatch; classifier failure ⇒ dispatch; dead owner ⇒ fix-up worker on the same branch; the 5th consecutive red SHA calls the resolver, not a 6th worker; a green head resets the streak.
- **Triage:** every action in the closed set has an effect test; `wait` is rewritten at 3 in a row / 30m; `restart`/`rotate` below 0.8 downgrade; invalid/timeout/no-runner executes the mechanical rung (golden-compare with today's ladder); `use_fast_brain=false` is byte-identical to today.
- **Resolver:** attempt caps per blocker and per day survive restart; "resolved" requires the blocker check, not the report; forbidden actions are refused by the ownership guard.
- **Final PR:** completion waits for no open run PRs; stale base is merged before the PR; the PR body is generated and idempotent; red final PR keeps the run `finalizing` and fixes via `final-fix-*`; no code path merges the final PR (grep-able test: no `gh pr merge` against a default-branch-based PR).
- **Manager loss:** terminate with record kept, dead tmux, and deleted record each respawn within one debounce; operator pause/stop/disable and an in-flight hot-swap do not; successor does not double-spawn workers.
- **Prompt chain:** each stage answers independently; destructive prompts reject by default; the human inbox mirrors only.
- **Limits:** quota-exhausted agent switches backend and keeps its task; none selectable ⇒ `resting_until` + timed resume; no `degraded|healing|backoff` run lacks `next_step` (invariant test over the state table).
- **E2E:** isolated daemon on an alt port (autopilot-s7 rig), 3-task plan with one injected red CI and one conflict: plan → PRs → land → fix → final PR green, with no agent calling `land`.

## L. Open questions flagged for implementation (not blocking the freeze)

1. `store.Status` has no explicit `terminated` value; the `terminal` liveness kind relies on `done|errored|orphaned` plus a `terminate_agent`-written marker. If `Terminate` does not currently record one, the implementation PR for §H adds it (additive store field) — decision here is only that it must be detectable.
2. `Session.SwapInFlight` (§H.2) may be satisfiable with an existing lifecycle field; if so reuse it.
3. Whether `wd agent terminate` on an autopilot manager should additionally *warn* at the CLI (recommended; copy in §H.2).

## J.3 Implementation status (t14-usage-limits)

- `internal/autopilot/limits.go`: `limitTick` runs each guardian interval for every live run. A `rate_limited` run agent (manager, worker, fix-up worker) is handed to `LimitRuntime.SwitchLimited` (daemon: `BackendRecoveryCoordinator.PreviewCandidates` + `OnHardLimit` — no second swap path). On `ErrNoAlternateBackend` the earliest reset (agent's recorded restore time, else `tierstate.earliestReset()`, else a 30m fallback) + buffer becomes the agent's `resting_until`; at that time `ResumeRateLimit` runs, and a still-limited agent is re-checked once and rescheduled — never left without a next action. Audit events: `autopilot_limit_switched`, `autopilot_limit_resume_scheduled`, `autopilot_limit_resumed`.
- A resting agent is not stalled: the watchdog (`watchdogDue`) and the manager heartbeat ladder skip a run whose manager (or any agent) rests until a future time.
- `internal/autopilot/nextstep.go`: `RunStatus.next_step {action, at, owner}` and `resting_until` are surfaced in `wd autopilot status` and the API. `nextStepViolation` encodes the §J.2 invariant (backoff needs a retry time; a zero `healNextAt` on a heal stage means "due immediately" and falls back to the next tick). The package's `TestMain` fails the suite if any guardian tick in any test observes a violation.

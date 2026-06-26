# Intelligent Inter-Agent Collaboration System Design

**Date:** 2026-06-14 (rewritten 2026-06-23)
**Feature:** Inter-agent awareness & file-conflict detection
**Status:** ✅ **Hardened MVP shipped** (PRs #20–23, merged 2026-06-23) — advanced phases deferred behind real usage
**Estimated Effort:** ~1 week (MVP); larger phases deferred behind real usage

---

## Reading guide

This document was originally a 4,000-line design covering three daemon-side
monitors (file conflicts, work-overlap detection, GitHub branch/CI tracking)
plus a large performance/optimization layer. A subsequent review found the
design over-built for warden's real concurrency (single-user, typically
5–20 agents) and internally contradictory (it shipped both a full design and a
"build the 200-line MVP instead" rebuttal with no decision recorded).

On 2026-06-23 the scope was locked to a **Hardened MVP**: file-conflict
detection only, corrected against the *actually shipped* APIs. The full
three-monitor design and the simplification debate are preserved, condensed,
in [Appendix A: Deferred design & rationale](#appendix-a-deferred-design--rationale)
so the engineering reasoning isn't lost — but they are **not** the plan of
record. The plan of record is the [Hardened MVP](#hardened-mvp-plan-of-record)
below.

The foundational primitives this feature builds on (`mailbox`, `ctxstore`, the
long-poll `hub`) are **already shipped and hardened** — see
[Foundational layer (shipped)](#foundational-layer-shipped).

---

## Goal

Enable warden agents to become aware when **another agent is editing the same
file**, so the user (or the agents themselves) can coordinate before merge
conflicts happen. Warnings are informational, delivered through the existing
mailbox, and never block an agent's flow.

Everything else from the original vision — work-overlap/deduplication
detection, CI/branch monitoring, real-time FSNotify — is explicitly out of
scope for the MVP and gated on observed need (see Appendix A).

---

## Foundational layer (shipped)

This collaboration feature routes automated warnings through the mailbox and
(in deferred phases) infers collaboration from the shared context store. Those
primitives were reviewed and hardened ahead of this work; **all six findings
(H1–H6) are implemented** as of v4.1.0:

| ID | Issue | Resolution (shipped) |
|----|-------|----------------------|
| H1 | Unauthenticated `from`/`updated_by` provenance | `daemon.sanitizeSender` write gate reserves `daemon`/`system`; agents can't forge them; `""` → `human`. Trust model documented in `mailbox`/`ctxstore` package docs. |
| H2 | No atomic read-modify-write on context store | `ctxstore.CompareAndSet` + `Append` (`internal/ctxstore/ctxstore.go`), exposed over HTTP/CLI/MCP (`ctx_cas`, `ctx_append`); 409 on conflict. |
| H3 | Unbounded inbox growth, full rewrite/op | `compact()` + `maxInboxMessages` cap + `readRetention`; ids from a high-water mark (`nextID`), never recycled. Unread never dropped. |
| H4 | No long-poll wait for MCP agents | `wait_for_message` MCP tool proxies `GET /sessions/{id}/messages/wait`. |
| H5 | Wake-on-send status TOCTOU | `handleSendMessage` re-`Get`s status immediately before tmux `Input`; wake is best-effort, inbox is source of truth. |
| H6 | `mailbox.All()` aborts on one corrupt inbox | Aggregate view skips-and-logs; `Messages(to)` stays strict. |

**Implication for this feature:** the MVP must send warnings through the
sanctioned daemon path so they're stamped with the reserved `daemon` sender —
it must not bypass the gate.

---

## Hardened MVP (plan of record)

### Summary

A single daemon-side goroutine polls each active agent's worktree with
`git diff --name-only HEAD` on an interval, builds a `file → [agents]` map, and
sends an inbox warning to every agent sharing a file with another. Read-only
HTTP/CLI/MCP surface lets users and agents query current conflicts. No
FSNotify, no caching, no GitHub, no new dependencies.

```
┌────────────────────── Warden Daemon ──────────────────────┐
│  poller (existing) ── pressure sampler ── collab.Monitor   │
│                                              │ git diff     │
│                                              ▼ per worktree │
│                                   file → [agents] (in-mem)  │
│                                              │              │
│                         ┌────────────────────┼───────────┐ │
│                         ▼ warn               ▼ query      │ │
│                   mailbox (from:"daemon")  GET /collab/... │ │
└────────────────────────────────────────────────────────────┘
```

### Three corrections vs. the original MVP sketch

The original spec's MVP code (Appendix A) is illustrative but wrong against the
shipped codebase in three ways. The plan of record fixes all three:

1. **Session filter.** The sketch kept only `sess.Status == "working"`. But
   agents in `waiting_for_input`, `idle`, or `rate_limited` still hold
   *uncommitted edits in their worktree* and are prime conflict candidates.
   Correct filter: **has a worktree AND status is not terminal**
   (`done` / `errored` / `orphaned`). Statuses are defined in
   `internal/store/types.go`.

2. **Mailbox API.** The sketch calls `m.mailbox.Send(ctx, agentID, msg)`, which
   does not exist, and a `go m.mailbox.Send(...)` fire-and-forget that would
   bypass the H1 provenance gate and swallow errors. The real surface is
   `mailbox.Store.Append(mailbox.Message{To, From, Body})`
   (`internal/mailbox/mailbox.go`). Internal daemon code is the legitimate
   origin for `From: "daemon"` (agents are blocked from forging it by
   `sanitizeSender`). Errors are logged, not dropped.

3. **Auth.** The original spec predates warden's bearer-token remote auth. The
   new `/collab/*` routes **must be registered behind `authMiddleware`**
   (`internal/daemon/middleware.go`), like every other daemon route.

### Package: `internal/collab`

`monitor.go`:

```go
type Monitor struct {
    store store.Store
    mbox  *mailbox.Store

    mu    sync.Mutex
    dedup map[string]time.Time // "agentID\x00file" -> last warned
}

func NewMonitor(st store.Store, mbox *mailbox.Store) *Monitor

// Run polls every interval until ctx is cancelled (mirrors poller.Run).
func (m *Monitor) Run(ctx context.Context, interval time.Duration)

// Conflicts recomputes current file conflicts (used by Run and the read API).
func (m *Monitor) Conflicts(ctx context.Context) ([]Conflict, error)
```

- `Conflicts` lists active sessions, filters to (worktree set ∧ non-terminal),
  runs `git -C <wt> diff --name-only HEAD` with a **5s `CommandContext`
  timeout** per worktree, and returns files touched by ≥2 agents.
- `Run` calls `Conflicts`, then for each participant of each conflict sends one
  warning via `m.mbox.Append(mailbox.Message{To: agentID, From: "daemon",
  Body: warn})`, gated by the dedup window. Append errors are logged.

**Dedup (kept — not cut).** Per-`(agent, file)` suppression of ~5 minutes. The
H3 inbox compaction bounds *storage growth* but not *re-warn spam*: without
this, an open conflict re-warns every tick. Implemented as the in-process
`dedup` map, pruned opportunistically each tick (entries older than the window
deleted inline) — no separate goroutine, no separate dependency.

**Warning format:**

```
⚠️  File conflict: internal/auth/auth.go
Also being edited by: agent-456 (refactor-jwt)
Coordinate before committing to avoid a merge conflict.
```

**Edge cases handled:** git timeout (skip that worktree this tick); missing /
GC'd worktree (`git diff` errors → skip, drop from map next tick); detached
HEAD irrelevant (diff against HEAD still works); no fsnotify, so no inotify
limits.

### Daemon wiring (`internal/daemon/server.go`)

- Add a `collab *collab.Monitor` field to `Server`; construct it in
  `NewServer` from the existing `st` and `mbox`.
- Start under `runCtx` alongside the poller and pressure sampler:
  `go s.collab.Run(runCtx, collabInterval)`. Cancellation on shutdown is
  automatic via `runCtx` (same lifecycle as `runPressureSampler`); no new
  WaitGroup needed since the loop returns promptly on `ctx.Done()`.
- Interval: 10s default; config key `collab_interval` (reuse the YAML config
  pattern in `internal/config`), `0` disables the monitor.

### Read-only surface

**HTTP** (`s.router()`, behind `authMiddleware`):

```
GET /collab/conflicts   → JSON [{file, agents:[{id,name}]}]
```

Recomputed on request (cheap at this scale; no shared cache to invalidate).

**CLI** (HTTP clients, matching existing verb style):

```
warden collab conflicts              # list all current file conflicts
warden collab who-is-editing <file>  # agents touching one file
```

**MCP** (optional, same package, day 4): `get_collaboration_status`
(conflicts scope) and `who_is_editing_file`, with path validation — reject
`..` segments, resolve and confine to repo root, 400 otherwise.

### Tests

- Table-driven `Conflicts` against a fake `store.Store`: no-worktree skipped,
  terminal-status skipped, two agents one file → one conflict, dedup suppresses
  the second tick within the window.
- One integration-style test: two fake sessions on the same file → each gets
  exactly one `from:"daemon"` message; a re-tick inside the window adds none.
- All run under `-race`.

### Deliverable

Users and agents can see, via CLI/HTTP/MCP, which agents are editing the same
file, and agents receive a deduplicated inbox warning within one tick (~10s).
No new dependency, one goroutine, passes the race detector.

---

## Implementation status (shipped 2026-06-23)

The Hardened MVP landed across four PRs, all merged to `main`:

| PR | What shipped | Where |
|----|--------------|-------|
| #20 | **Detection engine** — `collab.Monitor` daemon goroutine, `git diff` per worktree, dedup window, `from:"daemon"` inbox warnings; `GET /collab/conflicts` behind `authMiddleware`; `warden collab conflicts` / `who-is-editing`; `get_collaboration_status` / `who_is_editing_file` MCP tools. | `internal/collab`, `internal/daemon/collab_routes.go`, `internal/cli`, `internal/mcp` |
| #21 | **Dashboard panel** (beyond the documented MVP surface) — "File conflicts" card on the web Overview tab; polls `GET /collab/conflicts` every 5s, click-through to the editing agent. | `web/src/components/ConflictsPanel.tsx`, `OverviewTab.tsx` |
| #22 | **Conflict-check prompt hint** (beyond the documented MVP surface) — spawned agents get an `--append-system-prompt` nudge to call `who_is_editing_file` / check inbox before editing shared files, gated by `collab_hint` (default on). Closes the "warnings land in an inbox nobody polls" gap. | `internal/lifecycle`, `internal/config` |
| #23 | Docs reconciliation (FEATURES.md merge fixup). | `docs/FEATURES.md` |

**Config keys actually shipped:** `collab_enabled` (bool), `collab_interval`
(duration; `0` disables), `collab_hint` (bool). All three appear in
`warden config` under a `collaboration` section.

**Operational note:** the #22 prompt hint only applies to **newly spawned**
agents — already-running agents are unaffected until restarted. Detection runs
in the daemon-owned process; a rogue manual `warden daemon` shadowing the port
will not run the monitor.

**Update (2026-06-25): FSNotify real-time detection shipped.** The 10s git-diff
poll is now augmented by an fsnotify watcher (`internal/collab/watcher.go`) that
reacts to edits in subseconds. It watches each tracked worktree's directories
(recursively, skipping `.git`), reconciles the watch set against the
active-session view on every poll tick (warden has no termination event bus, so
the poll loop drives cleanup), debounces an edit burst into a single scan, picks
up newly created directories from their create events, and enforces an inotify
watch budget (80% of `/proc/sys/fs/inotify/max_user_watches`, Linux). It
degrades cleanly to pure polling when fsnotify can't initialize or the budget is
exhausted, so the poll loop remains the safety net. Tested under `-race` with a
fake `fsWatch` backend (reconcile/budget/debounce bookkeeping) plus one real-
fsnotify end-to-end test.

**Update (2026-06-26): BranchTracker shipped (#44).** A daemon-side monitor
(`internal/branchtrack`) reports, per active agent with a branch, its GitHub CI
status (`gh run list` inside the worktree → success/failure/pending/none) and its
standing vs `origin/main` (commits behind/ahead + merged), deduping gh/git work
by branch. Alerts are **informational, never blocking**: a newly-observed CI
failure delivers an inbox note **and** a desktop notification (desktop reserved
for CI failures); a merged branch or one >10 commits behind delivers an inbox
nudge. A 5-minute dedup window keyed on `(branch, signal-state)` re-alerts on a
state change (pending→failure) but suppresses steady-state repeats. Every
subprocess **fails open** (missing/unauthenticated `gh`, timeout, non-repo
worktree → skip that branch this tick). Read-only surfaces: `Statuses(ctx)`,
`GET /collab/branches`, client `BranchStatuses`, `warden branches` (`--json`),
the `get_branch_status` MCP tool. Opt-in: `branch_track_enabled` (default false),
`branch_track_interval` (default `2m`). Structurally mirrors `collab.Monitor`; no
optimization layer (warden runs ≤10 agents).

**Still remaining:** nothing from this spec is planned. The remaining "advanced
collaboration" ideas — OverlapDetector, collaboration groups, SSE replay + the
multi-cache/circuit-breaker layer — were **audited and dropped** (see Appendix A
and the Deferred section): the first has no live signal and overlaps the shipped
conflict detector, the second is redundant with the pipeline subsystem, and the
third optimizes for a scale warden doesn't carry.

---

## Configuration

```yaml
# warden config (internal/config) — as shipped
collab_enabled: true        # master switch for the file-conflict monitor
collab_interval: 10s        # file-conflict poll interval; 0 disables
collab_hint: true           # append the conflict-check nudge to spawned agents
branch_track_enabled: false # opt-in CI + branch-vs-main monitor (#44)
branch_track_interval: 2m   # branch-tracker scan interval
```

No `GITHUB_TOKEN`: BranchTracker shells out to the operator's already-authenticated
`gh`/`git` inside each worktree and fails open if either is unavailable. No
FSNotify tunables — those belong to deferred phases.

---

## Security

- **Provenance:** warnings sent as `From: "daemon"` through the internal
  `Append`; agents cannot forge that sender (H1). ✔
- **Auth:** `/collab/*` behind `authMiddleware`. ✔
- **Path injection:** `who_is_editing_file` / `who-is-editing` reject `..` and
  confine to repo root. ✔
- **No secrets:** the MVP touches no external API and stores no tokens.

---

## Deferred (gated on observed need)

Not built in the MVP; promote only when real usage justifies it (see
Appendix A for the full original design and the data to collect first):

- ~~**FSNotify real-time detection** + inotify watch-budget management.~~
  **Shipped 2026-06-25** — see the Implementation status update above and
  `internal/collab/watcher.go`. The "future FSNotify phase must either build a
  termination event bus or reconcile watchers against the active-session set on
  each tick" caveat from Appendix A.1 was resolved with the per-tick reconcile.
- ~~**BranchTracker / GitHub CI monitoring**.~~ **Shipped 2026-06-26 (#44)** —
  see the Implementation status update above and `internal/branchtrack`. Built as
  the lean version of the A.1 design: the per-branch dedup and merge/behind
  detection were kept; the ETag conditional requests, circuit breaker, and
  exponential backoff were **cut** (warden runs ≤10 agents on a 2m tick — there is
  no rate pressure to manage, and fail-open subprocesses are simpler than a
  breaker). The "orthogonal to conflict detection; partly duplicated by
  `gh pr checks`" caveat held, but bringing CI status into the agent's inbox + the
  operator's desktop (where warden's other alerts already land) earned its ~350 LOC.
- **OverlapDetector** (work deduplication) — **dropped, not deferred.** Its only
  signal was an agent's plan file matched by agent/session id, but real specs are
  named `YYYY-MM-DD-feature.md` and carry no agent id — the signal is dead under
  current conventions. The idea also overlaps the shipped file-conflict detector.
  Resurrecting it would mean inventing a fresh signal from scratch; not worth it at
  current scale.
- **Collaboration groups** — **dropped, not deferred.** Redundant with the pipeline
  subsystem, which already expresses grouped work through dependencies, handoffs,
  and shared context.
- **SSE event replay, parallel startup recovery, multi-cache layers, circuit
  breakers** — **dropped, not deferred.** All optimizations for a scale (100+
  agents) warden doesn't carry; straight serial recomputation is correct here.

Each deferred item should also be reconciled with systems that shipped after
the original design: **bearer-token remote auth**, **worktree GC**
(what happens to tracked files when a worktree is pruned), and **pipelines**
(siblings editing shared files is expected, not a conflict).

---

## Appendix A: Deferred design & rationale

> The material below is the **original full design** (three monitors +
> optimization layer) followed by the **simplification review** that argued for
> the MVP. It is retained for engineering rationale only and is **not the plan
> of record**. The git history holds the verbatim earlier revision if the full
> per-component pseudocode is ever needed.

### A.1 Full three-monitor architecture (original)

- **CollabMonitor** — FSNotify watchers per worktree (subsecond) + 10s git-diff
  reconciliation; thread-safe `CollabStore` with `map[file]map[agent]FileEdit`;
  worker pool for non-blocking event handling; inotify watch-budget enforcement
  (read `/proc/sys/fs/inotify/max_user_watches`, 80% budget, fall back to
  faster polling when exhausted).
- **OverlapDetector** — every 30s, compare active sessions pairwise on subject
  (token Jaccard), file overlap, and plan-file similarity; weighted score
  `subject*0.3 + files*0.4 + plan*0.3`, warn above 0.6; collaboration hierarchy
  (pipeline/group > messages/shared-context > same-branch) suppresses
  warnings; O(N²) mitigated by a 10-worker pool, 200-comparison/tick rate limit
  with round-robin offset, and git-diff/plan/similarity caches.
- **BranchTracker** — poll GitHub Actions every 2m, grouped by branch (one call
  per branch, not per agent), ETag conditional requests, circuit breaker +
  exponential backoff, merge/behind detection via git, desktop notifications
  with 5-minute dedup.
- **Cross-cutting** — strict lock hierarchy (`CollabStore.mu` never held across
  mailbox/hub/git I/O), graceful shutdown via context + WaitGroup, SSE event
  sequence numbers with `Last-Event-ID` replay, parallel startup recovery.

A detailed list of 15 concurrency issues found and fixed during the original
review (data races, FD/goroutine leaks, deadlock on mailbox-under-lock, SSE
publish ordering, etc.) lived here; the lock-hierarchy and
"return copies, never internal pointers" rules from that review are worth
carrying into any future phase.

**Why deferred:** ~3,500 LOC, 13+ goroutines, a new OS-specific dependency, and
scale targets (100–250 agents) that are asserted, not observed. The full design
also assumed a `store.OnSessionTerminated` event bus for watcher cleanup that
**does not exist** — warden is poll-based — so a future FSNotify phase must
either build that bus or reconcile watchers against the active-session set on
each tick.

### A.2 Simplification review (original, abridged)

The review argued the 80/20 win is file-conflict detection alone (~200 LOC,
single goroutine, no deps), cutting FSNotify, OverlapDetector, BranchTracker,
all caching, all deduplication, collaboration groups, parallel startup, and SSE
replay. It recommended collecting data before building more:

- *FSNotify:* how often do two agents touch one file within 10s? How many
  concurrent agents, and what repo file-counts (inotify limits)?
- *Overlap:* have users actually hit duplicate work? Do agents write plan files
  at all? Is ~60% token-similarity confidence trustworthy?
- *Branch/CI:* what share of users run GitHub Actions and want the daemon (vs.
  `gh`) watching CI?

The locked Hardened MVP adopts this conclusion, with the three corrections in
[the plan of record](#three-corrections-vs-the-original-mvp-sketch) (the
review's own sample code had the status-filter and mailbox-API bugs).

### A.3 Resolved design questions (still apply)

- Conflict warnings are **informational**, not blocking.
- Collaboration state is **in-memory/ephemeral** — no persistence.
- Collaboration-vs-duplication (deferred phases): explicit (pipeline/group) >
  inferred (messages/shared context) > heuristic (same branch).
- Desktop notifications were reserved for CI failures (deferred); conflicts use
  the inbox.

---

## Changelog

**2026-06-14:** Initial design (revisions 1–5: full three-monitor design,
concurrency-fix appendix, performance overhaul, then a simplification review
left as "PENDING HUMAN DECISION").
**2026-06-21 (rev 6–7):** Added and then **implemented** the foundational-layer
hardening (H1–H6) on `mailbox`/`ctxstore`/hub.
**2026-06-23 (rev 8):** Scope locked. Rewrote the document around the
**Hardened MVP** (file-conflict detection only) corrected against shipped APIs
(session filter, `mailbox.Append`, `authMiddleware`). Moved H1–H6 to
"Foundational layer (shipped)" and the full three-monitor design + the
simplification debate into Appendix A as deferred rationale. Recorded
interactions to reconcile before any deferred phase (remote auth, worktree GC,
pipelines).
**2026-06-23 (rev 9):** MVP **shipped** (PRs #20–23). Added the
[Implementation status](#implementation-status-shipped-2026-06-23) section,
including two surfaces added beyond the documented MVP — the web dashboard
"File conflicts" card (#21) and the `collab_hint` conflict-check prompt nudge
(#22) — and updated Configuration to the three shipped keys
(`collab_enabled` / `collab_interval` / `collab_hint`).
**2026-06-25 (rev 10):** First deferred phase — **FSNotify real-time
detection** — shipped (`internal/collab/watcher.go`): subsecond fsnotify
detection with per-tick watch-set reconciliation against the active-session
view (resolving the missing termination-event-bus caveat), edit-burst debounce,
dynamic directory pickup, and an inotify watch budget, all degrading to the
existing poll loop. Updated Implementation status and Appendix A's deferred
list. Remaining deferred work unchanged (OverlapDetector, BranchTracker,
groups, SSE replay / multi-cache).
**2026-06-26 (rev 11):** **BranchTracker** shipped (`internal/branchtrack`, #44) —
opt-in daemon monitor of per-agent GitHub CI status + branch-vs-`origin/main`
standing, delivering informational inbox/desktop alerts with a 5m dedup window,
every subprocess failing open. Built lean (per-branch dedup + merge/behind kept;
ETag/circuit-breaker/backoff cut). Surfaces: `GET /collab/branches`, client
`BranchStatuses`, `warden branches`, `get_branch_status` MCP tool; keys
`branch_track_enabled` (default false) / `branch_track_interval` (default `2m`).
This was the **last** piece of the advanced bucket judged worth building: an audit
**dropped** OverlapDetector (dead plan-file signal, overlaps the conflict
detector), collaboration groups (redundant with pipelines), and SSE replay +
multi-cache (optimizing a scale warden doesn't carry). Nothing from this spec
remains planned.

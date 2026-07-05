# Autopilot — Formal Design (P0)

**Date:** 2026-07-05
**Status:** Design (P0 of the staged delivery)
**Master plan:** [2026-07-05-autopilot-brain.md](2026-07-05-autopilot-brain.md)

This document formalizes the approved master plan into buildable contracts:
concepts and ownership, state machines, the run-ledger schema, the daemon
endpoint contract, the idempotent `land` contract, the guardian algorithm, the
cost-tier backend selection algorithm, the ownership guard, and the brain
persona. P1–P4 implement against this document.

---

## 1. Concepts & ownership

| Term | Definition |
|---|---|
| **Plan** | An owner-authored YAML brief (`autopilot.plan.yaml`): a goal, constraints, and optional coarse tasks. Source of truth; owner-editable mid-flight. |
| **Run** | The daemon's execution of one plan: one brain + its workers + its ledger. Keyed by `run_id` (stable hash of repo + plan path). |
| **Brain** | One long-lived headless agent per run, role `autopilot`, spawned by the Controller on the cheapest available backend. Orchestrates via existing MCP tools. |
| **Worker** | Any agent or pipeline the brain spawns to do actual work. Tagged `autopilot` + `run:<run_id>`. |
| **Ledger** | Durable, daemon-store-backed run state written by the brain (write-ahead) and by `land` (authoritative landings). |
| **Integration branch** | `autopilot/integration` (configurable). The ONLY branch autopilot merges into. Owner fast-forwards it to main. |

Ownership invariants:

- Every agent autopilot creates (brain and workers) carries the `autopilot` tag
  and a `run:<run_id>` tag. Tags are applied at spawn by the Controller (brain)
  and inherited via role defaults (workers) — not left to persona discipline.
- **At most one active run per repository.** `POST /autopilot` enabling a second
  plan on the same repo fails with 409.
- Manual agents are invisible to autopilot's destructive paths (§8).

## 2. State machines

### 2.1 Run

```
disabled ──enable──▶ starting ──brain healthy──▶ active
   ▲                    │                          │
   │                    │ spawn fails              │ brain wedged/limited
disable (kill switch)   ▼                          ▼
   └───────────────  degraded ◀──────────────── healing
                        │  guardian backoff loop (never parks)
                        └──heal succeeds──▶ active
active ──all tasks landed──▶ complete (brain torn down, ledger retained)
```

- `disable` at any state: Controller stops spawning + landing immediately
  (kill switch); in-flight workers keep running; brain is terminated gracefully.
- `degraded` is visible in status (backoff stage, last error, next retry at).
  There is no terminal failure state — owner choice #4 (never park).

### 2.2 Task (ledger-tracked, brain-written)

```
pending → assigned → in_progress → pr_open → gated (CI running)
   gated → landed            (land succeeded; authoritative, daemon-written)
   gated → fixing → gated    (CI red / conflict; brain heals worker or respawns)
any → replanned              (brain revises the plan decomposition; audit-logged)
```

### 2.3 Brain lifecycle (guardian-driven)

```
healthy → wedged?     (heartbeat timeout w/ pending work)
  wedged → nudged     (send steering message)                    [stage 1]
  nudged → restarted  (same backend, fresh context, ledger cold-start) [stage 2]
  restarted → rotated (next backend down the cost ladder)        [stage 3]
  rotated → backoff   (all backends limited/gated: wait capped-exponential,
                       notify, retry from stage 1 — forever)     [stage 4]
healthy → planned-rotation (context critical or cadence) → healthy
```

## 3. Plan file schema (v1)

```yaml
version: 1
goal: "Ship the notifications feature"          # required
constraints:                                     # optional, injected verbatim
  - "all changes behind a feature flag"
tasks:                                           # optional; brain authors/refines if absent
  - id: api                                      # unique within plan
    prompt: "Implement the notifications REST API per docs/specs/notify.md"
    after: [other-id]                            # optional dependency edges
done_when:                                       # optional acceptance criteria the brain
  - "wd check passes on integration"             # verifies before declaring complete
```

Unknown fields are rejected (strict decode) so typos surface at enable time.
The brain re-reads the file when its mtime changes (owner steering mid-flight).

## 4. Run ledger

**Storage:** the collaboration blackboard (`internal/collab` ctx store, FileDB-
backed) under the reserved namespace `autopilot/<run_id>/…` — reusing
`ctx_set`/`ctx_get`/`ctx_cas` wholesale; no new storage engine. Landings are
additionally recorded by the daemon inside the `land` handler (authoritative,
not trusted to the brain).

**Keys (values are JSON):**

| Key | Writer | Content |
|---|---|---|
| `autopilot/<run>/tasks` | brain (`ctx_cas`) | task ledger: `{id, state, worker_id, branch, pr, note, updated_at}[]` |
| `autopilot/<run>/intent` | brain (write-ahead) | the single action the brain is ABOUT to take: `{action, target, params, at}` — cleared after the action completes |
| `autopilot/<run>/landings` | **daemon** (`land`) | append-only: `{branch, sha, pr, landed_at}` |
| `autopilot/<run>/journal` | brain | rolling decision log (bounded, newest-first) for cold-start context |

**Write-ahead rule (persona-enforced, §9):** before any side-effectful call
(spawn, send, land, terminate, remove_worktree) the brain writes `intent`, acts,
then updates `tasks` and clears `intent`. A restarted brain reads `intent` first:
if present, it verifies whether the action already happened (e.g. `land` is
idempotent; `list_agents` shows a spawn) before re-issuing.

**Cold start:** fresh brain = plan file + `tasks` + `landings` + `journal` +
`list_agents` (live truth for in-flight workers). Nothing lives only in a brain's
context.

## 5. Daemon endpoint contract (spec-first: `openapi.yaml` → `make generate`)

```
POST /autopilot            {enabled: bool}         → 200 AutopilotStatus | 409
GET  /autopilot                                    → 200 AutopilotStatus
```

```jsonc
// AutopilotStatus
{
  "enabled": true,
  "runs": [{
    "run_id": "…",
    "plan_file": "autopilot.plan.yaml",
    "repo": "/home/…/project",
    "state": "active",                  // §2.1
    "brain": {
      "agent_id": "…", "backend": "antigravity", "tier": "free",
      "last_heartbeat": "2026-07-05T12:00:00Z",   // last MCP call
      "context_level": "ok"
    },
    "workers_in_flight": 3,
    "tasks": {"pending": 4, "in_progress": 2, "landed": 6},
    "backoff": null,                    // or {stage, next_retry_at, last_error}
    "landed_total": 6
  }]
}
```

409 on enable when: another daemon-registered run is active for the same repo,
plan file missing/invalid, or integration branch name collides with a protected
branch. Error body carries the reason verbatim.

Surfaces (all thin wrappers over these two routes): CLI `warden autopilot
on|off|status`, MCP `set_autopilot`/`autopilot_status` (in
`internal/mcp/tools_extra.go`), TUI header badge + keybind, web toggle + status
panel. Toggling is live — loops gate on the flag per tick; no daemon restart.

## 6. `land` contract (new MCP tool + `wd land`)

`land(agent_or_branch)` merges one worker branch into the integration branch.

**Preconditions (all daemon-checked; any failure → typed error, no side effects):**
1. Caller's run is `active` (kill switch honored).
2. Target branch is autopilot-owned (`run:<run_id>` tagged agent or recorded
   worker branch).
3. A PR exists whose base is the integration branch.
4. Latest CI on the PR head SHA is **green** (via `branchtrack`; "no CI
   configured" is a distinct error, not a pass — see master-plan risk).
5. PR is mergeable (no conflicts).

**Semantics:**
- Merge strategy from config (default squash); delete worker branch if
  `delete_branch`; record the landing in `autopilot/<run>/landings`; audit-log.
- **Idempotent:** if the head SHA is already recorded in `landings` (or the PR is
  already merged), return success with `already_landed: true` — a brain
  re-issuing after a mid-action restart is a no-op.
- Never accepts `main` (or the repo default branch) as target or source-base.

**Errors (enumerated for the brain to reason over):** `ci_pending`, `ci_red`,
`ci_missing`, `not_mergeable`, `not_owned`, `run_disabled`, `wrong_base`.

## 7. Cost-tier backend selection

Config classifies backends into `free` / `subscription` / `pay_per_use` lists
(order within a list = preference). Selection, for brains and workers alike:

```
for tier in [free, subscription, pay_per_use if allow_pay_per_use]:
  for backend in tier (config order):
    if available(backend): return backend
return NONE  → guardian backoff (wait for earliest known reset; notify)
```

`available(b)` = backend installed/authenticated AND not marked limited.
**Limit tracking:** a new small `tierstate` table (in `internal/autopilot`)
records `limited_until` per backend, fed by the poller's existing rate-limit
detection (`detectRateLimit` reset-time parse when available, else the
configured `retry_interval`/`spend_retry_interval` fallbacks). Entries expire;
expiry re-qualifies the backend, which naturally "climbs back up" the ladder on
the next selection.

**Rotation** (guardian stage 3 or planned): `handoff_agent`-style — terminate the
brain, spawn a fresh brain on the newly selected backend, cold-start from the
ledger (§4). Workers in flight are untouched; the new brain adopts them from
`list_agents` + tags.

**Gate:** `pay_per_use` backends are structurally excluded from the loop unless
`allow_pay_per_use: true`. Hitting the gate (nothing else available) emits a
distinct notification so the owner can flip it deliberately.

## 8. Ownership guard (daemon-side)

A request-scoped check in the daemon handlers for destructive operations
(`terminate_agent`, `delete_agent`, `remove_worktree`, `stop_agent`,
`snapshot_restore`): when the **calling session** is a brain (role `autopilot`),
the **target** must carry the `autopilot` tag and the caller's `run:<run_id>`
tag; otherwise 403 `not_owned`. Manual/foreign agents are mechanically
untouchable regardless of what the persona decides. (Caller identity: the brain's
MCP connection is already an authenticated agent session; reuse that identity.)

## 9. Brain persona (`internal/role/roles/autopilot.yaml`, embedded)

Defaults: `permission_mode` full-auto equivalent, `auto_approve: true`, tags
`[autopilot]`. Persona playbook (condensed contract — full text authored in P2):

1. Your brief is the plan file; re-read it when it changes. The ledger (§4) is
   your memory — **write intent before every side-effectful action**, update task
   state after, keep the journal current. Assume you can be restarted at any
   moment and must be able to resume from the ledger alone.
2. Decompose the goal into tasks; spawn workers (agents or pipelines) up to
   `max_parallel_workers`, each in its own worktree, PRs based on the
   integration branch — never main.
3. Monitor workers; steer stuck ones by reading transcripts and messaging;
   respawn hopeless ones (snapshot first).
4. Land only via `land`; on `ci_red`/`not_mergeable` fix via the worker (or a
   fix-up worker), then re-land. If integration is behind main, refresh it
   (merge main in, wait for CI) before landing more.
5. After landing: clean up the worker (terminate → remove_worktree; branch
   deletion is handled by `land`), mark the task landed, pull the next task.
6. Verify `done_when` before declaring the run complete; then report and stop
   spawning.
7. Never ask a human; reason and act. Escalation is not available to you — the
   guardian watches you instead.

## 10. Config (single source: `autopilot` block — see master plan for full schema)

Enabling autopilot OR-bundles `auto_approve.enabled`, `rate_limit.auto_resume`,
and `auto_restart` for autopilot-owned agents unless explicitly overridden.
Defaults are generous (frictionless-safeguards philosophy): guards fire only at
extremes.

## 11. Out of scope (this feature)

- Automated promotion of integration → main (owner fast-forwards; a separate
  opt-in may come later).
- Multi-repo plans; user-defined roles; cross-run prioritization.
- Making CI exist: repos without CI get `ci_missing` at the gate — configuring CI
  (and extending branch filters to the integration branch) is the adopter's job.

## 12. Verification (contract-level)

- **Ledger cold-start:** kill a brain mid-run (with `intent` set), restart, assert
  no duplicated side effects and correct resumption.
- **`land` idempotency:** land, restart brain, re-land same branch → `already_landed`.
- **Gate:** red CI / pending CI / no CI / conflict each return their typed error
  and merge nothing.
- **Ownership guard:** brain-session terminate of a non-autopilot agent → 403.
- **Tier ladder:** exhaust free tier → subscription selected; pay-per-use never
  selected while gated; `limited_until` expiry re-selects the cheaper tier.
- **Kill switch:** disable mid-run → no new spawns/landings, workers unharmed.
- **E2E rig:** isolated daemon on an alt port, free-tier backend brain, throwaway
  repo (`git commit-tree` seeded) with a 2–3 task plan → assert
  spawn → PR(base=integration) → gate → land → cleanup → next → complete.

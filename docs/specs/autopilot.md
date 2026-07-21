# Autopilot — Formal Design (P0)

**Date:** 2026-07-05
**Status:** Design (P0 of the staged delivery)
**Master plan:** [2026-07-05-autopilot-brain.md](2026-07-05-autopilot-brain.md)

This document formalizes the approved master plan into buildable contracts:
concepts and ownership, state machines, the run-ledger schema, the daemon
endpoint contract, the idempotent `land` contract, the guardian algorithm, the
cost-tier backend selection algorithm, the ownership guard, and the brain
persona. P1–P4 implement against this document; the build itself is staged as
S1–S8 in [autopilot-implementation.md](autopilot-implementation.md).

---

## 0. Design principle — frictionless after enable

Autopilot's contract with the owner: **all friction is front-loaded into the
moment of enabling** — the one moment a human is present. Enabling runs a
preflight (§5.1) that surfaces every problem that would otherwise stall the run
unattended (missing/invalid plan file, unauthenticated or untrusted backends,
missing integration branch, no CI, dead `gh` auth) as actionable errors *now*.
After enable, **no path may wait on a human**: every dead end routes to a
machine — auto-approve, the brain, the guardian, or a gate fallback — never to
a human inbox. The docs (P5) carry a prominent warning that unattended
operation is inherently risky; the mitigations are auditability and the kill
switch, **not** runtime confirmation prompts.

## 1. Concepts & ownership

| Term | Definition |
|---|---|
| **Plan** | An owner-authored YAML brief (`autopilot.plan.yaml`): a goal, constraints, and optional coarse tasks. Source of truth; owner-editable mid-flight. |
| **Run** | The daemon's execution of one plan: one brain + its workers + its ledger. Keyed by `run_id` (stable hash of repo + plan path). |
| **Brain** | One long-lived headless agent per run, role `autopilot`, spawned by the Controller on the cheapest available backend. Orchestrates via existing MCP tools. |
| **Worker** | Any agent or pipeline the brain spawns to do actual work. Tagged `autopilot` + `run:<run_id>`. |
| **Ledger** | Durable, daemon-store-backed run state kept current by the brain and written authoritatively by the daemon (`land` landings, spawn/terminate records). |
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
pending → assigned → in_progress → pr_open → gated (gate running, §6.1)
   gated → landed            (land succeeded; authoritative, daemon-written)
   gated → fixing → gated    (gate red / conflict; brain heals worker or respawns)
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
A **mid-run edit that fails validation never stops the run**: the run keeps the
last-good plan, the owner is notified with the decode error, and the brain
carries on — a steering typo must not wedge two weeks of work.

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
| `autopilot/<run>/landings` | **daemon** (`land`) | append-only: `{branch, sha, pr, landed_at}` |
| `autopilot/<run>/journal` | brain | rolling decision log (bounded, newest-first) for cold-start context |

**Recovery by construction (not persona discipline):** correctness across brain
restarts must not depend on the brain remembering to journal. Every
side-effectful operation is daemon-mediated and already recorded — spawns and
terminations in the store + audit log, landings written by the daemon inside
`land` (which is idempotent, §6). At brain (re)spawn the daemon composes a
**recovery digest** — plan file + `tasks` + `landings` + live `list_agents` +
the run's recent audit entries — and injects it as the brain's opening brief.
The brain keeps `tasks` and `journal` current as hygiene (they feed the status
panel and enrich its successor's digest), but a lapse degrades observability,
never correctness. Nothing lives only in a brain's context.

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
    "gate": "ci",                       // resolved gate mode (§6.1): ci | local
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

409 on enable when preflight fails (§5.1) — the body carries the full list of
actionable failures, not just the first, so the owner fixes everything in one
pass.

Surfaces (all thin wrappers over these two routes): CLI `warden autopilot
on|off|status|init`, MCP `set_autopilot`/`autopilot_status` (in
`internal/mcp/tools_extra.go`), TUI header badge + keybind, web toggle + status
panel. Toggling is live — loops gate on the flag per tick; no daemon restart.
A failed enable from TUI/web surfaces the preflight list verbatim with the hint
to run `warden autopilot init`.

### 5.1 Enable-time preflight & `warden autopilot init`

`warden autopilot init` scaffolds adoption in one command: writes a template
plan file (if absent), adds the `autopilot` config block pre-filled with the
backends detected on this machine (owner assigns cost tiers), creates the
integration branch off main (if absent), and prints a CI hint when workflows
don't cover integration PRs.

Enabling (`POST /autopilot`, any surface) runs a **preflight** and fails fast
with an actionable list instead of stalling unattended later:

- plan file exists and validates (strict decode);
- every ladder backend is installed, authenticated, and **trusted in this
  repo** — first-run trust prompts are a one-time operator step, so they
  surface NOW, not mid-rotation at 3am;
- `gh` is authenticated and can reach the remote;
- integration branch exists (auto-created off main when missing);
- no other active run on this repo; integration branch is not a protected name;
- gate mode resolved (§6.1): CI covering integration PRs detected, or the
  local-check fallback announced in the response.

Preflight failures are the **only** human interaction autopilot ever requests.

## 6. `land` contract (new MCP tool + `wd land`)

`land(agent_or_branch)` merges one worker branch into the integration branch.

**Preconditions (all daemon-checked; any failure → typed error, no side effects):**
1. Caller's run is `active` (kill switch honored).
2. Target branch is autopilot-owned (`run:<run_id>` tagged agent or recorded
   worker branch).
3. A PR exists whose base is the integration branch.
4. The gate (§6.1) is **green** for the PR head SHA.
5. PR is mergeable (no conflicts).

### 6.1 Gate modes — a no-CI repo must not dead-end

`merge.gate: auto | ci | local` (default **`auto`**):

- **`ci`** — latest CI run on the PR head SHA must be green (via `branchtrack`).
  "No CI configured" is the typed error `ci_missing` — only possible in this
  explicit mode.
- **`local`** — no CI required: the daemon runs the repo's project checks
  (the existing `check` rail, same as `wd check`) against the PR head worktree
  and gates on that result.
- **`auto`** — use `ci` when the repo has workflows covering integration PRs,
  else fall back to `local`. Resolved once at preflight and reported in
  `AutopilotStatus`, so the owner always knows which gate is live.

"Never merge red" holds in every mode. Under `auto`, a repo without CI degrades
to local checks instead of wedging the run — CI remains the stronger gate the
adopter can graduate to.

**Semantics:**
- Merge strategy from config (default squash); delete worker branch if
  `delete_branch`; record the landing in `autopilot/<run>/landings`; audit-log.
- **Idempotent:** if the head SHA is already recorded in `landings` (or the PR is
  already merged), return success with `already_landed: true` — a brain
  re-issuing after a mid-action restart is a no-op.
- Never accepts `main` (or the repo default branch) as target or source-base.

**Errors (enumerated for the brain to reason over):** `gate_pending`,
`gate_red` (carries the failing check/CI summary), `ci_missing` (mode `ci`
only), `not_mergeable`, `not_owned`, `run_disabled`, `wrong_base`.

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

## 8. Ownership guard & approval routing (daemon-side)

**Ownership guard.** A request-scoped check in the daemon handlers for
destructive operations (`terminate_agent`, `delete_agent`, `remove_worktree`,
`stop_agent`, `snapshot_restore`): when the **calling session** is a brain
(role `autopilot`), the **target** must carry the `autopilot` tag and the
caller's `run:<run_id>` tag; otherwise 403 `not_owned`. Manual/foreign agents
are mechanically untouchable regardless of what the persona decides. (Caller
identity: the brain's MCP connection is already an authenticated agent session;
reuse that identity.)

**Approval routing — no worker ever waits on a human.** While a run is active,
a worker prompt that the auto-approve policy cannot answer (unrecognized
prompt, tripped breaker, deny-matched but non-destructive) does **not** park in
the human approvals inbox. The daemon forwards it to the brain's mailbox
(`send_message`); the brain reads the pane/transcript and answers via
`approve`/`send_to_agent` — yes/no per policy spirit, multi-choice defaulting
to the agent's own recommended option unless the brain reasons otherwise. The
human inbox still **mirrors** the event (visibility + audit), but nothing
blocks on it. Human escalation remains the path only for non-autopilot agents.

## 9. Brain persona (`internal/role/roles/autopilot.yaml`, embedded)

Defaults: `permission_mode` full-auto equivalent, `auto_approve: true`, tags
`[autopilot]`. Persona playbook (condensed contract — full text authored in P2):

1. Your brief is the plan file plus the recovery digest injected at spawn;
   re-read the plan when it changes. Keep the task ledger and journal current —
   they feed the status panel and your successor's digest. Assume you can be
   restarted at any moment: verify before re-issuing anything (`land` is
   idempotent; `list_agents` shows what already exists).
2. Decompose the goal into tasks; spawn workers (agents or pipelines) up to
   `max_parallel_workers`, each in its own worktree, PRs based on the
   integration branch — never main. Before parallelizing over the same area,
   check `who_is_editing_file` and sequence instead of colliding.
3. Monitor workers; steer stuck ones by reading transcripts and messaging;
   answer forwarded approval prompts (§8) promptly; respawn hopeless ones
   (snapshot first). A worker rate-limited with a far-off reset may have its
   task respawned on the next available ladder backend instead of waiting.
4. Land only via `land`; on `gate_red`/`not_mergeable` fix via the worker (or a
   fix-up worker), then re-land. If integration is behind main, refresh it
   (merge main in, wait for the gate) before landing more.
5. After landing: clean up the worker (terminate → remove_worktree; branch
   deletion is handled by `land`), mark the task landed, pull the next task.
6. Verify `done_when` before declaring the run complete; then report and stop
   spawning.
7. Never ask a human; reason and act. Escalation is not available to you — the
   guardian watches you instead.

## 10. Config (single source: `autopilot` block — see master plan for full schema)

Enabling autopilot OR-bundles `auto_approve.enabled`, `rate_limit.auto_resume`,
and `auto_restart` for autopilot-owned agents unless explicitly overridden.
When the owner has configured no auto-approve rules, enabling installs a
**generous default policy for autopilot-owned agents** (allow recognized
non-destructive prompts; deny destructive patterns) so workers don't stall on
day one — anything the policy still can't answer routes to the brain (§8).
Defaults are generous (frictionless-safeguards philosophy): guards fire only at
extremes.

## 11. Out of scope (this feature)

- Automated promotion of integration → main (owner fast-forwards; a separate
  opt-in may come later).
- Multi-repo plans; user-defined roles; cross-run prioritization.
- Making CI exist: repos without CI degrade to the local-check gate (§6.1)
  automatically; configuring CI (and extending branch filters to the
  integration branch) remains the adopter's job to earn the stronger gate.

## 12. Verification (contract-level)

- **Recovery-digest cold-start:** kill a brain mid-run, restart, assert the
  digest reconstructs run state and no side effects are duplicated.
- **`land` idempotency:** land, restart brain, re-land same branch → `already_landed`.
- **Gate:** red / pending / conflict each return their typed error and merge
  nothing; `gate: auto` on a no-CI repo resolves to `local` and gates on
  project checks; `ci_missing` only under explicit `gate: ci`.
- **Preflight:** enable with a missing plan file / untrusted backend / dead `gh`
  auth → single 409 carrying ALL failures; `init` then a clean enable succeeds.
- **Approval routing:** unrecognized worker prompt while a run is active →
  brain mailbox message + mirrored inbox event; nothing blocks on a human.
- **Plan-edit resilience:** corrupt the plan file mid-run → run keeps last-good
  plan, owner notified, brain continues.
- **Ownership guard:** brain-session terminate of a non-autopilot agent → 403.
- **Tier ladder:** exhaust free tier → subscription selected; pay-per-use never
  selected while gated; `limited_until` expiry re-selects the cheaper tier.
- **Kill switch:** disable mid-run → no new spawns/landings, workers unharmed.
- **E2E rig:** isolated daemon on an alt port, free-tier backend brain, throwaway
  repo (`git commit-tree` seeded) with a 2–3 task plan → assert
  spawn → PR(base=integration) → gate → land → cleanup → next → complete.

---

# Project-level follow-up (2026-07-21)

Three follow-ups landed after the S1–S8 delivery above, all driven as the
`autopilot-projectlevel` pipeline. They refine *how the switch is scoped*, *how a
run signals it is finished*, and *how config edits take effect*. The §0
"frictionless after enable" principle and the §3 mid-run-edit philosophy now
extend to both the enabled-repo set and the config file.

## 13. Project-level (per-repo) enabling

The autopilot switch is **per-repository**, not one global flag. `warden
autopilot on` run inside a repo enables **only that repo**; other repos are
unaffected. The plan/brain/merge **template** stays global in the `autopilot`
config block — per-repo state is only the on/off bit and its run.

- **Persistence.** The enabled set is stored as marker files under
  `<data_dir>/autopilot/enabled/<hash>`, one file per repo root (the file's
  content is the absolute repo root; `<hash>` is a sha256 prefix mirroring the
  RunID hashing style). It is the **source of truth for which repos are on**, so
  previously-enabled repos come back up automatically across a daemon restart
  (boot re-`Enable`s each). A controller built without a `data_dir` (the unit
  tests) falls back to an in-memory store.
- **Targeting.** CLI `warden autopilot on|off` takes an optional `--repo <root>`
  (default: the current git repository, canonicalized to its git toplevel so it
  matches the repo autopilot resolves from a plan file). MCP `set_autopilot`
  takes an optional `repo` field (default: the daemon's working directory).
- **Status.** `AutopilotStatus` gains `enabled_repos: string[]` (the repo roots
  currently switched on). The scalar `enabled` now means **"any repo is on"**.
  `warden autopilot status` prints one `enabled: <repo>` line per repo plus the
  runs.
- **`off` is per-repo.** Disabling one repo leaves other enabled repos running;
  in-flight workers of the disabled repo keep running (kill switch semantics,
  §2.1).

## 14. Plan completion marker

When the brain has verified the plan's `done_when` criteria, it declares the run
complete via the MCP tool `autopilot_complete` (daemon `POST
/api/v1/autopilot/complete`). No arguments: the owning run is inferred from the
calling brain's own session identity, and **only a run's own brain may complete
it** (else `403`). The daemon then:

1. **Writes an in-place marker** into the plan file: `status: complete` and
   `completed_at: <RFC3339>`. The rewrite round-trips a `*yaml.Node` (decode →
   upsert the two keys → re-encode), so every **other key, its ordering, and the
   owner's inline comments survive** — it is not a re-marshal of the Plan struct.
   The daemon owns the clock (`completed_at` is supplied, not read from `time.Now`
   inside the writer).
2. **Tears the brain down** gracefully; in-flight workers keep running.
3. **Retains the run ledger** (state `complete`, §2.1).

**Preflight skips a complete plan** (§5.1): `Plan.Status == "complete"` means the
plan is not registered as an active run, so a finished plan is **never executed
again by mistake** on a future enable / daemon restart. The `status` /
`completed_at` keys are declared on the `Plan` struct precisely so a completed
plan still strict-decodes (the `KnownFields` decoder would otherwise reject
them); any non-`complete` status value is treated as active, so a hand-typed
status never blocks a plan from loading. `autopilot_complete` is **idempotent** —
a second call on an already-complete run is a no-op.

## 15. Config hot-reload

Editing `~/.warden/config.yaml` **live-applies with no daemon restart**. A
`config.Watcher` (fsnotify, 500 ms debounce) watches the config's parent dir and
matches the filename — editors and warden's own writes replace the file via
atomic rename, so an inode watch would go deaf. A burst of writes debounces into
**one** reload.

- **Bad edit keeps last-good.** Reload uses `config.LoadStrict`, which returns an
  error instead of degrading to defaults. On a parse/validate failure the daemon
  **keeps the last-good config** and notifies the owner ("config reload failed —
  keeping last-good settings"); it never falls back to defaults mid-run. (The
  historic lenient `config.Load` is unchanged for boot.)
- **Fan-out.** `Server.ApplyConfig` is the single reload entrypoint: it re-applies
  every subsystem that owns a live seam (`Set*`/`Reconfigure` idiom), then diffs
  vs the boot baseline and **logs the restart-only keys that changed** rather than
  silently ignoring them. Watcher failure is non-fatal (logged; feature disabled).
- **Autopilot under hot-reload.** `Controller.Reconfigure` swaps the **global**
  template (plans, integration branch, gate, strategy, backend ladder,
  `allow_pay_per_use`, guardian heal params) and re-runs the per-repo reconcile
  over the persisted enabled set — the EnableStore stays the source of truth, so
  **which repos are on is not reset**. A `plans[]` entry **removed** from config
  tears down its run (config-presence sweep, so a transient preflight failure
  never kills a still-configured run); a re-added plan starts again. Guardian
  **heal thresholds** hot-apply on the next tick; the guardian **tick cadence**
  (`interval`) is read once at loop start and needs a restart.

### 15.1 What hot-reloads vs what needs a restart

**Hot-reloads** (applied on the next tick/spawn, no restart):

- autopilot plan/brain/merge template + adding/removing `plans[]` + the per-repo
  reconcile;
- `auto_approve` policy;
- `tokens.*` (context/token guard: on/off, warn/critical bands, warn alert,
  auto-/force-compact, compact resume prompt);
- `rails.*`;
- `model_default`; `default_permission_mode`;
- pipeline / collab / memory system-prompt **hint gates**;
- `notify.*` + webhook;
- `api_docs` route gate; `scheduler_enabled` **route** gate (the 403 toggles
  live).

**Still needs a restart** (changed-but-needs-restart, logged on reload): `addr`,
`data_dir`, `claude_projects_dir`, `metrics` (recorder goroutine), the scheduler
reconcile **loop** (start/stop/cadence — only its route gate is live),
`trusted_proxies`, `http.timeout_*`, `plugins.*`, `local_llm.*`,
`collab.enabled`/interval loops, `branch_track.enabled`/interval loops,
`rate_limit.*` timers, `auto_restart.*`, `log.*`, `memory.curate`, snapshots, and
the guardian tick `interval`.

### 15.2 Verification (contract-level)

- **LoadStrict:** good file loads; a bad file returns an error (boot `Load` still
  degrades leniently to defaults + warn).
- **Watcher keep-last-good:** a mid-run edit that fails validation keeps the
  last-good config and fires `onError`; a burst of writes coalesces into one
  reload (debounce).
- **Subsystem seams:** `Controller.Reconfigure` swaps the template in place with a
  stable run id and sweeps a removed plan; `lifecycle.SetConfig`,
  `poller.SetContextGuard`, and `notify.Switch` each swap live; `ApplyConfig`
  fan-out is nil-poller-safe.

# Autopilot — Implementation Plan (orchestrator-executable)

**Date:** 2026-07-05
**Status:** Ready to execute
**Contracts:** [autopilot.md](autopilot.md) (P0 design) · [2026-07-05-autopilot-brain.md](2026-07-05-autopilot-brain.md) (master plan)

This document is written to be executed **autonomously by a warden orchestrator
agent**. Each stage below is a self-contained brief for one short-lived
implementer agent: goal, file surface, contract references, acceptance gates,
and explicit out-of-scope lines. The orchestrator spawns one implementer per
stage in an isolated worktree, gates each PR on CI, merges, tears down, and
moves to the next stage. Humans appear exactly twice: kickoff and the final
release tag.

---

## 0. Kickoff (the first human touchpoint)

Spawn the orchestrator against this file:

```
spawn_agent role=orchestrator backend=claude model=sonnet repo=<warden checkout> \
  prompt="Execute docs/specs/autopilot-implementation.md end to end. \
          Follow §1 protocol exactly; pick backends/models per §2; \
          implement stages S1–S8 in §4; \
          stop only at the §5 release-confirmation touchpoint."
```

Owner inputs at kickoff (defaults apply if unspecified):

- **Backends & models:** per the §2 matrix — implementation is **claude-only**
  (owner has a Max 5x subscription); free-tier backends take only §2.2 side
  tasks. If free-tier backends will be used, ensure they are trusted in this
  repo now (first-run trust is a one-time operator step).
- **Backend cost buckets** for the autopilot feature's own live-test config
  (S7 rig): default `free: [antigravity]`, `subscription: [claude]`,
  `pay_per_use: []`, `allow_pay_per_use: false`.
- **Version target:** minor bump (big feature, one tag — repo tagging style).

Kickoff also arms the supervision substrate (once, before the spawn):
confirm `rate_limit.auto_resume` + `auto_restart` are enabled on the daemon,
and create the hourly heartbeat-sentinel schedule per §2.4.

## 1. Orchestrator protocol (applies to every stage)

Division of labor: **each implementer owns its stage end-to-end** — worktree →
code → tests → commit → PR → CI → fixes → merge → notify. The orchestrator
plans, spawns, periodically checks, heals, cleans up, and sequences — it does
**not** merge PRs itself.

For each stage, in order (parallel pairs noted in §3):

1. **Clean up first:** before spawning anything, sweep children from previous
   stages — any completed agent or pipeline no longer needed is removed with
   `terminate_agent` → `remove_worktree` → `delete_agent` (this exact order;
   `delete_pipeline` for finished pipelines). Never leave finished agents or
   stale worktrees accumulating between stages.
2. **Sync:** `git fetch && git merge --ff-only origin/main` in the local main
   checkout — new worktrees branch off *local* main; a stale main breaks
   dependent stages.
3. **Spawn:** one implementer agent (`spawn_agent`, role `implementer`, **its
   own isolated worktree** — never the shared main tree — on branch
   `autopilot/s<N>-<slug>` off main). Brief = the stage section from this file
   **verbatim**, plus the §1 implementer duty cycle and global rules below,
   plus the pointer *"contracts live in docs/specs/autopilot.md — implement
   against the cited sections, do not redesign."*
4. **Periodic check-ins:** on a regular interval (every few minutes; also
   whenever a child message arrives), `read_inbox` + `list_agents` +
   `get_agent_output` for each in-flight child. Steer stuck children with
   `send_message`; a child looping or idle past ~15 minutes with an open task
   gets a nudge, then a restart with a digest of its branch state
   (`git log --oneline`, `git status`, its last output) — resume the stage on
   the existing branch, never restart it from scratch. Each check-in also
   watches **context pressure** (`get_pressure`) — see §2.3 for the
   compact/rotate ladder; never let a child die of context exhaustion mid-PR.
   Free-tier children (§2.2) get a tighter check-in interval and a review
   gate — they never self-merge.
5. **Completion:** the implementer merges its own PR (duty cycle below) and
   notifies the orchestrator. On receiving the completion message, verify
   independently — the merge commit is on `origin/main` and the PR is closed —
   before trusting it.
6. **Teardown:** remove the completed child (same order as step 1) and delete
   its remote branch if the merge didn't.
7. **Record:** append stage → PR# → merge SHA to a running log
   (ctx key `autopilot-impl/journal`), then return to step 1 for the next
   stage.

**Implementer duty cycle (include verbatim in every brief):**

1. Work **only inside your assigned isolated worktree and branch** — never
   touch the shared main checkout; peers may be running concurrently in the
   same repo.
2. Implement the stage per its brief; write the tests; run the stage's
   acceptance commands locally until green.
3. Commit via the `wd` rail (pre-commit `fmt-check lint`, pre-push
   `verify-fast` — no bypass exists; a red tree blocks all pushes and must be
   fixed, never worked around) and push your branch.
4. Open a PR to `main` (`gh pr create`) with the stage name, contract
   references, and the acceptance evidence in the body.
5. **Monitor your own PR's CI** (`get_branch_status` / `gh pr checks`) until
   it resolves. Red CI: read the failing logs, fix on the same branch, push
   again. Merge conflict with main: rebase (or merge main in), resolve,
   re-push, wait for CI to re-run. Repeat until green **and** mergeable.
6. Merge your PR yourself (squash, delete branch) — only ever green + mergeable,
   never red, never bypass.
7. **Notify the parent orchestrator** via `send_message`: stage id, PR number,
   merge SHA, acceptance results, and anything the next stage should know.
   Then stop working — the orchestrator handles your teardown.
8. If you are blocked by something outside your stage's file surface, message
   the orchestrator with specifics instead of expanding scope.

**Global rules for every implementer (include in every brief):**

- **Daemon API is spec-first:** new routes are edited into `openapi.yaml`, then
  `make generate` — never hand-write handlers/DTOs into `internal/daemon/oapi`.
  No autopilot route streams, so the `oapi/config.yaml` exclude list should not
  need changes; if you do add a WS/SSE route, it must go in that list.
- **CLI help is generated:** after changing any cobra `Use`/`Short`/`Long` or
  flags, run `make gendocs` and commit the result (`make gendocs-check` is
  CI-gated).
- **Tests are table-driven**, mirroring `breaker_test.go` /
  `autorestart_test.go`. Every stage lands with its unit tests; the stage's
  acceptance commands must pass locally before the PR opens.
- **Match repo idiom** (naming, error wrapping, config deprecated-alias
  pattern in `internal/config`). Read neighboring code first.
- **Scope discipline:** touch only the stage's file surface. If a needed
  change belongs to another stage, note it in the PR body and stop.
- Frictionless-safeguards philosophy: generous defaults; guards fire only at
  extremes.

## 2. Backends, models & context budget

### 2.1 Model matrix (claude backend for all implementation)

Principle: **the minimal model that can hold the stage's contract**. Core
daemon/contract work gets Opus; protocol-following and surface wiring get
Sonnet. All implementation runs on the claude backend (owner's Max 5x plan).

| Agent | Backend / model | Why |
|---|---|---|
| Orchestrator | claude / **sonnet** | Follows this written protocol; long-lived, so cheap+fast beats depth |
| S1 toggle core | claude / **opus** | Config + spec-first endpoint + Controller state machine |
| S2 init + TUI + web | claude / **sonnet** | Surface wiring over S1's contract; no new semantics |
| S3 brain lifecycle | claude / **opus** | Ledger/digest correctness is the feature's backbone |
| S4 `land` | claude / **opus** | Gate semantics + idempotency + typed errors |
| S5 guardian + failover | claude / **opus** | Heal ladder + tier selection edge cases |
| S6 guard + routing | claude / **opus** | Security-adjacent daemon guard + approval rerouting |
| S7 E2E rig | claude / **opus** | Live debugging across every subsystem |
| S8 docs + release | claude / **sonnet** | Prose + generated docs; may delegate drafts per §2.2 |

All claude agents share one subscription's rate limits. Parallel pairs
(S2∥S3, S4∥S6) are allowed but **optional** — if limits bite, run them
sequentially; warden's rate-limit auto-resume handles resets either way.

### 2.2 Free-tier side tasks (antigravity / codex) — monitored, never trusted blind

Free-tier backends may carry **small, isolated, non-core** tasks so the claude
budget goes to code: S8 doc drafts (guide/concepts prose), gap-doc write-ups,
fixture/test-data authoring. Rules, which override the implementer duty cycle
for these children:

- Same isolation: own worktree, own branch, PR to main.
- **No self-merge, ever.** The free-tier child stops at "PR open + CI green"
  and notifies. The orchestrator (or a claude reviewer it spawns) reviews the
  diff and performs the merge — free-tier output gets a review gate.
- Tighter monitoring: shorter check-in interval; steer early, respawn a child
  on claude/sonnet if a free-tier child is stuck twice on the same point.
- Never assign anything on the critical path (S1–S7 code) to a free tier.
- Trust prompts for these backends are cleared at kickoff (§0), not mid-run.

### 2.3 Context budget — compact and rotate before it hurts

Implementer stages are context-bounded by design (one stage per agent), but
long stages and the long-lived orchestrator still need active management:

- **Auto-compaction on:** run with warden's context auto-compaction enabled so
  claude children `/compact` under pressure automatically.
- **Check-in duty:** the orchestrator reads `get_pressure` each check-in; a
  child at high pressure mid-stage gets `set_force_compact` at its next safe
  point (between commits, never mid-edit).
- **Rotate when compaction isn't enough:** if a child is context-critical
  after compaction, `rotate_agent`/`handoff_agent` it — fresh context, same
  branch and worktree; the brief is re-derivable from this file + the branch
  state, so nothing is lost.
- **The orchestrator itself:** keep the §1 journal current after every merge —
  it makes the orchestrator cold-start-safe — and self-compact (or accept a
  guardian-style rotation) at stage boundaries, never mid-merge. Stage
  boundaries are the designed safe points: everything durable lives in git,
  the PR history, and the journal.

### 2.4 Session limits & the heartbeat sentinel

Two distinct failure modes, two distinct mechanisms — don't conflate them:

**Rate/session limits (Max plan windows) — configuration, not steering.**
warden's existing rate-limit machinery (`rate_limit.auto_resume`) detects the
limit banner, parses the reset time when one is shown (else the
`retry_interval`/`spend_retry_interval` fallbacks), and resumes the pane
automatically. Kickoff confirms `rate_limit.auto_resume` and `auto_restart`
are enabled on the daemon running this build. Because **all claude agents
share one subscription**, the orchestrator and its children hit a window
roughly together and resume together — a synchronized pause, not a failure:
branch state is in git, PRs are on GitHub, the journal is on the blackboard.
Nothing is lost by waiting a window out; no steering is needed or possible.

**Idle drift — the heartbeat sentinel (manual stand-in for the guardian this
plan is building in S5).** An LLM orchestrator can simply stop looping — end
its turn and wait — without being limited or errored, and it cannot watch
itself. Mitigation: an **hourly heartbeat via warden's own scheduler** (no
external cron). Note the scheduler fires agent-spawns, not messages to
existing agents — hence a sentinel, which is strictly stronger anyway: a bare
nudge message cannot fix a dead orchestrator. `create_schedule` (cron
`@hourly`, agent mode, claude/**haiku** — it only reads and nudges) spawning a
short-lived sentinel whose entire brief is:

1. Terminate + delete any previous sentinel instance (self-cleaning chain).
2. Read `autopilot-impl/journal`, `git log origin/main`, `list_agents`,
   `list_pipelines`: has anything progressed since the journal's last entry?
3. Fleet rate-limited → exit (auto-resume owns that case).
4. Progressing → exit silently (a healthy heartbeat costs almost nothing).
5. Orchestrator alive but idle/looping with work outstanding → `send_message`:
   "heartbeat: resume the §1 protocol — re-read the journal, check every
   child, continue from the last incomplete stage."
6. Orchestrator dead or errored → respawn it per §0 (the journal + §6 recovery
   rules make this safe by construction).

The schedule is created **at kickoff** — the sentinel, not the orchestrator,
is the outermost supervisor — and deleted after the §5 release confirmation.

## 3. Stage graph

```
S1 toggle core ──▶ S2 init + TUI + web        (S2 ∥ S3 — disjoint files)
        └────────▶ S3 brain lifecycle ──▶ S4 land        (S4 ∥ S6)
                          │              └──▶ S5 guardian + failover
                          └────────────▶ S6 ownership guard + approval routing
S1–S6 ──▶ S7 live E2E rig ──▶ S8 docs + release
```

Sequential merges into main; S2∥S3 and S4∥S6 may run concurrently because
their file surfaces are disjoint — everything else waits for its dependency's
merge (then re-sync main before spawning).

## 4. Stage briefs

### S1 — Toggle core: config + Controller + endpoint + CLI + MCP (inert)

**Goal:** the autopilot switch exists end-to-end and is visibly **inert** —
enable/disable/status work on every programmatic surface; no brain spawns yet.
**Contracts:** autopilot.md §5 (endpoint + AutopilotStatus shape), §5.1
(preflight — implement the checks that don't need later stages: plan file
exists + strict-decode per §3, `gh` auth, integration branch auto-create, no
second active run, protected-name check; stub backend-trust and gate-mode
checks with TODOs wired for S3/S4), §10 (config bundling semantics — the
bundle flags flip but nothing consumes them yet).
**Files:** `internal/config/config.go` (`autopilot` block per master-plan
schema: `enabled`, `plans[]`, `brain{backends{free,subscription,pay_per_use},
allow_pay_per_use, role, headless, max_parallel_workers}`, `merge{target_branch,
strategy, gate, delete_branch}`, `guardian{interval, heartbeat_timeout,
backoff_min, backoff_max, rotate_at_context, notify_each_escalation}`);
`openapi.yaml` (`POST /autopilot` `{enabled}` → 200 AutopilotStatus | 409
with full failure list, `GET /autopilot`) + `make generate`; new
`internal/autopilot/` (`controller.go` — run registry, run_id = stable hash of
repo+plan path, enable/disable state machine §2.1 without brain spawn;
`preflight.go`; `plan.go` — schema v1 strict decode); daemon wiring in
`internal/daemon/server.go` (handler + Controller construction); `internal/cli`
new `autopilot` cobra command (`on|off|status`; `init` is S2) + `make gendocs`;
`internal/mcp/tools_extra.go` (`set_autopilot`, `autopilot_status`).
**Acceptance:** `go test ./internal/autopilot/... ./internal/config/...`;
`make generate` and `make gendocs` diff-clean in the committed tree;
`make verify-fast`; manual smoke in the PR body: `warden autopilot on` against
a repo with a valid plan file → status `starting`→`active` (inert), `off` →
`disabled`, second plan on same repo → 409, missing plan file → 409 listing
the failure.
**Out of scope:** brain spawn, TUI/web, `init`, guardian, `land`.

### S2 — `init` scaffolder + TUI badge + web toggle/panel

**Goal:** the human-facing surfaces: one-command adoption and visible status.
**Contracts:** autopilot.md §5.1 (`init` behavior), §5 (status surfaces).
**Files:** `internal/autopilot/init.go` + CLI `warden autopilot init`
(template plan file if absent; append pre-filled `autopilot` config block with
detected backends for the owner to bucket; create integration branch off main
if absent; print CI-coverage hint) + `make gendocs`; `internal/tui` (header
badge showing autopilot state + run counts, toggle keybind, help entry in
`view.go`); `web/src` (toggle + status panel rendering AutopilotStatus: per-run
state, brain backend/tier, heartbeat, workers, tasks, backoff; failed enable
surfaces the 409 preflight list verbatim + "run `warden autopilot init`" hint).
**Acceptance:** `go test ./...` for touched packages; web build passes
(`make verify-fast` covers it); `init` on a bare throwaway repo then
`autopilot on` succeeds (record transcript in PR body); TUI badge reflects
on/off live without daemon restart.
**Out of scope:** any behavior change to the Controller.

### S3 — Brain role + run lifecycle (ledger, digest, plan watch)

**Goal:** enabling a run spawns a real headless brain; disabling tears it down.
**Contracts:** autopilot.md §1 (tags at spawn, one run per repo), §2.1/§2.3
(states), §3 (mtime re-read; invalid mid-run edit keeps last-good + notify),
§4 (ledger keys on the collab ctx store under `autopilot/<run_id>/…`; recovery
digest composed by the **daemon** at every brain (re)spawn: plan + tasks +
landings + live `list_agents` + recent audit), §9 (persona full text —
author the 7-rule playbook), §7 rotation hook signature only (selection logic
itself is S5 — S3 hardcodes "first configured free backend").
**Files:** `internal/role/roles/autopilot.yaml` (persona + defaults:
full-auto permission mode, `auto_approve: true`, tags `[autopilot]`);
`internal/autopilot/run.go` (brain spawn/teardown through the existing
lifecycle/spawn path, `run:<run_id>` tag injection), `ledger.go` (typed
read/write over `ctx_set`/`ctx_get`/`ctx_cas`), `digest.go`, plan-file watch;
Controller integration (enable → preflight → spawn brain with digest brief;
disable → kill switch semantics §2.1: stop spawns/landings, graceful brain
terminate, workers untouched); complete the preflight backend-trust check
stubbed in S1.
**Acceptance:** unit tests for run_id stability, ledger round-trip, digest
composition from fixture store state, last-good plan retention on a corrupt
edit; live smoke (isolated daemon, alt port — **never** the systemd one): on →
brain agent exists, headless, correctly tagged, opening brief contains the
digest; off → brain gone, a dummy tagged worker still alive.
**Out of scope:** guardian/heal ladder, tier selection, `land`, approval
routing.

### S4 — `land`: guarded merge-into-integration (MCP + `wd land`)

**Goal:** the brain's only merge path exists, gated and idempotent.
**Contracts:** autopilot.md §6 (preconditions 1–5, semantics, idempotency via
the daemon-written `landings` key, typed errors `gate_pending | gate_red |
ci_missing | not_mergeable | not_owned | run_disabled | wrong_base`), §6.1
(gate modes `auto|ci|local`; resolve `auto` at preflight — replace the S1
stub — and report the resolved mode in AutopilotStatus; `local` runs the
existing `check` rail against the PR head worktree; never merge red in any
mode).
**Files:** `openapi.yaml` route + `make generate`; `internal/autopilot/land.go`
(+ gate resolution reusing `internal/branchtrack` for CI and the `check` rail
for local); daemon handler writes `autopilot/<run>/landings` **inside** the
handler (authoritative) + audit-logs; MCP `land` tool
(`internal/mcp/tools_extra.go`); CLI `wd land <agent-or-branch>` +
`make gendocs`.
**Acceptance:** table-driven tests: every typed error path, `already_landed`
no-op on re-issue, `main` rejected as target and as base, squash + branch
deletion honored, `gate: auto` resolving to `local` on a no-CI fixture repo
and to `ci` when workflows cover integration PRs; `ci_missing` reachable only
under explicit `gate: ci`.
**Out of scope:** brain persona edits, guardian.

### S5 — Guardian + tierstate + cost-tier failover

**Goal:** the brain can no longer die quietly, and backend selection walks the
cost ladder.
**Contracts:** autopilot.md §2.3 (heal ladder: nudge → restart → rotate →
capped-backoff-forever, planned rotation at context critical), §7 (selection
loop, `tierstate` `limited_until` fed by the poller's `detectRateLimit`
reset-time parse with `retry_interval`/`spend_retry_interval` fallbacks;
expiry re-qualifies; pay-per-use structurally excluded unless
`allow_pay_per_use`, distinct notification when the gate is the only thing
left; rotation = terminate brain → spawn on new backend → cold-start from
digest; workers untouched and re-adopted).
**Files:** `internal/autopilot/guardian.go` (daemon loop in the
`worktree_prune.go`/`scheduler.go` pattern, launched from `server.go`;
heartbeat = brain MCP tool-call recency from the audit log vs
`guardian.heartbeat_timeout`), `tierstate.go`, `select.go` (replace S3's
hardcoded pick for brains AND the backend the brain hands workers); notify on
each escalation (`internal/notify`); status fields (`backoff`, `tier`,
`last_heartbeat`, `context_level`) go live.
**Acceptance:** table-driven tests for ladder progression + backoff cap +
never-park, selection across all tier/gate/limited permutations,
`limited_until` expiry climb-back; simulated-wedge test (fake clock, stale
heartbeat) walks nudge→restart→rotate; guardian tick honors the kill switch.
**Out of scope:** approval routing, ownership guard.

### S6 — Ownership guard + approval routing + breaker reroute + default policy

**Goal:** autopilot is mechanically fenced in, and no path waits on a human.
**Contracts:** autopilot.md §8 (403 `not_owned` when a role-`autopilot` caller
targets anything without `autopilot`+`run:<run_id>` tags on
`terminate_agent|delete_agent|remove_worktree|stop_agent|snapshot_restore`;
caller identity from the authenticated agent session; worker prompts the
policy can't answer forward to the brain's mailbox while the run is active,
human inbox mirrors but never blocks), §10 (enable installs the generous
default auto-approve policy for autopilot-owned agents when the owner has no
rules; OR-bundle `auto_approve`/`rate_limit.auto_resume`/`auto_restart`),
plus the master-plan substrate note: the approval **breaker** for
autopilot-owned agents escalates to the brain/guardian, not the human inbox.
**Files:** daemon handler guard (request-scoped, shared helper);
`internal/approval` routing seam + breaker reroute; Controller enable-path
policy install; mailbox forward via the existing `send_message` machinery.
**Acceptance:** tests: guard allows own-run targets, 403s manual/foreign
agents, no-ops for non-autopilot callers; unanswerable worker prompt while
active → brain mailbox message + mirrored inbox event, and the same prompt
with autopilot off → normal human inbox; breaker trip on an autopilot worker
→ no human escalation entry.
**Out of scope:** new prompt-parsing heuristics per backend.

### S7 — Live E2E rig (verification, no feature code)

**Goal:** prove the full loop unattended, $0-ish, per autopilot.md §12.
**Brief:** isolated daemon on an alt port (never the systemd instance);
throwaway repo seeded via `git commit-tree`; brain on the kickoff free-tier
backend; a 2–3 task plan. Assert, via `autopilot_status` + audit log +
`landings`: enable-preflight catches a seeded failure → `init` fixes it →
clean enable → brain spawns workers → PRs base = integration → gate → `land`
(re-issue → `already_landed`) → cleanup → next task → `done_when` → complete.
Then: kill the brain mid-run (digest cold-start, no duplicated side effects);
wedge it (guardian nudge/restart observed); flip the kill switch (spawns stop,
workers unharmed). Deliverable: a written report (repo-committed under
`docs/specs/`, or PR-body if trivial) listing each §12 assertion pass/fail.
**Gate to proceed:** every §12 assertion exercisable at this stage passes;
failures spawn fix-up rounds against the owning stage's code before S8.
**Out of scope:** feature changes (bugs found here are fixed via fix-up PRs
scoped to the owning stage's surface).

### S8 — Docs + release (second human touchpoint at the very end)

**Goal:** CLAUDE.md Definition of Done, walked explicitly.
**Delegation:** prose drafts (site guide, concepts page) may go to free-tier
children under the §2.2 rules (review-gated, never self-merged); the S8
claude implementer owns final wording, generated docs, and the release prep.
**Files:** `README.md` (feature surface); root `FEATURES.md` matrix +
`docs/FEATURES.md` prose (keep both catalogs + website mirror in sync);
`docs/USAGE.md`; `site/src/content/docs/` — a guide (`guides/autopilot.md`:
adoption walkthrough around `init`, cost-tier configuration, and the
**prominent warning that unattended operation is inherently risky** with the
kill switch + integration-branch boundary + audit log as the mitigations),
a concepts entry (brain/run/ledger/guardian), and the generated
`reference/cli.md` via `make gendocs` (never hand-edit); `skills/warden/`
(how agents should drive autopilot: `set_autopilot`, `autopilot_status`,
`land`, ledger keys); MCP/CLI parity check in both feature matrices.
**Acceptance:** `make gendocs-check`; site builds; DoD checklist reproduced in
the PR body with each item checked or explicitly ruled out.
**Release:** after merge, prepare the tag (**minor** bump, one tag for the
whole feature) and **STOP — report to the owner and wait for explicit
confirmation before `git push` of any `v*` tag** (the push cuts the public
GoReleaser release; ≤3 tags per push). This is the only mid-run human
interaction and it is by design.

## 5. Human touchpoints (exhaustive)

1. **Kickoff** (§0): spawn the orchestrator; optionally override backend
   buckets / version target.
2. **Release confirmation** (§4 S8): approve the `v*` tag push. Everything
   between is autonomous — stuck stages get respawned/steered by the
   orchestrator, never escalated, matching the feature's own §0 principle.

## 6. Recovery rules for the orchestrator itself

- Persist progress after every merge (§1 step 7); on restart, re-read the
  journal + `git log origin/main` to find the last landed stage, re-adopt any
  still-live children via `list_agents` (they carry the stage branch name),
  and resume the check-in loop — do not respawn a child that is still healthy.
- A stage PR with red CI is never merged and never abandoned: the implementer
  keeps fixing on the same branch until green (the `wd` rail already blocks
  broken trees); if the implementer itself dies, restart it on that branch.
- If a dependency stage's merge invalidates a parallel in-flight stage
  (conflict on main), message the in-flight child to rebase and re-gate rather
  than starting over.
- Before every spawn, the cleanup sweep (§1 step 1) must have run — completed
  agents, finished pipelines, and their worktrees are deleted first so the
  fleet only ever contains children that are still doing work.

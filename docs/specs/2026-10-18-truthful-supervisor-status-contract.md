# Truthful Supervisor Status Contract

- **Status:** Proposed (audit + contract; docs only, no production code)
- **Plan:** `autopilot-supervisor-status-contract` (run `ap-71ce8558e6a0`)
- **Builds on:** [`2026-09-26-drop-guardian-session.md`](2026-09-26-drop-guardian-session.md), [`2026-10-05-autopilot-run-to-final-pr.md`](2026-10-05-autopilot-run-to-final-pr.md) §J
- **Feeds:** `truthful-supervisor-status`, `legacy-migration-and-consumer-compatibility`, `supervision-recovery-validation`

## 1. Problem

The drop-guardian-session work removed the hollow `<scope>-guardian` session
record. Supervision is now done by a **daemon-internal goroutine**
(`Controller.RunGuardian` → `guardianTick` / `superviseRun` / `overwatchTick`).
Nothing in the status surface says so. Today:

- `GET /autopilot/status` and `wd autopilot status --json` still emit
  `guardian_id` and `guardian_slot_id` = `<scope>-guardian`. **No session with
  that id exists.** An operator or manager that tries `send_message` /
  `get_agent` on it gets "not found".
- `next_step.owner` is `"guardian"` for most scheduled actions, which reads as
  "message the guardian". The OpenAPI/Go comments also list `overwatch`, a
  value no code ever emits, and omit `daemon`, which code does emit.
- Docs (`docs/USAGE.md`, `docs/FEATURES.md` §34, `site/.../concepts/autopilot.mdx`,
  `guides/autopilot.md`, `skills/warden/SKILL.md`) still draw a
  `<scope>-guardian  # inspectability session` in the run tree.
- The web panel synthesises a fake idle "guardian" session from `guardian_id`
  when none is found (`AutopilotPanel.tsx`).
- Supervisor health/recovery state (heal stage, backoff, next retry) is shown
  but is **in-memory only** and carries no explicit "supervisor is the daemon /
  is it running" field.

Goal: every surface reports *what is supervising*, *whether it can be
addressed*, *what state it is in*, and *who owns the next step* — truthfully.

## 2. Audit — inventory

### 2.1 Guardian ids and slots

| Symbol | Location | Reality | Notes |
|---|---|---|---|
| `guardianSlotSuffix = "-guardian"`, `GuardianSlotID(scope)`, `guardianSlotIDOrEmpty` | `internal/autopilot/identity.go` | Pure string derivation. **No backing session.** | Doc comment still says "stable guardian session id" — false. |
| `reservedPlanSuffixes` (`-autopilot`, `-guardian`) | `identity.go`, `validatePlanNameReservedSuffixes` | Real: rejects plan names ending in a reserved suffix. | Keep — protects the id namespace and legacy sessions. |
| Claim registry `runBySlotID[GuardianSlotID(scope)]` | `internal/autopilot/claims.go` | Real: reserves the name so no agent/run can take it. | Keep; this is the *only* functional use of the id. |
| `RunStatus.GuardianID`, `.GuardianSlotID` | `internal/autopilot/status.go:69,72`; set in `controller.go:1033,1038`, `lifecycle.go:592,594` | Both always equal `<scope>-guardian` (or empty with no scope). Duplicate of each other. | Misleading. |
| `client.AutopilotRunStatus.GuardianID/GuardianSlotID` | `internal/client/client.go:1622,1625` | Mirror. | |
| OpenAPI `guardian_id` (no description), `guardian_slot_id` ("stable guardian session id") | `internal/daemon/apidocs/openapi.yaml` ~6913, ~7041, ~7044 | Mirror; description is false. | |
| `RunRecord.GuardianID` | `internal/autopilot/store.go` | **Already removed** (D5). Not persisted. | Persisted run state carries only `slot_scope`. |
| `store.AutopilotSlotGuardian = "guardian"`; `Session.AutopilotSlot` enum doc `autopilot\|guardian\|worker` | `internal/store/types.go:129,418`; `lifecycle.go:1058`; `agentstore/types.go:87`; openapi ~4383 | Legacy value on pre-drop sessions only. No code writes it now. | Read-compat only. |
| `tree.NodeTypeGuardian` ("legacy; new builds omit guardians") | `internal/tree/types.go:27` | Emitted only when a legacy session claims the slot. | |
| `runSessionSlot` / legacy claim by `r.GuardianID` | `internal/tree/membership.go:354`, `service.go:175` | Looks up a session by synthetic id; finds one only for pre-drop data. | |
| Legacy retirement | `autopilot_reconcile.go` | `retireLegacyGuardian` / `SpawnGuardian` / `GuardianAgentRuntime` **gone**. | `guardianSessionLive()` remains but is a generic "session status is live" helper (misnamed). |

### 2.2 Next-step owners (`internal/autopilot/nextstep.go`)

Actually emitted: `operator` (needs attention), `daemon` (final-PR merge poll),
`guardian` (everything else: resting-agent resume, heal ladder stages, spawn /
verify manager, steady-state "supervise: landing, overwatch and stall triage").
Documented but never emitted: `overwatch`. Consumers: `internal/cli/autopilot.go:156-162`
prints `(owner)` verbatim; `nextStepViolation` special-cases `operator`.
Web and TUI do **not** render `next_step`. Persistence: none — derived each
call from in-memory `healStage`, `healNextAt`, `backoffNextRetry`, resting map.

Semantic problem: `guardian` = "the daemon's supervision loop acts", not "an
agent you can talk to". `daemon` and `guardian` are the same actor
(`RunGuardian` also runs landing, final-PR polling, overwatch).

### 2.3 Manager prompts / digests / roles

- `internal/role/roles/autopilot.yaml`: "Escalation is not available to you; the
  guardian watches you instead." — accurate as a statement of a daemon loop,
  but ambiguous; never instructs messaging the guardian. Fine, tighten wording.
- `digest.go`, `spawn_prompt.go`, `restart_context.go`: contain **no** guardian
  id or instruction to address one. `restart_context.go` carries restart
  reasons (`watchdog` etc.). No change required beyond tests that pin it.
- Nudges are sent *from* the trusted `autopilotForwardSender`
  (`guardianNudgeSender`) *to* the manager; the manager has no reply path to a
  guardian. Poller `HandOff` ("gives the prompt to the guardian as a blocker")
  is `NotifyEscalation` to the **operator notifier**, not a session message.
- `AGENTS.md` rules injection (antigravity): no guardian content.

### 2.4 API / CLI surfaces

| Surface | Fields | Truthfulness |
|---|---|---|
| `/autopilot/status` `RunStatus` | `guardian_id`, `guardian_slot_id`, `manager_slot_id`, `slot_scope`, `next_step{action,at,owner}`, `backoff`, `watchdog`, `resting_until`, `last_diagnosis`, `needs_attention`, `brain{agent_id,last_heartbeat,context_level}` | `guardian_*` false; no supervisor-kind/availability/health field; heal stage not exposed except via `backoff`/`next_step.action` text. |
| `wd autopilot status` | prints `next:` line with `(owner)`; does not print guardian id | Owner string misleads (`guardian`). Help text line 71 says "the guardian's" detail lines. |
| Plan APIs (`PlanExecutorBackoff`, comment "guardian backoff detail") | `plans_crud.go:167` | Wording only. |
| Audit action `autopilot.guardian_diagnosis` | `audit.go:42` | Event name is a wire/audit contract; keep. |
| Config `autopilot.guardian.*`, `autopilot.guardian_diagnosis` | `config.go` | Public config; keep names. |

### 2.5 TUI / Web / Cockpit

- **TUI**: `tree_adapter.go` filters `system:true` sessions; `list.go`
  `apSlot` may be `guardian` for legacy sessions; tests assert guardian is
  *not rendered*. `control_pane.go` comments mention guardian. TUI does not
  consume `guardian_id` for display. No `next_step` rendering.
- **Web**: `web/src/lib/autopilot-tree.ts` `sessionSlot`/`buildRunTree` classify a
  session as `guardian` if `s.id === run.guardian_id`; `AutopilotPanel.tsx:70-72`
  **fabricates** a placeholder guardian row from `run.guardian_id` when no
  session exists — the one place that actively shows a nonexistent entity.
  `web/src/lib/api.ts:391` / `types.ts` carry `guardian_id`.
- **Cockpit** (TUI control pane): inherits the TUI tree; legacy-only.

### 2.6 Persisted run state

| Store | Persisted | Not persisted |
|---|---|---|
| `autopilot_runs` `RunRecord` (`autopilot/store.go`; mirrored `autopilotstore/migrate.go`) | run_id, name, repo, plan_file, plan_id, project_id, state, integration_branch, gate, strategy, delete_branch, `brain_id`, `slot_scope`, last_progress_at, progress_fp, timestamps | `guardian_id` (removed), heal stage, backoff, resting map |
| `autopilotstore.Autopilot` (schema 2, `types.go`) | `manager_agent_id`, `brain_agent_id`, `parent_agent_id`, `Diagnostics{State, LastError, ...}` | supervisor kind/health |
| Ledger `autopilot.<run>.surface` (`surface.go`) | last_diagnosis, resolver, final_pr, awaiting_merge, verified, final_notified | |
| Agent `Session` | `autopilot_run_id`, `autopilot_slot`, `autopilot_task_id` | |

Consequence: after a daemon restart `healStage`/`backoff` reset and are rebuilt
from the next tick; `next_step` is recomputed. That is acceptable (the
supervisor *is* the daemon, and restarts re-evaluate) but must be **stated**,
and status must not imply durable heal history.

### 2.7 Test/doc references to update later

`internal/tui/list_test.go:347-409`, `internal/daemon/autopilot_routes_test.go:263`,
`internal/autopilot/status` tests, `web/src/lib/autopilot-tree.test.ts`,
`internal/cli/autopilot_status_test.go`; docs listed in §1.

## 3. Contract

### 3.1 Supervisor kinds

A run's supervision is described by a `supervisor` object. `kind` is an
explicit enum:

| `kind` | Meaning | Addressable? |
|---|---|---|
| `daemon` | In-process `RunGuardian` goroutine (heal ladder, limit recovery, landing, overwatch, watchdog). **The only kind implemented today.** | **No** — no session id; reached through the daemon API/CLI only. |
| `session` | *Reserved/future:* a real agent session performs supervision. | Yes — `session_id` is a live agent id. |
| `none` | No supervision (run `registered`, `paused`, `stopped`, `complete`, or per-repo disabled). | No |

Invariant **S1**: `supervisor.addressable == true` ⇔ `kind == "session"` ⇔
`supervisor.session_id` is non-empty **and** resolves to a live agent. No other
field may present a session-shaped id for a non-addressable supervisor.

### 3.2 `supervisor` status object

```
supervisor: {
  kind:        "daemon" | "session" | "none"
  addressable: bool                  // S1
  session_id:  string?               // only for kind=session
  state:       "supervising" | "idle" | "suspended" | "disabled"
               // supervising: run is being ticked; idle: nothing due;
               // suspended: paused/stopped/complete; disabled: repo kill switch
  health:      "healthy" | "healing" | "backoff" | "parked" | "unknown"
               // healthy: manager heartbeat fresh; healing: heal ladder stage 1-3;
               // backoff: stage 4; parked: needs_attention set; unknown: no tick yet since daemon start
  heal_stage:  "none"|"nudged"|"restarted"|"rotated"|"backoff"
  last_tick_at: RFC3339?             // last guardianTick that visited this run
  persisted:   false                 // heal state is rebuilt after daemon restart
}
```

`health` derives from existing fields (`reportedState`, `healStage`,
`needsAttention`, `backoff`); no new persisted state is introduced.
`last_tick_at` is in-memory; `unknown` until the first tick after boot.

Invariant **S2**: `health=parked` ⇔ `needs_attention != ""` ⇔ `next_step.owner == "operator"`.
Invariant **S3**: `kind=daemon` with `state=supervising` ⇒ a non-operator
`next_step` with `at` set (existing `nextStepViolation` rule, kept).

### 3.3 Next-step ownership

`next_step.owner` becomes a closed enum with actor semantics, plus an explicit
addressability hint:

| owner | Who acts | Addressable via | Used for |
|---|---|---|---|
| `daemon` | The daemon supervisor loop | Not a session — use `wd autopilot status`, `wd plan pause/stop/restart` | **All** heal ladder, resting-agent resume, spawn/verify manager, landing, final-PR merge poll, steady-state supervision |
| `manager` | The run's manager agent | `manager_slot_id` / `brain.agent_id` | Reserved: when the next action is the manager's (e.g. awaiting completion verification) |
| `worker` | A task worker | worker session id | Reserved: fix-loop dispatch to an owner worker |
| `operator` | A human | The notifier / CLI | `needs_attention`, final PR merge by a human |

- `guardian` is **deprecated** as an owner value (see §4). It is accepted by
  consumers for one release but the daemon emits `daemon` instead.
- `overwatch` is removed from the documented enum (never emitted; overwatch is
  a sub-pass of the daemon loop, surfaced as `daemon`).
- New optional field `next_step.addressable_session_id` is present only when the
  owner is `manager` or `worker`; it is never present for `daemon`/`operator`.

Invariant **S4**: no status field, CLI line, digest, prompt, or UI may direct a
reader to message/attach/get an agent id that is not `addressable` (S1).

### 3.4 Recovery state semantics

- Recovery (nudge → restart → rotate → backoff → retry; limit-resume; watchdog)
  is **daemon-owned and automatic**. Status must express it as `supervisor.health`
  + `heal_stage` + `backoff` + `next_step`, never as "ask the guardian".
- Operator actions are exclusively `wd plan pause|resume|stop|restart`,
  `wd autopilot status`, and clearing `needs_attention`. Terminating the manager
  is *not* a stop (existing guidance in `wd agents stop` help stays).
- After daemon restart: `health=unknown, last_tick_at=""` until the first tick;
  `next_step` is recomputed; no history is claimed. Durable parked state
  (`needs_attention`, `final_pr`, `awaiting_merge`) remains authoritative.

## 4. Legacy field transition

`guardian_id` and `guardian_slot_id` are public wire fields; removing them
abruptly would break the web build, external clients and the hub. Plan:

| Phase | `guardian_id` | `guardian_slot_id` | `supervisor` | `next_step.owner` |
|---|---|---|---|---|
| **Now** | `<scope>-guardian` (false) | same (false) | absent | `guardian` / `daemon` / `operator` |
| **P1 (truthful-supervisor-status)** | **Omitted** (empty). Marked `deprecated: true` in OpenAPI with description "Removed: no guardian session exists; see `supervisor`". | Kept **only as a reserved-name marker**, description corrected to "reserved slot name; not an addressable session". Still emitted so name-collision tooling keeps working. | Added (§3.2). | `daemon` replaces `guardian`; `guardian` accepted on read. |
| **P2 (legacy-migration-and-consumer-compatibility)** | Not emitted. Go/TS fields kept as `omitempty` for decode compat. | Same. | Consumers switch to `supervisor`. | Consumers treat `guardian` as alias of `daemon`. |
| **Removal (≥ 1 minor after P2)** | Field deleted from Go/OpenAPI/TS. | Deleted or kept as reservation-only. | — | `guardian` no longer accepted. |

Compatibility rules:
1. Old clients reading a new server see missing `guardian_id` — all in-tree
   consumers must treat it as optional (they already do: `omitempty`, `?:`).
2. New clients reading an old server (no `supervisor`) must synthesise
   `{kind:"daemon", addressable:false}` and treat `owner=guardian` as `daemon`.
3. Legacy **sessions** with `autopilot_slot=guardian`/`system:true` still found
   in old stores keep rendering under their run (tree adapters unchanged) but
   are never *created*; `slot`-enum docs say "legacy".
4. Reserved suffix `-guardian` and the claim-registry reservation stay
   **indefinitely** (cheap, prevents resurrecting collisions with legacy data).
5. Audit action `autopilot.guardian_diagnosis` and `autopilot.guardian.*`
   config keys are **not renamed** (stable contracts). Docs may describe
   "the daemon supervisor (config key `guardian`)".

Surface-specific changes:

- **API/OpenAPI/client**: add `supervisor` schema + Go/TS types; fix
  descriptions; enum for `next_step.owner` = `daemon|manager|worker|operator`
  (+`guardian` deprecated). Regenerate via `make generate`.
- **CLI** `wd autopilot status`: add `supervisor: daemon (healthy)` line; keep
  `next:` line; map legacy `guardian` → `daemon` when talking to old daemons.
  Run `make gendocs` after help-text edits.
- **TUI/Cockpit**: render supervisor health in the run row/detail and
  `next_step`; never create a guardian node; keep legacy node rendering.
- **Web**: delete the synthetic guardian placeholder in `AutopilotPanel.tsx`;
  render a "Supervisor: daemon · healthy" badge; `buildRunTree` stops matching
  by `run.guardian_id`, matches only `autopilot_slot==='guardian'` legacy rows.
- **Prompts**: reword `autopilot.yaml` to "the daemon's supervisor watches you;
  it is not an agent and cannot be messaged — you cannot escalate". Any
  digest/nudge text keeps referring to "warden"/"the daemon", never an id.
- **Docs**: remove `<scope>-guardian  # inspectability session` from run-tree
  diagrams (USAGE §, FEATURES §34.2, concepts/autopilot.mdx, guides/autopilot.md,
  skills/warden/SKILL.md); describe the daemon supervisor and the `supervisor`
  field; keep the heal-ladder tables.

## 5. Requirements per downstream task

### 5.1 `truthful-supervisor-status`

1. Implement `supervisor` (§3.2) in `internal/autopilot/status.go`, derived in
   `runStatusLocked` and `Status()`; add `last_tick_at` stamped in
   `guardianTick`; unit-test every `kind/state/health` combination incl.
   boot-`unknown`.
2. Emit `next_step.owner` per §3.3 (`daemon` for all former `guardian`
   cases); fix the doc-comment (`overwatch` removal); keep
   `nextStepViolation` invariant (S3) and add S2 check.
3. Stop emitting `guardian_id`; correct `guardian_slot_id` semantics (§4 P1).
4. Update OpenAPI + `make generate`; client types; `wd autopilot status` line;
   `make gendocs`.
5. Tests: status JSON golden for each run state; owner enum; assertion that no
   emitted id fails `get_agent` (S1/S4) — e.g. test that
   `supervisor.addressable` is false and no `*_id` field equals
   `GuardianSlotID` except the reservation marker.

### 5.2 `legacy-migration-and-consumer-compatibility`

1. Web: remove placeholder (AutopilotPanel), update `autopilot-tree.ts`, `api.ts`,
   `types.ts` (+ tests) to consume `supervisor`; fallback synthesis for old
   servers (§4 rule 2).
2. TUI/Cockpit: supervisor/next-step rendering; legacy-node handling unchanged
   and covered by tests (`list_test.go`, `tree_adapter`).
3. Tree service: stop claiming sessions via synthetic `GuardianID`
   (`membership.go`, `service.go`); claim only by `autopilot_run_id`/slot tags.
4. Alias handling for `owner=guardian` in CLI/TUI/web and `hub`/remote
   consumers; contract test across an old-server fixture.
5. Docs/site/skill/README/FEATURES updates (§4); prompt rewording
   (`autopilot.yaml`) with prompt-compat tests (`prompts_compat_test.go`).
6. Document and schedule the removal release; no destructive store migration is
   required (nothing persisted — §2.6).

### 5.3 `supervision-recovery-validation`

1. Characterization tests: for each heal stage (nudged → restarted → rotated →
   backoff), limit-resting, awaiting-merge, parked, paused/stopped/complete —
   assert `supervisor.health`, `heal_stage`, `next_step{owner,at}` and CLI line.
2. Daemon-restart test: after rebuild, `health=unknown` then converges after
   one tick; parked/awaiting-merge state survives.
3. Negative test (S4): no status/CLI/digest/nudge text contains a
   non-addressable id or "message the guardian"; manager prompt/digest snapshot
   tests.
4. Live/E2E recipe (isolated daemon on alt port per the S7 rig): kill the
   manager, observe `healing → healthy` through `wd autopilot status --json`;
   confirm no `get_agent` call is ever suggested for the supervisor.
5. Forward-compat test: a stubbed `kind=session` supervisor with a live id
   satisfies S1 and is rendered as addressable (guards the reserved variant).

## 6. Non-goals

- Reintroducing a guardian session or changing the heal ladder, tick cadence,
  or any `autopilot.guardian.*` config key.
- Renaming audit actions or config keys.
- Persisting heal state (explicitly documented as rebuilt after restart).
- Implementing `kind=session`; only reserving it in the schema.

## 7. Open questions

1. Should `guardian_slot_id` be dropped from the wire in P1 (it only duplicates
   the reservation)? Proposed: keep until the removal release — zero cost.
2. Do remote hub/Android consumers read `next_step.owner` literally? Check
   `warden-hub` before P2; keep the `guardian` alias window ≥ 1 minor.

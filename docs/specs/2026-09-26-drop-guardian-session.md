# Drop Autopilot Guardian Session — Spec Freeze

- **Status:** Locked (Phase 0 — spec freeze; docs only, no production code)
- **Repo:** warden. Baseline: `autopilot/drop-guardian-session` integration branch.
- **Scope:** Remove the hollow guardian session record created by autopilot while keeping the daemon guardian goroutine and API compatibility entirely intact.

---

## 1. Problem Statement

Today, when an autopilot run starts, `SpawnGuardian` in `internal/daemon/autopilot_runtime.go` creates a bare `store.Session` (no tmux pane, no LLM backend / aicli process) whose sole purpose is to appear in the TUI agent list with the suffix `-guardian`.

Every actionable piece of guardian state — run health, backoff stage, next retry time, last heartbeat, context-window pressure — already lives directly on the run record exposed by `wd autopilot status` (and internal `RunRecord` / `RunStatus`).

The guardian session record introduces unnecessary overhead:
1. **Roster Noise:** It appears as an artificial idle session in `list_agents` and the cockpit agent roster.
2. **Lifecycle Overhead:** It incurs extra store writes on every run startup, updates, reconciliations, and teardown sweeps.
3. **Dead Complexity:** Extra interfaces (`GuardianAgentRuntime`), migration helpers (`retireLegacyGuardian`), tag parsers (`guardianRunIDFromTags`), and tag checks (`containsTag(sess.Tags, guardianSystemTag)`) exist solely to manage this hollow session record.

---

## 2. Locked Decisions

### D1. Remove `GuardianAgentRuntime` Interface and Brain Call-sites
- **Symbol / File:** `internal/autopilot/run.go`
- Remove the `GuardianAgentRuntime` interface definition:
  ```go
  type GuardianAgentRuntime interface {
      SpawnGuardian(ctx context.Context, runID, slotScope, repo string) (agentID string, err error)
      TerminateGuardian(ctx context.Context, agentID string) error
      ReconcileGuardians(ctx context.Context, valid map[string]string) (missingRunIDs []string, err error)
  }
  ```
- In `spawnBrain` (`internal/autopilot/run.go`), remove the guardian check and spawn block:
  ```go
  wantGuardian := GuardianSlotID(r.slotScope)
  if r.guardianID != "" && r.guardianID != wantGuardian {
      r.guardianID = ""
  }
  if gr, ok := c.runtime.(GuardianAgentRuntime); ok && r.guardianID == "" {
      id, gerr := gr.SpawnGuardian(ctx, r.runID, r.slotScope, r.repo)
      if gerr != nil {
          _ = c.runtime.TerminateBrain(ctx, handle.AgentID)
          r.brain = nil
          r.state = StateDegraded
          return fmt.Errorf("spawn guardian: %w", gerr)
      }
      r.guardianID = id
  }
  ```
- In `teardownBrain` (`internal/autopilot/run.go`), remove the guardian termination block:
  ```go
  if gr, ok := c.runtime.(GuardianAgentRuntime); ok && r.guardianID != "" {
      errs = append(errs, gr.TerminateGuardian(ctx, r.guardianID))
      r.guardianID = ""
  }
  ```

### D2. Remove Guardian Session Methods, Constants, and Assertions from Daemon Runtime
- **Symbol / File:** `internal/daemon/autopilot_runtime.go`
- Remove the compile-time interface assertion:
  ```go
  _ autopilot.GuardianAgentRuntime = autopilotRuntime{}
  ```
- Remove package-level constants:
  ```go
  guardianSystemTag = "system:true"
  guardianRunPrefix = "autopilot-run:"
  ```
- Remove `SpawnGuardian(ctx context.Context, runID, slotScope, repo string) (string, error)` method.
- Remove `TerminateGuardian(ctx context.Context, agentID string) error` method.
- Remove `ReconcileGuardians(ctx context.Context, valid map[string]string) ([]string, error)` method.
- In `internal/daemon/autopilot_reconcile.go`, remove the `guardianRunIDFromTags(tags []string) string` helper used only by legacy guardian session logic.

### D3. Remove Legacy Guardian Session Retirement from Reconciler
- **Symbol / File:** `internal/daemon/autopilot_reconcile.go`
- Remove `retireLegacyGuardian` function:
  ```go
  func (rt autopilotRuntime) retireLegacyGuardian(ctx context.Context, sessions []*store.Session, spec autopilot.BootReconcileRun) error
  ```
- In `ReconcileSessions`, remove the invocation:
  ```go
  errs = append(errs, rt.retireLegacyGuardian(ctx, sessions, spec))
  ```
- In `reconcileWorkerBackRefs`, remove the guardian skip guard:
  ```go
  if sess == nil || containsTag(sess.Tags, guardianSystemTag) {
      continue
  }
  ```
  The condition was skipping guardian sessions; with no guardian sessions created or managed, that check is dead code and simplifies to `if sess == nil { continue }`.

### D4. Preserve `guardian_id` and `guardian_slot_id` in API Shapes
- **Symbol / File:** `internal/autopilot/status.go`, `internal/autopilot/identity.go`, `internal/daemon/apidocs/openapi.yaml`
- The `guardian_id` and `guardian_slot_id` fields **STAY** in `RunStatus` (`internal/autopilot/status.go`):
  ```go
  GuardianID     string `json:"guardian_id,omitempty"`
  GuardianSlotID string `json:"guardian_slot_id,omitempty"`
  ```
- The OpenAPI schema definitions in `internal/daemon/apidocs/openapi.yaml` for `guardian_id` and `guardian_slot_id` **STAY**.
- `GuardianSlotID(scope string) string` in `internal/autopilot/identity.go` **STAYS**.
- In `Controller.Status()` and `runStatusLocked`, both fields are computed deterministically from the slot scope (`guardianSlotIDOrEmpty(r.slotScope)`), ensuring zero store dependency while preserving complete backward compatibility for external API consumers (web UI, CLI, TUI).
- Slot claims in `internal/autopilot/claims.go` continue to register `GuardianSlotID(scope)` to avoid namespace collisions.

### D5. Remove `guardianID` Tracking from Controller Run and Store Records
- **Symbol / File:** `internal/autopilot/controller.go`, `internal/autopilot/lifecycle.go`, `internal/autopilot/store.go`, `internal/autopilot/reconcile.go`
- Remove `guardianID string` field from `type run struct` in `internal/autopilot/controller.go`.
- Remove the `if gr, ok := rt.(GuardianAgentRuntime); ok { ... }` boot reconciliation block in `Controller.SetRuntime` (`internal/autopilot/controller.go`).
- In `internal/autopilot/lifecycle.go`:
  - Remove reading `rec.GuardianID` into `r.guardianID` in run reconstruction loop.
  - Remove writing `rec.GuardianID = GuardianSlotID(r.slotScope)` in `recordLocked`.
  - In `runStatusLocked`, populate `GuardianID: guardianSlotIDOrEmpty(r.slotScope)`.
- In `internal/autopilot/store.go`:
  - Remove or zero `GuardianID string` (`json:"guardian_id,omitempty"`) on `RunRecord`.
- In `internal/autopilot/reconcile.go`:
  - Remove `r.guardianID` normalization checks in `reconcileRunsAtBootLocked` and `normalizeSlotIDsLocked`.

### D6. Test Cleanup
- **Symbol / File:** `internal/autopilot/run_test.go`, `internal/autopilot/reconcile_test.go`, `internal/autopilot/slot_spawn_test.go`, `internal/daemon/autopilot_reconcile_test.go`, `internal/daemon/autopilot_routes_test.go`
- In `internal/autopilot/run_test.go`:
  - Remove `guardianAgentFake` struct.
  - Remove tests asserting session lifecycle: `TestGuardianAgentPersistsAndStopsWithRun`, `TestGuardianAgentStopsOnCompletion`, `TestMissingGuardianIsRecreatedAfterRestart`.
- In `internal/autopilot/reconcile_test.go`:
  - Remove embedded `*guardianAgentFake` from `migrationFake`.
  - Remove `TestSetRuntimeReconcileGuardiansUsesSlotIDs`.
- In `internal/autopilot/slot_spawn_test.go`:
  - Replace `guardianAgentFake` with `newFakeRuntime()`; update or drop assertions on `rec.GuardianID`.
- In `internal/daemon/autopilot_reconcile_test.go`:
  - Remove `TestReconcileSessionsRetiresLegacyGuardian`.
  - Remove `TestGuardianBootReconcileTerminatesLegacyGuardianForSlot`.
- In `internal/daemon/autopilot_routes_test.go`:
  - Remove `TestGuardianBootReconcileTerminatesOrphans`.
  - Update `TestGuardianVisibilityAndRunStopCleanup` (or replace with manager session assertions).
- **Untouched:** `internal/autopilot/guardian_test.go` is **UNTOUCHED**. All tests verifying the daemon guardian goroutine behavior (`RunGuardian`, `guardianTick`, `superviseRun`, heal ladder transitions, context rotations) remain exactly as they are.

---

## 3. Invariants

1. **Daemon Guardian Goroutine Unchanged:**
   There is **NO** behaviour change to the daemon guardian goroutine (`RunGuardian`, `guardianTick`, `superviseRun`) in `internal/autopilot/guardian.go`. The goroutine continues to tick at the configured interval, supervise run health, execute the 4-stage heal ladder (nudge, restart, rotate, backoff), monitor heartbeats (`gr.BrainActivity`), inspect context window limits (`gr.BrainContextLevel`), send steering messages (`gr.NudgeBrain`), and trigger the overwatch worker-tending pass (`c.overwatchTick`).
2. **GuardianRuntime Preserved:**
   `GuardianRuntime` in `internal/autopilot/run.go` remains intact. The runtime surface providing brain activity timestamps, context levels, nudges, and escalations is unchanged. Only `GuardianAgentRuntime` (the hollow session abstraction) is dropped.
3. **API & Wire Backward Compatibility:**
   `wd autopilot status`, `/autopilot/status`, and OpenAPI schemas continue to emit `guardian_id` and `guardian_slot_id` (e.g. `<scope>-guardian`). Existing API clients and web UI components reading these fields observe identical identifiers without needing changes.
4. **Collision Protection via Claims:**
   The claim registry in `internal/autopilot/claims.go` continues to reserve `GuardianSlotID(scope)` to ensure that no agent session or competing run claims the reserved guardian slot name.

---

## 4. Phase 1 Implementation File-Touch List

| File | Changes |
|---|---|
| `internal/autopilot/run.go` | Remove `GuardianAgentRuntime` interface (~lines 152–161). Remove `SpawnGuardian` call in `spawnBrain` (~lines 242–255). Remove `TerminateGuardian` call in `teardownBrain` (~lines 273–276). |
| `internal/daemon/autopilot_runtime.go` | Remove `_ autopilot.GuardianAgentRuntime = autopilotRuntime{}` assertion (~line 35). Remove `guardianSystemTag` and `guardianRunPrefix` constants (~lines 42–44). Remove `SpawnGuardian` (~lines 188–229), `TerminateGuardian` (~lines 231–247), and `ReconcileGuardians` (~lines 249–286). |
| `internal/daemon/autopilot_reconcile.go` | Remove `rt.retireLegacyGuardian` call in `ReconcileSessions` (~line 27). Remove `retireLegacyGuardian` implementation (~lines 85–101). Remove `containsTag(..., guardianSystemTag)` in `reconcileWorkerBackRefs` (~line 107). Remove `guardianRunIDFromTags` (~lines 175–182). |
| `internal/autopilot/controller.go` | Remove `r.guardianID` field from `type run struct` (~line 127). Remove `if gr, ok := rt.(GuardianAgentRuntime)` in `SetRuntime` (~lines 246–265). Compute `GuardianID` in `Status()` via `guardianSlotIDOrEmpty(r.slotScope)` (~line 892). |
| `internal/autopilot/lifecycle.go` | Remove `r.guardianID` assignment in run restore loop (~lines 55–57). Remove `rec.GuardianID` assignment in `recordLocked` (~lines 103–105). In `runStatusLocked`, compute `GuardianID` via `guardianSlotIDOrEmpty(r.slotScope)` (~line 548). |
| `internal/autopilot/store.go` | Remove `GuardianID string` from `RunRecord` struct (~line 35). |
| `internal/autopilot/reconcile.go` | Remove `r.guardianID` normalization checks in `reconcileRunsAtBootLocked` (~lines 62–66) and `normalizeSlotIDsLocked` (~lines 99–101). Clean up `LegacyGuardianID` / `GuardianSlotID` fields on `BootReconcileRun` if no longer consumed. |
| `internal/autopilot/identity.go` | **Unchanged:** Keep `GuardianSlotID` and `guardianSlotIDOrEmpty` for computed status fields. |
| `internal/autopilot/claims.go` | **Unchanged:** Keep `GuardianSlotID(scope)` reservation to prevent name collisions. |
| `internal/autopilot/status.go` | **Unchanged:** Keep `GuardianID` and `GuardianSlotID` fields on `RunStatus`. |
| `internal/daemon/apidocs/openapi.yaml` | **Unchanged:** Keep `guardian_id` and `guardian_slot_id` properties. |
| `internal/autopilot/guardian.go` | **Unchanged:** Daemon guardian goroutine (`RunGuardian`, `guardianTick`, `superviseRun`) remains intact. |
| `internal/autopilot/guardian_test.go` | **Unchanged:** Guardian loop tests remain intact. |
| `internal/autopilot/run_test.go` | Remove `guardianAgentFake` (~lines 118–145) and tests `TestGuardianAgentPersistsAndStopsWithRun`, `TestGuardianAgentStopsOnCompletion`, `TestMissingGuardianIsRecreatedAfterRestart`. |
| `internal/autopilot/reconcile_test.go` | Remove embedded `*guardianAgentFake` from `migrationFake`. Remove `TestSetRuntimeReconcileGuardiansUsesSlotIDs` (~lines 49–62). |
| `internal/autopilot/slot_spawn_test.go` | Update `TestSlotSpawnFirstStartCreatesSlotIds` to use `newFakeRuntime()` and remove `rec.GuardianID` assertion. |
| `internal/daemon/autopilot_reconcile_test.go` | Remove `TestReconcileSessionsRetiresLegacyGuardian` (~lines 38–54) and `TestGuardianBootReconcileTerminatesLegacyGuardianForSlot` (~lines 98–115). |
| `internal/daemon/autopilot_routes_test.go` | Remove `TestGuardianBootReconcileTerminatesOrphans` (~lines 110–134). Update `TestGuardianVisibilityAndRunStopCleanup` (~lines 69–108). |

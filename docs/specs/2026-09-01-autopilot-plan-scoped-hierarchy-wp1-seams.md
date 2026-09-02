# Autopilot Plan-Scoped Hierarchy — WP1 Seam Inventory

**Date:** 2026-09-02  
**Status:** Baseline characterization (WP1)  
**Parent:** [2026-09-01-autopilot-plan-scoped-hierarchy.md](./2026-09-01-autopilot-plan-scoped-hierarchy.md)

This document records the exact production seams frozen by WP1 characterization tests. Downstream packages (WP4–WP6, WP9) must modify only these surfaces unless a new seam is discovered during implementation.

---

## WP4 — Deterministic slot spawn

**Goal:** Manager and guardian use `SpawnRequest.Ticket = <scope>-autopilot|guardian`; adopt live session on `store.ErrExists`.

| Seam | Location | Current behavior (frozen) | WP4 change |
|---|---|---|---|
| Brain spawn | `internal/daemon/autopilot_runtime.go` `SpawnBrain` (~52-78) | `Lifecycle.Spawn` with free-form id → `agent-<8hex>` via `resolveID` | `Ticket = <scope>-autopilot`; adopt on `ErrExists` |
| Guardian spawn | `internal/daemon/autopilot_runtime.go` `SpawnGuardian` (~98-121) | `guardian-` + trimmed `run_id` | `Ticket = <scope>-guardian`; adopt on `ErrExists` |
| Controller spawn hook | `internal/autopilot/run.go` `spawnBrain` (165-209) | Calls `Runtime.SpawnBrain`; no ticket/slot id | Pass slot scope; record slot id on run |
| Boot reconciliation | `internal/autopilot/controller.go` `SetRuntime` (260-289) | Always `spawnBrain` for live runs; ignores stored `BrainID` | Adopt live tmux into slot when present |
| Run restore | `internal/autopilot/lifecycle.go` `restoreStoredRuns` (44-48) | Deliberately omits `BrainID` restore | May read slot id fields once WP3 lands |
| Run persist | `internal/autopilot/lifecycle.go` `recordLocked` (56-65) | Persists `BrainID` from in-memory brain handle | Persist `<scope>-autopilot` slot id |

**Characterization tests:** `TestCharacterization_RestartSpawnsWithoutAdoptingStoredBrainID`, `TestCharacterization_CanBrainCompleteAfterSimulatedRestart`

---

## WP5 — In-place hot-swap rotation

**Goal:** Replace `rotateBrain` terminate-spawn with `Lifecycle.HotSwap` on the manager slot.

| Seam | Location | Current behavior (frozen) | WP5 change |
|---|---|---|---|
| Rotation hook | `internal/autopilot/run.go` `rotateBrain` (147-158) | `TerminateBrain` then `spawnBrain` → new `agent-<hex>` | `Runtime.RotateBrainSlot` → `HotSwap` |
| Heal ladder — restart | `internal/autopilot/guardian.go` (~169) | `rotateBrain(ctx, r, cur)` same backend | HotSwap pinned backend |
| Heal ladder — rotate | `internal/autopilot/guardian.go` (~193, ~247) | `rotateBrain(ctx, r, sel.Backend)` | HotSwap selected backend |
| Planned rotation | `internal/autopilot/guardian.go` context-pressure path | `rotateBrain` on critical context | Same HotSwap seam |
| Handoff path | `internal/handoff/serialize.go` (15-26) | `<workdir>/.warden/handoff-<agent-id>.md` | Stable `<scope>-autopilot` path |
| HotSwap primitive | `internal/lifecycle/switch.go` `HotSwap` (95-167) | Used by pipelines, not autopilot | Called from guardian rotation |

**Characterization tests:** `TestCharacterization_RotateBrainMintsNewAgentID`, `TestCharacterization_GuardianRotationWalksIdChurn`

---

## WP6 — Stop parenting autopilot workers

**Goal:** MCP worker spawn omits `ParentID`; set `autopilot_run_id` / `autopilot_slot` / `autopilot_task_id` back-refs instead.

| Seam | Location | Current behavior (frozen) | WP6 change |
|---|---|---|---|
| Worker spawn | `internal/mcp/server.go` (~297) | Sets `ParentID` to manager session | Omit `ParentID` when `AutopilotSlot` set |
| Ownership guard | `internal/daemon/ownership_guard.go` `guardOwnership` (48-64) | Tag-based: `autopilot` + `run:<run_id>` | Unchanged (tags remain auth channel) |
| Tag inheritance | `internal/daemon/ownership_guard.go` `inheritOwnershipTags` (88-102) | Appends tags at spawn via `strict_lifecycle.go:69` | Also set back-ref fields |
| Parent tombstone | `internal/daemon/strict_lifecycle.go` `DeleteSession` (288-311) | Tombstones parent with live children | Rotated manager without children → normal delete |
| Tombstone reap | `internal/daemon/tombstone_reap.go` (40-91, 115-129) | Reaps when no live children | Dead managers reap after workers finish |
| Parent chain in lifecycle | `internal/lifecycle/lifecycle.go` (1493-1497) | `ParentID` one-way, no bulk re-parent | Not used for autopilot ownership |

**Characterization tests:** `TestCharacterization_AutopilotRotatedManagerTombstonesWithLiveWorkers` (`internal/daemon/autopilot_characterization_test.go`), existing `TestDeleteTombstonesParentWithLiveChild`

---

## WP9 — Per-plan integration branch

**Goal:** Resolve branch at register/preflight; persist on `RunRecord`; consumers read stored value only.

| Seam | Location | Current behavior (frozen) | WP9 change |
|---|---|---|---|
| Config default | `internal/config/config.go` `AutopilotIntegrationBranch` (1571-1573) | Global `autopilot.merge.target_branch` | Template/`{{plan}}` + per-plan derive for new runs |
| Controller field | `internal/autopilot/controller.go` `integrationBranch` (set in `NewController` ~152) | Single global string on controller | Per-run resolved value in store |
| Preflight branch block | `internal/autopilot/preflight.go` (87-106) | `branch := c.integrationBranch` | Resolve once per run; sanitizer |
| Run record write | `internal/autopilot/lifecycle.go` `recordLocked` (59) | `IntegrationBranch: c.integrationBranch` | Persist resolved per-run branch |
| Land chokepoint | `internal/autopilot/controller.go` `LandParams` (880) | `IntegrationBranch: c.integrationBranch` | Read `RunRecord.IntegrationBranch` |
| Land guard | `internal/autopilot/land.go` (154-155) | `pr.BaseRef != req.IntegrationBranch` → `ErrWrongBase` | Unchanged semantics; different branch per run |
| Daemon land route | `internal/daemon/land_routes.go` (68-78) | Forwards `LandParams.IntegrationBranch` | Unchanged wire; value becomes per-run |
| Gate resolution | `internal/autopilot/land.go` `resolveGateMode` (225-237) | `auto` → `ci` only when workflows cover branch | Warn when derived branch uncovered |
| Grandfather | `RunRecord.IntegrationBranch` on existing runs | Mirrors global at write time | Never re-derive `autopilot/integration` records |

**Characterization tests:** `TestCharacterization_ConcurrentRunsShareGlobalIntegrationBranch`

---

## TUI rendering (WP8 consumes; WP1 freezes baseline)

| Seam | Location | Current behavior (frozen) |
|---|---|---|
| Run grouping | `internal/tui/list.go` `projectGroupedItems` (700-714) | Plan tasks then tag-matched sessions at depth 1 |
| Run tag parse | `internal/tui/list.go` `sessionRunID` (734-743) | Parses `run:` and `autopilot-run:` tags |
| Run header | `internal/tui/list.go` `renderItemLine` apRun case (1041-1047) | Name, state, task counts; no integration branch |
| Flat list filter | `internal/tui/control_pane.go` (223-235) | Hides run-tagged agents from flat forest |

**Characterization tests:** `TestCharacterization_RenderAutopilotRunTreeGolden`, `TestCharacterization_RenderAutopilotRunCollapsedGolden` (`internal/tui/list_test.go`)

---

## Test index

| Test | Package | Scenario |
|---|---|---|
| `TestCharacterization_RotateBrainMintsNewAgentID` | `internal/autopilot` | Rotation id churn |
| `TestCharacterization_GuardianRotationWalksIdChurn` | `internal/autopilot` | Full heal-ladder id sequence |
| `TestCharacterization_RestartSpawnsWithoutAdoptingStoredBrainID` | `internal/autopilot` | Daemon restart spawn-without-adopt |
| `TestCharacterization_CanBrainCompleteAfterSimulatedRestart` | `internal/autopilot` | Auth break after restart |
| `TestCharacterization_ConcurrentRunsShareGlobalIntegrationBranch` | `internal/autopilot` | Shared integration branch defect |
| `TestCharacterization_AutopilotRotatedManagerTombstonesWithLiveWorkers` | `internal/daemon` | Tombstone on delete with live children |
| `TestCharacterization_RenderAutopilotRunTreeGolden` | `internal/tui` | TUI run tree rendering |
| `TestCharacterization_RenderAutopilotRunCollapsedGolden` | `internal/tui` | TUI collapsed run header |

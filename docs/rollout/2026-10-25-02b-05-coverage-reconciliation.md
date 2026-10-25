# 02b-05 coverage reconciliation and plan-03 retirement handoff

- **Plan / task:** `plan-e5fdd3fb`, task `coverage-reconciliation-and-retirement-handoff`.
- **Verified source SHA:** **`4a057dea`** (`docs(pty): truthful 02b-05 rollout runbook, evidence templates, corrected specs (#1057)`) on the integration branch `autopilot/02b-05-pty-end-to-end-validation-and-rollout-gates`; contains 02b-01 (`c01ef360`), 02b-02 (`32dc1a42`), 02b-03 (`5a1aac71`), 02b-04 (`48997bbf`), 02b-05 t1 (`7d0dabc4`), t2. Test/function names below were checked to exist at this SHA. Results (PASS) are those recorded in the [t1 matrix](2026-10-25-02b-05-t1-adversarial-matrix.md); this task re-ran `internal/terminalcontract` and the project checks (§9).
- **Scope of the claim:** *code readiness* only. Nothing here is a deployment, release, cohort, soak or drain observation. No `runtime.pty` switch, restart, deploy or release was performed.

Status key: **OWNED** = verified code + test at the SHA · **BLOCKED** = cannot be operated/proven yet, reason given · **UNSUPPORTED** = product does not support it · **MISSING-OBSERVATION** = code owned but production evidence absent.

## 1. Verdict

| Question | Answer |
|---|---|
| Code readiness: plain-shell terminals on the direct PTY runtime (Linux) | **READY at fixture level.** Owned and tested; no observed production cohort. |
| Code readiness: direct AI agents | **BLOCKED.** The runtime can host a scripted agent only through the test seam `Manager.SetAgentDirectReady(true)` (`internal/ptyhost/manager.go:629`, no non-test caller). `ptyhost.IsAdapterDirectCapable` (`internal/ptyhost/resolve.go:20`) is true only for `terminal`/`shell`/``; every AI adapter receives a typed unsupported result. No live provider smoke was run. |
| Release / deployment readiness | **NOT READY, by design.** No shadow baseline, authorised cohort, ≥ 20 × 24 h soak, drain or live rollback observation exists (runbook §9: all MISSING). |
| Plan 03 may start | **No.** All ten `Plan03EntryGate` items are unsatisfied; `G03-DIRECT` cannot pass for agents. |

## 2. Support matrix (what an operator gets today)

| Session kind | Linux | macOS | Windows | Direct hosting reachable by operator | Authoritative state/permission source |
|---|---|---|---|---|---|
| Plain shell (`kind=terminal`) | fixture-verified | compile/vet only, runtime NOT-RUN | UNSUPPORTED | yes: `runtime.pty: on`, new sessions only | none (shells never reach the prompt engine) |
| AI agent, any adapter (claude, antigravity, codex, cursor, aider, opencode, crush, goose) | tmux | tmux | tmux/legacy | **BLOCKED** | pollers (tmux); `on` adds a read-only mirror only |
| Autopilot-owned agent | tmux | tmux | — | BLOCKED (#919) | pollers |
| Unmanaged terminals | untouched | untouched | untouched | UNSUPPORTED | — |

Verify per session with `wd inspect pty runtime <id>`; `requested_mode` and a tmux mirror are never proof of direct hosting.

## 3. Reconciliation: plan-48de9357 (PTY01) acceptance

| # | Acceptance item | Status | Owner (code → test) | Gap |
|---|---|---|---|---|
| 01.1 | Interactive agents supervised via warden PTY event stream, durable events, safe reconnect, polling not primary | **BLOCKED for agents** / OWNED for runtime + shells | `internal/ptyruntime`, `internal/ptyhost`, `internal/ptyholder` → `ptyruntime.TestF_VT_01_FullscreenTracesRoundTrip`, `ptyattach.TestReplayThenLiveOrderingMidStream`; direct agent seam: `ptyhost.TestAgentAndShellLaunchDirectWithoutTmux` | Agents are tmux + mirror in every mode; poller still primary for them |
| 01.2 | Classifier: work/idle/waiting/permission/decision/unknown, never from silence alone | OWNED (library + direct-record wiring) | `internal/stateclassifier/classifier.go` → `TestQuiescenceInvestigationNeverFromSilenceAlone`, `TestFusingEvidenceIdleMarkerAndSilence`; `authority_test.go: TestStatusWrittenOnlyForDirectRecords`, `TestNeverOverwritesFinalStatusOrWritesExit` | Only reachable for direct records, i.e. not for operators (agents blocked) |
| 01.3 | Normalized permission extraction, fingerprint, learning is scoped/expiring | OWNED | `internal/stateclassifier/normalize.go`, `internal/promptengine` → `TestNormalizeAntigravityFixtures`, `TestNormalizeTrustWorkspaceScope`, `TestNormalizeDriftedMenuIsNotConfident`, `TestLearnedShapeFullLifecycle`, `TestHumanContradictionRetiresShape` | None for code |
| 01.4 | Four postures enforced; dangerously-execute-all accepts confident permissions, never strategic | OWNED | `internal/promptengine/engine.go`, `internal/decisionrouter` → `promptengine.TestE2EKnownPermissionByPosture`, `TestE2ETrustPrompt`, `TestE2EStrategicMenuIsNeverAPermission`, `TestE2EPostureBypassLaunch` | Operator effect only on direct records |
| 01.5 | Autopilot strategic → Tier-1 brain; plain/pipeline → human; ambiguity → attention | OWNED | `internal/decisionrouter/router.go` → `TestStrategicDecisionHumanInboxVsAutopilotConsult`, `TestTOCTOURevalidationAborts` | Autopilot direct agents BLOCKED (#919) |
| 01.6 | Redacted queryable decision trace + metrics by adapter/version | OWNED | `internal/ptyobs`, `internal/daemon/strict_ptyobs.go`, `internal/cli/pty_inspect.go` → `promptengine.TestE2EReleaseGateTraceMetricsAndRedaction`, `daemon.TestSessionRuntimeAndDecisionsEndpoints`, `cli.TestInspectPTYScriptedFixture` | No live metrics observed |
| 01.7 | Antigravity end-to-end (PTY, recognition, bypass, trust, changed-format, reconnect, redaction) | OWNED as fixtures | `internal/agentbackend/backends/antigravity.go` → `TestE2EDriftedPermissionNeedsAttention`, `TestE2EAnsweredOnceAcrossReconnect`, `TestE2EStalePromptTOCTOU` | No live `agy` smoke; not direct-capable |
| 01.8 | `wd check`, `make verify-fast`, generated docs current, scripted CLI fixture | OWNED | §9 | CI green ≠ deployment |
| 01.c | Constraint: no secret leakage in stream/durable/metrics | OWNED, limits stated | `ptyhost.TestF_PRIV_01_PlantedSecretsAbsentFromDurableAndRemoteViews`, `ptyruntime.TestSecretsNeverReachTheJournal` | Shape-based detection (t1 matrix §5.5) |
| 01.d | Constraint: default never widens permissions | OWNED | `promptengine.TestE2EKnownPermissionByPosture` (default posture rows) | — |

## 4. Reconciliation: plan-23988b36 (PTY02) acceptance

| # | Acceptance item | Status | Owner → test | Gap |
|---|---|---|---|---|
| 02.1 | All warden-created plain terminals (manual + auto, every surface) use the shared PTY runtime; name/project/env/cwd preserved | OWNED (`runtime.pty: on`; default `off`) | `internal/daemon/terminal_lifecycle.go`, `internal/terminalcontract/matrix.go` → `terminalcontract.TestTerminalLaunchFunnel`, `TestMatrixCoversRequiredSurfaces`, `daemon.TestSpawnTerminalRollbackLeavesNothing` | Not default; macOS NOT-RUN |
| 02.2 | Shell attach/detach, one controller, resize, signals, exit, replay, backpressure, no AI interception | OWNED | `ptyattach.TestCtrlCAndEOFArePassedToTheRemotePTY`, `TestJobControlOverAttach`, `ptyhost.TestF_ATTACH_03_ResizeArbitrationAndSIGWINCH`, `ptyruntime.TestSlowSubscriberNeverBlocksThePTY`, `stateclassifier.TestShellSessionNeverReachesPromptEngine`, `terminalcontract.TestTerminalPathsDoNotImportDecisionPackages` | — |
| 02.3 | Shell AND agent survival over daemon restart/update/crash; authenticated adoption; fencing; actionable failures | Shells OWNED; agents OWNED in runtime only (BLOCKED to operate) | `ptyholder.TestF_SURV_01/02/03/04`, `TestF_AUTH_01/02/03`, `daemon.TestRecoverPTYAdoptsLiveHolderTerminal`, `TestRecoverPTYAdoptsLiveHolderAgentAfterRollback`, `TestRecoverPTYLostHolderAgentIsOrphanedNotRelaunched` | No observed survival on a live system; autopilot-owned #919 |
| 02.4 | Documented opt-in/shadow/default rollout, numeric thresholds, observed parity artifacts, per-session cohort rollback | Thresholds OWNED; **observed artifacts MISSING-OBSERVATION** | `internal/terminalcontract/budgets.go` (`Budgets`, `Stages`, `Plan03EntryGate`) → `TestBudgetsSane`, `Test02bBudgetsAgreeWithSpec`, `TestPlan03EntryGateRequiresObservedEvidence` | Per-cohort config does not exist (library-only `SetCohortMode`); rollback is global and affects future launches only |
| 02.5 | Operator checklist + explicit plan-03 gate; merge alone does not authorise cleanup | OWNED (corrected here) | `docs/RUNBOOK-pty-rollout.md` §11 + `terminalcontract.Plan03EntryGate` | The original gate wrongly treated plan 01/02 fixtures as sufficient; superseded by §11 |

## 5. Reconciliation: archived plan-001ead2b (02b original) acceptance → slices 02b-01…05

| # | Item | Status | Owning slice and verified owner | Gap |
|---|---|---|---|---|
| A1 | Requirement→code→test→observation→gap matrix; unsupported cases explicit | OWNED | 02b-01: `docs/specs/2026-10-19-pty-02b-audit-and-runtime-contract.md` §4–§5; this document | — |
| A2 | `on`: every supported new AI agent AND terminal launches direct and works without agent-hosting tmux | **BLOCKED (agents) / OWNED (terminals)** | 02b-03: `lifecycle.TestDirectAgentLaunchIsHostAware`, `TestUnsupportedDirectAdapterIsRejectedBeforeSessionOrChild`, `TestAgentLaunchStaysTmuxWhenNotPlanned`; `ptyhost.TestAgentAndShellLaunchDirectWithoutTmux`; t1 `TestT1_*` with tmux failing/absent | No adapter capability declaration; guard has no production caller; no live smoke |
| A3 | Runtime identity distinct from tmux/conversation id; additive fields; resolved not inferred | OWNED | 02b-01: `ptyhost.TestResolve_Precedence`, `daemon.TestPTYRuntimeViewActualKindTruthfulWhenGlobalModeDiffers`, `terminalcontract.TestNoRuntimeInferringTmuxSessionCheck`, `agentstore` `runtime_test.go` | — |
| A4 | CLI/TUI/Cockpit list/tree/detail/attach/reconnect by stable identity for direct, legacy, mixed; viewer close never kills child | OWNED (CLI, TUI, API); web xterm NOT-RUN | 02b-02: `tui.TestRuntimeAttach*` (`runtime_attach_contract_test.go`), `daemon.TestAttachUnknownRuntimeFailsExplicitlyInsteadOfFallingBackToTmux`, `ptyattach.TestNotDirectIsReportedBeforeAnyOutput`, `ptyattach.TestReconnectResumesWithoutDuplicatingInput` | Browser Cockpit xterm against a direct session not run |
| A5 | Survival across stop/start/update/SIGKILL; no duplicate launch; daemon-absent output, offline exit, holder loss contracts | OWNED (non-autopilot) | 02b-03: `daemon.TestAgentLaunchAdoptsUnrecordedHolderWithoutDuplicateTaskStart`, `TestRecoverPTYAgentRetainsBoundedOfflineOutput`, `TestRecoverPTYAgentExitWhileDaemonDownIsNotRelaunched`, `TestRecoverPTYRejectsStaleAgentHolderIdentity`, `ptyhost.TestAgentLaunchFailsClosedWhenExistingHolderCannotBeAdopted` | #918/#919 BLOCKED rows; policy "block at kernel PTY buffer" (no spool) |
| A6 | Direct agent state + permission/strategy event-authoritative, traceable; pollers cannot duplicate input | OWNED for direct records (reachable only via seam) | 02b-04: `poller/direct.go` + `poller.TestDirect*`, `lifecycle.inputauthority_test.go`, `ptyhost.authority_test.go`, `stateclassifier.authority_test.go`, `daemon.prompt_engine_context_test.go` | Not operator-reachable for agents |
| A7 | Shell and nested manual AI CLI never automated | OWNED | `stateclassifier.TestShellSessionNeverReachesPromptEngine`, `TestManuallyLaunchedAICLIInShellGetsNoAutomation` | — |
| A8 | Launch/model/permission/seeding/resume/switch/restore/fork/handoff/rotation + autopilot/pipeline/schedule paths validated or explicitly gated | Partially OWNED; direct variants **gated** | `lifecycle.TestDirectAgentRejectsLegacyLifecycleOperations`, `TestDirectRedeliverPromptFailsClosed`, `lifecycle/switch_test.go`, `TestSpawnFork*`, `TestRestoreRecreatesAndResumes`, `TestRestoreTerminalConcurrentLaunchesExactlyOnce` | Direct-agent fork/handoff/rotation/switch are rejected (gated), not supported. Autopilot/pipeline/schedule launches of direct agents are gated by the same guard. |
| A9 | Race/adversarial, fidelity, secrecy, backpressure, mixed runtime, protocol compat, resource regression pass | OWNED | 02b-05 t1: matrix §2 (M/F/C/R/S/U), `-race -count=2` sweep, 4 gaps fixed (G-A…G-D) | macOS, busy-AI RSS, web xterm NOT-RUN |
| A10 | Runbook separates tested capability from observed evidence; plan 03 gated | OWNED | 02b-05 t2/t3: `docs/RUNBOOK-pty-rollout.md`, evidence templates, `terminalcontract.Plan03EntryGate` | Observed evidence MISSING |
| A11 | Mixed fleet, per-session pin survives config flip, rollback keeps control | OWNED | `ptyhost.TestF_ROLL_01/03/04`, `TestDurablePin_GlobalModeFlipRestartAndSameNameReuse`, `daemon.TestRecoverPTYAdoptsLiveHolderAgentAfterRollback` | Rollback verified on fixtures only |
| A12 | Protocol/old-binary compatibility and downgrade boundary | OWNED + documented | `ptyholder.TestF_ROLL_02_ProtocolHandshakeRefusalOnVersionMismatch`, `ptyhost.TestPlanFrameFidelityIsAdditiveForOlderClients`; runbook §5 | Downgrade rehearsal MISSING |
| A13 | Spec-first API, no direct DB edits, destructive-rename-free migration | OWNED | `internal/daemon/apidocs/openapi.yaml` + `make generate` (clean); additive `agentstore`/`terminalstore` fields | — |
| A14 | #918/#919 not fixed opportunistically; affected scenarios reported | OWNED (reported) | t1 matrix R10/R11; runbook §8 | Remain BLOCKED |

## 6. Remaining risks (ordered)

1. **Direct AI agents are not operator-reachable** (guard with no production caller, zero direct-capable adapters). This is the structural blocker for plan 03 and for any broad rollout. Needs a later, separately reviewed plan: readiness gate, per-adapter capability declarations, own evidence, an *authorized* live provider smoke.
2. **No observed evidence of any kind** (runbook §9).
3. **#918** (graceful stop under autopilot load may become SIGKILL) and **#919** (boot recovery before `PlanSource`): autopilot-owned direct-agent restart survival unverified.
4. **macOS runtime NOT-RUN**; web xterm against a direct session NOT-RUN; busy-AI-CLI RSS not measured.
5. **Rollback is global**, not per cohort (no cohort config key/CLI/API exists).
6. Emulator = bounded VT subset (crop/pad resize); replay/snapshot are `redacted-text`; secret detection is shape-based; daemon-absent output blocks at the kernel PTY buffer; legacy `/attach` over-size resize gets no message.
7. Contract drift risk: `Stages` lists `agent` for S2–S4 as the *target*; reachable today is terminals only (comment added in `internal/terminalcontract/budgets.go`).

## 7. Numeric rollout budgets and required operator observations

Unchanged from the reviewed contract (`internal/terminalcontract/budgets.go`, runbook §3 Step 3); lowering any needs a reviewed code + doc change.

| Class | Budget |
|---|---|
| Sample | ≥ 20 verified-direct sessions **and** ≥ 24 h continuous soak, **per cohort** |
| Abort | stage error rate ≥ 1.0 % or fallback rate ≥ 5.0 % |
| Latency p99 (idle loopback 80×24) | keystroke echo ≤ 50 ms · attach→first byte ≤ 250 ms · resize→SIGWINCH ≤ 100 ms · adopt ≤ 2 s · lease takeover ≤ 200 ms · agent launch→first byte ≤ 500 ms |
| Resources per session | RSS ≤ 16 MiB · FDs ≤ 6 · goroutines ≤ 6 · journal ≤ 64 MiB · 50 concurrent clean |
| Zero tolerance | survival loss · duplicate processes · duplicate input bytes · stale-controller writes · durable secret leaks · false auto-approvals · classifier events from human shells (all 0) · actionable-error rate 100 % |
| Parity | byte-exact visible output 100 % · replay order violations 0 · state accuracy ≥ 98 % |

Operator must observe and file (templates A–E): `wd version` + SHA of the running daemon; per session `wd inspect pty runtime` (`actual_kind: direct`, `authoritative: true`, `evidence_freshness: fresh`); `warden_pty_*` metrics and alerts; TUI/CLI/native workflow checks (attach, Ctrl-C/EOF/job control, fullscreen, resize, Cockpit pane, `wd send`, restore, **web xterm**); graceful restart and SIGKILL with autopilot idle (record stop duration; > 30 s ⇒ blocked-by-#918); live rollback and downgrade rehearsal; natural drain counts to 0. Fixture numbers (t1 §2.4) never substitute.

## 8. Plan 03 (`plan-5cd3c58b`) handoff

**Done in the repository:** runbook §11 now lists ten gate items with stable IDs and the sources that never count as evidence; `terminalcontract.Plan03EntryGate` / `NotEvidence` mirror them and `TestPlan03EntryGateRequiresObservedEvidence` fails on drift. This is the supported, reviewable gate; completed plan flags, merged PRs, green CI, fixtures and requested mode are not accepted by it.

**Plan 03's ScrivaDB record (revision 4, pending) already names the corrective IDs** (constraint 10 and task `t1-readiness-and-drain` list plan-8eb533f5 → plan-06a9917e → plan-ce2a039b → plan-30011980 → plan-e5fdd3fb and say not to advance on completed flags). I did **not** call `update_plan` on it (a whole-list constraint replacement on a record another process may be revising; the existing text is already correct on the IDs). Proposed addition, if the maintainer wants it, appended as one extra constraint via `update_plan(plan_id=plan-5cd3c58b, expected_revision=4, constraints=<existing 10> + this)`:

> ENTRY EVIDENCE RULE: the only accepted evidence for readiness is the observed items in docs/RUNBOOK-pty-rollout.md §11 (IDs G03-DEPLOYED … G03-PLAN-TEXT; code mirror terminalcontract.Plan03EntryGate), filed with the rollout evidence templates at a recorded deployed SHA. Completed plan status or task flags, merged PRs, green CI, fixture/unit-test results, the requested runtime.pty mode and tmux mirrors of agents are NOT evidence of deployment, direct ownership, soak or legacy drain. While direct AI-agent hosting is not operator-reachable (no direct-capable adapter / readiness guard without production caller), G03-DIRECT cannot pass and this plan must report the unmet prerequisite and make no removal.

## 9. Checks run

- `go test ./internal/terminalcontract` — ok, including the new gate test.
- `wd check`, `make verify-fast`, `make gendocs-check` — see the PR description for results at the PR head (no CLI `Use/Short/Long/flags` changed, so `make gendocs` produces no diff).

## 10. Definition-of-Done checklist (CLAUDE.md)

| Item | Status |
|---|---|
| Tag & release | **Skipped (not applicable):** docs/contract-data slice; no feature shipped, no tag proposed. Needs maintainer confirmation before any tag. |
| README.md | Skipped: already states `on` hosts terminals only and agents are not converted. |
| `docs/` (FEATURES.md, USAGE.md, specs) | Updated: runbook §11, this reconciliation, `docs/FEATURES.md`/`USAGE.md` already truthful (verified, unchanged). |
| Website `site/` | Verified truthful (`guides/pty-runtime.md` rollout-status paragraph); unchanged. |
| Skill `skills/warden/` | Skipped: no change to how agents drive warden. |
| CLI help / generated `reference/cli.md` | Checked: `warden inspect pty` help is accurate; no change, `make gendocs-check` clean. |

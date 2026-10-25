# 02b-05 t1 — adversarial full-matrix evidence

- **Plan / task:** `plan-e5fdd3fb` (02b-05-pty-end-to-end-validation-and-rollout-gates), task `adversarial-full-matrix`.
- **Tested source:** integration branch `autopilot/02b-05-pty-end-to-end-validation-and-rollout-gates` at **`48997bbf`** (`feat(pty): complete direct-agent authority and readiness (#1053)`; 02b-01…04 merged), plus the changes in this PR. The PR head commit is the exact tree the results below were produced on.
- **Environment:** Linux 7.0.0-38-generic x86_64, Go 1.26.4, tmux 3.6 installed (used only by the legacy/tmux-path fixtures; the direct-path fixtures run with tmux **failing every call** and, separately, with tmux **absent from `PATH`**).
- **What this is:** automated, deterministic fixture evidence. Fake CLIs are the scripted re-exec'd test binary (`fakeREPL`) or `/bin/sh` scripts, loopback only. **No live provider smoke was authorized; none was run.** Nothing was deployed, released, or switched (`runtime.pty` untouched, systemd daemon and live agents untouched, isolated test daemons only). **Nothing here is production, soak or cohort evidence** — see §5.

Status key: **PASS** = fixture ran and passed · **FAIL** = ran and failed · **UNSUPPORTED** = the product does not support it on this path; the matrix states that rather than claiming it · **NOT-RUN** = not executed here (reason given) · **BLOCKED** = gated on an external issue (#918/#919).

## 1. Verdict

1. **Code validation of the fixture matrix: PASS**, after fixing four gaps found here (§3). Every PTY-scoped package passes under `-race` (run twice) and the full `go test ./...` passes (§4).
2. **Rollout of direct AI agents: BLOCKED (architectural, not a test failure).** Direct AI-agent hosting works end to end **only through the internal test seam** (`Manager.SetAgentDirectReady(true)`, which has no production caller), and **no AI adapter is direct-capable** (`ptyhost.IsAdapterDirectCapable` returns true only for `terminal`/`shell`; every registered AI adapter gets a typed unsupported result before launch, per the 02b-04 readiness contract). So "direct AI agents function without agent-hosting tmux" is **proven for the runtime, not reachable by an operator**. Shipping direct agents needs a reviewed operator readiness gate plus per-adapter capability declarations. That is work for a later plan, not something this slice can fix by flipping a guard.
3. **Plain terminals (direct shells):** supported path, all matrix rows PASS on Linux.
4. **macOS:** NOT-RUN (no darwin host). It cross-compiles and `go vet` passes for `GOOS=darwin GOARCH=arm64` across the PTY, daemon and CLI packages. Runtime evidence on darwin is still required before any darwin cohort.

## 2. Matrix

### 2.1 Runtime ownership — direct without agent-hosting tmux

| # | Entry | Status | Evidence |
|---|---|---|---|
| M1 | Direct AI agent (fake CLI) + direct shell launch and run with tmux failing every call | PASS | `ptyhost.TestAgentAndShellLaunchDirectWithoutTmux`, every `ptyhost.TestT1_*` (asserts `noTmux.calls == 0`) |
| M2 | Same with **tmux absent from `PATH`** (`PATH` holds only sh/sleep/printf/stty/cat) | PASS | `ptyhost` `TestT1_*` + `TestAgentAndShellLaunchDirectWithoutTmux`, and the full `ptyattach` suite, run under a stripped `PATH` |
| M3 | Operator-reachable direct AI-agent launch | **UNSUPPORTED → rollout BLOCKED** | `ptyhost/resolve.go IsAdapterDirectCapable`, `Manager.AgentDirectReady` default false; `lifecycle.TestUnsupportedDirectAdapterIsRejectedBeforeSessionOrChild` |
| M4 | TUI/Cockpit keeps presentation tmux while the child is direct (pane runs `warden attach`) | PASS | `tui.TestRuntimeAttach*` (`runtime_attach_contract_test.go`), `cli` runtime-view tests |
| M5 | Runtime identity resolved, never inferred from `TmuxSession` | PASS | `ptyhost.TestResolve_Precedence`, `daemon.TestPTYRuntimeViewActualKindTruthfulWhenGlobalModeDiffers` |

### 2.2 Terminal fidelity

| # | Entry | Status | Evidence |
|---|---|---|---|
| F1 | ANSI/VT fullscreen, cursor, alternate screen | PASS | `ptyruntime.TestF_VT_01_FullscreenTracesRoundTrip`, `TestControlSequencesAndAltScreenSnapshot`, `ptyattach.TestFaithfulBytesForUTF8ANSIAndFullscreen` |
| F2 | Partial UTF-8 across reads / idle flush | PASS | `ptyruntime.TestPartialUTF8AcrossReads`, `TestIdleFlushReleasesPartialRune` |
| F3 | Unicode wide/combining/emoji cells | PASS | `ptyruntime.TestF_VT_02_WideCombiningEmojiCells` |
| F4 | Combining-mark flood / oversized snapshot (untrusted output) | PASS **after fix G-A** | `ptyruntime.TestScreenModelBoundsAndSnapshotCap` |
| F5 | Resize + SIGWINCH, controller-only, last controller wins | PASS | `ptyhost.TestF_ATTACH_03_ResizeArbitrationAndSIGWINCH`, `ptyattach.TestResizeFollowsLocalTerminal` |
| F6 | Adversarial resize (65535×65535, >1024 cols, >256Ki cells) | PASS **after fix G-B** | `ptyruntime.TestResizeBeyondBoundsRefused`, `ptyhost.TestT1_AdversarialResizeRefusedAndSessionSurvives` |
| F7 | Selection / copy-paste | PASS (client-side) / limit | Bracketed paste and pasted bytes are ordinary input (`ptyattach` faithful bytes). Selection is done by the local terminal emulator; warden does no selection. Inside the cockpit, tmux mouse mode needs Shift-drag (documented TUI behavior). |
| F8 | Scrollback / reconnect replay-then-live, no gap/dup/reorder | PASS | `ptyattach.TestReplayThenLiveOrderingMidStream`, `TestReconnectResumesWithoutDuplicatingInput`, `ptyhost.TestTransportReconnectAcrossDaemonRestart` |
| F9 | Ctrl-C / EOF / job control | PASS | `ptyattach.TestCtrlCAndEOFArePassedToTheRemotePTY`, `TestJobControlOverAttach`, `ptyhost.TestT1_ShellAndCLIExitsAreDistinctAndReaped` (SIGINT via tty on a direct agent) |
| F10 | Shell exit vs CLI exit, distinct codes, kind never crosses, group reaped | PASS | `ptyhost.TestT1_ShellAndCLIExitsAreDistinctAndReaped`, `daemon.TestDirectTerminalExitRecordsStatus`, `daemon.TestRecoverPTYAgentExitWhileDaemonDownIsNotRelaunched` |
| F11 | Golden byte parity tmux vs direct | PASS | `ptyhost.TestF_PAR_01_GoldenByteStreamParityTmuxVsDirect` |

### 2.3 Concurrency, messaging, backpressure, resources

| # | Entry | Status | Evidence |
|---|---|---|---|
| C1 | One controller, viewers demoted, takeover bumps generation, stale gen rejected | PASS | `ptyhost.TestF_FENCE_01/02/03`, `ptyholder.TestFencingAndPrecedenceSurviveAdoption` |
| C2 | Concurrent viewers + controller input: identical ordered stream, viewer keystrokes dropped | PASS | `ptyhost.TestT1_ConcurrentViewersAndInputKeepOneOrderedStream` |
| C3 | Lost / retried input messages (idempotent `input_id`, reconnect) | PASS | `ptyhost.TestF_FENCE_03_ReconnectNeverDuplicatesInput`, `ptyattach.TestReconnectResumesWithoutDuplicatingInput` |
| C4 | Viewer cap (16/session, shared `/pty`+`/attach`), 429 actionable, slot freed on close, client does not retry | PASS **after fix G-C** | `ptyhost.TestT1_ViewerCapBoundedAndReleased`, `daemon.TestPTYViewerCapSharedAcrossPTYAndAttachIs429`, `ptyattach.TestViewerCapRefusalIsNotRetried` |
| C5 | Slow/blocked viewer never stalls the PTY; bounded then resync | PASS | `ptyruntime.TestSlowSubscriberNeverBlocksThePTY`, `TestLaggedRawViewerResyncsAtAFreshCut`, `TestWriterBackpressureJournalsGap` |
| C6 | Invalid / partial / oversized client frames close the socket, never crash | PASS | `ptyhost.TestTransportClosesInvalidAndOversizedClientFrames`, `TestTransportRequiresHello` |
| C7 | Bounded journal under 10× flood | PASS | `ptyhost.TestF_RES_02_JournalRetentionHoldsUnderOutputFlood`, `ptyruntime.TestJournalRetentionTrimsWholeSnapshotSegments` |
| C8 | Mixed agent+shell soak (50) without tmux: per-session FD/goroutine budget, no FD/goroutine/child leak | PASS — measured 3.00 FDs, 5.00 goroutines per session (budget 6/6) | `ptyhost.TestT1_MixedDirectSoakNoLeaksWithoutTmux`, `TestF_RES_01_SoakSessionsResourceLimitsAndNoLeaks` (terminals) |
| C9 | Race detector over the whole PTY scope | PASS (`-count=2`) after fixing test-harness races G-D | §4 |

### 2.4 Latency budgets measured on a direct AI agent (§6.9)

Loopback, idle, uninstrumented build. `TestT1_DirectAgentLatencyBudgetsWithoutTmux` asserts p99 ≤ budget (×8 only under `-race`). The ~20 ms floor is the rig's `IdleFlush: 20ms` output batching.

| Budget | Bound | Measured p99 | Status |
|---|---|---|---|
| Agent launch → first journal byte (**includes** fake-CLI exec) | ≤ 500 ms | 22.2 ms | PASS |
| Runtime identity resolve (no process exec) | ≤ 5 ms | 1.5 µs | PASS |
| Attach → first frame | ≤ 250 ms | 21.8 ms | PASS |
| Keystroke echo | ≤ 50 ms | 21.0 ms | PASS |
| Resize → SIGWINCH observed (incl. size-query echo) | ≤ 100 ms | 21.1 ms | PASS |
| Lease takeover | ≤ 200 ms | 20.6 ms | PASS |
| Adopt after restart | ≤ 2 s | not separately timed; adoption fixtures complete well inside their 5 s waits | PASS (bound only) |
| Busy AI CLI RSS per session | ≤ 16 MiB | not measured with a real provider CLI | NOT-RUN (no authorized provider) |

### 2.5 Fault injection and recovery boundaries

| # | Boundary | Status | Evidence |
|---|---|---|---|
| R1 | Launch: crash after child creation, before record persistence → retry adopts, task starts once | PASS (test de-flaked, G-D) | `daemon.TestAgentLaunchAdoptsUnrecordedHolderWithoutDuplicateTaskStart` |
| R2 | Launch: existing holder that fails adoption → fail closed, no second launch | PASS | `ptyhost.TestAgentLaunchFailsClosedWhenExistingHolderCannotBeAdopted` |
| R3 | Adopt: daemon SIGKILL / graceful restart → same pid, no relaunch | PASS | `ptyholder.TestF_SURV_01/02/04`, `daemon.TestRecoverPTYAdoptsLiveHolderTerminal`, `TestRecoverPTYAdoptsLiveHolderAgentAfterRollback` |
| R4 | Adopt: holder gone / stale identity / recycled pid / wrong credential → orphaned, actionable, never relaunched | PASS | `ptyholder.TestF_SURV_03`, `TestF_AUTH_01/02/03`, `daemon.TestRecoverPTYLostHolderAgentIsOrphanedNotRelaunched`, `TestRecoverPTYRejectsStaleAgentHolderIdentity` |
| R5 | Daemon absence: output bounded by the kernel PTY buffer (no spool); exit while offline recorded, not relaunched | PASS (policy = block at kernel buffer) | `daemon.TestRecoverPTYAgentRetainsBoundedOfflineOutput`, `TestRecoverPTYAgentExitWhileDaemonDownIsNotRelaunched` |
| R6 | Protocol mismatch (old/new holder) refused with a version error | PASS | `ptyholder.TestF_ROLL_02_ProtocolHandshakeRefusalOnVersionMismatch`, `TestProtocolMismatchRefused`, `ptyhost.TestPlanFrameFidelityIsAdditiveForOlderClients` |
| R7 | Store faults: corrupt segments/index, startup interruption, lock after SIGKILL | PASS | `e2e.TestIndexFaultsNeverServeWrongData`, `TestCorruptSegmentRefusesToStartAndChangesNothing`, `TestStartupInterruption`, `TestLockReleasedAfterSIGKILL` |
| R8 | Attach while not direct / unknown runtime → explicit error, no tmux guess | PASS | `daemon.TestAttachUnknownRuntimeFailsExplicitlyInsteadOfFallingBackToTmux`, `ptyattach.TestNotDirectIsReportedBeforeAnyOutput` |
| R9 | Spawn rollback leaves nothing; concurrent restore launches once | PASS | `daemon.TestSpawnTerminalRollbackLeavesNothing`, `lifecycle.TestRestoreTerminalConcurrentLaunchesExactlyOnce` |
| R10 | Autopilot-owned direct agent restart survival | **BLOCKED: #919** | Excluded by the 02b audit §7: boot recovery builds run records before `PlanSource` is wired |
| R11 | Graceful daemon stop during autopilot activity | **BLOCKED: #918** | `Controller.mu` held across guardian I/O; the stop may become a SIGKILL. Plain/terminal graceful-stop rows (R3) were run with no autopilot |
| R12 | Mixed legacy+PTY fleet list/attach/send/approve/kill; per-session pin survives mode flip/rollback | PASS | `ptyhost.TestF_ROLL_01/03/04`, `TestDurablePin_GlobalModeFlipRestartAndSameNameReuse` |

### 2.6 Secrecy and injection

| # | Entry | Status | Evidence |
|---|---|---|---|
| S1 | Secret input (no-echo / fenced secret context) never journaled | PASS | `ptyhost.TestFencedSecretInputNeverReachesTheJournal`, `ptyruntime.TestSecretsNeverReachTheJournal` |
| S2 | Credentials/env never in holder state; planted secrets absent from journal, audit, diagnostics, SSE, remote views | PASS | `ptyholder.TestHolderStateNeverHoldsEnvironmentSecrets`, `ptyhost.TestF_PRIV_01_PlantedSecretsAbsentFromDurableAndRemoteViews` |
| S3 | Raw authorized live bytes vs redacted durable replay; replay labelled `redacted-text`, never `exact-live`; read-only token gets no raw bytes and no lease | PASS | `ptyhost.TestTransportReplayIsRedacted`, `ptyruntime.TestF_VT_03_SGRKeptLocallyDroppedInRedactedViews`, `daemon.TestPTYStreamReadonlyTokenIsObserveOnly` |
| S4 | Terminal injection: secret inside OSC title / SGR redacted in journal and replay; OSC/DCS payloads never in the screen snapshot | PASS | `ptyhost.TestT1_TerminalInjectionAndSecretsInControlSequences` |
| S5 | Untrusted approval-shaped prompts in a shell / nested AI CLI → no automation | PASS | `stateclassifier.TestShellSessionNeverReachesPromptEngine`, `TestManuallyLaunchedAICLIInShellGetsNoAutomation`, `ptyhost.TestT1_TerminalInjection…` (shell part) |
| S6 | Labelled AI prompt corpus accuracy ≥ 98 %, 0 false approvals | PASS (corpus only covers fixtures that exist) | `stateclassifier.TestF_PAR_02_LabelledAIPromptCorpusAccuracyVsLegacyPoller` |

### 2.7 Surface consistency

| # | Entry | Status | Evidence |
|---|---|---|---|
| U1 | API `GET /sessions/{id}/runtime` = CLI `inspect pty runtime` = MCP `session_runtime` (one document) | PASS | `daemon.TestSessionRuntimeAndDecisionsEndpoints`, `cli.TestInspectPTYScriptedFixture`, `tui` runtime-view tests |
| U2 | No blind task/run reset: recovery never relaunches or resets an existing task; lost holder → orphaned + explicit restore | PASS (non-autopilot) / BLOCKED #919 (autopilot runs) | R1, R4, R10 |
| U3 | Web cockpit xterm against a direct agent | NOT-RUN | No browser rig in this slice; `/attach`/`/pty` WS contracts are covered by the daemon tests |

## 3. Gaps found and fixed in this slice

| ID | Gap | Fix | Test |
|---|---|---|---|
| **G-A** | Untrusted output could stack unbounded combining marks on one cell, and the snapshot (resync/attach screen) had no size bound (`SnapshotMaxBytes` was a budget no code enforced). | `ptyruntime/screen.go`: ≤ 32 bytes of combining marks per cell; rendered snapshots capped at `MaxSnapshotBytes` (1 MiB = budget), dropping rows from the top to keep the cursor rows. | `TestScreenModelBoundsAndSnapshotCap` |
| **G-B** | **Daemon OOM by resize.** Any controller (`/pty`) or legacy `/attach` client could send `{"cols":65535,"rows":65535}`, and the screen model allocated two 4.29 G-cell grids. | `Session.Resize` refuses sizes beyond 1024 cols / 1024 rows / 262 144 cells with an actionable `ErrResizeTooLarge`; the session keeps its size. `Screen.Resize`/`NewScreen` clamp the model on every other path. `/pty` reports the refusal with an additive `notice` frame, which `wd attach` prints. | `TestResizeBeyondBoundsRefused`, `TestT1_AdversarialResizeRefusedAndSessionSurvives` |
| **G-C** | Viewer cap (`ViewersPerSession` = 16) was required by the 02b-01 checklist but never enforced. Each viewer may pin up to 4 MiB, so the count was unbounded memory. | `Manager.AdmitViewer`: a shared per-session slot across `/pty` and direct `/attach`, taken before the upgrade. Over the cap returns HTTP 429 with cause and next step (OpenAPI updated spec-first). `ptyattach` treats 429 as final instead of re-dialing. | `TestT1_ViewerCapBoundedAndReleased`, `TestPTYViewerCapSharedAcrossPTYAndAttachIs429`, `TestViewerCapRefusalIsNotRetried` |
| **G-D** | Under `-race` (not run by CI): a flaky direct-adopt test (it asserted pre-crash output in a non-repainting child's *screen*; the journal is the right check), and harness data races in `daemon` `fakeStore` reads and `lifecycle.FakeRunner` (background seed and successor goroutines). These were test-only; no product race was found. | Test reads via the journal and a locked `fakeStore.statusOf`; `FakeRunner.Run` serialized. | race sweep §4 |

## 4. Commands run (results at the PR head)

- `go test -race -count=2` over `ptyattach ptyevent ptyholder ptyhost ptyobs ptyruntime terminalcontract terminalstore stateclassifier promptengine decisionrouter lifecycle e2e`, `daemon -run 'PTY|Direct|Terminal|Attach|Runtime|Adopt|Hibernate|Spawn'`, and (`-count=1`) `tui cli mcp -run 'Runtime|Attach|Direct|Terminal|Inspect|PTY'`: **all ok** (before the fixes: 2 daemon and 2 lifecycle failures, all races or flakes in G-D).
- `ptyhost` `TestT1_*` + `TestAgentAndShellLaunchDirectWithoutTmux` and the full `ptyattach` suite with tmux absent from `PATH`: **PASS**.
- `GOOS=darwin GOARCH=arm64 go vet` (PTY, daemon, cli): **ok**.
- `wd check`: build, lint (vet) and fmt **pass**. The `test` step first failed only with `disk quota exceeded` on the shared `/tmp` tmpfs (peer agents' build caches); the same `go test ./...` with `TMPDIR` on the disk-backed home volume passes.
- `make generate`: no generated diff (the WS routes are excluded from codegen; only the OpenAPI description and the 429 responses changed). `make gendocs-check`: clean (no CLI help changed).

## 5. Residual limitations (stated, not disguised)

1. **Direct AI agents are not operator-enableable** (M3). All direct-agent evidence above is fixture-only, through `SetAgentDirectReady(true)`. **Rollout of direct agents is BLOCKED** until a reviewed readiness gate and per-adapter direct capability exist and get their own evidence.
2. **No observed evidence exists:** no production direct cohort, no ≥ 20 sessions × ≥ 24 h soak, no stage error/fallback rates. The fixture budgets in §2.4 do not certify a live provider CLI. Owner: task `truthful-rollout-and-runbooks` and the operator-authorized deployment gate.
3. **macOS runtime NOT-RUN** (compiles and vets only).
4. **Emulator/replay limits:**
   - The screen model is a bounded VT subset, not xterm. Reflow on resize is crop/pad only.
   - After adoption, the screen holds only what the child repaints after the SIGWINCH nudge. Pre-crash output of a non-repainting program is in the journal and replay, not the new screen model.
   - Replay and snapshots are redacted plain or SGR text (`fidelity: redacted-text`). Only live frames after `head` are exact.
   - Replayed output events carry the program's original control sequences except secrets: an OSC 52 clipboard write or title set is replayed to a viewer's emulator just as tmux would show it live. Whether it acts is up to the viewer's terminal (xterm.js has no OSC 52 by default).
5. **Secret detection is shape-based** (`terminalcontract.SecretDetectionLimits`): echoed shapeless secrets, base64-wrapped secrets (e.g. inside OSC 52) and secrets split across control sequences or redraws are not caught.
6. **Daemon-absent output** blocks the child at the kernel PTY buffer (no spool). This is bounded but stalls a chatty agent while the daemon is down.
7. **Resize bounds** (1024×1024, 256 Ki cells) refuse absurd sizes. A legacy `/attach` client gets no message (the raw protocol has no text frames); its size simply stays.
8. **#918 / #919:** autopilot-owned direct-agent restart survival (R10) and graceful stop under autopilot load (R11) are BLOCKED, not passed.
9. Busy-AI-CLI RSS budget and the web xterm direct-agent path are NOT-RUN.

# Operator Runbook: PTY Plain-Terminal & AI Runtime Staged Rollout

This runbook documents the operational lifecycle, rollout stages, per-session runtime pinning, downgrade boundaries, and parity verification for the PTY plain-terminal migration (`02-pty-plain-terminal-migration-and-rollout`).

---

## 1. Rollout Ladder & Stages

The rollout proceeds through five strictly sequenced stages (`docs/specs/2026-10-19-plain-terminal-inventory-and-contract.md` §7):

| Stage | Name | Kinds | Authoritative | Description |
|---|---|---|---|---|
| `S0` | `off` | agent, terminal | No | Default baseline. All sessions run in tmux with no PTY runtime or event journal. |
| `S1` | `shadow` | agent only | No | Observe-only mirrors via tmux `pipe-pane`. **Never mutates authoritative status and never sends input.** Terminals are excluded from shadow (a second owner of one PTY master cannot exist). |
| `S2` | `opt-in` | agent, terminal | Yes | Per-cohort `runtime.pty: on`. Direct sessions run on warden-owned PTY with independent holder process (`internal/ptyholder`). Survival and fencing gates (`G1`–`G4`) active. |
| `S3` | `default-new` | agent, terminal | Yes | Default for all new supported sessions once all numeric budgets are verified over `MinSessionsPerStage` × `MinSoakHoursPerStage`. |
| `S4` | `legacy-drain` | agent, terminal | Yes | Legacy tmux sessions complete naturally. Entry gate for Plan 03 (no forced live conversions). |

---

## 2. Per-Session Runtime Pinning & Cohort Rollback

### Runtime Pinning
- Every session's runtime mode (`off`, `shadow`, or `on`) is **pinned at creation time**.
- Flipping `runtime.pty` in config or changing cohort settings affects **future launches only**.
- Already-running direct sessions keep their direct PTY host, their independent holder process, and remain authoritative (`IsAuthoritative(name) == true`).
- Live PTY sessions are **never unsafely converted live** into tmux panes or killed on config changes.

### Cohort Management & Rollback
- Cohorts can be assigned per session or project:
  - `Manager.SetCohortMode(cohort, mode)` configures cohort runtime mode.
  - `Manager.SetSessionCohort(name, cohort)` assigns a session to a cohort.
  - `Manager.SetSessionRuntimePin(name, mode)` explicitly overrides runtime for an individual session.
- Rolling back a cohort or global mode:
  - Updates the mode for subsequent launches in that cohort.
  - Appends an audit entry to the rollback ledger (`Manager.Rollbacks()`).
  - Active sessions in the rolled-back cohort continue running undisturbed until completion or explicit termination.

---

## 3. Observational Shadow Boundaries (`S1`)

- **Observe-Only:** Shadow sessions connect in observe mode (`Interactive: false`, `Controller: false`).
- **No Input:** Fenced input frames (`Type: "input"`), binary keystroke frames, and takeover attempts are strictly rejected and dropped.
- **No Dual Ownership:** Shadow mirrors read from tmux `pipe-pane` only; the tmux server remains the sole process owner of the pane.
- **Non-Authoritative:** Pollers remain 100% authoritative for session state in `agentstore` and `terminalstore`. Classifier state transitions in shadow mode log discrepancies for observability but do not update authoritative store status.

---

## 4. Broker Protocol Compatibility & Supported Downgrade Boundaries

### Protocol Versioning (`ProtoVersion = 1`)
- The holder wire protocol embeds `ProtoVersion` in every handshake, request, and reply.
- The daemon client checks holder socket permissions (`0600`), peer UID (`SO_PEERCRED` / `LOCAL_PEERCRED`), 256-bit per-session credential, holder and child identity `(pid, start-time)`, and protocol version.

### Downgrade Boundary & Refusal Contract
- **Old binary connecting to new broker (`Proto < ProtoVersion`):**
  - The broker rejects the handshake with `protocol X unsupported (holder speaks Y)`.
  - The refusal is audited to `<session_id>.audit`.
  - The running shell process is **untouched and remains alive**.
- **New binary connecting to old broker (`Proto < ProtoVersion`):**
  - The client detects the protocol mismatch and returns `ErrProto`.
  - The client refuses adoption without terminating or corrupting the process.
- **Operational Downgrade Procedure:**
  - A downgraded warden binary (older than the broker protocol) **cannot adopt or control broker-hosted sessions**.
  - Before downgrading to a pre-plan-02 binary:
    1. Check for active broker-hosted terminals: `wd agent list --kind terminal`.
    2. Allow active sessions to drain naturally, or terminate them gracefully (`wd agent stop <id>`).
    3. Verify all holder processes have exited.
    4. Downgrade the binary.

---

## 5. Parity & Acceptance Budgets

Thresholds defined in `internal/terminalcontract.Default`:

- **Parity:**
  - `ParityByteExactPct = 100%`: Visible output in attached clients must match legacy tmux output byte-for-byte (`F-PAR-01`).
  - `ReplayOrderViolations = 0`: Replay and live streams maintain monotonic cursor ordering without duplicate or dropped events.
  - `StateAccuracyPct >= 98%`: AI session activity and permission prompt detection vs labelled ground truth (`F-PAR-02`).
  - `PromptFalseApprovals = 0`: Zero false auto-approvals.
  - `ShellClassifierEvents = 0`: Plain human shell sessions must produce zero classifier prompt events or state mutations.
- **Resource Limits (Idle Shell):**
  - RSS per session: ≤ 16 MiB.
  - File descriptors: ≤ 6.
  - Goroutines: ≤ 6.
  - Journal retention bound: ≤ 64 MiB.
- **Evidence Recording:**
  - Unit and integration tests (e.g. `F-PAR-01`, `F-PAR-02`, `F-RES-01`, `F-RES-02`, `F-PRIV-01`, `F-ROLL-04`) verify deterministic contractual bounds.
  - Real production deployment evidence must be recorded separately during stage bake periods and explicitly labelled as observed data.
  - **No automated deployment or fabricated evidence:** Operators must never synthesize or fake soak logs, and the system must never auto-promote stages without human validation.

---

## 6. Staged Deployment, Soak & Rollback Procedures

### Stage Advancement & Promotion Ladder

Promotion between stages requires satisfying strict numeric sample size and soak time thresholds across all active cohorts:

| Parameter | Threshold | Scope | Enforcement |
|---|---|---|---|
| `MinSessionsPerStage` | ≥ 20 sessions | Per cohort | Gate check prior to advancing stage |
| `MinSoakHoursPerStage` | ≥ 24 hours | Per cohort | Continuous soak without interruption |
| `MaxStageErrorRatePct` | < 1.0% | Stage-wide | **> 1.0% aborts stage immediately** |
| `MaxStageFallbackPct` | < 5.0% | Stage-wide | **> 5.0% aborts stage immediately** |

### Latency, Resource & Safety Acceptance Thresholds

Every promoted stage must demonstrate compliance with the objective bounds in `terminalcontract.Default`:

- **Latency (p99, idle loopback, 80×24):**
  - Keystroke echo: `KeystrokeEchoP99 ≤ 50 ms`
  - Attach to first byte: `AttachToFirstByteP99 ≤ 250 ms`
  - Window resize: `ResizeToSIGWINCHP99 ≤ 100 ms`
  - Adoption after recovery: `AdoptAfterRestoreP99 ≤ 2 s`
  - Controller lease takeover: `LeaseTakeoverP99 ≤ 200 ms`
- **Steady-State Resources (idle shell):**
  - Resident memory: `PerSessionRSSKiB ≤ 16 MiB` (16,384 KiB)
  - Open descriptors: `PerSessionFDs ≤ 6`
  - Runtime threads/goroutines: `PerSessionGoroutines ≤ 6`
  - Journal retention bound: `JournalMaxBytes ≤ 64 MiB`
  - Concurrency capacity: `SoakSessions = 50 concurrent` (`F-RES-01` clean)
- **Safety & Error Budgets (zero-tolerance):**
  - `SurvivalLossPct = 0%`: 0 lost shell/agent processes across daemon restarts/updates.
  - `DuplicateProcesses = 0`: 0 duplicate processes spawned during recovery.
  - `DuplicateInputBytes = 0`: 0 duplicate keystrokes across reconnects or controller handoffs.
  - `StaleControllerWrites = 0`: 0 writes accepted from stale lease generations.
  - `SecretLeaksInDurable = 0`: 0 secrets leaked to durable journals, audit logs, or snapshots (`F-PRIV-01`).
  - `ActionableErrorPct = 100%`: 100% of failure modes report clear cause and remediation steps.

### Metric Scraping & Observability

Operators must monitor live health via daemon Prometheus metrics:

```bash
# Query active PTY runtime metrics
curl -s http://127.0.0.1:8765/api/v1/metrics | grep pty

# Inspect PTY redaction activity
curl -s http://127.0.0.1:8765/api/v1/metrics | grep warden_pty_redactions_total
```

### Operational Rollback Procedure

If error rate exceeds `1.0%` or fallback exceeds `5.0%`:

1. **Cohort-scoped Rollback:**
   ```bash
   # Revert cohort to legacy tmux path for all future launches
   # (Active sessions remain undisturbed and authoritative)
   warden config set cohorts.<name>.runtime.pty off
   ```
2. **Global Emergency Rollback:**
   ```bash
   # Revert all new launches globally to tmux
   warden config set runtime.pty off
   ```
3. **Verify Rollback Ledger:**
   Confirm rollback event is recorded in daemon memory and logs:
   `WARN ptyhost: cohort rolled back (live sessions retain runtime) ...`

---

## 7. Legacy Session Inventory & Drain Tracking

### Inventory Inspection

Operators monitor active session presentation and runtime pinning using the CLI:

```bash
# List all active terminals and their runtime host
wd agent list --kind terminal

# Inspect detailed session JSON for runtime presentation (direct vs tmux)
wd agent list --json
```

### Supported vs. Retained Compatibility Matrix

| Session Kind | OS / Platform | PTY Runtime Status | Retained Legacy Path |
|---|---|---|---|
| Plain Shell (`kind: terminal`) | Linux, macOS | Direct PTY (`S2`–`S4`) | Tmux fallback supported via `runtime.pty: off` |
| AI Agent (`kind: agent`) | Linux, macOS | Observe Shadow (`S1`–`S4`) | Tmux authoritative host (required for Plan 02) |
| Non-PTY Platforms | Windows | Unsupported (`//go:build !linux && !darwin`) | Tmux / legacy execution must remain functional |
| Unmanaged Terminals | Any | Unsupported | External sessions remain untouched |
| Holder-Disabled (`WARDEN_PTY_HOLDER=off`) | Linux, macOS | Direct PTY (daemon-owned) | Process does not survive daemon SIGKILL |

### Natural Drain Protocol (`S4`)

- Under Stage `S4 (legacy-drain)`:
  - Existing tmux sessions finish naturally as human operators exit or tasks complete.
  - **No forced live conversions:** A running tmux pane is never live-converted into a PTY master (doing so causes process corruption).
  - **No code removal in Plan 02:** All legacy tmux execution paths, poller loops, and lifecycle hooks MUST be preserved intact throughout Plan 02.

---

## 8. Plan 03 Entry Gate Contract

Plan 03 (`legacy-runtime-retirement`) is **strictly gated** and may not begin until all prerequisites are fulfilled:

1. **Both Plans Merged & Released:**
   - Plan 01 (`01-pty-runtime-and-permission-decision-engine`, PR #979) is merged.
   - Plan 02 (`02-pty-plain-terminal-migration-and-rollout`) is merged to the main trunk and tagged in a production release.
2. **Staged Rollout Completion:**
   - Stages `S0` through `S3` completed with verifiable soak logs recorded by human operators.
   - `MinSessionsPerStage ≥ 20` and `MinSoakHoursPerStage ≥ 24` satisfied for every active cohort.
3. **Parity & Budget Verification:**
   - Byte-exact visible output parity verified (`ParityByteExactPct = 100%`).
   - Classifier parity verified (`StateAccuracyPct ≥ 98%`, 0 false approvals).
   - Zero resource leaks over 50-session soak (`F-RES-01`, `F-RES-02`).
   - Zero privacy leaks (`F-PRIV-01`).
4. **Natural Drain Evidence:**
   - Production telemetry confirms 100% of legacy tmux plain-terminal sessions have completed naturally.
   - Active tmux plain-terminal count is 0 in `S4`.
5. **Retained Compatibility Verified:**
   - Platform limitations (Windows) and AI agent execution models are ratified before any legacy code is retired.
6. **No Deletions in Plan 02:**
   - Zero legacy tmux code removed in this plan.


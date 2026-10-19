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
  - Unit and integration tests (e.g. `F-PAR-01`, `F-PAR-02`) verify deterministic contractual bounds.
  - Real production deployment evidence must be recorded separately during stage bake periods and explicitly labelled as observed data.

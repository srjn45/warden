# Operator Runbook: PTY Runtime Staged Rollout

Release and operator procedure for the warden-owned PTY runtime. It separates
three things that earlier revisions of this runbook blurred:

1. **Implemented and fixture-tested** capability (what the code can do),
2. **Operator-reachable** capability (what a running daemon will actually do for you), and
3. **Observed deployment evidence** (what a real cohort has measurably shown).

Only (3) can promote a stage, and **no observed evidence exists yet** (§9).

> **Status at the 02b-05 validation slice.**
> - **Plain-shell terminals (`kind=terminal`)** can be hosted directly with
>   `runtime.pty: on`. This is the only operator-reachable direct path.
> - **Direct AI-agent hosting is NOT shippable.** No AI adapter is direct-capable
>   (`ptyhost.IsAdapterDirectCapable` is true only for `terminal`/`shell`), and the
>   agent readiness guard (`Manager.AgentDirectReady`, default `false`) has no
>   production caller; only tests flip it. Every AI agent launches in tmux in every
>   mode, and `runtime.pty: on` never converts agents, including after an upgrade.
> - The agent-direct evidence in the repo is **fixture-only** (fake CLIs, loopback,
>   no live provider smoke). See
>   [`rollout/2026-10-25-02b-05-t1-adversarial-matrix.md`](rollout/2026-10-25-02b-05-t1-adversarial-matrix.md).
> - Nothing in this runbook deploys, promotes, restarts or tears down anything.
>   Every step below is an operator action, taken deliberately.
> - Completed plan status (`plan-48de9357`, `plan-23988b36`, 02b-01…05) and green
>   CI are **claims about code**, never proof of deployment, a cohort, a soak or a
>   legacy drain.

---

## 1. What each mode really does

`runtime.pty` (`off` | `shadow` | `on`, default `off`, hot-reloaded). A change
applies to sessions created **afterwards**; existing sessions keep the runtime
they were created with (durable per-session pin).

| Mode | AI agents | Plain-shell terminals | Journal / stream |
|---|---|---|---|
| `off` (default) | tmux | tmux | none |
| `shadow` | tmux; output mirrored read-only via `pipe-pane`; pollers stay authoritative | tmux, mirrored | yes, observe-only |
| `on` | **tmux (+ read-only mirror). Not direct. No operator opt-in exists.** | **direct** on a warden-owned PTY under an independent holder process | yes; interactive for direct terminals |

Verify, never assume: `wd inspect pty runtime <id>` (API
`GET /api/v1/sessions/{id}/runtime`, MCP `session_runtime`) returns
`actual_kind`, `process_owner`, `authoritative`, `evidence_source`,
`evidence_freshness`, `unsupported_reason`. `requested_mode` is informational
only. A tmux mirror is always `actual_kind: tmux`, `authoritative: false`.
`evidence_freshness: fresh` is the only liveness proof; `recorded` is not.

Capabilities that do **not** exist today (do not plan around them):

- Operator-configurable cohorts. `Manager.SetCohortMode` /
  `SetSessionCohort` / `SetSessionRuntimePin` are library functions with no
  config key, CLI command or API. There is no `cohorts.<name>.runtime.pty`
  setting. The only operator levers are the global `runtime.pty` and which
  sessions you create while it is set (§4).
- A direct-agent readiness gate or per-adapter capability declaration.
- Autopilot-owned direct agents (BLOCKED, #919) and graceful stop under
  autopilot load (BLOCKED, #918). See §8.
- A production direct cohort, soak, or parity data of any kind.

## 2. Rollout ladder

The numeric ladder lives in `internal/terminalcontract.Stages`. Stage names
describe the **target** design; the "Reachable today" column is the truth.

| Stage | Name | Target kinds | Reachable today |
|---|---|---|---|
| `S0` | off | agent, terminal | Yes (default). |
| `S1` | shadow | agent only | Yes. Read-only mirrors; never mutates status, never sends input. Terminals are excluded (a second owner of one PTY master cannot exist). |
| `S2` | opt-in direct cohort | agent, terminal | **Terminals only.** Direct agents: BLOCKED. |
| `S3` | default-new | agent, terminal | **Terminals only, and only after S2 evidence exists.** Direct agents: BLOCKED. |
| `S4` | legacy-drain | agent, terminal | Terminals only. Legacy tmux sessions are never converted or killed; they finish naturally. |

Agent rows of S2–S4 stay **BLOCKED** until a later, separately reviewed plan
adds the readiness gate and per-adapter capability and produces its own evidence
(including a live provider smoke that was explicitly authorized). Do not read the
`agent` column as a commitment that already works.

## 3. Release procedure

No step is automatic. Each needs a human, and each gate is evidence that must be
**observed on a running system**, not derived from tests.

### Step 0 — Shadow baseline (`S1`)
1. Confirm the installed binary and its source SHA: `wd version`; record both.
2. Run with `runtime.pty: shadow` (or `off`). Shadow is observe-only.
3. Record the baseline per session class: error rate, status accuracy of the
   existing pollers, resource use. This is the comparison point for later steps.
4. Exit criterion: baseline recorded (template A, §10). Shadow itself proves nothing
   about direct hosting.

### Step 1 — Operator-authorized small direct cohort (`S2`, terminals only)
1. The operator explicitly authorizes the cohort, scope and window in writing
   (template B). No agent or script may do this.
2. Prerequisite: the daemon runs a release containing plans 01, 02 and 02b-01…05,
   **deployed** (not merely merged). Verify from `wd version`, not from plan status.
3. `warden config set runtime.pty on`. This changes future launches only.
4. Create the cohort deliberately: only plain terminals you create now are direct.
   Existing terminals and every agent stay on tmux.
5. **Verify actual runtime per session** (mandatory, every cohort member):
   `wd inspect pty runtime <id>` must show `actual_kind: direct`,
   `process_owner` = the holder/daemon, `authoritative: true`,
   `evidence_freshness: fresh`. A session reporting `tmux` is not a cohort member
   and must not be counted.
6. Do not enroll an AI agent. There is no supported way to, and `unsupported_reason`
   will say so.

### Step 2 — Validate TUI / CLI / native workflows
For each cohort member, on each OS you intend to support (Linux has fixture
evidence; **macOS has none — NOT-RUN, compile/vet only; a darwin cohort needs its
own runtime evidence first**):
- `wd attach <id>`: Ctrl-C, EOF, job control, fullscreen program, resize,
  UTF-8/ANSI, detach with Ctrl-], `.`.
- Cockpit: the terminal pane and `a` full-screen attach (presentation tmux stays;
  the child is direct).
- `wd send`, `wd agent list`, `wd agent restore`, `wd inspect pty runtime`.
- API / CLI / MCP consistency of the runtime document.
- Web cockpit xterm against a direct session (**NOT-RUN in fixtures** — must be
  observed).
- Daemon restart and update (graceful, SIGKILL): same pid and start time, no
  relaunch, `lifecycle{recovered}` in the journal. Run these with **autopilot
  idle** (§8).

### Step 3 — Measured soak and parity (`S2` → `S3` gate)
Per cohort, all must hold **from observed data** (template C):

| Parameter | Threshold | Notes |
|---|---|---|
| Sessions | ≥ 20 (`MinSessionsPerStage`) | per cohort, counted by §3 step 1.5 verification |
| Soak | ≥ 24 h (`MinSoakHoursPerStage`) | continuous, per cohort |
| Stage error rate | < 1.0 % | ≥ 1.0 % aborts the stage |
| Stage fallback rate | < 5.0 % | ≥ 5.0 % aborts the stage |

The minimum 20 sessions / 24 h per cohort is unchanged by this runbook. Lowering it
needs an explicitly justified, reviewed change to
`internal/terminalcontract` and this document, not an operator judgment call.

Latency (p99, idle loopback, 80×24): keystroke echo ≤ 50 ms · attach→first byte
≤ 250 ms · resize→SIGWINCH ≤ 100 ms · adopt after recovery ≤ 2 s · lease takeover
≤ 200 ms.
Resources (idle shell, per session): RSS ≤ 16 MiB · FDs ≤ 6 · goroutines ≤ 6 ·
journal ≤ 64 MiB · 50 concurrent clean.
Zero-tolerance: survival loss 0 % · duplicate processes 0 · duplicate input bytes 0 ·
stale-controller writes 0 · secret leaks to durable stores 0 · actionable-error
rate 100 % · false auto-approvals 0 · classifier events from human shells 0.
Parity: byte-exact visible output vs legacy 100 % · replay order violations 0 ·
state accuracy ≥ 98 %.

Fixture results (the t1 matrix) show the code *can* meet these budgets. They are
not observed cohort data and never satisfy this step.

Monitor live: `curl -s http://127.0.0.1:8765/api/v1/metrics | grep warden_pty`
(use your bearer token), plus `wd inspect pty metrics`. Alerts and dashboard:
`deploy/observability/`.

### Step 4 — Expand new sessions (`S3`)
Only when Step 3 passed for every cohort and the operator signs off (template D):
leave `runtime.pty: on` as the default for **new plain terminals**. Existing
sessions keep their pin. Re-check Step 1.5 on a sample of new sessions.

### Step 5 — Natural legacy drain (`S4`)
Do nothing to existing tmux sessions. They end when their owners finish or exit.
Never live-convert a tmux pane and never terminate a session to speed this up.
Track inventory (§7) until the legacy plain-terminal count is 0 by attrition.

## 4. Per-session runtime pinning

- Every session records its runtime (`direct`, `tmux`, mirror) when created.
- Flipping `runtime.pty` affects future launches only; existing sessions keep their
  host, holder process and authority.
- A direct terminal keeps working after the global mode is set back to `off` or
  `shadow`; restart adoption of live direct sessions follows the durable pin, not
  the current global mode.
- Unknown or unsupported adapters fail with a typed unsupported result before any
  pane, holder or process is created. They never silently claim direct.

## 5. Rollback, downgrade and control recovery

### Rollback (future launches)
```bash
warden config set runtime.pty off      # or shadow
```
Effects: new sessions launch in tmux again. **Running direct terminals are not
touched** and remain controllable. Confirm with
`wd inspect pty runtime <id>`. There is no per-cohort rollback command; rollback
is global.

Abort conditions (stop and roll back immediately): stage error rate ≥ 1.0 %,
fallback ≥ 5.0 %, any lost or duplicated process, any stale-controller write, any
secret in a durable store, or an unexplained `orphaned` terminal.

### Control recovery of existing direct sessions
- Daemon restarted or crashed: the next daemon authenticates to the holder and
  re-adopts the same process. It never relaunches a live shell.
- Holder gone, stale identity, recycled pid or wrong credential: the terminal is
  shown `orphaned` with the cause in its journal. Nothing is relaunched.
  `wd agent restore <id>` explicitly starts a **fresh** shell (process state is
  not preserved).
- Controller lease stuck: `wd attach <id>` takes control (human precedence;
  stale generations are fenced).
- `WARDEN_PTY_HOLDER=off` hosts shells as daemon children (they die with the
  daemon). Do not use this in a cohort that has a survival requirement.
- Daemon stopped for a long time: output blocks at the kernel PTY buffer (no
  spool). A chatty child stalls until the daemon returns.

### Downgrade
The holder protocol is versioned (`ProtoVersion = 1`). A binary older than the
holder protocol **cannot adopt or control holder-hosted sessions** (refused with
`protocol X unsupported`; the process is left running). Before downgrading to a
pre-plan-02 binary:
1. List direct terminals: `wd agent list --kind terminal`.
2. Let them finish, or stop them deliberately: `wd agent stop <id>`.
3. Verify no holder process remains.
4. Only then install the older binary.

Never downgrade while direct sessions you need are running.

## 6. Observational shadow boundaries

Shadow sessions are observe-only (`Interactive: false`, `Controller: false`).
Input frames, binary keystrokes and takeover attempts are rejected. The tmux
server remains the sole owner of the pane, and the pollers remain authoritative.

## 7. Legacy inventory and compatibility

```bash
wd agent list --kind terminal     # terminals and their host
wd agent list --json              # per-session runtime document
```

| Session kind | Platform | Direct PTY | Retained path |
|---|---|---|---|
| Plain shell | Linux | Supported (fixture-verified; no observed cohort) | tmux via `runtime.pty: off` |
| Plain shell | macOS | Compiles and vets only; **runtime NOT-RUN** | tmux |
| AI agent (any adapter) | Linux, macOS | **BLOCKED. No adapter is direct-capable** | tmux is the process owner |
| Any | Windows | Unsupported (`!linux && !darwin`) | tmux / legacy |
| Unmanaged terminals | Any | Unsupported | untouched |
| Autopilot-owned agent | Any | BLOCKED (#919) | tmux |

Unsupported compatibility paths are **retained**, not scheduled for removal, until
the plan-03 gate (§11) passes.

## 8. Known issues and limits

| Item | Impact on this rollout |
|---|---|
| **#918** `Controller.mu` held across guardian I/O | Graceful daemon stop during autopilot activity can exceed the systemd timeout and become a SIGKILL. Run restart/update scenarios with autopilot idle and record stop duration. A stop > 30 s is reported as blocked-by-#918, never as a pass. |
| **#919** boot recovery builds run records before `PlanSource` is wired | Autopilot-owned direct-agent restart survival is unverified/BLOCKED. |
| Direct AI agents not operator-enableable | Rollout of direct agents BLOCKED (§1). |
| macOS runtime NOT-RUN | No darwin cohort without runtime evidence. |
| Busy AI CLI RSS (≤ 16 MiB budget) not measured with a real provider CLI | Needs authorized live smoke. |
| Web xterm against a direct session NOT-RUN | Observe in Step 2. |
| Emulator is a bounded VT subset, not xterm; resize is crop/pad | Replay and snapshot are `redacted-text`; only live frames after `head` are exact. |
| Secret detection is shape-based | Shapeless echoed secrets, base64-wrapped secrets (e.g. OSC 52) and secrets split across control sequences are not caught. |
| Resize bounds 1024×1024 / 256 Ki cells | Larger requests are refused; a legacy `/attach` client gets no message. |
| Viewer cap 16 per session | Over-cap attach returns HTTP 429. |

## 9. Evidence status (observed)

| Evidence | Status |
|---|---|
| Fixture matrix (Linux) | PASS at the t1 slice (automated; not observed deployment) |
| Shadow baseline on the installed daemon | **MISSING** |
| Operator-authorized direct terminal cohort | **MISSING** (none has ever existed) |
| ≥ 20 sessions × ≥ 24 h per cohort | **MISSING** |
| Error / fallback rates | **MISSING** |
| Parity and latency on a live system | **MISSING** |
| macOS runtime | **MISSING** |
| Direct AI agents (any evidence beyond fixtures) | **MISSING / BLOCKED** |
| Legacy drain | **MISSING** (nothing has begun draining) |
| Rollback verified on a live cohort | **MISSING** |

Operators must never synthesize soak logs. Nothing here auto-promotes.

## 10. Evidence templates

Fill these with observed values only. Leave a field `MISSING` rather than guess.
Store filled copies under `docs/rollout/` (one file per cohort and date); the
blank forms are in
[`rollout/2026-10-25-02b-05-evidence-templates.md`](rollout/2026-10-25-02b-05-evidence-templates.md).

## 11. Plan 03 entry gate (`plan-5cd3c58b`, legacy retirement)

Plan 03 must not start until **every** item below is satisfied from observed
evidence. Plan status and green CI count for none of them.

- [ ] Plans 01 (`plan-48de9357`), 02 (`plan-23988b36`) and 02b-01…05
      (`plan-8eb533f5`, `plan-06a9917e`, `plan-ce2a039b`, `plan-30011980`,
      `plan-e5fdd3fb`) are **merged, released and deployed**; deployed SHA/version
      recorded from `wd version` on the running daemon.
- [ ] Supported agents **and** shells are demonstrably direct: per-session
      `wd inspect pty runtime` shows `actual_kind: direct` for the supported set.
      (Today: shells only; agents are BLOCKED — this item cannot pass until the
      agent readiness gate and adapter capabilities exist and are proven.)
- [ ] Measured soak/parity met per cohort (≥ 20 sessions, ≥ 24 h, error < 1 %,
      fallback < 5 %, all §3 Step 3 budgets) with filed templates.
- [ ] Metrics healthy: `warden_pty_*` clean, zero-tolerance counters at 0.
- [ ] No supported workflow still needs tmux as the process owner.
- [ ] Legacy inventory drained naturally (count 0), with no forced conversion or
      termination.
- [ ] Unsupported compatibility retained (Windows, unmanaged terminals, any
      adapter not direct-capable) and ratified.
- [ ] Rollback/downgrade and control recovery verified on a live cohort.
- [ ] #918 / #919 resolved or their affected scenarios explicitly accepted.
- [ ] Plan 03 text references the corrective plan IDs above and requires a
      separately authorized deployment.

This runbook performs none of plan 03 and removes no code.

# Plain-terminal inventory and contract — plan 02 t1

- **Status:** contract + audit (t1 of plan `02-pty-plain-terminal-migration-and-rollout`, autopilot run `ap-56fa31d81791`). No runtime behavior changes. Machine-checked by `internal/terminalcontract` (data + tests) and `internal/stateclassifier/terminal_boundary_test.go`.
- **Date:** 2026-10-19
- **Entry gate:** plan 01 (`01-pty-runtime-and-permission-decision-engine`) is merged — PR #979 is `dae3cbd4` on `origin/main`. Audited at that commit. Plan 01 is **frozen**: nothing here modifies, stops or restarts it; its shortfalls are recorded as gaps below and **owned by plan 02 tasks**.
- **Consumers:** t2 lifecycle · t3 attach/recovery · t4 rollout/parity · t5 validation.
- **Rule of change:** this document, `internal/terminalcontract` and the fixtures change together. Every path/gap/fixture/stage ID and every budget field is back-ticked here and the test fails if one is missing; every code anchor is re-checked, so moving code without updating the matrix fails CI.

---

## 1. What exists today (audit of plan 01 at `dae3cbd4`)

| Layer | Package | What it actually is |
|---|---|---|
| Contract | `internal/ptyevent` | Types, redaction, `Arbitrate` (human > operator > warden input), `Step` state reducer, prompt/posture/TOCTOU. Pure. |
| Runtime | `internal/ptyruntime` | `Session`: in-**daemon** `pty.Open`, child with `Setsid+Setctty`, reader → `SplitUTF8` → `Redactor` → `ControlScanner` → `Screen` → append-only `Journal` → fan-out. `Mirror`: observe-only journal of a tmux pane via `pipe-pane`. |
| Wiring | `internal/ptyhost` | `Manager` + `runtime.pty: off\|shadow\|on` (default `off`, hot-reloaded, **applies to sessions created afterwards**). `routed` host sends only `kind=terminal` to the direct runtime in `on`; **agents stay in tmux** (mirrored). `/api/v1/sessions/{id}/pty` §6 framed transport (`interactive`/`observe`). |
| Consumers | `stateclassifier`, `promptengine`, `decisionrouter` | Observe **every** runtime-backed session; agent-record lookup decides what applies (see §5). |

**Which process holds the PTY master:** in `on` mode for a plain terminal, the **warden daemon process** (`ptyruntime.Session.master`, `session.go`). The shell is its child. When the daemon exits the master closes and the shell receives SIGHUP. Reconnect is **not** survival: startup recovery (`recoverPTY` → `RestoreTerminal`) starts a *new* shell on the old journal with a `crashed`/`spawned`+`reset` epoch. For tmux-hosted sessions (everything in `off`/`shadow`, every agent in `on`) the **tmux server** holds the master and survives the daemon; that is the only survival mechanism that exists today and it has no warden-owned PTY, authentication or fencing. Plan 01 §6 itself says "re-adopts live processes (compat: the tmux server keeps them)" — the *direct* half of that sentence is not implemented. This is `G1`/`G2`.

## 2. Coverage matrix — every managed plain-terminal path

"Runtime" = how the path reaches a shell: **host** = through `tmuxproc.Host` (routed to the direct PTY when `runtime.pty=on`, otherwise tmux); **tmux** = tmux only; **n/a** = metadata.

| ID | Surface | Op | Runtime | Preserves | Code |
|---|---|---|---|---|---|
| `P01` | CLI `wd agent start --kind terminal --dir` | create | host | cwd, kind | `internal/cli/lifecycle.go` |
| `P02` | API `POST /api/v1/spawn {kind:terminal}` | create | host | id, auto name, project, cwd, `$SHELL` | `strict_lifecycle.go` → `spawnTerminal` |
| `P03` | API legacy alias `backend=terminal` / `ai_cli=terminal` | create | host | same as `P02` (back-compat, must keep working) | `strict_lifecycle.go` |
| `P04` | MCP `spawn_agent {kind:terminal}` | create | host | same `SpawnRequest` as `P02` | `internal/mcp/server.go` |
| `P05` | TUI cockpit `t` | create | host | project of cursor row, row's dir as cwd | `internal/tui/cmds.go` |
| `P06` | Web cockpit `createTerminal(cwd)` | create | host | cwd; project by path-match | `web/src/lib/api.ts` |
| `P07` | automatic terminal spawn | — | n/a | **none exists**: "Terminals are never auto-spawned" | `internal/tui/control_pane.go` |
| `P08` | project open / clone / scaffold | — | n/a | **spawns no terminal** (D10: no auto-spawn) | `strict_projects.go` |
| `P09` | project close (hibernate) | stop | host | record kept `orphaned`; shell **killed** | `terminal_lifecycle.go` |
| `P10` | project reopen | restore | host | same id/workdir, **fresh shell** | `terminal_lifecycle.go` |
| `P11` | API `POST /sessions/{id}/restore` | restore | host | orphaned only (else 409) | `strict_lifecycle.go` |
| `P12` | daemon start `recoverPTY` | restore | host | running direct terminal → new shell on same journal | `pty_runtime.go` |
| `P13` | terminal watcher | restore | host | lost pane restored | `terminal_poller_deps.go` |
| `P14` | API `GET /attach` (raw WS) | attach | host | direct sessions bridged via `BridgeLegacy` | `attach.go` |
| `P15` | API `GET /sessions/{id}/pty` (§6) | attach | host | cursor/epoch replay; `interactive`/`observe` | `ptyhost/transport.go` |
| `P16` | TUI/cockpit terminal pane | attach | **tmux** | `tmux attach`; **unavailable for direct sessions** (`G9`) | `internal/tui/control_pane.go` |
| `P17` | API list `?kind=terminal`, SSE | list | n/a | `terminalstore` projected to Session DTO | `terminal_lifecycle.go` |
| `P18` | project tree | list | n/a | terminal node | `internal/tree/service.go` |
| `P19` | project membership edge `Project.Terminals[]` | create | n/a | authoritative; spawn rolls back if it cannot be recorded | `terminal_lifecycle.go` |
| `P20` | API terminate | stop | host | kill + delete record + drop membership | `terminal_lifecycle.go` |

Findings from the inventory:

1. **Single creation funnel.** Every create path (`P01`–`P06`) ends in `Server.spawnTerminal` → `Lifecycle.SpawnTerminal` → `RestoreTerminal` → `tmuxproc.Host.NewSession`. That is the one seam t2 must keep; no path needs separate conversion.
2. **No automatic creation exists** (`P07`, `P08`). Plan text "auto-spawn" therefore means: the contract forbids introducing one that bypasses `spawnTerminal`; t2 adds a test that fails if a second `NewSession`-for-terminal caller appears.
3. **Restore is reachable from four independent triggers** (`P10`–`P13`) with a check-then-act race (`G7`).
4. **Web cockpit** sends only `cwd`; the project comes from path-matching (`resolveTerminalProject`). It must not gain a path that skips project resolution.

### 2.1 Preserved identity (t2 must keep bit-for-bit)

| Attribute | Rule |
|---|---|
| id | `resolveID(req)`; tmux session name == terminal id; stable across restore |
| name | caller name, else `terminal-<shortID>`; `store.ValidateName`; **never** model-backed (`assignSpawnName` is agent-only) |
| project | explicit `project_id` must exist (reopened if hibernated, absolute path registered) else path-match/register from cwd else **400**; never project-less; membership edge rolled back on failure |
| cwd | `Terminal.Workdir`; restore refuses a missing dir (`ErrWorkdirMissing`) |
| shell | `Terminal.Shell=$SHELL` at spawn. Direct host: `$SHELL`, else `/bin/sh`. Tmux path: nested login shell via `TerminalBackend().LaunchCmd` + exit-code capture |
| environment | direct: daemon `os.Environ()` + `TERM=xterm-256color` + `WARDEN_SESSION_ID`/`AGENTCTL_SESSION_ID`; tmux: tmux server environment (**differs** — `F-LIFE-05` pins them equal) |
| persistence | `terminalstore` (ScrivaDB `terminals-db`) + per-session journal `<data>/pty/direct/<id>` |

### 2.2 Platforms

Linux and macOS only (`ptyruntime` is `//go:build linux || darwin`; `fifo_darwin.go` for the shadow FIFO). Windows has no PTY runtime and no terminals. Checked by `TestPlatformsMatchBuildTags`.

## 3. Session-kind boundary: shell vs AI

A **plain terminal** is `kind=terminal`: a human-controlled shell. An **AI session** is an `agentstore.Agent` created through `Lifecycle.Spawn`. The contract:

1. A shell's output is **never** sent to the state classifier's prompt path, `promptengine`, `decisionrouter`, Fast-Brain, brain consult or any permission/strategic classifier. Warden **never auto-approves, answers or types** into a shell on its own (`OriginWarden` input to a shell is forbidden; operator/human input only).
2. Password, `sudo`, `[y/N]`, "trust this folder"-shaped shell screens are the human's. Launching `claude`/`codex` **manually inside a shell** does not change the session's kind and does not enable automation; the session stays a terminal.
3. Kind is determined by **record type**, not by name, screen content or process tree. AI runtime decisions apply only to explicitly managed AI sessions.
4. Terminal records carry no AI fields (`terminalstore.Terminal`: id, project, name, tmux session, workdir, shell, pid, status, exit code).

**Today's enforcement is incidental (`G5`).** `Classifier.OnPTYEvent` is registered with `Manager.Observe` and sees *every* session; a shell is shielded only because `backendFor(name)` returns nil when no agent record exists, and `promptengine.handle` returns when `agents.Get` fails. `F-KIND-01` (`TestShellSessionNeverReachesPromptEngine`) seals that outcome now with a permissive resolver and four shell-shaped screens. t2 makes it **structural**: terminals are not registered with the classifier/prompt engine at all (kind-gated at `Observe` registration), keeps `F-KIND-02` (agent-name collision) and `F-KIND-03` (manual nested AI CLI), and adds a lint-style test that no `terminal` path imports the decision packages.

## 4. Broker lifecycle, adoption and compatibility

Plan 01 built no broker (`G1`). Plan 02 **reuses plan 01's journal, transport, arbitration and event model** and adds only the missing *owner of the PTY master* beneath them. Decision recorded for t3 (do not build a second event stack):

- **Holder.** A per-user `warden-ptyhost` broker process (same binary, re-exec'd with a hidden subcommand), started with `Setsid`, holding the master fds and children. It speaks the existing `ptyruntime.Session` API over a Unix socket; the daemon's `Manager` becomes a client. The daemon never holds the master of a broker-hosted session, so daemon exit/update/SIGKILL cannot HUP it.
- **Authentication (`G3`).** Socket mode `0600` in a `0700` dir; peer uid checked (`SO_PEERCRED` / `LOCAL_PEERCRED`); a per-session random credential issued at spawn and persisted `0600` beside the journal; adoption presents `(session id, credential, expected epoch)`. Identity of the child is `(pid, start-time)` — a recycled pid is never adopted (`F-AUTH-02`). Refusals are journaled and surfaced as actionable errors.
- **Re-adoption across failures.**
  - *Daemon restart/update/crash:* daemon reconnects, authenticates, `epoch+1 lifecycle{recovered}`, resumes the journal; **no relaunch, no kill** (`F-SURV-01`, `F-SURV-02`). If the broker is gone: sessions are `orphaned` with an error naming cause and next step — never silently relaunched (`F-SURV-03`).
  - *Detached clients:* nothing changes for the process; clients resume by cursor (§6 of plan 01).
  - *Idempotence:* recovery is one serialized path keyed by session id; the four restore triggers (`P10`–`P13`) funnel into it (`G7`, `F-LIFE-01`).
- **Agents (`G2`).** Agents are tmux-hosted; their process survival today is the tmux server. t3 either moves agent launch onto the broker or documents tmux as the explicit compat holder with the same adoption checks; either way `F-SURV-04` must pass, and "reconnect works" is never accepted as survival evidence.
- **Hibernate is not survival.** `P09`/`P10` (project close/reopen) deliberately terminate and relaunch (`G6`); it must stay labelled a relaunch and must never be used on a daemon-restart path.
- **Compatibility.** Live legacy (tmux) sessions are never converted or killed. A session's runtime is pinned at launch (`G11`); flipping `runtime.pty` affects future launches only. The broker handshake carries a protocol version; mismatched old/new refuse with a version error (`F-ROLL-02`). Downgrade boundary: a binary older than the broker protocol must not adopt; broker-hosted sessions then remain controllable only by a new-enough binary (documented in the t4 runbook).

### 4.1 Controller leases and input fencing (`G4`)

Plan 01 gives *origin* precedence (human > operator > warden) and a per-session `Lease` (holder, `HumanSeq`), but **no controller identity**: any number of interactive clients may write at once, and a client that reconnects after another took over, or after an epoch `reset`, is not fenced. Contract for t3:

1. Per session: many `observe` viewers, **one controller**. A controller holds a lease `{client_id, generation}`; `generation` increments on every takeover and on every epoch change.
2. Every input frame carries `(generation, input_id)`; the broker rejects stale generations (`F-FENCE-02`, budget `StaleControllerWrites`=0) and dedupes `input_id` across reconnects (`F-FENCE-03`, `DuplicateInputBytes`=0).
3. Takeover is explicit and journaled; the previous controller is demoted to `observe`, bounded by `LeaseTakeoverP99`. Resize follows the controller (last controller wins, SIGWINCH delivered; `F-ATTACH-03`).
4. Origin precedence from plan 01 is unchanged and reused (`F-FENCE-04`, already tested in `ptyevent`).

## 5. Gap register against plan 01

Verified at `dae3cbd4`. `Closed` flips only when the owner's fixtures exist (`TestGapsAndFixturesConsistent`). Open gaps carry a code anchor that proves the shortfall still exists.

| ID | Severity | Gap | Owner | Fixtures |
|---|---|---|---|---|
| `G1` | **release-critical** | Daemon holds the PTY master; direct shell dies with the daemon; recovery relaunches a fresh shell | t3 | `F-SURV-01` `F-SURV-02` `F-SURV-03` |
| `G2` | **release-critical** | Agents stay in tmux in `on`; mirrors are observe-only; no warden-owned PTY to adopt | t3 | `F-SURV-04` |
| `G3` | **release-critical** | No authenticated adoption (pid reuse, foreign process, other uid) | t3 | `F-AUTH-01` `F-AUTH-02` `F-AUTH-03` |
| `G4` | **release-critical** | No controller lease generation / input fencing; concurrent humans; stale clients can type | t3 | `F-FENCE-01` `F-FENCE-02` `F-FENCE-03` `F-ATTACH-03` |
| `G5` | **release-critical** | Shell/AI boundary is incidental (no agent record), not structural | t2 | `F-KIND-01` (sealed now) `F-KIND-02` `F-KIND-03` |
| `G6` | **release-critical** | Hibernate kills + relaunches; must stay explicit and off the restart path | t2 | `F-LIFE-03` |
| `G7` | **release-critical** | Restore is check-then-act across four triggers; double process possible | t2 | `F-LIFE-01` `F-LIFE-02` |
| `G8` | hardening | `Screen` is a minimal VT: 1 cell/rune (wide/combining wrong), attributes dropped, simplified scroll regions — snapshots lossy for full-screen TUIs (live raw bytes unaffected) | t3 | `F-VT-01` `F-VT-02` `F-VT-03` |
| `G9` | hardening | Cockpit/TUI attach is `tmux attach`; a direct terminal can't be shown | t3 | `F-ATTACH-01` |
| `G10` | hardening | Legacy `/attach` on a direct session is a raw bridge without §6 resync | t3 | `F-ATTACH-02` |
| `G11` | hardening | Global mode switch; no per-session runtime pin, cohort record or broker protocol version | t4 | `F-ROLL-01` `F-ROLL-02` |
| `G12` | hardening | Direct shell exit code journaled but `terminalstore.ExitCode` set only by the tmux exit-file path | t2 | `F-LIFE-04` |

**Recording in plan 01:** plan 01's canonical record is ScrivaDB state owned by its active run and is deliberately **not edited** (constraint: do not modify, stop or restart plan 01). The gaps are recorded here, in `internal/terminalcontract.Gaps`, and as plan 02 task ownership; the plan 01 run's own review can cite this file. Release-critical gaps are covered by **plan 02 tasks**, not assumed solved.

**Incremental VT interpretation.** Where plan 01 interprets the screen incrementally (`ptyruntime.Screen`), it is a hand-written minimal parser fed *redacted* bytes; it exists only to produce snapshots/`CapturePane`. It is not authoritative for rendering — attached clients render the raw byte stream — but resync after `reset`/cursor loss uses it, hence `G8`. t3 must either upgrade it to cover the `F-VT-*` traces or restrict snapshots to what it can represent and say so.

## 6. Privacy and retention boundaries

- **Local authorized view is faithful.** Live raw frames and the in-memory screen are not redacted; an authorized local operator sees what the program wrote.
- **Redaction from the journal outward.** The journal, snapshots, SSE/cockpit state, audit, diagnostics, crash excerpts and anything off-node are redacted (`Redactor`, input `RedactInput` with live termios `ECHO`). Typed no-echo input never reaches the journal. Input bytes must not be logged anywhere (audit records kinds/ids, not content; `F-PRIV-01` verifies). Retention is bounded per session (`JournalMaxBytes`, whole-segment trimming).
- **Unavoidable limits** (`SecretDetectionLimits`, asserted non-empty by test): echoed secrets with no recognizable shape; secrets split across redraws/control sequences; programs that echo no-echo input back themselves; the faithful local view by design.
- Hub/WebSocket remote infrastructure stays deferred; remote-safe views are the redacted ones.

## 7. Rollout ladder, budgets and fixtures

Stages (`TestStagesShadowIsAgentOnlyAndNonAuthoritative`):

| ID | Stage | Kinds | Authoritative | Entry |
|---|---|---|---|---|
| `S0` | off | agent, terminal | no | default today |
| `S1` | shadow | **agent only** | no | observe-only; never mutates status, never writes input. No terminal shadow: a second owner of one PTY master cannot exist |
| `S2` | opt-in | agent, terminal | yes | per-cohort `runtime.pty=on`; survival gate `G1`–`G4` green |
| `S3` | default-new | agent, terminal | yes | every budget met over `MinSessionsPerStage` × `MinSoakHoursPerStage` |
| `S4` | legacy-drain | agent, terminal | yes | legacy sessions finish naturally; plan 03 entry gate |

Cohort rollback changes **future launches only**; live PTY sessions keep their runtime and control (no unsafe live conversion).

### 7.1 Budgets (`terminalcontract.Default`; pass/fail on the fixtures)

Values are generous round numbers — they fire on real regressions, not noise. "Observed" production numbers are recorded separately by t5 and always labelled observed; a green unit test is never deployment evidence.

| Group | Budget | Bound |
|---|---|---|
| Parity | `ParityByteExactPct` | 100 (visible bytes tmux vs direct) |
| | `ReplayOrderViolations` | 0 (gaps/dups/reorders) |
| | `StateAccuracyPct` | ≥ 98 (AI sessions vs labelled corpus) |
| | `PromptFalseApprovals` | 0 |
| | `ShellClassifierEvents` | 0 |
| Latency (p99, loopback, idle, 80×24) | `KeystrokeEchoP99` | ≤ 50 ms |
| | `AttachToFirstByteP99` | ≤ 250 ms |
| | `ResizeToSIGWINCHP99` | ≤ 100 ms |
| | `AdoptAfterRestoreP99` | ≤ 2 s |
| | `LeaseTakeoverP99` | ≤ 200 ms |
| Resource (idle shell, steady) | `PerSessionRSSKiB` | ≤ 16 MiB |
| | `PerSessionFDs` | ≤ 6 |
| | `PerSessionGoroutines` | ≤ 6 |
| | `JournalMaxBytes` | ≤ 64 MiB |
| | `SoakSessions` | 50 concurrent |
| | `SlowViewerMaxBytes` | ≤ 4 MiB pinned, then `ErrResync` |
| | `SlowViewerPTYStall` | ≤ 10 ms PTY read delay |
| Error / survival | `SurvivalLossPct` | 0 |
| | `DuplicateProcesses` | 0 |
| | `DuplicateInputBytes` | 0 |
| | `StaleControllerWrites` | 0 |
| | `SecretLeaksInDurable` | 0 |
| | `ActionableErrorPct` | 100 (cause + next step) |
| | `RecoveryBound` | ≤ 30 s then fail loudly |
| Rollout gates | `MinSessionsPerStage` | ≥ 20 |
| | `MinSoakHoursPerStage` | ≥ 24 |
| | `MaxStageErrorRatePct` | > 1 aborts the stage |
| | `MaxStageFallbackPct` | > 5 aborts the stage |

### 7.2 Deterministic fixture matrix

Deterministic = injected clocks / event waits (no sleep-for-correctness), fixed seeds, loopback only, no external CLI or network; fake CLIs live in `ptyruntime/fakecli_test.go`. Rows marked ✔ exist now; the rest are written by their owner before the gate stage opens.

| Fixture | Class | Kinds | Asserts | Owner | Gate | |
|---|---|---|---|---|---|---|
| `F-SURV-01` | survival | terminal | SIGKILL daemon: same shell pid, child alive, re-adopted, scrollback intact | t3 | `S2` | |
| `F-SURV-02` | survival | terminal | graceful stop + binary swap: no relaunch/duplicate | t3 | `S2` | |
| `F-SURV-03` | survival | both | broker death: actionable error, never silent relaunch | t3 | `S2` | |
| `F-SURV-04` | survival | agent | agent tree survives daemon SIGKILL | t3 | `S2` | |
| `F-AUTH-01` | auth | both | credential required; refusal journaled | t3 | `S2` | |
| `F-AUTH-02` | auth | both | recycled pid not adopted | t3 | `S2` | |
| `F-AUTH-03` | auth | both | socket 0600, peer-uid checked | t3 | `S2` | |
| `F-FENCE-01` | fence | both | one controller; takeover bumps generation | t3 | `S2` | |
| `F-FENCE-02` | fence | both | stale-generation write rejected | t3 | `S2` | |
| `F-FENCE-03` | fence | both | reconnect never duplicates input | t3 | `S2` | |
| `F-FENCE-04` | fence | both | human > operator > warden | plan 01 | `S2` | ✔ |
| `F-KIND-01` | kind | terminal | shell screens never reach prompt engine | t1 | `S2` | ✔ |
| `F-KIND-02` | kind | terminal | name collision with an agent still no prompt | t2 | `S2` | |
| `F-KIND-03` | kind | terminal | manual nested AI CLI: no automation | t2 | `S2` | |
| `F-LIFE-01` | lifecycle | terminal | concurrent restore ×3 → one process | t2 | `S2` | |
| `F-LIFE-02` | lifecycle | terminal | spawn rollback leaves nothing | t2 | `S2` | |
| `F-LIFE-03` | lifecycle | terminal | hibernate→reopen keeps identity, labelled relaunch | t2 | `S2` | |
| `F-LIFE-04` | lifecycle | terminal | `exit 3` → ExitCode 3 | t2 | `S2` | |
| `F-LIFE-05` | lifecycle | terminal | id/name/project/cwd/shell/env identical tmux vs direct | t2 | `S2` | |
| `F-VT-01` | vt | both | alt-screen traces round-trip | t3 | `S2` | |
| `F-VT-02` | vt | both | wide/combining/emoji cells | t3 | `S2` | |
| `F-VT-03` | vt | both | SGR kept locally, dropped in redacted views | t3 | `S2` | |
| `F-ATTACH-01` | attach | terminal | cockpit drives a direct terminal; raw mode restored; Ctrl-C/EOF/job control | t3 | `S2` | |
| `F-ATTACH-02` | attach | both | replay→live order across epoch reset; slow viewer bounded | t3 | `S2` | |
| `F-ATTACH-03` | attach | both | resize arbitration, SIGWINCH | t3 | `S2` | |
| `F-ROLL-01` | rollout | both | per-session runtime pin | t4 | `S3` | |
| `F-ROLL-02` | rollout | both | broker protocol handshake refusal | t4 | `S3` | |
| `F-ROLL-03` | rollout | both | shadow mutates nothing, sends nothing | t4 | `S1` | |
| `F-ROLL-04` | rollout | both | mixed legacy+PTY fleet fully operable | t5 | `S3` | |
| `F-PAR-01` | parity | both | golden byte stream tmux vs direct | t4 | `S3` | |
| `F-PAR-02` | parity | agent | labelled AI prompt corpus accuracy | t4 | `S3` | |
| `F-RES-01` | resource | both | 50-session soak, `-race`, no leak | t5 | `S3` | |
| `F-RES-02` | resource | both | journal bound under 10× flood | t5 | `S3` | |
| `F-PRIV-01` | privacy | both | planted secrets absent from durable/remote views | t5 | `S3` | |

## 8. Hand-off per task

- **t2:** make `G5`/`G6`/`G7`/`G12` structural; keep `spawnTerminal` as the only funnel (`P01`–`P06`); serialize restore; record direct exit codes; equalize shell/env (`F-LIFE-05`); keep legacy tmux terminals untouched.
- **t3:** build the broker holder + authenticated adoption + lease generation/input fencing (`G1`–`G4`) reusing plan 01's journal/transport/arbitration; fix `G8`–`G10`.
- **t4:** per-session runtime pin + cohort record + protocol versioning (`G11`); apply §7.1; parity artifacts.
- **t5:** run the whole matrix, soak, privacy audit, runbook; gate plan 03 on both plans merged **and** deployed with drain evidence. Nothing here authorizes cleanup or deploy.

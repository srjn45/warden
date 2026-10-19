# PTY 02b — acceptance audit and runtime identity / attach contract

- **Status:** binding audit + contract. **Docs only** — this task changes no production code, enables nothing, restarts nothing, edits no plan history. Task `audit-and-contract` of plan `02b-01-pty-runtime-identity-and-attach-foundations` (`plan-8eb533f5`), autopilot run `ap-29bd233fd90d`.
- **Date:** 2026-10-19
- **Audited baseline:** `origin/main` @ **`0d531421`** (`feat(terminal): release validation, mixed fleet, resource soak, and operator runbook (plan 02 t5) (#985) (#987)`), release **v11.5.0**. The installed binary (`warden version`) reports `warden 11.5.0 / Commit: 0d531421 / Built 2026-10-19T15:50:02Z` — installed release and source baseline are the same commit. The operator's `~/.warden/config.yaml` has `runtime.pty: "shadow"` (so even the installed daemon is not in `on`).
- **Audited against:** plan 01 `plan-48de9357` (`01-pty-runtime-and-permission-decision-engine`, status `completed`), plan 02 `plan-23988b36` (`02-pty-plain-terminal-migration-and-rollout`, status `completed`), the superseded corrective plan `plan-001ead2b` (archived; replaced by series 02b-01 `plan-8eb533f5` → 02b-02 `plan-06a9917e` → 02b-03 `plan-ce2a039b` → 02b-04 `plan-30011980` → 02b-05 `plan-e5fdd3fb`), and the in-repo specs [`2026-10-19-pty-runtime-contract.md`](2026-10-19-pty-runtime-contract.md), [`2026-10-19-plain-terminal-inventory-and-contract.md`](2026-10-19-plain-terminal-inventory-and-contract.md), [`../RUNBOOK-pty-rollout.md`](../RUNBOOK-pty-rollout.md).
- **Method:** "completed" flags on plans 01/02 are *claims*. Every row below was re-derived from source at the baseline SHA (file:line), then cross-checked against the tests that exist. Nothing here was run against a live provider; "observed" below means observed in source or in the installed binary/config, never production soak data (none exists — see §9).
- **Rule of change:** later 02b plans that change a fact in §2–§4 update this file in the same PR (a stale audit row is a bug).

**Ownership key** — every gap names exactly one owning plan: **02b-01** identity + attach foundations (this series' first plan) · **02b-02** UI (TUI/CLI/Cockpit) · **02b-03** agent launch + recovery · **02b-04** authority (state/permission) + readiness · **02b-05** final evidence + rollout. **No gap is deferred to plan 03** (`plan-5cd3c58b`, legacy retirement); plan 03 only starts after §9's gates.

---

## 1. Verdict

1. **The baseline claim is confirmed.** v11.5.0 sends only plain terminals to the direct runtime: `routed.NewSession` takes the direct branch only when `r.terminal && mode == ModeOn` (`internal/ptyhost/manager.go:612`). Every AI agent still launches into a tmux session and is at most *mirrored* (`pipe-pane`, observe-only) in `shadow`/`on` (`manager.go:625-637`, `shadow.go:121`). The daemon wires the agent host as `ptyMgr.Host()` (non-terminal `routed`) and only plain terminals as `ptyMgr.TerminalHost()` (`internal/cli/daemon.go:230-231`).
2. **Plans 01/02 delivered real, reusable components** — event contract, journal, classifier, prompt engine, posture engine, decision router, holder with authenticated adoption, lease/fencing transport, local attach client — but **delivered them end-to-end only for `kind=terminal`**. For AI agents the "PTY runtime" is a read-only mirror of a tmux pane; the classifier, prompt engine and posture engine run on it **dry-run / observe-only** (`stateclassifier/classifier.go:438`, `promptengine/engine.go:237`). Plan 01's headline goal ("Warden must launch native interactive CLIs through it") is **not met**.
3. **Plan 02's gap register over-closes G2.** `internal/terminalcontract/gaps.go:37-38` marks `G2` ("agents stay in tmux; no warden-owned PTY to adopt") *closed* because t3 documented tmux as the "tmux-compat holder" and added a pane-identity check (`F-SURV-04`). That is a documented alternative, **not** agent PTY ownership, and it contradicts the corrective plan's done-when #2 and #4. This audit **reopens it** under 02b-03 (§4, row `RC2`/`RC4`).
4. **Runtime identity does not exist as a concept.** Runtime is inferred from names and side effects: persisted `TmuxSession` is both the tmux session name *and* the key of the direct journal/holder (`daemon/pty_runtime.go:121-124`), and `PTYRuntimeView.Mode` reports the *global requested* mode, not what the session actually runs on (`daemon/strict_ptyobs.go:55`). Per-session pins and cohorts are in-memory only (`ptyhost/manager.go:234-285`) and `SetSessionCohort`/`SetCohortMode` have **no production caller**. This is the 02b-01 core work (§6).
5. **Reusable foundations are strong:** `ptyholder.Launcher.Launch` already takes a generic `argv/cwd/env` (`ptyholder/launcher.go:25`); the §6 transport, lease fencing and `ptyattach.Run` client already exist (`ptyhost/transport.go`, `ptyattach/attach.go:104`). The shortfall for agents is wiring and contract, not a new broker. **No new third-party dependency is needed** (§6.8).

---

## 2. Confirmed code facts (current behavior — not intended behavior)

Every item cites source at `0d531421`. Intended architecture is kept strictly in §6.

### 2.1 `routed.NewSession` terminal-only direct branch

| Fact | Evidence |
|---|---|
| Two `routed` hosts exist: `Host()` (`terminal:false`) and `TerminalHost()` (`terminal:true`). | `ptyhost/manager.go:362`, `:366` |
| The direct branch is `if r.terminal && mode == ModeOn`. Agents (`terminal:false`) can never reach it regardless of mode. | `ptyhost/manager.go:612-621` |
| Everything else goes to tmux; in `shadow`/`on` the new pane is mirrored best-effort (`mode.mirrors()`), and a mirror failure never fails the launch. | `manager.go:630-637`, `mode.go:36` |
| The agent host is wired as `lc.SetProcessHost(ptyMgr.Host())`; the terminal host as `lc.SetTerminalHost(ptyMgr.TerminalHost())`. | `internal/cli/daemon.go:230-231` |
| `newAgentSession` → `Proc().NewSession` is the single agent session-creation seam, used by free-form spawn, typed spawn, job spawn, resume, hot-swap and role-switch. | `lifecycle/lifecycle.go:2303-2306`; callers `:2100`, `:2185`, `:2392`, `:3250`, `switch.go:472` |
| The runtime **pin** is written at every `NewSession` (`SetSessionRuntimePin(name, effectiveMode)`), including for agents in `off`/`shadow`; a same-name reuse overwrites it. Pins live only in memory. | `manager.go:606`, `:234-241`, `:337-349` |

### 2.2 Agent launch is a shell line typed into tmux, bypassing the host seam

| Fact | Evidence |
|---|---|
| `Backend.LaunchCmd`/`ResumeCmd` return *strings typed into a tmux pane and executed by a shell*; adapters shell-quote embedded values. There is no argv form. | `agentbackend/backend.go:149-156` (interface comment + `LaunchCmd`) |
| Launch is delivered with raw `tmux send-keys -t <id> <line> Enter` via `l.run.Run`, **not** through `tmuxproc.Host`: free-form `:2136`, typed `:2242`, job `:3294`, resume `:2396`, hot-swap/successor `switch.go:507`. | `lifecycle/lifecycle.go`, `lifecycle/switch.go` |
| The launch line appends an exit-capture suffix `; printf '%s' "$?" > <exits>/<id>`, consumed by the poller. A direct PTY has its own wait status, so this suffix would be wrong there. | `lifecycle.go:2987-2998` (`exitSuffix`), `:3086` (`ReadExit`) |
| Prompt seeding waits for a pane marker by polling `tmux capture-pane`, then pastes via `tmux load-buffer/paste-buffer`. | `lifecycle.go:2839-2940` (`seedInteractivePrompt`, `deliverSeed`, `waitPaneReady` `:2924`), `:2742`, `:2785` |
| Operator I/O primitives are raw tmux too: `Input` `:2733`, `SendKeys` `:2795`, `Output` (`capture-pane -S`) `:2946`. The daemon routes a *direct* session first (`strict_lifecycle.go:555`, `:580`) and falls back to these. | `lifecycle.go`, `daemon/strict_lifecycle.go` |
| `Restore` refuses on `tmux has-session` (not the host), so it cannot know about a direct agent. `Adopt` live-register uses `tmux has-session`/`rename-session`. `Terminate` and `RemoveWorktree` probe `tmux has-session`. | `lifecycle.go:2439`, `:2592-2596`, `:2682` |
| The holder's `Launch(argv, cwd, env, cols, rows)` is generic and `ptyruntime.Host.NewSession` hard-wires `argv` to the *shell* (`h.shell()`), i.e. there is **no agent argv entrypoint** into the runtime yet. | `ptyholder/launcher.go:25`, `ptyruntime/host.go:64-75`, `ptyholder/holder.go:78-80` |

### 2.3 `IsAuthoritative` guards

`Manager.IsAuthoritative(name)` is true **iff a direct session exists for the name** (`ptyhost/manager.go:386-394`). Since only terminals are ever direct, it is true for no AI agent. Its three consumers:

| Consumer | Effect today | Evidence |
|---|---|---|
| Legacy poller | Skips pane capture/classification only for authoritative sessions → never skips an agent. | `poller/poller.go:1129` (wired `daemon/pty_runtime.go:35`) |
| State classifier | Writes `store.UpdateStatusIf` only if authoritative; in `shadow` it merely logs a disagreement. Hence for agents it never owns status. | `stateclassifier/classifier.go:438-450` |
| Prompt engine | `DryRun: !Authoritative(name)` → for every mirrored agent the engine decides and traces but **never writes input**. | `promptengine/engine.go:237`, `daemon/prompt_engine.go:94` |

Consequence: the pollers remain the only actors that actually approve prompts and set status for agents, exactly as before plan 01. `ModeOn` for an agent therefore changes only *observation*, never *authority*.

### 2.4 `recoverPTY` agent exclusion

- `recoverPTY` collects agent names (`liveStatus && TmuxSession != ""`, `daemon/pty_runtime.go:94-100`) **only** to feed `RecoverShadow`, which re-mirrors live tmux panes after `VerifyPane` identity check (`ptyhost/manager.go:434-460`).
- The direct recovery loop (`pty_runtime.go:120-141`) iterates `terms` (terminalstore) only: holder adoption (`:124-129`) or a labelled fresh-shell relaunch (`:131-140`). There is **no code path that adopts a direct AI agent**, because none can exist.
- It is also gated by `s.pty.Mode() == ModeOff → return` (`:90`) and `Mode() != ModeOn → return` (`:117`): with the **global mode flipped to `off`, a still-live direct terminal is not adopted at boot**, contradicting plan 02's "rollback keeps control of live direct sessions". (The pin keeps `IsAuthoritative` true only while the in-memory `direct` map has the session; after a daemon restart with global `off` nothing re-adopts it.) → owner 02b-03 (§4 `RC4`).

### 2.5 promptengine / poller decisions (who may type into an agent)

| Actor | Condition | Result | Evidence |
|---|---|---|---|
| Prompt engine | agent record exists; `Act = AutoApprove || AutopilotRunID != ""`; `DryRun = !IsAuthoritative` | For tmux-mirrored agents: dry-run (trace only). Writes would go via `TmuxWriter{Host: Host(), Name}` if ever non-dry. | `promptengine/engine.go:232-237`, `daemon/prompt_engine.go:68-72` |
| Legacy poller | not authoritative | Captures pane, classifies, `tryAutoApprove`, `tryTrustPrompt`, `tryLimitMenu`, seeds, resumes — all via tmux `send-keys`. | `poller/poller.go:1129+`, `poller/promptchain.go` |
| Shell sessions | `ObserveAgents` filter + durable `terminalstore` resolver | Never reach classifier/prompt engine (structural since plan 02 t2). | `ptyhost/manager.go:180`, `:189`, `stateclassifier/classifier.go:81` |

Two writers can never both be live for an agent today only because one of them (the engine) is permanently dry-run. The moment 02b-04 makes the engine authoritative for a direct agent, the poller must be *structurally* excluded for that session, not just by a map lookup that is false for agents (§4 `RC5`).

### 2.6 TUI / CLI / web attach assumptions

| Surface | Assumption | Evidence |
|---|---|---|
| CLI `warden attach` | Native §6 attach only `if sess.IsTerminal()`; **every agent falls through to `tmux attach -t <TmuxSession>`**. | `cli/lifecycle.go:803-824` |
| TUI agent pane | `respawn-pane … "env -u TMUX tmux attach -t <agentSession>"`; standalone switch uses `tmux switch-client -t`. | `tui/control_pane.go:2810-2813`, `:2798`, `:2828-2840` |
| TUI terminal pane | `respawn-pane … warden attach <id>` (correct for direct terminals; tmux fallback inside `attach`). | `tui/control_pane.go:2820-2823`, `:2846-2860` |
| TUI "is this live/openable" | `liveStatus(status) && TmuxSession != ""`. A non-empty name is taken as proof that a tmux session exists. | `tui/control_pane.go:2882`, `:675`, `:602`, `:523`, `:559`, `:2141` |
| TUI terminal title/cwd/liveness | `tmux display-message #{pane_current_path}` / `#{pane_dead}` — returns "" for a direct terminal, so the stored name goes stale instead of reporting "unavailable". | `tui/cmds.go:165-185` |
| TUI cockpit shell | wholly tmux (`compositor.go`, `tmuxnative.go`, `bind-key`, `remain-on-exit`) — **presentation** only, allowed to stay. | `tui/compositor.go:62-337`, `tui/tmuxnative.go:85-92` |
| Daemon `GET /attach` (raw WS) | Direct session → `BridgeLegacy`; else `tmux set-option mouse on` + `bridgeTmux` (spawns `tmux attach-session`). | `daemon/attach.go:89-109`, `:152-164` |
| Daemon `GET /sessions/{id}/pty` (§6) | Looks up by `sess.TmuxSession`; for a mirrored agent it serves an **observe-only** stream (no `Direct`); 409 if no journal. Read-only token → `t.Direct=nil`. | `daemon/pty_runtime.go:230-262` |
| Web cockpit / xterm | uses `/attach` (raw bridge) and `/cockpit` (the literal TUI under tmux) | `daemon/attach.go:112+` |

### 2.7 Persisted `TmuxSession` semantics — who infers runtime from it

`TmuxSession` is persisted on both `agentstore.Agent` (`agentstore/types.go:36`) and `terminalstore.Terminal` (`terminalstore/store.go:48`) and serialized as `tmux_session` in the public `Session` DTO. For warden-spawned sessions it equals the record ID (`terminal_lifecycle.go:151` sets it to the terminal ID). The audit found it used as **three different things**:

1. *tmux session name* — `tmux has-session/capture-pane/send-keys -t` (`lifecycle.go:1794`, `:2439`, `:2796`, `poller_deps.go:55-60`, `attach.go:48`, `cli/lifecycle.go:821`).
2. *Runtime lookup key* — journal directory `…/pty/direct/<name>` and holder id, and `Manager.Lookup/Direct/HolderHosted(name)` (`ptyhost/manager.go:369`, `:407`, `daemon/pty_runtime.go:121-124`, `strict_ptyobs.go:60-62`).
3. *Liveness/openability proxy* — "non-empty ⇒ there is a tmux session I can attach/switch to" (`tui/control_pane.go:2882` et al.; `cli/repair.go:290` builds the "known tmux" set from it; `daemon/recover.go:94` treats empty as "no process record").

For a direct session meaning (1) is **false** while meanings (2) and (3) are used. Nothing today can tell a consumer which of the three applies; the answer is derived by probing the live process tables (`Lookup`, `tmux has-session`). **No persisted field says "this session is direct/tmux/holder-hosted"**; the only durable evidence is the existence of `<data>/pty/direct/<name>` (`HasDirectJournal`, `manager.go:399`) and a holder record (`ptyholder.Exists`). `store.Session.Kind` (`store/types.go:351`) separates agent vs terminal, not runtime. Identity fields that *do* exist and must be preserved: `Agent.ID`, `Agent.AICLISessionID` (conversation/resume id, legacy `claude_session_id`), `Agent.PID`, `Agent.Name`.

### 2.8 Other confirmed facts feeding the contract

- **Mode hot reload** applies to *new* sessions only (`daemon/reconfigure.go:92-95` → `Manager.SetMode`, `manager.go:146-163`); turning `off` stops mirrors but leaves direct terminals running.
- **Pins/cohorts** are three in-memory maps (`pins`, `cohorts`, `cohortOf`, `manager.go:123-124`); not persisted; `SetSessionCohort`/`SetCohortMode`/`CohortMode` have no non-test caller (grep of `internal`, `cmd`). A daemon restart forgets every pin; `AdoptDirect` re-pins `ModeOn` (`manager.go:416`).
- **Holder does not drain the master.** The holder only holds the master and serves adoption (`ptyholder/holder.go:90-140`); all reads happen in the daemon's dup. While no daemon is attached the kernel PTY buffer fills and **the child blocks on write** — documented as "normal terminal back-pressure" (`plain-terminal-inventory-and-contract.md` §5.2). For a plain shell that is acceptable; for an AI CLI that is mid-turn it freezes the agent during any daemon outage. Undefined today: bounded loss vs blocking policy for agents (§6.7, owner 02b-03).
- **Screen model keeps only alt-screen private modes** (`?47/1047/1049`, `ptyruntime/screen.go:477-482`); cursor visibility (DECTCEM `?25`), bracketed paste (`?2004`), mouse (`?1000-1006`), application cursor keys (`?1`), focus reporting (`?1004`) are **not tracked**, and `Screen.String()` (used for journal snapshots) is plain text from *redacted* bytes. A snapshot therefore cannot reproduce cursor/modes (§6.6). Re-adoption relies on `nudgeRedraw` (SIGWINCH to the foreground group) to repaint (`ptyruntime/control.go:152-162`).
- **Attach protocol (§6)** already provides: viewer vs controller roles, `takeover`, generation fencing, `input_id` dedupe, `ack` cursors, `plan` (tail/replay/snapshot), `demoted`/`rejected`/`resync` frames (`ptyhost/transport.go:23-119`); detach chord Ctrl-] (`ptyattach/attach.go:24`); `ErrNotDirect` for mirrored sessions (`attach.go:28`).
- **Adapters with PTY prompt fixtures:** only Claude and Antigravity (`ptyevent/testdata/prompts/claude_*.json`, `agy_trust_*.json`); codex, cursor, aider, opencode, crush, goose have posture audits (`agentbackend/backends/posture_audit_test.go`) but no extracted-prompt fixtures. Prompt seeding differs per backend (`PromptSeeder`/`ReadyMarker`, `backend.go:208-221`) and is tmux-pane based.
- **Dependencies present in `go.mod`:** `github.com/creack/pty v1.1.24`, `github.com/coder/websocket v1.8.13`, `golang.org/x/sys v0.44.0`; `charmbracelet/x/ansi v0.11.6` and `x/cellbuf v0.0.15` only as *indirect* (not imported by `ptyruntime`). `Screen` is hand-written.

---

## 3. Entrypoint inventory

"Runtime today" = where the process lives in v11.5.0 with `runtime.pty: on`. "Owner" = the 02b plan that must make it correct. `T` = tmux, `D` = direct warden PTY (holder-hosted), `M` = tmux + observe-only mirror.

### 3.1 Spawn / launch

| ID | Entrypoint | Reaches | Runtime today | Owner |
|---|---|---|---|---|
| `E01` | CLI `wd agent start`/`spawn` (`cli/lifecycle.go:189`, `:247`) → `POST /api/v1/spawn` | `strict_lifecycle.go:133` → `life.Spawn` | M | 02b-03 |
| `E02` | MCP `spawn_agent` (`mcp/server.go:370`, `mcp/tools_extra.go:1055/1073`) | same `SpawnRequest` | M | 02b-03 |
| `E03` | TUI `n` new agent (`tui/cmds.go:72`) | same API | M | 02b-03 (launch) / 02b-02 (UX) |
| `E04` | REPL spawn (`repl/registry.go:331`) | same API | M | 02b-03 |
| `E05` | Autopilot manager/worker/resolver spawn (`daemon/autopilot_runtime.go:87`, `:567`; brain `brainconsult_spawner.go:40`; fix `fix_runtime.go:188/227`; plan agents `plan_agent_adapters.go:93`) | `life.Spawn` | M | 02b-03; authority 02b-04 |
| `E06` | Pipeline job (`daemon/executor.go:416`) | `life.SpawnJob` → raw `tmux send-keys` `lifecycle.go:3294` | M | 02b-03 |
| `E07` | Scheduled agent (`daemon/schedule_routes.go:98`) | `life.Spawn` | M | 02b-03 |
| `E08` | Fork (`cli/fork.go:67`; `SpawnRequest.ForkFrom`, `lifecycle.go:1135`, `buildLaunch` `:635-663`, sibling worktree off source branch) | `life.Spawn` | M | 02b-03 |
| `E09` | Handoff / rotate (`cli/handoff.go:106`, `cli/rotate.go:95`, MCP `tools_extra.go:1319`, autopilot guardian `autopilot/guardian.go:169`) | spawn successor via API | M | 02b-03 |
| `E10` | Backend/model switch (`lifecycle/switch.go:136` `HotSwap`; callers `autopilot_runtime.go:139`, `backend_recovery.go:432`) → `launchSuccessor` raw `send-keys` `switch.go:507` | in-place relaunch | M | 02b-03 |
| `E11` | Role switch (`life.SwitchRole` `lifecycle.go:2471`, route `strict_lifecycle.go:808`) → resume | in-place relaunch | M | 02b-03 |
| `E12` | Terminal manual spawn: CLI `--kind terminal` (`cli/lifecycle.go`), API `kind=terminal`/legacy `backend=terminal`, MCP, TUI `t` (`tui/cmds.go:103`), web `createTerminal` | `terminal_lifecycle.go:252` → `RestoreTerminal` → `TerminalHost` | **D** in `on`, else T/M | existing (verify w/o tmux: 02b-03) |
| `E13` | Terminal "auto" spawn | **none exists** (`plain-terminal-inventory` `P07/P08`) | — | guard: keep single funnel |

### 3.2 Restore / adopt / recovery / lifecycle

| ID | Entrypoint | Runtime today | Owner |
|---|---|---|---|
| `E14` | `POST /sessions/{id}/restore` agent (`strict_lifecycle.go:518` → `life.Restore`, `tmux has-session` guard `lifecycle.go:2439`) | T (resume line via raw `send-keys` `:2396`) | 02b-03 |
| `E15` | Project reopen (`strict_projects.go:374`), auto-restart (`autorestart.go:88`), rate-limit resume (`ratelimit.go:296`), backend recovery (`backend_recovery.go:427`), plan restart (`plan_restart.go`), executor restart (`executor_restart.go`) | T | 02b-03 |
| `E16` | `wd adopt` / `POST /adopt` (`cli/lifecycle.go:895`, `strict_lifecycle.go:212-241`, `lifecycle.Adopt` `:2546-2596` live-register by tmux name or resume) | T only (an *external* tmux session) | 02b-03: define "adopt" for direct = unsupported-explicit |
| `E17` | Daemon boot `recoverPTY` (`pty_runtime.go:89`) | terminals D; agents M re-mirror | 02b-03 |
| `E18` | Terminal watcher/guardian respawn (`poller/terminal_watcher.go:72/111`, `terminal_poller_deps.go:89`) | host | 02b-03 (no race with adoption) |
| `E19` | Orphan/tombstone reaper (`tombstone_reap.go:61/125`), repair (`cli/repair.go:290/315`) | tmux probes | 02b-03 |
| `E20` | Terminate / stop / delete (`strict_lifecycle.go:319/388/533`, `terminal_lifecycle.go:125/276`, `lifecycle.Terminate` `:2663`) | host | 02b-03 |

### 3.3 Interact / observe

| ID | Entrypoint | Runtime today | Owner |
|---|---|---|---|
| `E21` | `send`/`Input` (`strict_lifecycle.go:555-559`, `lifecycle.Input` `:2733`) | direct → `directInput`; else tmux paste | 02b-04 (ownership) |
| `E22` | `SendKeys` (`strict_core.go:347`; menu answers) | tmux | 02b-04 |
| `E23` | `tail`/`Output` (`strict_lifecycle.go:580`, `lifecycle.Output` `:2942`) | direct capture or `tmux capture-pane` | 02b-02 |
| `E24` | Poller (`poller.go:1129+`), terminal watcher, context guard, crash triage (`poller/crash.go`), pane-driven digests (`lifecycle.go:1794`) | tmux capture | 02b-04 |
| `E25` | Seeding (`seedInteractivePrompt`, `RedeliverPrompt` `:2873`) | tmux wait+paste | 02b-03 (launch) / 02b-04 (ownership) |

### 3.4 Attach

| ID | Entrypoint | Runtime today | Owner |
|---|---|---|---|
| `E26` | CLI `warden attach <id>` (`cli/lifecycle.go:803`) | terminal D native; **agent → `tmux attach`** | 02b-01 (client/API), 02b-02 (CLI wiring) |
| `E27` | TUI agent pane (`control_pane.go:2810`) | `tmux attach` | 02b-02 |
| `E28` | TUI terminal pane (`control_pane.go:2820`) | `warden attach` (D native / T fallback) | 02b-02 (verify) |
| `E29` | TUI standalone switch (`switchClientCmd` `:2798`) | `tmux switch-client` | 02b-02 |
| `E30` | TUI pipeline/autopilot/job detail panes (`:2904-2959`) | re-exec `warden tui …` in pane | 02b-02 |
| `E31` | Daemon `GET /attach` raw WS (`attach.go:89-109`) | D → `BridgeLegacy`; else tmux bridge | 02b-01 (unify via shared protocol) / 02b-02 |
| `E32` | Daemon `GET /sessions/{id}/pty` §6 (`pty_runtime.go:230`) | D interactive; M observe-only; 409 if no journal | 02b-01 |
| `E33` | Web cockpit xterm + `GET /cockpit` (`attach.go:112+`) | /attach bridge; cockpit = tmux | 02b-02 |
| `E34` | Android app / external WS clients | `/attach` raw protocol (frozen legacy) | 02b-01 (compat) |

---

## 4. Requirement → code → test → observation → gap matrix

**Classes:** `existing reusable` = already works and is reusable as-is · `partial` = exists but not for the full scope the requirement states · `confirmed missing` = absent after source inspection · `unknown` = no code or test evidence either way (named as an explicit work item, not assumed OK). Compound rows retain each applicable class rather than hiding a gap.

### 4.1 Plan 01 (`plan-48de9357`) — `done_when` and binding constraints

| ID | Requirement (abridged) | Actual code | Test | Observation at `0d531421` | Class | Owner |
|---|---|---|---|---|---|---|
| `R1.1` | Warden-created interactive agents supervised via a Warden-owned PTY stream; no polling as primary | Agents: tmux + `pipe-pane` mirror (`ptyhost/shadow.go:110-121`, `manager.go:630-637`); pollers primary (`poller.go:1129`) | `TestShadowMirrorsTmuxPaneWithParity` (`ptyhost/host_test.go:64`) | Journal + redacted replay exist for agents but input/process are tmux's; pollers still primary | partial | 02b-03 (host), 02b-04 (authority) |
| `R1.2` | Warden **launches** native CLIs through the PTY runtime (t2) | `routed.NewSession` terminal-only (`manager.go:612`); `Host.NewSession` shell-only (`ptyruntime/host.go:75`) | none launches an AI CLI direct | **Confirmed missing** | missing | 02b-03 |
| `R1.3` | Generic classifier: working/idle/waiting/permission/decision/attention, no state from silence alone | `stateclassifier` (`classifier.go`), hysteresis/evidence | `stateclassifier/*_test.go`, `daemon/pty_runtime_test.go:230` | Real and tested; runs on the mirror but cannot own status (`classifier.go:438`) | existing (logic) / partial (authority) | 02b-04 |
| `R1.4` | Normalized permission extraction (capability, target, options, fingerprint, provenance) | `ptyevent/prompt.go`, `promptengine`, `knownprompts` | `ptyevent/testdata/prompts/*` (claude, agy only) | Contract solid; fixtures cover 2 of 8 backends | partial | 02b-04 (adapter fixtures) |
| `R1.5` | Four postures enforced consistently across adapters; trust is a permission | `ptyevent/posture.go`, `ptyevent/posture_test.go`, `agentbackend/backends/posture_audit_test.go`, `lifecycle.go:109` `effectivePosture` | those tests | Mapping audited; enforcement only reachable via tmux writer, dry-run for agents | partial | 02b-04 |
| `R1.6` | Strategic prompts: Tier-1 brain for autopilot, human for plain/pipeline; fail-open to attention | `decisionrouter`, `brainconsult` | `decisionrouter/*_test.go` | Library-complete; live only under authoritative direct, which agents lack | partial | 02b-04 |
| `R1.7` | Per-session redacted decision trace + metrics by adapter/version | `ptyobs/trace.go`, `daemon/strict_ptyobs.go`, `/decisions`, `/metrics/pty` | `daemon/pty_obs_test.go` | Present; `PTYRuntimeView.Mode` is the *global requested* mode (`strict_ptyobs.go:55`), no shadow/direct/legacy label on a session | partial | 02b-01 (identity label), 02b-04 (metrics labels) |
| `R1.8` | Antigravity E2E: PTY, prompts, bypass, trust, drift, reconnect, redaction | `agentbackend/backends/antigravity*.go`, scripted fixtures | `antigravity_shape_test.go`, `ptyevent` drift fixtures | Scripted fixtures only; never against a direct-launched agent | partial | 02b-04 (fixtures on direct), 02b-05 (evidence) |
| `R1.9` | `wd check`/`verify-fast` pass; generated API/docs current | `Makefile`, CI | CI on #979/#985/#987 | Not independently re-run for all packages in this docs-only task beyond `wd check` | unknown (inherited) | 02b-05 |
| `C1.a` | Preserve native CLI UX; attach/reconnect semantics | `ptyattach` raw-mode attach | `ptyattach/attach_test.go`, `F-ATTACH-01` | Native for terminals; agents use tmux (native by definition) | partial | 02b-02 |
| `C1.b` | No state from silence alone | `stateclassifier` evidence rules | classifier tests | holds | existing | — |
| `C1.c` | Default does not widen permissions; dangerous mode explicit | `daemon/strict_ptyobs.go:15-20` default `allowlisted` | `ptyevent/posture_test.go` | holds | existing | 02b-04 (re-verify per direct launch) |
| `C1.d` | Retain stored modes/auto-approve through migration | agent record fields | migration tests (`agentstore`) | Not exercised with a direct launch path (none exists) | unknown | 02b-03 |
| `C1.e` | Redaction/retention boundaries for all durable/remote views | `ptyevent/redact.go`, journal retention (`ptyruntime/journal.go:32`) | `F-PRIV-01` (`ptyhost/privacy_test.go`) | Verified for terminals; agents' mirror journal is redacted too; faithful-vs-redacted *snapshot* semantics unspecified for attach (§6.6) | partial | 02b-01 (spec+test), 02b-05 |
| `C1.f` | OpenAPI-first | `internal/daemon/apidocs/openapi.yaml` + `make generate` | `make gendocs-check` | holds | existing | all |

### 4.2 Plan 02 (`plan-23988b36`) — `done_when` and binding constraints

| ID | Requirement | Actual code | Test | Observation | Class | Owner |
|---|---|---|---|---|---|---|
| `R2.1` | All Warden-created plain terminals (manual + automatic) use the shared PTY runtime; naming/project/env/cwd preserved | `spawnTerminal` funnel → `RestoreTerminal` (`lifecycle.go:2316-2346`); `TerminalHost` | `TestTerminalLaunchFunnel`, `TestTerminalIdentityEqualTmuxAndDirect` (`ptyhost/identity_test.go:23`), `daemon/terminal_ptylife_test.go` | True **in `on`** for terminals. Whether it works **with no tmux binary/server available** is untested (the launch path still calls `l.Proc().HasSession` and `ensureScrollback` tmux probes) | existing (terminals) / unknown (tmux-absent) | 02b-03 |
| `R2.2` | Shell attach/detach, multi-viewer, single controller, resize, signals, exit, replay, backpressure without AI interception | `ptyattach`, `ptyhost/transport.go`, `ptyruntime/control.go` | `F-ATTACH-01..03`, `F-FENCE-*`, `attach_test.go` | Terminals only; `warden attach` for agents is tmux | existing (terminals) / missing (agents) | 02b-01 (client for agents), 02b-02 |
| `R2.3` | Shell **and agent** survive daemon restart/update/crash; authenticated adoption; lease generation; human precedence | Holder (`ptyholder/holder.go`), `AdoptDirect` (`manager.go:411`), fencing | `ptyholder/survive_test.go`, `F-SURV-01..03`, `F-AUTH-*` | Terminals ✔. Agents survive only because tmux survives; `RecoverShadow` re-mirrors read-only (`manager.go:434`). Plan 02 `G2` was closed by documentation (`terminalcontract/gaps.go:37`), not by hosting | partial → **G2 reopened** | 02b-03 |
| `R2.4` | Exit/offline/holder-loss contracts with actionable errors | `MarkLost` (`ptyruntime/host.go:155`), `F-SURV-03`, `ErrGone` path (`pty_runtime.go:146-155`) | `survive_test.go` | Terminals ✔. Output while daemon absent = kernel-buffer blocking; no agent-specific policy; exit-while-offline for agents only via exit file | partial | 02b-03 |
| `R2.5` | Opt-in/shadow/default rollout with numeric thresholds; per-session cohort rollback | `ptyhost/manager.go:234-285` pins/cohorts; budgets `terminalcontract/budgets.go`; `RUNBOOK-pty-rollout.md` | `F-ROLL-01` (`rollout_test.go:18`) | Library only: pins in memory, cohort APIs unused in production, no operator command; global `off` skips adoption of live direct sessions (`pty_runtime.go:90`) | partial | 02b-01 (durable pin), 02b-03 (boot), 02b-05 (evidence) |
| `R2.6` | Observed parity artifacts / deployment evidence | `docs/RUNBOOK-pty-rollout.md` | `F-PAR-01/02`, `F-RES-*` | Fixture-only. Installed daemon runs `shadow`; **no production direct cohort has ever existed** | missing (observed evidence) | 02b-05 |
| `R2.7` | Plan-03 entry gate delivered | RUNBOOK §gate | `terminalcontract` tests | Documented. Must be re-asserted: plans 01/02 do **not** satisfy it (this audit) | existing (doc) / invalid premise | 02b-05 |
| `C2.a` | Plain terminals never reach AI classifiers; nested manual AI CLI gets no automation | `ObserveAgents` + durable resolver (`manager.go:180-215`) | `kind_test.go:33-96`, `F-KIND-01..03` | holds | existing | 02b-04 (re-verify for direct agents) |
| `C2.b` | Document **which process holds the PTY master** | `ptyholder/doc.go:20-28`, inventory §5.2 | docs | Terminal: holder+daemon dup; agent: tmux server | existing | 02b-01 (add to identity) |
| `C2.c` | Inventory all launch paths | `plain-terminal-inventory` `P01–P21` | `terminalcontract` tests | Terminals only. Agent entrypoints inventoried first here (§3) | partial → closed by §3 | 02b-01 |
| `C2.d` | Reuse plan 01 lifecycle/transport/replay/arbitration; keep tmux layouts; legacy sessions unconverted | as above | `F-ROLL-04` (`rollout_test.go:155`) | holds | existing | — |
| `C2.e` | Bounded recovery with actionable failures; no silent kill/relaunch of a live process | `RecoveryBound` budget; `recoverPTY` | `F-SURV-*` | Terminals ✔; **uncertain-launch idempotency for agents untested** (no direct agent launch) | partial | 02b-03 |

### 4.3 Corrective plan (`plan-001ead2b`, superseded) — `done_when` and constraints

| ID | Requirement | Observation / gap | Class | Owner |
|---|---|---|---|---|
| `RC1` | Committed requirement-to-code/test matrix for all plan 01/02 criteria and all production launch/restore/attach/control/status paths | **This document** (§3, §4) | existing (delivered here) | 02b-01 |
| `RC2` | In `on`, every supported new AI agent **and** terminal launches direct and works with its agent-hosting tmux **absent**; off/shadow/legacy still work; "no mode label alone counts" | Agent launch: **confirmed missing** (`manager.go:612`, `lifecycle.go:2136/2242/3294`). Terminal without tmux: **unknown** (§R2.1). Needs host-aware launch contract (§6.4), not just lifting the guard | missing | 02b-03 |
| `RC3` | CLI + TUI list/tree/detail/selection/panes/attach/detach/reconnect by **stable session identity** for direct, legacy and mixed fleets; closing a viewer never kills the child | CLI agent attach = tmux (`cli/lifecycle.go:803`); TUI agent pane = `tmux attach` (`control_pane.go:2810`); liveness = `TmuxSession != ""` (`:2882`); terminal cwd/liveness from tmux (`cmds.go:165-185`); no runtime identity in the DTO | missing (identity) / partial (UI) | 02b-01 (identity + shared client), 02b-02 (UI) |
| `RC4` | Direct agents/terminals keep identity across daemon stop/start/update/SIGKILL and client disconnect; no duplicate launch; explicit contracts for output during daemon absence, exit while offline, holder loss | Terminals ✔ (`F-SURV-01..03`). Agents: no holder-hosted agent exists; holder-absent output = blocking; global-`off` boot skips adoption (`pty_recovery:90,117`); partial-launch idempotency (crash between PTY start and store write) has no record to detect it | missing (agents) / partial | 02b-03 |
| `RC5` | Direct agent state + permission/strategic decisions are event-authoritative and traceable; legacy pollers cannot duplicate input or overwrite; shell/nested prompts never trigger agent policy | Authority = false for every agent (§2.3); structural poller exclusion absent (map lookup only); seeding/answers use tmux writers | missing | 02b-04 |
| `RC6` | Native launch/model/permissions/seeding/resume/backend-switch/restore/fork/handoff/rotation/autopilot/pipeline/scheduled paths validated or explicitly gated; no stale metadata or accidental billing/permission expansion | Every path in §3.1–3.2 is tmux-shell-line based (`lifecycle.go` raw `send-keys`); no adapter has an argv launch form; seeding is pane-marker based | missing | 02b-03 (launch/recovery), 02b-04 (permissions) |
| `RC7` | Race/adversarial, fidelity, secrecy, backpressure, mixed-runtime, protocol-compat, resource regression pass with artifacts | Terminal-scoped fixtures exist (`F-VT-*`, `F-RES-*`, `F-PRIV-01`, `F-ROLL-04`); none for direct agents; snapshot fidelity limited (§2.8) | partial | 02b-01 (attach/snapshot fixtures), 02b-05 (matrix) |
| `RC8` | Release runbook separates tested capability from observed evidence; plan 03 gated on deploy + small cohort + sample/soak/parity + natural drain + rollback verified | Runbook exists (`RUNBOOK-pty-rollout.md`, min 20 sessions / 24 h per cohort); its gate was marked satisfied by plan 02 on fixtures; no deployment evidence | partial (premise wrong) | 02b-05 |
| `CC.1` | Reuse PTY/holder/WebSocket/client/screen/journal; no universal provider-API CLI, no Hub, no TUI redesign | followed in §6 | n/a | all |
| `CC.2` | No forced conversion/termination of live legacy sessions; ownership explicit and pinned; rollback affects future launches only; unknown adapters fail explicitly | pin exists in memory only (§2.8); no adapter capability gate | partial | 02b-01 |
| `CC.3` | Preserve project/plan/run/parent-child ownership, record IDs, provider conversation IDs, worktrees; **new runtime identity ≠ tmux existence ≠ AI conversation ID**; no DB edits/rename migration | fields exist (§2.7) but the three meanings of `TmuxSession` are conflated | partial | 02b-01 |
| `CC.4` | One process/control authority per session; human precedence; fencing; no duplicate poller approvals, no hidden relaunch after uncertain execution; no network/store call under a global lock | lease/fencing ✔ for direct; "one authority" unenforced for agents (§2.5); relaunch-after-uncertainty untested | partial | 02b-03/04 |
| `CC.5` | Spec-first API; generated clients; deterministic fixtures first | `openapi.yaml` pipeline in place | existing | all |
| `CC.6` | Secrecy/scope/retention audit: raw local rendering vs redacted durable/replay | Policy stated (inventory §6) but the *attach snapshot* path mixes redacted replay and raw live (§6.6) | partial | 02b-01 |
| `CC.7` | Do not fix #918/#919 opportunistically; report affected scenarios | §7 | existing (reported) | 02b-01 |

---

## 5. Gaps assigned (no gap dropped, none deferred to plan 03)

Roll-up of every `missing`/`partial`/`unknown` row, one owner each.

| Owner | Gaps |
|---|---|
| **02b-01 (this plan)** | No runtime identity/kind/capabilities model; `TmuxSession` conflation (`CC.3`); non-durable pins and unused cohorts (`R2.5`,`CC.2`); `PTYRuntimeView.Mode` reports requested not actual (`R1.7`); no stable shared attach client/API usable by TUI/Cockpit/web for agents (`R2.2`,`RC3`); snapshot/replay fidelity contract and tests (`C1.e`,`CC.6`); `/attach` raw vs §6 unification and compat statement (`E31–E34`); #918/#919 report (§7). |
| **02b-02** | CLI `attach` for agents; TUI agent pane, standalone switch, liveness (`TmuxSession != ""`), terminal cwd/title from tmux, return-to-TUI; list/tree/detail by identity; web cockpit xterm; stale-selection and empty/mixed-fleet UX; reuse #986 (`RC3`). |
| **02b-03** | Host-aware **agent** launch (argv/env/process-group, no pane markers), exit capture, prompt seeding without `capture-pane`; every launch/resume entrypoint of §3.1–3.2; `Restore`/`Adopt`/`Terminate` host-awareness; holder-hosted agent adoption in `recoverPTY`; global-`off` boot adoption; offline-output policy; partial-launch idempotency; tmux-absent proof for terminals (`R1.2`,`R2.1`,`R2.3`,`R2.4`,`RC2`,`RC4`,`RC6`,`C1.d`,`C2.e`). Reopens `G2`. |
| **02b-04** | Event-authoritative agent status + decisions; structural poller/engine mutual exclusion; shared input ownership for all automatic sends/seeds; adapter prompt fixtures for the 6 backends without them; adapter support matrix enforcement; metrics labels shadow/direct/legacy (`R1.1`,`R1.3–R1.8`,`RC5`,`CC.4`). |
| **02b-05** | Execute the §9 matrix end-to-end; real direct-cohort evidence vs fixtures; re-assert plan-03 gate (`R1.9`,`R2.6`,`R2.7`,`RC7`,`RC8`). |

---

## 6. Intended contract (target — NOT current behavior)

Everything in this section is a **requirement on 02b-01…05**. Where it says "reuse", the existing component is named in §2.

### 6.1 Identity model — four distinct identities

| Identity | Meaning | Source of truth | Never to be confused with |
|---|---|---|---|
| **Warden session id** | Stable record id (`Agent.ID` / `Terminal.ID`); what users, API, project/plan/run/parent links reference | agentstore / terminalstore | runtime key, tmux name |
| **Runtime key** | The string under which the runtime indexes journal, holder, mirror (`<data>/pty/direct/<key>`, `holder/<key>`). **Equals the session id for sessions created from this contract on**; for legacy records equals `TmuxSession` | resolver (below) | proof that tmux exists |
| **Presentation** | How a human sees it: `direct` (warden PTY, no multiplexer) or `tmux` (compat; tmux may also host the process) — already `ptyevent.Presentation` (`ptyevent/event.go:152-162`) | resolver, per live session | process owner |
| **Process identity** | `{holder_pid, holder_start, child_pid, child_start, proto}` for holder-hosted; `{tmux server, pane_pid, pane_start}` for tmux-hosted | holder meta (`ptyholder/proto.go` `Meta`) / `Shadow.VerifyPane` | AI conversation |
| **AI conversation id** | Provider resume handle (`Agent.AICLISessionID`, legacy `claude_session_id`) | agentstore | everything above |

### 6.2 Runtime kind and additive fields

Define one resolver — `ptyhost.Manager.Resolve(sessionID) RuntimeInfo` — as the **only** way any client or daemon code learns runtime (replaces the three uses in §2.7). It never reads `TmuxSession != ""` as evidence.

```
RuntimeInfo {
  kind:          "direct" | "tmux" | "unknown"      // actual, not requested
  presentation:  "direct" | "tmux"                  // = ptyevent.Presentation
  key:           string                             // runtime key
  process_owner: "holder" | "daemon" | "tmux" | "none"
  holder:        { pid, start_time, proto } | null  // only for holder-hosted
  pinned_mode:   "off"|"shadow"|"on"|""             // per-session pin (durable)
  requested_mode:"off"|"shadow"|"on"                // current global (informational)
  authoritative: bool                               // today: IsAuthoritative
  capabilities:  []string                           // §6.5; honest, per-session
  source:        "live" | "record" | "legacy-probe" // how it was decided
}
```

Resolution precedence (first hit wins, each step is a *positive* proof): (1) live direct session in the manager → `direct` (`Manager.Direct`); (2) holder record exists and verifies (`Launcher.Has` + identity) → `direct`/`holder`; (3) live mirror (`Manager.shadow.Mirror`) **and** `tmux has-session` → `tmux`; (4) persisted record field (below) when none of (1)–(3) is live (reports last-known kind with `source:"record"`, `authoritative:false`); (5) `tmux has-session` on `TmuxSession` for records predating the field → `tmux`, `source:"legacy-probe"`; else `unknown`. A missing/dead session is `unknown` with an actionable reason — never silently `tmux`.

**Persisted additive fields (reuse first, add the minimum):**

- *Reused unchanged:* `Agent.ID`/`Terminal.ID` (session id), `Agent.PID` (leader pid), `Agent.AICLISessionID` (conversation), `Agent.TmuxSession`/`Terminal.TmuxSession` (kept as the legacy *runtime-key alias*; documented as "name under which the runtime indexes this session, **not** proof of tmux"), holder state under `<data>/pty/holder/<key>` (process identity truth), journal dir existence (`HasDirectJournal`).
- *Added (one optional object, `omitempty`, on `agentstore.Agent` and `terminalstore.Terminal`, projected onto the public `Session` DTO):* `runtime: { kind, pinned_mode, launch_id, holder_pid, holder_start, proto, launched_at }`. `launch_id` is a caller-generated idempotency token written **before** the child is started so a crash between start and store update can be detected and adopted instead of relaunched (consumed by 02b-03). Nothing else is added; `kind` here is the *last known actual* value used for fallback step (4).
- *Not added:* a second session-id, any rename of `tmux_session`, any copy of the conversation id.

### 6.3 Old / new / mixed record compatibility; no DB rewrite

1. A record without `runtime` is **legacy**: it decodes unchanged, resolves via steps (1)–(3),(5), and is never rewritten merely by being read. The field is written only at (a) new launch, (b) explicit relaunch/restore that changes ownership, (c) adoption. No bulk migration, no startup rewrite pass.
2. A new record read by an **old binary** is safe: unknown JSON field is ignored by the decoder; `tmux_session` still holds the key, so an old binary treats a direct session as a vanished tmux session (it may flag it `orphaned`) — the supported-downgrade boundary and its operator procedure (stop writing direct launches, drain, then downgrade) is a 02b-05 runbook item (`RC8`).
3. API: `Session` gains additive `runtime` (object) and the `/api/v1/capabilities` flag **`runtime-identity`** (string, advertised only when the daemon actually resolves identity). Old clients ignore the field. New clients must treat absent `runtime` as legacy and fall back to the pre-02b behavior (tmux attach for agents; native attach for `kind=terminal` with `ErrNotDirect` fallback). `PTYRuntimeView` keeps `mode` (documented as **requested** global mode) and gains `requested_mode`, `actual_kind`, `process_owner` (additive; `mode` semantics clarified, not removed).
4. Mixed fleet: one list response may contain `direct`, `tmux`, `unknown` rows; every consumer must handle all three.
5. OpenAPI-first (`openapi.yaml` → `make generate`; streaming routes excluded per `oapi/config.yaml`); `make gendocs` for help text; no hand-written handlers/DTOs.

### 6.4 Safe launcher / argv / environment / process-group contract (consumed by 02b-03; foundations tested by 02b-01)

1. **Argv, never a typed line.** Launch = `Launcher.Launch(ctx, key, argv, cwd, env, cols, rows)` (`ptyholder/launcher.go:25`). A new `ptyruntime.Host.NewSessionArgv(ctx, key, argv, cwd, env…)` (alongside `NewSession`) is the agent entrypoint (today `h.shell()` is hard-wired, `host.go:64-75`).
2. **Adapter launch form.** Backends today return a shell line (`backend.go:149-156`). Target: an optional `agentbackend.ArgvLauncher` (`LaunchArgv(LaunchOpts) (argv []string, env []string, ok bool)` and `ResumeArgv`). A backend that implements it is *direct-capable*. Until an adapter implements it, the **only** permitted bridge is `argv = ["/bin/sh","-c", <line>]` where `<line>` is the existing adapter output **minus** the `exitSuffix`, the final command prefixed with `exec` when the line is a single command, and every embedded value already `shellQuoteArg`-quoted by the adapter. A backend with neither form is **unsupported-explicit** (§6.5), never silently tmux-with-a-direct-label.
3. **Environment** = daemon `os.Environ()` + `WARDEN_SESSION_ID`/`AGENTCTL_SESSION_ID` (+ `WARDEN_PIPELINE_ID`, `WARDEN_JOB_ID`, `AGENTCTL_*` for jobs, `lifecycle.go:3250`) + `TERM=xterm-256color` when unset (`manager.go:599`). The spec file carrying env is deleted by the holder on read (`holder.go:54`) and never logged; env values are never put in argv, journal, audit, or process title.
4. **Prompt seeding** without a pane: positional/flag backends keep the file-backed `$(cat …)`/argv fragment; `PromptSeeder` backends write through the session's **input lease** (`origin=warden`, `promptengine.DirectWriter`) after a *positive* readiness signal from the journal/classifier (not `capture-pane`); the 1024-byte tty-line limit (macOS `MAX_CANON`) no longer applies to the launch line but still applies to any later typed line.
5. **Process group / signals.** The child is session leader (`Setsid+Setctty`, `holder.go:81`); stop = SIGHUP→SIGTERM→SIGKILL the leader's group after `KillGrace` (existing `Close`); descendants in other groups (job control) are reached by the CLI's own forwarding, same as tmux. Exit status comes from the holder's wait status; the exit-file suffix is **not** used for direct sessions.
6. **No hidden relaunch.** If `launch_id` exists but no live process verifies, launch fails with an actionable error (cause + `warden agent restore <id>`); nothing is started automatically after an *uncertain* execution.
7. **Locking.** Launch, adoption and verification must not hold a global controller/DB lock across the spawn, socket, git, GitHub or store calls (see #918); take the per-key lock (`termLaunch` pattern, `lifecycle.go:2318`), snapshot state, do I/O, re-validate generation, then apply.

### 6.5 Mode hot reload, per-session pins, and the adapter support matrix

**Mode semantics.** `runtime.pty` (`off|shadow|on`) remains the *default for sessions created afterwards* (`reconfigure.go:92-95`). The **pin is durable and authoritative for a live session**: `pinned_mode` is persisted at launch (`runtime.pinned_mode`) and re-read at boot; flipping the global mode (including to `off`) never demotes, kills, relaunches or de-authorizes a live pinned direct session, and the boot recovery path must adopt it regardless of the global mode (fixes `pty_runtime.go:90,117`). Same-name reuse of a record id starts a **new** pin only after the previous runtime is verified gone. Cohorts (`SetCohortMode`) get a persisted operator-visible command or are removed from the claimed surface — an unused API may not be cited as rollback evidence.

**Adapter support matrix (binding statuses; "direct" = may launch on the warden PTY in `on`).** `unsupported-explicit` = spawn returns a typed error naming the adapter and "not direct-capable" when a direct launch is requested and falls back to tmux **only if the cohort/policy allows and the DTO says `kind: tmux`**; it is never a silent label change.

| Adapter | Launch form today | PTY prompt fixtures | Resume/seed notes | Status at baseline | Required before `direct` |
|---|---|---|---|---|---|
| claude | shell line + `--session-id`, positional prompt | ✔ (`claude_*.json`) | pins session id; transcript | tmux+mirror | ArgvLauncher (02b-03); authority tests (02b-04) |
| antigravity (`agy`) | shell line | ✔ (`agy_trust_*`, shape tests) | trust prompt answered by Enter; discover-then-pin | tmux+mirror | ArgvLauncher; E2E on direct (02b-04/05) |
| codex | shell line; `ForkCmd` (`-C`) | ✘ | dir-scoped resume `--last` | tmux+mirror | argv + prompt fixtures |
| cursor | shell line (`--trust`) | ✘ | blocked on transcript Tier C | tmux+mirror | argv + fixtures |
| aider | shell line (`--message`) | ✘ | `PromptSeeder`, no resume id | tmux+mirror | seeding via lease + fixtures |
| opencode | shell line (`--prompt`) | ✘ | — | tmux+mirror | argv + fixtures |
| crush | shell line | ✘ | `PromptSeeder` | tmux+mirror | seeding + fixtures |
| goose | shell line | ✘ | `PromptSeeder` | tmux+mirror | seeding + fixtures |
| shell (`terminal`) | `$SHELL` argv | n/a (never classified) | — | **direct** in `on` | unchanged; add tmux-absent proof (02b-03) |

An adapter becomes `direct` only when (a) it has an argv/bridged launch form, (b) launch/resume/seed paths pass fixtures with no tmux, and (c) it has permission/state fixtures for its prompts (02b-04). Capabilities in `RuntimeInfo.capabilities` are computed from that matrix and the session's real runtime (e.g. `native-attach`, `input`, `resize`, `authoritative-state`, `survives-daemon`), so a client never infers support from the backend name.

### 6.6 Terminal snapshot / replay design (raw local vs redacted durable)

**Two planes, never mixed under one label:**

| Plane | Content | Producer | Consumers | Retention |
|---|---|---|---|---|
| **Local-authorized live plane** | Faithful, *unredacted* PTY bytes (binary frames) and, for a fresh/reset attach, a **screen reconstruction** | `Session` live fan-out; `Screen.StyledString()` | `ptyattach`, TUI pane, web xterm, only over authenticated interactive connections with controller or viewer scope; **never persisted** | in-memory subscriber queues only |
| **Durable / remote-safe plane** | Redacted events; snapshots are `Screen.String()` plain text over redacted bytes | `Journal` (`ptyruntime/journal.go`) | SSE, `/pty` `event` frames, audit, diagnostics, exports | bounded: 64 MiB, 8 snapshot segments, 14 days (`DefaultRetention`) |

Rules (binding):

1. A journal snapshot is labelled **`redacted-text-snapshot`**, not "terminal state". It must never be advertised as an exact reconstruction of cursor, attributes, alternate screen or modes. The `plan` frame (`PlanFrame`, `transport.go:92-110`) gains an additive `fidelity` field: `exact-live` (post-`head` bytes), `redacted-text` (snapshot/replay events), `repaint` (client was told to request a redraw).
2. **Exact local reconstruction** on reconnect is achieved the way a native terminal does it: reset + replay of the *local* screen model **plus a forced repaint** (`nudgeRedraw` SIGWINCH to the foreground group, `control.go:152`) so the program redraws itself. Because `Screen` tracks only alt-screen modes (§2.8), the screen model must be extended (or the repaint relied on and **documented as best-effort for programs that do not redraw**) to track: cursor position/visibility (DECTCEM), DECCKM, bracketed paste (`?2004`), mouse reporting (`?1000/1002/1003/1005/1006`), focus (`?1004`), origin/autowrap, charset, and synchronized output; the attach client re-asserts the recorded modes on the local terminal after reset. The mode set is part of the `reset` marker payload and lives only on the local plane.
3. **Replay-to-live ordering** is already defined (replay events ≤ `head` from the journal, everything after from binary frames, events past `head` advance the cursor only, `transport.go:44-52`). 02b-01 adds tests that prove no gap/dup/reorder across: epoch reset, daemon adoption (`epoch+1`), reconnect with ack, cursor too old (`reject`), and slow-viewer `resync`.
4. **Secrecy:** typed no-echo input never reaches either plane's *durable* side (existing `RedactInput` + `F-PRIV-01`); the live plane is by design unredacted, so it is gated by authentication and scope (read-only token → observe-only frames **without raw bytes**, `pty_runtime.go:250-253`). Raw frames must never be copied into SSE, audit, trace, metrics labels or crash excerpts. Residual detection limits stay as documented (`SecretDetectionLimits`).
5. **Limits stated honestly:** hand-written VT model (`ptyruntime/screen.go`), no sixel/kitty graphics, no terminal-query responses beyond what the PTY child gets from the real attached client; snapshots lose SGR in the durable plane (`F-VT-03`).

### 6.7 Shared attach / control protocol (the single client for agents and shells)

Keep the existing §6 WebSocket protocol as the **one** wire protocol (`ptyhost/transport.go`). Direct **and** mirrored sessions are served by it; the legacy `/attach` raw bridge stays frozen for existing clients (Android/web) and is implemented *on top of* the same session ownership (`BridgeLegacy`), not extended.

| Concern | Contract |
|---|---|
| **Roles** | `controller` (≤ 1 per session, holds lease `{client_id, generation}`) and `viewer` (any number; raw feed, no input, no resize). Read-only credentials are viewers *without raw bytes*; they can never acquire the lease (`pty_runtime.go:250`; add a protocol test). |
| **Human precedence** | `human > operator > warden` origin arbitration (`ptyevent/input.go`) unchanged; an automatic send (seed, answer, resume) is invalidated by newer human bytes (TOCTOU, `ptyevent/toctou.go`). |
| **Lease / generation fencing** | Generation = `epoch<<32 | n`; every takeover **and** epoch change advances it; input frame carries `(gen, id)`; stale gen → `rejected`; `id` dedupes across reconnects via `LastInputID` (`control.go:164`). Takeover is explicit and journaled; previous controller gets `demoted`. |
| **Mirrored (tmux) sessions** | Observe-only until 02b-03 makes them direct; a client attach to a mirror is a **viewer** with `process_owner: tmux`; interactive use falls back to `tmux attach` via the shared client's `Fallback` (below). No fake lease on tmux. |
| **Resize** | Controller's size wins (last controller wins); viewers' sizes ignored; SIGWINCH delivered; reconnect re-sends current size. |
| **Detach chord** | Default **Ctrl-]** (`0x1d`) then `.` detach, `t` take control, `Ctrl-]` twice = literal Ctrl-]; configurable via `Options.DetachKey` (and `WARDEN_ATTACH_DETACH_KEY` env, documented). The chord is consumed by the client and **never forwarded** to the child; a pending chord prefix is flushed as literal bytes only after the chord times out or a non-chord key arrives, and never produces input through a stale generation. |
| **Reconnect** | Client stores `cursor` + highest `input_id`; backoff `250ms·2^n` capped 5 s, max 8 re-dials (`Options`); `ack` checkpoints per `client_id`; replay-to-live as §6.6 rule 3; **reconnect never relaunches or restarts the child**; closing a viewer or the socket is never a stop. |
| **Bounds** | ≤ 1 MiB inbound frame (`SetReadLimit`), `pendingCap` 4096 unsent inputs, slow viewer pinned ≤ 4 MiB then `resync`/`ErrResync` (budget `SlowViewerMaxBytes`), journal bounds as §6.6, subscribers per session ≤ **16** (proposed new budget; reject with an actionable error), invalid/partial frames close the socket with a status code and never crash the session. |
| **Gaps/overflow** | Every drop is explicit: `resync` (fell behind), `reject` (cursor unservable), `demoted`, `rejected`; the client prints a one-line notice with cause + next step. |
| **Raw mode** | Terminal modes restored on every exit path (`ptyattach/raw*.go`, signals SIGTERM/SIGHUP/SIGINT); Ctrl-C/Ctrl-D/Ctrl-Z are forwarded as bytes to the remote line discipline so signals/EOF/job control are the PTY's own. |

**Stable client interface** (what 02b-02 consumes; to be exported from `internal/ptyattach`, no behavior change to existing `Run`):

```go
type Target struct { SessionID string; Info ptyhost.RuntimeInfo }          // from Resolve
type Client interface {
    Attach(ctx context.Context, t Target, o Options) (Result, error)        // interactive or observe
    Fallback(t Target) (argv []string, ok bool)                              // e.g. ["tmux","attach","-t",key] for runtime.kind==tmux
}
func Open(daemonURL, token string) Client
```

`Attach` returns `ErrNotDirect` only for `kind=tmux`; for `unknown` it returns a typed `ErrUnknownRuntime` with the resolver's reason. TUI/Cockpit/web never construct `tmux attach` strings themselves.

### 6.8 Libraries (verified in `go.mod`, nothing invented)

PTY: `github.com/creack/pty v1.1.24` (used by holder/runtime). WebSocket: `github.com/coder/websocket v1.8.13` (daemon transport and `ptyattach` dial). Termios/ioctl/SCM_RIGHTS: `golang.org/x/sys v0.44.0`. VT parsing: in-tree `ptyevent.ControlScanner` + `ptyruntime.Screen`; `charmbracelet/x/ansi` and `x/cellbuf` exist as *indirect* deps and may be evaluated by 02b-01 for the mode-tracking work — adding an **import** of an already-present module is allowed, adding a new module requires a justification in the PR. No new dependency is required by this contract.

### 6.9 Bounded numeric budgets

All values inherit `terminalcontract.Default` (`internal/terminalcontract/budgets.go`; plain-terminal inventory §7.1) and apply **per runtime kind and per adapter**; a budget measured only for terminals does not certify agents. New 02b budgets are added to the same struct by 02b-01 (so one test suite enforces them) and may only be *tightened* later without a reviewed contract change.

| Group | Budget | Bound | Applies to |
|---|---|---|---|
| Parity | visible bytes tmux vs direct | 100 % | agent + terminal (`ParityByteExactPct`) |
| | replay order violations (gap/dup/reorder) | 0 | both (`ReplayOrderViolations`) |
| | state accuracy vs labelled corpus | ≥ 98 % per adapter | direct agents (`StateAccuracyPct`) |
| | false approvals / shell classifier events | 0 / 0 | both |
| Latency (p99, loopback, idle, 80×24) | keystroke echo | ≤ 50 ms | both |
| | attach → first byte | ≤ 250 ms | both |
| | resize → SIGWINCH | ≤ 100 ms | both |
| | lease takeover | ≤ 200 ms | both |
| | **runtime identity resolve (no process exec)** | **≤ 5 ms** | **new** (`RuntimeIdentityResolveP99`) |
| | **agent launch → first journal byte** (excl. CLI startup) | **≤ 500 ms** | **new** (`AgentLaunchToFirstByteP99`) |
| | **agent adopt after daemon restart** | **≤ 2 s** (= `AdoptAfterRestoreP99`) | agents + terminals |
| Resource (idle, steady) | RSS / FDs / goroutines per session | ≤ 16 MiB / ≤ 6 / ≤ 6 | both; **re-measure for a busy AI CLI** (new) |
| | journal per session | ≤ 64 MiB | both |
| | soak | 50 concurrent, `-race`, 0 leaks | mixed agent+terminal (new mix) |
| | viewers per session | ≤ **16** | **new** (`ViewersPerSession`) |
| | slow viewer | ≤ 4 MiB pinned then `resync`; PTY read stall ≤ 10 ms | both |
| | snapshot size (plain text) | ≤ **1 MiB** | **new** (`SnapshotMaxBytes`) |
| Recovery | survival loss / duplicate processes / duplicate input bytes / stale-controller writes / secret leaks in durable | 0 / 0 / 0 / 0 / 0 | both |
| | actionable errors | 100 % (cause + next step) | both |
| | recovery bound | ≤ 30 s then fail loudly | both |
| | **daemon-absent output policy** | block ≤ kernel PTY buffer **or** spool ≤ **8 MiB**/session with a `gap` marker — 02b-03 must pick one, measure, and document; unbounded loss or unbounded blocking is not allowed | **agents (new)** (`DaemonAbsentSpoolMaxBytes`) |
| Rollout | min sessions / soak hours per stage | ≥ 20 / ≥ 24 h per cohort | observed evidence only |
| | stage abort | error > 1 %, fallback > 5 % | observed |

---

## 7. Interaction with #918 and #919 (reported, not fixed)

Neither issue is touched by this plan. They affect **how acceptance scenarios may be claimed**.

| Issue | Defect (per issue text) | PTY acceptance scenarios affected | Handling |
|---|---|---|---|
| **#918** | `Controller.mu` held across the whole `guardianTick`/`superviseRun` (runtime, process, GitHub I/O); status/lifecycle requests 503; SIGTERM shutdown waits on the lock until systemd SIGKILL | (a) any "daemon stop/start/update" scenario that SIGTERMs the daemon during autopilot activity — shutdown may exceed 90 s and be force-killed, which turns a *graceful* survival test into a SIGKILL test; (b) autopilot-launched agents (`E05`, `E09`) whose guardian rotation/restart overlaps a direct-launch or adoption window (a stuck guardian blocks `Status`, hiding runtime state); (c) any acceptance check that polls autopilot status for run membership during recovery | 02b-03/05 restart-order tests must run **with autopilot idle** and record the stop duration; a stop > 30 s is reported as blocked-by-#918, **not** as a PTY failure and not a pass. Direct launch/adopt code must obey §6.4 rule 7 so it does not add new lock-held I/O. |
| **#919** | Boot recovery constructs `RunRecord`s before `PlanSource` is wired → plan-bound runs with empty `PlanFile` marked *degraded*, managers not recovered | (a) autopilot managers/workers/resolvers recovered after a daemon restart (`E05`, `RC4`, `CC.3` run/plan ownership); (b) any scenario asserting "same child/conversation + run membership survive restart" for an autopilot-owned **direct** agent; (c) 02b-04's authority scenarios that rely on the run record for `AutopilotRunID`-gated automation (`engine.go:232`) | Autopilot-owned direct agents are **explicitly excluded from restart-survival acceptance** until #919 is fixed; plain/pipeline/terminal restart scenarios are unaffected. Each such scenario is recorded `blocked: #919` in the 02b-05 matrix. No workaround in PTY code. |

Both issues are independent of the runtime-identity data model; the identity resolver must not depend on autopilot controller state.

---

## 8. Binding implementation / test checklist — next two tasks

Each item is a merge requirement for its task's PR. "Fixture" = deterministic, loopback, injected clock, no live provider, tmux not required unless stated.

### 8.1 Task `additive-runtime-identity` (02b-01 #2) — **no direct AI launch**

- [ ] `ptyhost.Manager.Resolve` implementing §6.2 precedence; unit tests: direct, holder-hosted-after-restart, mirrored tmux, legacy record (probe), dead/missing → `unknown` with reason; **never** infers from `TmuxSession != ""`.
- [ ] Persisted additive `runtime` object on `agentstore.Agent` and `terminalstore.Terminal` (`omitempty`); round-trip tests for **old record → new binary**, **new record → old decoder (unknown field ignored)**, **mixed list**; golden fixtures under `testdata/`; no migration, no rewrite-on-read test (`stat`/revision unchanged after read).
- [ ] Durable pin: write `pinned_mode` at launch; boot reload; test that flipping global `off`/`shadow`/`on` and a restart do not change a live session's pin or authority; same-name reuse test.
- [ ] Fix/clarify `PTYRuntimeView`: add `requested_mode`, `actual_kind`, `process_owner`; keep `mode`; test against a live direct terminal **and** a mirrored agent that `actual_kind` is truthful when global mode ≠ session reality.
- [ ] Spec-first API: `openapi.yaml` additive `runtime` on `Session`; capability flag `runtime-identity` in `/api/v1/capabilities`; `make generate`, `make gendocs`, `make gendocs-check`; client (`internal/client`) + MCP session payload carry the object.
- [ ] Replace every runtime-inferring read of `TmuxSession` in §2.7 (`daemon/pty_runtime.go`, `strict_ptyobs.go`, `attach.go`, `strict_lifecycle.go:555/580`, `tui/control_pane.go:2882` data source, `cli/lifecycle.go:821`) with the resolver or an explicit "runtime key" accessor; a lint-style test fails if a new `\.TmuxSession != ""` is introduced outside an allow-list.
- [ ] Capabilities computed from the §6.5 matrix + live runtime; unknown adapter → `unsupported-explicit` (typed error code `ErrAdapterNotDirectCapable`), table-driven test over all 8 backends.
- [ ] Add the new 02b budgets (§6.9) to `terminalcontract.Budgets` with a test that the doc table and struct agree.
- [ ] Do **not** change `routed.NewSession`'s terminal-only guard; add a test that pins the current behavior (agent never direct) so 02b-03 flips it deliberately.
- [ ] No behavior change in off/shadow; run `wd check` and `make verify-fast`.

### 8.2 Task `shared-attach-client` (02b-01 #3)

- [ ] Export the §6.7 `Client` interface and `Target` from `internal/ptyattach` (no change to `Run`'s existing semantics); `Fallback` returns `tmux attach` for `kind=tmux`, `ErrUnknownRuntime` for `unknown`.
- [ ] Protocol compat statement + tests: new client ↔ current daemon; old `/attach` raw clients unchanged (`BridgeLegacy` golden); `PlanFrame.fidelity` additive and ignored by old clients.
- [ ] Fixtures on already-supported direct **shell** runtime and on a **mirrored legacy agent** (observe-only): multiple viewers, one controller, takeover, expired/stale lease, controller handoff, `input_id` dedupe across reconnect, human-over-warden precedence, cancellation, partial/invalid/oversized WebSocket frames, read-only token never gets the lease or raw bytes, close-viewer-is-not-stop, reconnect-never-relaunches.
- [ ] Detach chord: `.`/`t`/doubled, configurable key, chord never forwarded, chord prefix flush on timeout, chord during reconnect, never produces input through a stale generation.
- [ ] Local terminal restoration on every exit path (detach, child exit, daemon loss, context cancel, SIGTERM/SIGHUP/SIGINT) incl. alt-screen, bracketed paste, mouse modes re-asserted/cleared.
- [ ] Snapshot/replay (§6.6): fixtures proving replay→live has 0 gap/dup/reorder across epoch reset, adoption, ack resume, too-old cursor, slow-viewer `resync`; **a test that a journal snapshot is never presented as `exact-live`**; mode-tracking extension (or documented repaint-only limitation with a test that fails when the limitation text and behavior diverge).
- [ ] Secrecy: planted secrets in the live plane never appear in SSE/journal/audit/trace/metrics/crash excerpts after an attach/reconnect cycle (extend `F-PRIV-01`).
- [ ] Bounds: viewer cap (16), snapshot cap (1 MiB), inbound frame cap, pending-input cap; each overflow returns an actionable message (cause + next step).
- [ ] Resize arbitration + SIGWINCH for controller vs viewer; reconnect re-sends size.
- [ ] Docs: protocol compat, detach chord, raw-vs-redacted snapshot limits in `docs/specs/2026-10-19-pty-runtime-contract.md` §6 pointer, `site/src/content/docs/guides/pty-runtime.md`, `docs/FEATURES.md`; CLI help for `attach` updated + `make gendocs`.
- [ ] Production AI launch stays tmux; no TUI/Cockpit rewiring in this task (02b-02 consumes the interface).
- [ ] `wd check`, `make verify-fast`, `-race` on `ptyattach`, `ptyhost`, `ptyruntime`.

---

## 9. Remaining readiness gates for later plans

Gates are **operator-checked**; the task DAG does not enforce them.

| Gate | Plan | Exit criteria (all required) |
|---|---|---|
| **G-02b-02** UI parity | 02b-02 | CLI+TUI+web list/tree/detail/attach consume `runtime` identity; agent attach through the shared client for direct and `Fallback` for tmux; no `TmuxSession != ""` liveness proxy remains in `tui/`; terminal title/cwd/liveness truthful or explicitly "unavailable" for direct; scripted TUI/CLI/browser integration (not mocked tmux argv only); fresh/empty/mixed-fleet/stale-selection cases; Closing/recreating a pane never kills the child or leaves duplicate attach clients. |
| **G-02b-03** launch + recovery | 02b-03 | `routed` guard replaced by a host-aware agent launch (§6.4); all §3.1–3.2 entrypoints launch/restore/relaunch correctly for each **direct-capable** adapter and are explicitly gated for the rest; direct agent + shell run with tmux unavailable (scripted fake CLI, no `tmux` on `PATH`); holder-hosted agent survives daemon graceful stop/update/SIGKILL (child pid + conversation id identical, no duplicate); adoption works with global mode `off`; partial-launch (`launch_id`) crash tests; daemon-absent output policy chosen + measured (§6.9); holder-death/recycled-pid/stale-credential tests; `G2` re-closed with evidence, not documentation. #918/#919 interactions recorded per §7. |
| **G-02b-04** authority | 02b-04 | Direct agent status and prompt decisions event-authoritative; legacy poller/engine mutually exclusive **structurally** for a session (not by map lookup); all automatic sends/seeds/resumes use the lease and TOCTOU fence; fixtures for known/changed/unknown permission + strategy prompts for **every** direct-capable adapter; shell/nested-CLI/password never automated; metrics distinguish shadow/direct/legacy; trace redaction verified. |
| **G-02b-05** evidence + rollout | 02b-05 | §6.9 budgets measured on direct agents (not just terminals); full adversarial/fidelity matrix on supported OS (Linux, macOS); mixed-fleet and upgrade/downgrade (old binary vs new records, §6.3 rule 2) tests; **observed** production evidence kept separate from fixtures: operator-authorized small direct cohort → ≥ 20 sessions × ≥ 24 h per cohort → expand → natural legacy drain → rollback verified; `RUNBOOK-pty-rollout.md` corrected to the verified behavior; plan-03 handoff checklist. |
| **G-03** plan 03 entry (`plan-5cd3c58b`) | 03 | 02b-01…05 merged, **released and deployed**; supported agents and shells demonstrably direct in production metrics; no supported workflow needs tmux as process owner; legacy inventory drained; unsupported compatibility explicitly retained; #918/#919 resolved or their blocked scenarios re-run. **Merge of any PR does not authorize cleanup.** |

Not claimed anywhere in this document: that any production session has run direct as an AI agent, that survival of an agent has been observed under a warden holder, or that the budgets hold for a live provider CLI. Those are exactly what 02b-03 and 02b-05 must produce.

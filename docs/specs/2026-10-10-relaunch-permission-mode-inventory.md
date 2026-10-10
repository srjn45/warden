# Relaunch / resume permission-mode inventory and compatibility policy

Status: inventory + policy (autopilot run ap-76f2e30cc1b2, task `relaunch-path-inventory`).
No behaviour change ships with this note; it is the handoff for the resolver /
implementation tasks. Code added alongside: `PermissionIntent.Rank/AtMost`
(`internal/agentbackend/permission.go`) and guard tests in
`internal/agentbackend/backends/permission_test.go`.

## 1. Relaunch / resume paths (existing agent, new process)

All terminate in `resumeInTmuxWithHints` (`internal/lifecycle/lifecycle.go`) →
`Backend.ResumeCmd(ResumeOpts{Mode})`, or `launchSuccessor` (`internal/lifecycle/switch.go`) →
`Backend.LaunchCmd`.

| # | Path | Mode source today | Translated / validated? |
|---|------|-------------------|-------------------------|
| 1 | `Lifecycle.Restore` (lifecycle.go ~2311) | `agent.PermissionMode`, else `config.GetDefaultPermissionMode()` | **No.** Passed verbatim to the agent's own backend. |
| 2 | `Lifecycle.SwitchRole` (~2365) | same as #1 | **No.** Also does not re-derive role posture (`applyRoleBackendMode`), so a role switch keeps the old role's mode. |
| 3 | `Lifecycle.Adopt` resume mode (~2433) | `config.GetDefaultPermissionMode()` on the default (Claude) backend | n/a — Claude vocabulary, Claude backend. Not persisted from agent. |
| 4 | `Lifecycle.HotSwap` → `successorMode` (switch.go ~239) | stored mode; same-backend keeps it unchanged, cross-backend `TranslateMode` then `fallbackMode` | **Yes** (only path). Same-backend branch does not validate the stored string. |

Callers (all reach #1 unless noted):

- `internal/daemon/lifecycle_adapter.go` `Restore` (used by schedulers/recovery) and `SwitchRole` (#2); `HotSwap` at :262.
- `internal/daemon/strict_lifecycle.go:517` `RestoreSession` (operator restore, TUI `r`, MCP/CLI `restore`), :771 `SwitchRole`.
- `internal/daemon/autorestart.go:88` auto-restart of errored agents.
- `internal/daemon/ratelimit.go:296` rate-limit resume.
- `internal/daemon/strict_projects.go:374` project hibernation reopen.
- `internal/daemon/backend_recovery.go:410` (Restore, same pool) and :415 (HotSwap, other pool/backend).
- `internal/daemon/recover.go` `Server.Recover` (reconcile orphans; reaches Restore via RestoreSession semantics).
- `internal/daemon/strict_models.go:180` model switch (HotSwap).
- `internal/daemon/autopilot_runtime.go:139` `RotateBrain` (HotSwap).
- `internal/cli/daemon.go:530` (HotSwap), `internal/tui/cmds.go:294`, `internal/mcp/server.go:790`, `internal/repl/registry.go:396` (clients of the restore route).

Role-derived defaults (spawn-time, but feed the persisted value later relaunched):
`resolveRole` → `applyRolePermissionOverrides` (Claude vocab) → `applyRoleBackendMode`
(`plannerModeForBackend`/`workerModeForBackend`/`autonomousModeForBackend`) in
lifecycle.go ~380-520. `SwitchRole`/`HotSwap(Role)` do not call these.

Write side: `PATCH …/permission-mode` (`strict_lifecycle.go:719`) and the spawn route
(`lifecycle_routes.go:66`) validate against `lifecycle.PermissionModes` — the
**Claude** vocabulary only — regardless of the agent's backend. Role spawns
legitimately persist backend-native values (`workspace-write`, `force`, `yolo`…).
So a stored value is "some backend's vocabulary, unlabelled".

## 2. Hazards found (why the policy is needed)

1. **Foreign string into Claude.** Claude `base()` always emits `--permission-mode '<mode>'`
   verbatim. A record whose backend is claude but whose mode is native to another
   backend (after a failed/partial swap, manual DB edit, or legacy data) makes the CLI reject its args.
2. **Antigravity fails open.** `agyPermFlag`: `""`, `default`, and *any unknown string* →
   `--dangerously-skip-permissions`. A restore of an agy agent with stored `default` or empty mode
   runs skip-all. (`agyModeTable` classifies `default` as IntentDefault; the launch seam disagrees.)
3. **Launch seams fold "dontAsk" and "auto" upward.** codex (`codexSandbox`: `auto`,`dontAsk` →
   danger-full-access + never), cursor (`cursorModeFlag`: `auto`,`acceptEdits`,`dontAsk` → `-f`),
   aider/crush/opencode similar. Claude `dontAsk` means *deny unapproved tools*, i.e. restrictive, yet
   it is escalated to skip-all on those backends.
4. **Unclassified Claude modes.** `claudeModeTable` omits `auto` and `dontAsk`; `agy` omits `sandbox`;
   cursor omits `auto-review`; goose omits `smart_approve`. `TranslateMode` returns ok=false and the
   swap falls back with **no stored intent**, so `fallbackMode`'s non-escalation guard is skipped
   (`storedIntent == ""`) and e.g. a general-role claude `auto` agent can land on goose `auto` (skip-all).
5. **String collision.** `auto` is Claude's classifier mode (between accept-edits and skip-all) but
   goose's `auto` is skip-all; `default` is Claude's prompt posture, codex has no equivalent.
6. **Empty mode** is resolved with the *Claude-vocabulary* config default (`GetDefaultPermissionMode`)
   on non-Claude backends in Restore/SwitchRole (#1, #2); only `fallbackMode` in HotSwap translates it.
7. **No persistence of the corrected mode** after a successful translation except HotSwap's caller.
   Restore never writes back.

## 3. Compatibility policy (to implement in the resolver task)

### 3.1 Intent ordering

```
plan = read-only (0)  <  default (1)  <  accept-edits (2)  <  skip-all (3)
```
Implemented as `PermissionIntent.Rank()` / `AtMost()`; unknown intents rank 3 (fail closed).

### 3.2 Resolution algorithm for any relaunch of an existing agent

Input: agent (backend B, stored mode S, role R), target backend T (= B except HotSwap).

1. **Classify the stored intent I(S).**
   - S empty → intent of the *role default for B* if R is set, else the configured default
     (config default is Claude vocabulary: classify with Claude's table), else `default`.
   - S non-empty: look up in B's own table. If missing, look up in the **legacy map** (§3.4);
     if still missing, try every registered backend's table only if all matches agree
     (otherwise unknown).
   - Unknown ⇒ treat as the most conservative classifiable intent: `default`
     (never skip-all). Surface a note/event.
2. **Render for T**: `T.ModeForIntent(I)`; the result must be in `T.Capabilities().PermissionModes`
   and `back = T.ModeIntent(result)` must satisfy `back.AtMost(I)` (guard test enforces this for
   every backend table).
3. **No equivalent**: step *down* the order to the nearest lower intent T supports
   (skip-all → accept-edits → default → plan/read-only). Never step up. If T supports none,
   refuse the relaunch (ErrNoSafeMode) rather than guess.
4. **Role posture** (planner/worker/autonomous) may only *tighten* the result for non-HotSwap relaunches
   (role defaults are applied at spawn; relaunch must not widen them).
5. **Same backend**: if S is accepted by B and classified, keep S byte-for-byte. Otherwise apply 1–3 and
   persist the corrected value (Restore/SwitchRole/HotSwap alike) with an audit event.
6. Launch seams must not widen: unknown or empty mode ⇒ backend default posture, never skip-all
   (fix `agyPermFlag` default branch; stop folding `dontAsk` upward).

### 3.3 Backend accepted modes and intent mapping (current tables)

| backend | accepted `PermissionModes` | native → intent | intent → native (render) |
|---|---|---|---|
| claude | acceptEdits auto bypassPermissions default dontAsk plan | default, plan, acceptEdits, bypassPermissions | default, plan, read-only→plan, accept-edits, skip-all |
| codex | read-only workspace-write danger-full-access | same three (ro, accept-edits, skip-all) | plan/read-only→read-only, accept-edits, skip-all; **no default** |
| cursor | default plan ask auto-review force | default, plan, ask(ro), force | default, plan, read-only→ask, skip-all; **no accept-edits** |
| antigravity | default plan accept-edits sandbox dangerously-skip-permissions | default, plan, accept-edits, acceptEdits, skip-perms | default, plan, read-only→plan, accept-edits, skip-all |
| opencode | default plan dangerously-skip-permissions | default, plan, skip | default, plan, ro→plan, skip-all; **no accept-edits** |
| aider | default yes-always | default, yes-always | default, skip-all only |
| crush | default yolo | default, yolo | default, skip-all only |
| goose | auto approve chat smart_approve | approve→default, chat→plan, auto→skip-all | default→approve, plan/ro→chat, skip-all→auto |

Step-down examples: claude `acceptEdits` → cursor/opencode/aider/crush `default` (not `force`);
claude `default` → codex has no default ⇒ `read-only` (lower) is acceptable, never `workspace-write`.

### 3.4 Legacy / unclassified persisted values

| value | where seen | intent (conservative) |
|---|---|---|
| `""` | pre-roles records, general role | resolved per §3.2.1 |
| `auto` (claude) | worker role on claude | accept-edits (classifier-approved; not skip-all) |
| `auto` (goose) | worker/autonomous on goose | skip-all (goose's own table) — disambiguate by agent backend |
| `dontAsk` | claude only | plan/read-only-equivalent (deny unapproved) — never escalate |
| `auto-review` | cursor | default |
| `smart_approve` | goose | default |
| `sandbox`, `proceed-in-sandbox` | antigravity | default (no skip-all; warden never passes `--sandbox`) |
| `acceptEdits` on agy | legacy | accept-edits (already in agy table) |
| `yes-always`, `yolo`, `force`, `dangerously-skip-permissions`, `bypassPermissions`, `danger-full-access` | any | skip-all |
| `workspace-write` | codex | accept-edits |

Classification is by **(agent backend, value)**, never by value alone, because of the `auto` collision.

## 4. Handoff for the resolver task

- Add one `ResolveRelaunchMode(agent, targetBackend, cfg) (mode string, note string, err error)` in
  `internal/lifecycle` implementing §3.2; have `Restore`, `SwitchRole`, and `successorMode` call it.
- Extend each backend's `ModeTable` with the legacy rows in §3.4 (or a shared legacy map keyed by
  backend) so `storedIntent` is never empty for a non-empty stored value.
- Persist the corrected mode through `agentstore.UpdatePermissionMode` + event in the daemon callers
  (`lifecycle_adapter.Restore`, `strict_lifecycle.RestoreSession`, `autorestart`, `ratelimit`).
- Fix launch-seam fail-open (`agyPermFlag` default; `dontAsk` folding) behind the resolver so seams only see valid native modes.
- Tests to add: per-path table tests (Restore/SwitchRole/HotSwap) asserting
  `ModeIntent(result).AtMost(I(stored))`, plus the §3.4 table as fixtures.

## 5. Resolver landed (task `shared-safe-mode-resolver`)

`lifecycle.ResolveRelaunchMode(RelaunchModeInput) (mode, RelaunchRationale, error)`
(`internal/lifecycle/relaunch_mode.go`) implements §3.2 as a **pure** function: no
store write, no event. It returns the accepted native mode plus a structured
rationale (stored intent + source, accepted intent, outcome kept/translated/
stepped-down/defaulted, role-tightened flag, reasons) and `ErrNoSafeMode` when the
target has nothing at most as permissive. The legacy map of §3.4 lives in the
resolver (keyed by backend id). It is not yet wired into Restore/SwitchRole/
HotSwap, nor does it persist; those are follow-up tasks (persist only after a
successful launch).

## 6. Resolver wired into every relaunch path (task `apply-to-all-relaunch-paths`)

`Lifecycle.Restore`, `SwitchRole`, `HotSwap` and resume-mode `Adopt` now all call
`ResolveRelaunchMode`; the old `successorMode`/`fallbackMode` and the raw
`agent.PermissionMode || config default` launches are gone. Every daemon relaunch
(auto-restart, rate-limit resume, hibernation reopen, backend recovery, recover,
operator restore, model switch, brain rotate, poller hot-swap) reaches one of these.

- **Refuse before retiring.** `ErrNoSafeMode` is returned before the tmux session is
  created (Restore) or killed (SwitchRole, HotSwap), so a refusal leaves a live agent running.
- **Persist only after launch.** The corrected mode is applied to the record, and
  `Lifecycle.OnModeNormalized` fires, only after the replacement launched (HotSwap:
  after the liveness verify). A failed launch changes nothing and emits nothing.
- **Durable record.** The daemon hook (`daemon.NewModeNormalizedHook`) persists the
  mode, appends an agent event `relaunch-permission-mode`, and writes an audit record
  `relaunch_mode_normalized` (path, from/to mode, outcome, intents, backends).
- **Empty stored mode** launches with the configured default when the target accepts
  it (byte-compatible with before), otherwise a rendered, never-wider mode. It is
  audited but not persisted, so the record keeps tracking the config default.
- **Behaviour changes** (all non-escalating): claude `auto`/`dontAsk` and other modes
  no longer fold upward on cursor/codex/etc.; a target with no mode at most as
  permissive as the stored intent (e.g. claude `plan` → aider) refuses the swap.

## 7. Regression validation and compatibility (task `backend-mode-regression-validation`)

### 7.1 Seam audit (every existing-agent relaunch)

| Seam | Mode source | Status |
|---|---|---|
| restore / auto-restart / rate-limit resume / hibernation reopen / recover / backend-recovery same-pool | `Lifecycle.Restore` → `ResolveRelaunchMode` | covered (§6) |
| switch-role | `Lifecycle.SwitchRole` → resolver | covered (§6) |
| hot-swap: model switch, brain rotate, poller swap, backend-recovery other pool, **handoff/rotate** | `Lifecycle.HotSwap` → resolver | covered (§6) |
| adopt (resume mode) | resolver on the default backend | covered (§6) |
| **fork** (`fork_from`) | previously the request mode, else the raw Claude-vocabulary config default — the fork could run **wider** than a restricted source | **fixed here**: the adapter threads `ForkSourceMode`; with no explicit mode the fork resolves it through `ResolveRelaunchMode` on the source's backend (same-backend, role tightening only) and refuses with `ErrNoSafeMode` before any side effect. The "explicit mode" test is taken *before* role defaults fill the request, so a worker/autopilot fork of a read-only source stays read-only (role only tightens). An explicit `--permission-mode` is the operator's choice and is kept. A translated/defaulted fork mode is audited (`relaunch-permission-mode` event + `relaunch_mode_normalized`, path `fork`) via `Lifecycle.EmitForkNormalization`, called by the spawn route after the record is stored; a failed launch/insert emits nothing. |

Fresh spawns (`Spawn`, `SpawnJob`) are not relaunches of an existing agent and keep
request → role → config-default behaviour.

### 7.2 Compatibility

- Stored modes that are valid for the agent's backend are launched byte-for-byte; no
  record changes and no audit is emitted (`outcome=kept`).
- Invalid/legacy/foreign-vocabulary values are translated by intent; a target without
  an equivalent steps strictly **down**; if nothing is at most as permissive the
  relaunch is refused (`ErrNoSafeMode`) and the live agent keeps running.
- A corrected mode is persisted, and `relaunch-permission-mode` /
  `relaunch_mode_normalized` audited, only after the replacement process launched.
  Failed or refused relaunches leave the stored mode and audit untouched.
- Empty stored mode is not frozen: it keeps tracking the configured default.
- Fork inherits the source's mode instead of the config default (new, tightening only).

### 7.3 `done_when` evidence

| Requirement | Test |
|---|---|
| lifecycle across supported backends, invalid legacy modes, role fallbacks | `TestRelaunchSeams_LifecycleMatrix` (restore + switch-role × every backend × own/foreign/legacy/unknown/empty modes × role ``/planner), `TestResolveRelaunchMode_Table`, `TestResolveRelaunchMode_RoleConfigFallbacks` |
| successful translation audit | `TestHotSwap_TranslationAudit`, `TestRestoreNormalizesStoredModeAfterSuccessfulLaunch` |
| launch failure preserves stored mode and audit | `TestRelaunch_LaunchFailurePreservesStoredModeAndAudit`, `TestHotSwap_LaunchFailureKeepsModeAndAudit`, `TestRestoreLaunchFailureDoesNotPersistOrReport` |
| unrepresentable restrictive intent rejected | `TestRelaunch_UnrepresentableRestrictiveIntentRefused` |
| strict non-escalation | `TestResolveRelaunchMode_NonEscalationMatrix` (all backends × modes × roles), plus the `AtMost` assertions in every matrix/translation test |
| fork seam | `TestSpawnFork_InheritsSourceModeNeverWider`, `TestSpawnFork_ExplicitModeWins`, `TestSpawnFork_RoleDefaultsNeverWidenSource`, `TestSpawnFork_NormalizationAuditedAfterStore`, `TestSpawnFork_LaunchFailureEmitsNothing`, `TestAdapterForkInheritsSourcePermissionMode` |

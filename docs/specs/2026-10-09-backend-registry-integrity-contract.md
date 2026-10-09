# Backend-registry integrity contract (issue #841)

Status: design record (t1 baseline). Executable counterpart:
`internal/backendstore/integrity_contract_test.go` + `integrity_helpers_test.go`
(skipped `TestContract*` tests carry `TODO(t2..t4)` and are the acceptance gates
for later tasks). Sibling of
`2026-10-06-agent-store-integrity-contract.md` (agent store, #795) — agent-store
semantics are unchanged by this work.

## 1. Incident

`wd update` v9.25.0 → v9.27.0 rolled back: the new daemon exits at startup because
`<data>/backends` (collection `backends`) fails scriva's open-time integrity gate
(default `IntegrityPolicy=fail`) with 34 × `conflict-revision-regression`
(`id 1 update has revision 68 after revision 70`). v9.25.0 opened the same
store. Trigger of the second writer is unconfirmed (candidate: a manual
`warden daemon` / `wd models` / `wd role` beside the systemd daemon).

## 2. Collection and consumer map

Single ScrivaDB directory `<data>/backends` (`backendstore.NewStore`), six
collections, all opened with `SyncModeNone` and **no explicit IntegrityPolicy**
(⇒ fail):

| Collection | Key | Content (user-owned facts in **bold**) | Writers |
|---|---|---|---|
| `backends` | backend id; reserved `__settings__` | detection (Installed/BinaryPath/DetectedAt) is derived; **Tier, Enabled, Default**, LimitedUntil; settings row: **InternalThinkingMode, AllowPaidAutopilot** | `Reconcile` (detection only), `SetTier/SetEnabled/SetDefault/SetThinkingMode/SetAllowPaidAutopilot`, `SetLimited` |
| `models` | `backend\0model` | catalog; **Tier, Enabled, custom entries (IsCustom), QuotaScope** | `AddModel/SetModelTier/SetModelEnabled/UpsertModel`, seed, discover |
| `role_tiers` | role name | **role → default model tier** | `SetRoleTier`, seed |
| `handover_settings` | `__handover_settings__` | **Enabled, ContextFillThreshold, CooldownPeriod** (+2 deprecated thresholds) | `SetHandoverSettings` |
| `quotas` | `backend:scope` | **limits/window config**; usage window + events (semi-derived, `RecordQuotaUsage`, usage sync) | quota.go, `backendusage` sync |
| `rl_cooldowns` | `backend\0model` | confirmed-hard-limit cooldown evidence (expires; fail-open reads) | `SetRLCooldown` |

Only detection fields are rebuildable (`rescan_backends` / `Reconcile`). Tier,
enabled, default, settings, models, role tiers, handover, quotas and cooldowns are
**not derivable** — the issue's "rebuild by rescan" loses them, so the registry is
*derived only in part* and must never be recreated blindly.

Persistence consumers (all go through `*backendstore.Store`; there is no other
raw open of this dir):

| Consumer | Opens store? | Notes |
|---|---|---|
| Daemon `internal/cli/daemon.go` | **yes** (`NewStore`, then `Reconcile`) | open error ⇒ daemon returns error and exits; the incident |
| CLI `openBackendStore` (`internal/cli/models.go`; used by `models`, `role`) | **yes, direct, daemon-oblivious** | in-process second open is refused by scriva ("database already open"); cross-process exclusivity is **not** guaranteed by warden and is the prime second-writer suspect |
| Router resolver, capacity resolver, `backendusage` service/snapshots, daemon `backend_recovery`, lifecycle poller/switch, pipeline, autopilot (via resolver/API), TUI/CLI/MCP/web (via REST) | no — receive the daemon's handle | read/write through the daemon only; all inherit daemon boot failure |
| `usage-snapshots`, `quota-impact` | separate dirs, out of scope | |

## 3. What scriva v1.4.0 can and cannot repair

Observed by `integrity_contract_test.go` against a programmatically built
damaged store (no real data dir is ever read):

- `engine.VerifyDir(Full)` reports revision regressions as severity **Conflict**,
  code `conflict-revision-regression`, per collection; untouched collections are clean.
- Strict open (`PolicyFail`) refuses, leaves bytes untouched, error is
  `*OpenIntegrityError` (`errors.Is(engine.ErrIntegrity)`).
- `engine.Repair` **never resolves conflicts** (`ConflictReport` lists them,
  `ConflictAbort` returns `ErrRepairConflict` before any mutation/backup).
  Repair-safe (automatic): torn tail, stale/missing index/sidx rebuild, damaged
  segments only with `Salvage` (quarantined, backed up). Operator/our decision:
  every conflict (duplicate ids, id reuse, write-after-delete, revision regression).
- ⚠ **Surprise 1:** after a report-mode `Repair`, the history conflict *remains on
  disk* (Verify still reports it) but the persisted rebuilt index makes the next
  strict open succeed — silently serving last-write-wins values. Repair alone
  *hides* the regression; it is not recovery.
- ⚠ **Surprise 2:** `PolicyReport` open resolves history last-write-wins, so the
  **stale replayed update wins** (a tier the user changed later reverts), and
  under that tolerant open the `_key` secondary index is not rebuilt:
  `GetByKey` returns `ErrKeyNotFound` while `Scan` returns rows. `Store.get`,
  `upsert`, `SetTier` … are all key-based and would misbehave. A salvage-open
  handle must be used only for read-only extraction, not as the live `Store`.
- Because the revision number is only a conflict *signal*, scriva cannot say which
  record is "right"; the append-order/`ts` of the line is the only evidence.

## 4. Preservation set

`requireRegistryPreserved` (test helper) is the single definition of "recovery
lost nothing": backend rows (tier, enabled, default, detection), default backend,
settings, models, role tiers, handover settings, quotas, rate-limit cooldowns.
`buildRegistry`/`injectRevisionRegressions`/`damagedRegistry` build the fixture in a
`t.TempDir()` (history with revisions > 1 per row, a stale CRC-valid update
re-appended, derived index files removed so the open gate scans).

## 5. Contract for t2–t5

1. **Strict stays strict for authoritative stores** (agents-db and everything not
   named here). Relaxation, if any, is per-store and explicit; never a global default change.
2. **Recovery is backup-first and offline-only**: verified backup outside the data
   dir (reuse `engine.Repair` backup semantics / `RepairOptions.BackupDir`), no
   live handle on the dir, original bytes preserved/quarantined, atomic install,
   resumable. The daemon never self-repairs while serving.
3. **Resolution is explicit, not last-write-wins by accident.** t2 extracts the
   user-owned facts from the damaged collection (read-only salvage handle),
   chooses per key by an auditable rule (latest `ts`/append order — record the
   rule and every discarded revision in a report), writes a clean collection and
   proves it with `requireRegistryPreserved` against the *latest-writer* state.
   Unresolvable keys are reported, never guessed.
4. **No loss on the fallback**: if recovery cannot run, the daemon must still be
   able to start in a degraded, clearly-reported mode (t3) rather than refuse
   (the issue's expectation), without silently reverting tiers/default.
5. **Surfaces (t3–t5):** daemon startup logs findings + the repair command;
   `wd update` preflights `VerifyDir` on the backends store *before* restarting
   and prints findings + repair command (t4); CLI direct opens (`models`, `role`)
   route through the daemon or take the same ownership guard (t3/t5); docs
   (`doctor`, site guide, CLI reference via `make gendocs`) (t5).
6. Second-writer prevention is a separate hardening item (mirror agent-store
   `.agents-store.lock`); the lock must be its own file, not the engine's.
7. No REST/OpenAPI change is required by t1; any later one is spec-first
   (`openapi.yaml` → `make generate`).

## 6. Open items

- Which process produced the second writer (CLI direct open vs second daemon).
- Whether to open `rl_cooldowns`/`quotas` (volatile) with a more tolerant policy
  than `backends`/`models`/`role_tiers`/`handover_settings` (user-owned).

## 7. Recovery design (t2, implemented in `internal/backendstore/integrity.go`)

### 7.1 Classification (`Verify`)

Per collection, from a full `engine.VerifyDir`:

| Findings | Verdict |
|---|---|
| none above info | `clean` |
| only `repairable-index` (stale/missing index) | `recoverable` (scriva rebuild, lossless) |
| `data-corruption` | `ambiguous` — salvage is never automatic |
| `conflict` other than `conflict-revision-regression` (duplicate id, id reuse, write-after-delete) | `ambiguous` |
| `conflict-revision-regression` only | `recoverable` iff **every** regression line is provably stale, else `ambiguous` |
| any finding outside the six registry collections | `ambiguous` |

**Provably stale** uses one of two per-line rules, recorded in the audit report.
`newest-revision-with-concordant-timestamp` accepts an `update` whose revision is
lower than the id's newest revision **and** whose `ts` is older than that newest
line's `ts`. `preferences-equal-detection-only` accepts a lower-revision line even
when its `ts` is newer if it agrees with the winner on every non-rebuildable field.
For `backends`, only `installed`, `binary_path`, and `detected_at` are rebuildable;
all other fields (including tier, enabled, default, settings, and limits) must be
equal. Other registry collections have no rebuildable fields. Thus the exact #841
shape—34 later revision-68 writes after revision 70 with only detection
differences—is safe: recovery retains the revision-70 preference record, backs up
first, and records every decision. **Ambiguous** (never resolved): any preference
field differs (the report names each field and both values); equal revision with
different content; a higher revision with an older `ts`; any delete on an affected
id; unparseable lines; or a pending compaction manifest.

### 7.2 Repair sequence (`Repair`, offline only)

1. Authority check + exclusive flock on the engine `LOCK` (live holder ⇒ `ErrOwned`).
2. `Verify`. Clean ⇒ no-op (idempotent: no backup, no report). Any ambiguous
   collection ⇒ `*RecoveryRequiredError`, **nothing mutated** (only a JSON report
   is written next to the backups).
3. Verified backup (copy + size/SHA-256 of every file) of the whole registry to
   `<backups>/backends-backup-<UTC>/` (default parent
   `<data>/backend-registry-backups`, never inside the registry dir).
4. Remove the stale lines (atomic temp+rename), drop the collection's derived
   index files, run `engine.Repair` (`ConflictAbort`, so it can never resolve a
   conflict itself), recreate the `_key` unique index (scriva does not invent it;
   without it keyed reads miss and the store would re-seed over user data), then
   a full `VerifyDir` must be clean.
5. Any failure ⇒ restore every collection from the backup (byte-identical) and
   return `*RecoveryRequiredError` with `BackupPath`, `ReportPath`, `Cause`.
6. Audit: one structured `slog` record per step (findings, backup, removed
   revision count, result) plus `backend-registry-report-<UTC>.json` listing every
   discarded line (segment, line, id, key, rev, ts, winner rev/ts, rule).

### 7.3 Exported API (for t4 preflight and t5 command)

```go
backendstore.Open(dir, Options) (*Store, *Result, error) // strict; auto-recovers a recoverable registry (daemon)
backendstore.Verify(ctx, dir) (*Report, error)           // read-only; Report.Clean()/Recoverable(), per-collection verdicts
backendstore.Repair(ctx, dir, Options) (*Result, error)  // explicit, backup-first, offline
backendstore.RecoveryRequiredError                       // errors.As; errors.Is(ErrRecoveryRequired); Collections, Severities,
                                                         //   ReportPath, BackupPath, RepairCommand, Cause (keeps engine.ErrIntegrity)
backendstore.RepairCommand       = "warden repair backends"
backendstore.RepairDryRunCommand = "warden repair backends --dry-run"
```
`dir` is the registry directory (`<data>/backends`). `NewStore` is unchanged and
strict; agent-store semantics are untouched. The daemon calls `Open`. CLI direct
opens (`models`, `role`) stay strict (t3/t5).

## 8. Data-directory ownership (t3, `internal/ownerlock`)

### 8.1 Open-site map (every opener of `<data>`)

| Site | What it opens | Role | Policy now |
|---|---|---|---|
| `internal/cli/daemon.go` (`warden daemon`) | every store: agent store, terminals, context, inbox, schedules, known-prompts, `backends` (via `backendstore.Open`), usage-snapshots, … | single writer | takes the ownership lock **first**, before `agentstore.New`; refusal exits before any store/listener/goroutine |
| `openBackendStore` (`internal/cli/models.go`) → `wd models list/set/import…`, `wd role …` | `<data>/backends` only | direct CLI | `ownCLIDataDir`: refused with guidance while a daemon owns the dir; otherwise takes the same lock (kind=cli) for the command's life so a daemon cannot start mid-write |
| `backendstore.Repair` / `Open`→Repair (`warden repair backends`, t5) | `<data>/backends`, offline | repair | probes the parent data dir; refuses (`ErrOwned`+`ownerlock.ErrOwned`) while a *foreign* process owns it; the owning process itself is exempt (the daemon's own `Open` auto-recovery) |
| `backendstore.Verify` | read-only | verify | never takes or checks the lock (read-only scan; engine LOCK reported as Info) |
| `doctor.go` (`agentstore.New`) | agent store | read | unchanged (agent-store lock, #795 semantics) |
| `agentstore` writer lock `.agents-store.lock` | agent store | guard | unchanged; now nested **inside** the data-dir lock for the daemon |

All backend-registry writes go through `*backendstore.Store`; the only processes that construct one are the daemon (after the lock) and `openBackendStore` (after the lock) — verified by grep of `backendstore.NewStore/Open` outside tests. Every other consumer receives the daemon's handle or goes through REST/MCP.

### 8.2 Lock contract

- File `<data>/.warden-owner.lock`, exclusive non-blocking `flock` (open-file-description scoped: a second acquire in the same process is also refused; the kernel releases on process death — **no stale-lock cleanup, no PID-liveness guessing**, so a crash can never cause a false refusal).
- Content is advisory JSON `{pid, started, version, kind: daemon|cli, launch: systemd|manual, addr, command}`; it is only read while the flock is actually held by someone else. `launch` is `systemd` when `INVOCATION_ID`/`JOURNAL_STREAM` is set (systemd sets them for units), else `manual`.
- `ownerlock.Acquire` → `*OwnedError` (wraps `ErrOwned`) with owner PID, launch mode, version, start time, address and `NextStep()`:
  - systemd owner: `systemctl --user status warden`; stop with `systemctl --user stop warden` only to replace it; never start a second `warden daemon`.
  - manual owner: stop that process (PID given) or use it; stop it before enabling the service.
  - CLI owner: wait and retry.
  - Never suggests deleting the lock file.
- `ownerlock.Probe(dataDir)` is the non-blocking foreign-owner check; own-PID ownership reads as unowned.
- The engine's own `<data>/backends/LOCK` (t2 `dirLock`) stays as an inner defence for the registry dir alone; the data-dir lock is the authority and sits above it (it spans every store, which engine LOCK cannot).

### 8.3 Direct-CLI safe modes

| Situation | Behaviour |
|---|---|
| Daemon owns dir, CLI **writer** (`models set/import`, `role set`) | refused: error names the daemon (PID/launch/addr) and points to daemon-backed REST/MCP (`set_model_tier`, `set_role_tier`, …) or stopping the daemon |
| Daemon owns dir, CLI **read-only** (`models list`, `role list/show`) | currently the same refusal (the daemon is the only safe reader of a live engine); the REST/MCP equivalents (`list_models`, `list_roles`, `list_backends`) are the supported live view. Rerouting these CLI commands through `internal/client` is possible follow-up work |
| No daemon | CLI takes the lock (kind=cli), runs, lock dropped at exit; a daemon started meanwhile is refused with "wait for the CLI command" |
| `Repair` with a daemon running | refused; `Verify` still allowed |

### 8.4 Tests

`internal/ownerlock` (real subprocesses of the test binary): two/eight racing owners → exactly one, loser exits <5 s with PID + systemctl guidance; SIGKILL leaves no stuck lock; same-process re-acquire refused / own-probe nil. `internal/backendstore/contention_test.go`: six concurrent writer subprocesses × 3 rounds against one temp data dir under the lock → registry `Verify` stays clean (no revision regression, no split brain); `Repair` refused while a foreign process owns the dir. `internal/cli/ownership_test.go`: CLI direct open refused under a foreign daemon, and CLI-held lock blocks a daemon acquire. `TestContractDaemonStartDoesNotRefuseOnDerivedStore` (unskipped): the daemon open path recovers a regressed registry and preserves all user facts while holding the lock.

### 8.5 Notes for t5/t6

- Docs should state: "run exactly one daemon per data dir; with the systemd user service, use `systemctl --user …`, not `warden daemon`".
- Not done here: a *degraded start* for **ambiguous** registry findings (contract §5.4). Daemon still exits with the typed `*RecoveryRequiredError` (repair command in message) in that case; recoverable regressions auto-heal. If a degraded mode is wanted it is a separate change.
- `wd update` (t4) may `ownerlock.Probe(dataDir)` to report/anticipate the owner.

## 9. `wd update` transaction contract (t4, `internal/updater/transaction.go`)

`warden update` is an explicit transaction over small interfaces
(`ServiceController`, `Prober`, `BinaryInstaller`, `Clock`; real implementations
in `service.go`, fakes in the tests).

```
resolve release ─▶ capture ─▶ preflight ─▶ download+verify ─▶ swap ─▶ codesign ─▶ migrate
                                   │                                                  │
                          PreflightError (no change)                              restart
                                                                                      │
                                                  ┌── rollback ◀── any failure ── readiness
                                                  ▼
                               restore binary → restart service → verify PRIOR version healthy
```

1. **Capture**: binary path+version, service kind (`systemd-user` / `launchd` /
   `manual` / `none`), whether the daemon answers `/healthz` and its version.
2. **Preflight** (before download and swap): `backendstore.Verify` on
   `<data_dir>/backends`, read-only, using the *running* binary's classification.
   Ambiguous/unreadable findings → `PreflightError` with the findings and
   `backendstore.RepairCommand`; nothing changes. Recoverable findings are
   printed as notes and do not block (the new daemon's `Open` repairs them).
3. **Swap/restart**: atomic binary swap (`.bak` kept until success), codesign,
   config migrate, restart via the service manager. With no manager
   (`ErrNoServiceManager`) the binary is updated and the user is told to restart
   manually; no readiness check is possible.
4. **Readiness**: bounded backoff (250ms ×1.5, cap 2s) under one overall deadline
   (`--ready-timeout`, default 90s). Success needs `status == ok` **and**
   advertised version == target. Each poll also checks whether the supervised
   process exited.
5. **Diagnostics**: on failure the journal tail (`journalctl --user -u warden`) or
   `/tmp/warden.daemon.err` (launchd) is attached, so the real startup error
   (e.g. the integrity refusal) is shown rather than "connection refused".
6. **Rollback** (any failure after the swap): restore the previous binary; restart
   the service if the new daemon had been started; if a daemon was serving before,
   require it healthy on the prior version. The report always contains both the
   original failure and the rollback outcome.

### Error taxonomy (all `errors.As`-able; wrapped in `*FailureError` after a swap)

| type | meaning | message headline |
|---|---|---|
| `*PreflightError` | store blocker; nothing swapped | `update blocked before any change` |
| `*StartupFailureError` | process exited / restart command failed before healthy | `startup failure` |
| `*ReadinessTimeoutError` | no healthy response by deadline | `readiness timeout` |
| `*WrongVersionError` | healthy but advertises another version | `wrong version` |
| `*RollbackError` | restoring the previous version failed | `ROLLBACK FAILED` |

Limitations: the preflight uses the installed binary's `Verify`, not the new
binary's; a daemon-written startup-failure file is not required (journal/stderr
capture covers supervised daemons).

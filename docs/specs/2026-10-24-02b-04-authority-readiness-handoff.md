# 02b-04 handoff: direct-agent control authority readiness

## Scope and current safety boundary

02b-03 provides a host-aware launch seam and holder-backed recovery for a
**fixture-only** direct AI agent.  `ptyhost.Manager.AgentDirectReady()` remains
false by default and is not config-derived.  Do not turn it on in the daemon,
change a running daemon, or use this work as rollout authorization.

`web/dist/.gitkeep` is intentionally tracked: `web/embed.go` embeds `all:dist`,
and predecessor 02b-02 established the placeholder so a clean checkout builds
without first producing the UI bundle. It is a required build input, not an
ignored local artifact.

The direct path is safe to exercise only through the explicit test seam:

- `lifecycle.newAgentSession` records the pending direct launch and does not
  create a tmux pane.
- `lifecycle.launchAgent` starts `/bin/sh -c <native backend command>` through
  the host, preserving cwd, launch environment, backend argv, model,
  permission mode, and network scope; it stamps `Runtime.Kind=direct` only
  after successful child/holder creation.
- `ptyhost.routed.LaunchCommand` refuses an unplanned request, rejects an
  already live direct process, and adopts an existing holder if a prior launch
  reached child creation before the caller could persist its record.
- daemon recovery adopts only a live holder whose recorded PID/start/protocol
  identity matches.  It never starts a replacement agent; failure marks the
  existing record orphaned for an explicit restore.

## Audited lifecycle matrix

| Entry/control path | Shared seam or current behavior | 02b-03 evidence / remaining gate |
| --- | --- | --- |
| Plain CLI, REST API, MCP, TUI create | All construct a `lifecycle.SpawnRequest`; free-form and typed spawns reach `newAgentSession` then `launchAgent`. | Host-aware unit coverage; CLI/API/MCP/TUI transport parity remains existing coverage. |
| Pipeline, schedule, autopilot manager/worker, resolver | All job-style creation reaches `SpawnJob`, which uses the same `newAgentSession` → `launchAgent` pair and preserves pipeline/job env. | Direct fixture is covered by the common seam.  02b-04 must establish one authority before enabling it for live orchestrators. |
| Restore/adopt/restart | Current resume/adopt flow remains tmux-native. Direct lost-holder recovery marks orphaned and requires explicit recovery; it never silently relaunches a provider conversation. | Deliberate non-promotion: make direct resume/control authority-aware in 02b-04 before enabling it. |
| Fork | Fork uses typed spawn, so a fresh child reaches the common seam and retains its dedicated worktree rule. | Source/child transcript pinning and at-most-once retry authority remain 02b-04 work. |
| Handoff/rotation/backend/model switch | A live `Runtime.Kind=direct` agent is rejected before any handoff write, tmux probe, kill, or replacement launch. | `TestHotSwapRejectsExistingDirectAgent` proves no replacement child or tmux control action. |
| Restore/restart/role/permission changes | Restore and role switch reject a live direct record before legacy tmux control; stop remains process-host based. Fresh spawn preserves the native backend mode and permission flags unchanged. | `TestDirectAgentRejectsLegacyLifecycleOperations` proves the explicit fence. |
| Attach/UI and process-group stop | The direct-agent fixture exercises HTTP input/output, legacy attach resize, viewer close, and host process-group stop without tmux. | 02b-04 must add controller-lease fencing to these already-proven transport operations; it must not reintroduce a tmux fallback. |
| Worktree/history cleanup | Failed fresh launches use the shared cleanup helper and only remove a worktree created by that launch. Adoption never creates or deletes the pre-existing worktree. | Retain this invariant in the 02b-04 retry controller. |

## Failure, retry, and recovery evidence

The fixture suite covers the unsafe boundaries that must remain true when 02b-04
adds authority:

- `internal/ptyhost/agentlaunch_test.go`: direct launch defaults disabled;
  off/shadow remain tmux; direct agent plus shell starts with tmux unavailable;
  duplicate direct launch is refused; a holder whose adoption fails is
  fail-closed (one adoption attempt and no replacement launch).
- `internal/lifecycle/agentdirect_test.go`: host-aware launch preserves command,
  cwd and env, stamps runtime identity only after launch, and avoids an
  exit-capture shell wrapper. It also proves Restore and SwitchRole reject an
  existing direct runtime rather than invoking legacy tmux control.
- `internal/daemon/pty_adopt_test.go`: restart adopts the exact direct child
  after a mode rollback; missing holder and stale holder identity orphan without
  relaunch. Its direct-agent fixtures retain a bounded offline output burst and
  record an offline nonzero exit without relaunch. `internal/ptyholder/auth_test.go`
  rejects stale credentials and recycled PID identity.
- `internal/daemon/pty_runtime_test.go`: a direct agent accepts HTTP input and
  output, legacy attach resize, and viewer close without stopping the child;
  host stop then owns its process group. This is fixture transport coverage,
  not authorization to enable direct agents in production.

Prompt delivery is launch-line based where native adapters support it and
post-launch seeding only for `PromptSeeder` adapters. The current fixture
boundary never retries a direct task: `LaunchCommand` adopts an extant holder
instead of starting a duplicate child, while all direct legacy relaunch paths
are rejected. This is the at-most-once safety behavior for this slice; durable
seed-attempt ownership across an authoritative guardian retry is 02b-04 work.

Offline output uses the kernel PTY buffer while the daemon is absent. The holder
keeps the master descriptor but does not drain it, so output is bounded by that
buffer and the child blocks under normal PTY backpressure when it fills. The
fixture asserts a bounded burst only; it does not claim unbounded or lossless
offline capture.

## 02b-04 implementation checklist

1. Establish one durable direct-session authority/lease before a live direct
   launch.  Poller, guardian, restore, and operator controls must fence on its
   generation instead of independently spawning or input-seeding.
2. Add crash injection at each boundary: before/after child creation, holder
   metadata persistence, provider-ready observation, task seed persistence, and
   adoption.  Assert one child, one task start, and no orphan/recycled-PID
   adoption.
3. Keep adapter readiness, trust prompts, native mode/permission behavior,
   provider credentials, and env scopes byte-for-byte equivalent to the tmux
   path.  Unsupported adapters must return a typed capability result, never a
   silent runtime-label change.
4. Add controller-lease fencing around the already-covered direct-agent attach,
   resize/input, viewer-close, and supported process-group-stop operations;
   verify all cleanup paths leave unrelated worktrees, sessions, and history
   untouched.
5. Add guardian race tests that prove a recovery/adoption cannot cause a second
   manager or worker spawn.  Keep direct activation default-off until these
   tests and the authority implementation land.

## Explicit interactions blocked on #918 and #919

- **#918 — guardian lock contention:** do not put holder adoption, provider
  readiness, process control, prompt seeding, or any other PTY/runtime I/O
  under `autopilot.Controller.mu`.  A blocked guardian must not prevent status,
  shutdown, or release of an authority lease.  The requested lock-free
  snapshot/revalidate pattern and cancellation tests are prerequisites for
  trusting guardian-driven direct recovery.
- **#919 — plan-bound boot recovery wiring:** do not let a guardian attempt
  direct manager/worker recovery until `PlanSource` and all recovery
  dependencies are installed.  An empty `PlanFile` with a canonical `PlanID`
  must hydrate from ScrivaDB; otherwise a false degraded run could be mistaken
  for authority loss and produce duplicate or skipped direct launch attempts.

Neither issue is fixed by 02b-03.  They are explicit blocked scenarios for
02b-04's live autopilot authority work.

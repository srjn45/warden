# CLI command/help redesign: inventory and vocabulary decisions

**Date:** 2026-09-01  
**Status:** Frozen contract for the namespace implementation phases  
**Authority:** Supplements, and does not override, `2026-09-01-cli-command-help-redesign.md`.

This record freezes names only. It authorizes no REST, MCP, storage, lifecycle,
JSON, exit-code, confirmation, or safeguard change. The machine-readable
snapshot in `internal/cli/testdata/current_command_inventory.json` is generated
from a fresh `newRootCmd()` and records all 166 current Cobra nodes, their paths,
`Use`, aliases, visibility, runnable state, local/inherited flags, and JSON flag
support.

## Reconciliation findings

The approved mapping covers the current tree with these corrections/additions:

- `autopilot unregister` exists and controls a registered run through the same
  run-action endpoint family as start/pause/resume/stop. Canonical placement is
  `autopilot run unregister`; the old path remains compatible.
- `usage` already exists and reports provider subscription quota snapshots. It
  cannot also be the current `cost` aggregate without changing bare-command
  dispatch. The financial views move below `usage`; bare legacy `cost` remains a
  compatibility command with its current combined output.
- Existing Cobra aliases are `backends list` → `ls`, `models list` → `ls`,
  `role tier list` → `ls`, `worktree list` → `ls`, `auto-approve rules` →
  `policy`/`show`, `library` → `lib`, `prompt-template` →
  `prompt-templates`/`pt`, and `repl` → `interactive`/`i`. They are part of the
  compatibility inventory even where the mapping table did not spell them out.
- The root also has runnable bare parents (`cost`, `worktree`, and `usage` among
  others). Namespace migration must preserve each bare-parent dispatch rather
  than replacing it with help implicitly.

No implementation inconsistency is fixed here. The `usage`/`cost` collision in
the approved table is the semantic inconsistency that downstream work must
honor through distinct factories and compatibility wrappers.

## Permanent visible shortcuts

The permanent visible shortcut list is exactly:

`ls`, `start`, `status`, `send`, `commit`, `push`, `sync`, and `check`.

These are the documented high-frequency, unambiguous paths identified by the
approved plan and current operational skill. They remain supported wrappers and
visible in normal help/completion. Every other relocated path is a hidden
compatibility alias. In particular, similarly short but less frequent or
ambiguous paths (`stop`, `done`, `spend`, `savings`, `stats`, and `land`) are not
permanent visible shortcuts.

## Decisions and acceptance examples

### 1. Autopilot enablement versus run lifecycle

Use `autopilot enable|disable` for the per-repository switch and `autopilot run
start|pause|resume|stop|unregister <run>` for one registered run. Keep
`autopilot status` as the aggregate view and place registered-run enumeration at
`autopilot run list`. Legacy `on`, `off`, `start`, `pause`, `resume`, `stop`,
`unregister`, and `list` remain equivalent aliases.

- Positive: `wd autopilot disable --repo /r` posts the existing enablement body
  (`enabled:false`, repo `/r`) and leaves in-flight workers running.
- Positive: `wd autopilot run stop ap-123` posts the existing `stop` action for
  that run only; it does not disable the repository.
- Legacy equivalence: `wd autopilot off --repo /r` and the canonical disable
  command have identical client calls, stdout/stderr, errors, and exit class.
- Machine output: no JSON mode is invented; current text and errors remain byte
  compatible. Enable-time preflight stays on stderr and keeps its current exit.
- Out of scope: changing preflight, run states, ownership, landing gates, or the
  fact that disabling leaves workers running.

### 2. Agent lifecycle vocabulary

Retain all five distinct verbs below `agent`; their difference is behavior, not
synonymy.

| Command | Process | Record | Worktree/branch | Confirmation |
|---|---|---|---|---|
| `agent terminate` | terminate | keep | keep | none |
| `agent done` | terminate | clear/archive (`--hard` preserves current hard-delete meaning) | keep | none; optional PR is attempted first |
| `agent delete` | unchanged | archive, or hard-delete with `--hard` | unchanged | none |
| `agent remove-worktree` | unchanged | unchanged | remove | required unless `--yes`; current force/branch guards remain |
| `agent stop` | terminate by default | clear by default | remove by default | required exactly when removal is selected; `--keep-record` and `--keep-worktree` subtract those steps |

- Positive: `wd agent terminate A-1` calls only terminate.
- Positive: `wd agent stop A-1 --keep-record --keep-worktree` calls only
  terminate, while default stop confirms and orders terminate → delete →
  remove-worktree.
- Legacy equivalence: every existing flat verb selects the same steps and flags
  as its canonical `agent` form; no wrapper may translate into another verb.
- Machine output: current stdout, errors, and exit classification are retained;
  declined removal is a successful abort with no daemon call.
- Out of scope: consolidating endpoints, changing archival, making confirmation
  non-interactive, or weakening ownership/dirty/unpushed/branch protections.

### 3. `status`, `show`, and resource statistics

Use `status` for live operational state, `show` for one persisted resource's
detail, and `resources` for aggregate host/agent resource measurements. Preserve
existing exceptions where renaming would imply unsupported semantics:

- `agent status <agent>` and `autopilot status` remain `status` because they
  report live state (the agent response may also contain detail).
- `pipeline show <pipeline>` remains `show`.
- `schedule get <id>` becomes `schedule show <id>`; the legacy path remains exact.
- `stats` becomes `inspect resources`; `--history` remains the same mode of that
  command. Do not call this financial/provider `usage` or expose `metrics` as a
  new public synonym.

Acceptance examples: `wd agent status A-1 --json` retains its current stable JSON;
`wd pipeline show p-1` retains its current request and output; `wd inspect
resources --json` matches legacy `wd stats --json`; unknown identifiers and
daemon failures retain their current errors/exits. Out of scope are response
shape changes, splitting `stats --history`, or normalizing server resource names.

### 4. `usage`, `spend`, `savings`, and metrics

`usage` is the umbrella, but its bare action remains the current provider quota
snapshot. Its children are:

- `usage spend`: measured transcript-attributed currency spend, with current
  grouping and units.
- `usage savings`: the existing estimator/ledger for avoided context tokens and
  represented dollars; its basis labels continue to distinguish measured and
  heuristic data.
- `usage insights`: historical parallelization/token-efficiency analysis.

Operational CPU, memory, pressure, daemon, and per-agent resource samples belong
at `inspect resources`, not under usage. `metrics` remains an internal/API term,
not a canonical CLI command. Bare `cost` keeps its current combined spend/savings
compatibility output; it is not allowed to capture bare `usage` dispatch.

- Positive: `wd usage --json` keeps the provider snapshot and partial-result
  exit code 2; `wd usage spend --json` and `wd usage savings --json` keep their
  distinct current schemas and units.
- Legacy equivalence: `wd spend --json`, `wd savings --json`, and `wd cost
  spend|savings --json` match their canonical child; `wd cost` keeps its current
  two-section text behavior.
- Machine output: no shared JSON envelope, field rename, unit conversion, or
  deprecation text on stdout is permitted.
- Out of scope: pricing/model changes, combining measured spend with estimated
  savings, changing windows, or moving resource statistics into financial usage.

## Characterization coverage

The inventory test freezes dispatch-visible Cobra structure and JSON capability.
`autopilot_contract_test.go` freezes repository-switch versus run-action HTTP
dispatch and output. Existing focused lifecycle tests freeze call ordering,
confirmation/decline behavior, force/keep flags, and PR-before-teardown safety;
existing command tests freeze agent status, stats, spend/savings/cost, usage,
hooks, git/check, worktree, and portability JSON. `TestUsageExitCode` freezes the
partial-result exit code. Namespace PRs must compare canonical and legacy forms
against these fixtures; this phase intentionally adds no canonical commands.

## Integration audit follow-ups (2026-09-02)

Resolved during integration:

- **`schedule get` → `schedule show`:** canonical detail view is `warden schedule
  show`; `schedule get` remains a hidden alias with identical dispatch.
- **`cost spend`/`cost savings` alias metadata:** compatibility children under bare
  `cost` now point at `usage spend`/`usage savings`, not at themselves.

Intentionally retained (not fixed — separate decision required):

- **Bare `cost` vs bare `usage`:** bare `cost` keeps its combined spend+savings
  summary; bare `usage` keeps the provider quota snapshot. They cannot share one
  canonical path without changing dispatch semantics.
- **Deprecation stderr notices:** compatibility aliases run silently today; isolated
  stderr deprecation text is scoped to a future PR (plan phase 6).
- **Privacy-safe command-path telemetry:** not implemented; permanent-shortcut list
  remains frozen from this decision record, not from runtime telemetry.

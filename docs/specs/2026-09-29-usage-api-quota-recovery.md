# Usage API Quota Recovery

Date: 2026-09-29
Status: Phase 1 contract freeze
Plan: `plan-69eb481d`

## Goal

Make provider usage APIs the authoritative capacity signal for rate-limit
recovery while preserving pane detection as a fast supplemental signal. The
daemon must bind each running AI agent to the provider/account/model capacity
domain selected by daemon-owned routing, reconcile exhausted mandatory buckets to
the affected agents, and invoke the existing backend recovery coordinator for
hot-swap and handoff behavior.

This phase is documentation only. It freezes terminology, signal precedence,
operator surfaces, and migration rules for the implementation phases.

## Current Baseline

The existing system already has provider usage adapters, scoped quota projection,
pane-based hard-limit detection, scheduled resume behavior, and durable
per-agent recovery generation fences. It also has backend recovery coordination
that can select a replacement AI CLI/model candidate, stabilize it, and preserve
manual override behavior.

The missing contract is a daemon-owned binding from an agent to the exact quota
domain that limits it. Without that binding, a provider usage result can show
capacity loss without a deterministic way to identify every affected live agent,
especially when the pane text does not include a parseable reset banner.

## Canonical Terms

`UsageSnapshot`: A daemon-recorded provider capacity observation for one
capacity domain. It has a source, timestamp, freshness state, provider-native
safe diagnostics, bucket measurements, optional reset times, and a monotonically
ordered revision for idempotency.

`CapacityDomain`: The non-secret quota scope resolved by the daemon. It includes
the AI CLI/provider, account or profile fingerprint, route/model identity, and
provider-defined scope keys needed to compare snapshots with agent bindings.

`QuotaBinding`: The persisted agent/session value that maps an agent to one
capacity domain and the ordered mandatory buckets for the selected route. Agents
do not self-assert this value; the backend registry/resolver writes it at spawn,
restore, and hot-swap time.

`CapacityBucket`: A provider-defined capacity unit inside a capacity domain,
such as a weekly Claude bucket, rolling-window bucket, model-specific bucket, or
account-wide bucket. Bucket keys are opaque strings normalized by the provider
adapter and registry.

`Mandatory bucket`: A capacity bucket that must be available for the route to be
usable. If any mandatory bucket is fresh and exhausted, the route is exhausted.

`Freshness`: Whether a snapshot may be used for recovery decisions. A snapshot
is fresh only after a successful authoritative provider response within the
configured `stale_after` window. Failed, unsupported, partial, or timed-out
fetches are visible, but they do not become forced exhaustion evidence.

`Exhaustion`: A fresh successful provider result that marks a mandatory bucket
as exhausted, or a provider-specific pane/menu observation recognized as a
confirmed local hard-limit signal for the current agent.

`Reconciliation`: The deterministic mapping from exhausted fresh buckets to
eligible live agents whose daemon-owned quota bindings require those buckets.

`Recovery generation`: The per-agent idempotency fence that ensures one incident
starts at most one recovery attempt, even if usage API, menu, and banner evidence
arrive at nearly the same time.

## Data Flow

```text
daemon route selection
  -> backend registry resolves CapacityDomain + mandatory bucket keys
  -> agent/session persists QuotaBinding

provider usage poll or manual fetch
  -> provider adapter returns UsageSnapshot
  -> usage snapshot store records freshness + bucket states
  -> reconciliation finds affected live agents
  -> existing backend recovery coordinator starts generation-fenced recovery

pane menu/banner detector
  -> provider-specific confirmed local observation
  -> same reconciliation/evidence path for the current agent
  -> same backend recovery coordinator
```

## Evidence Precedence

| Evidence | Recovery authority | Notes |
| --- | --- | --- |
| Fresh successful provider API snapshot with mandatory bucket exhausted | Authoritative for every bound affected agent | Drives bulk reconciliation by domain and bucket. |
| Provider API success with bucket available | Authoritative availability for the observed domain until stale | Candidate selection should use the freshest available snapshot. |
| Provider API timeout, auth failure, unsupported provider, stale data, or partial/ambiguous response | Not authoritative exhaustion | Preserve prior safe data; expose unknown/stale health. |
| Provider-specific pane menu selection such as Claude wait-for-limit-reset | Immediate supplemental confirmed signal for that agent | Does not require a later reset banner to start recovery. |
| Provider-specific pane banner with recognized hard-limit wording | Immediate supplemental confirmed signal for that agent | Remains fail-closed and provider-specific. |
| Generic pane text, unknown menu, stale capture, or failed selection | Diagnostic only | Never forces recovery. |

Repeated evidence for the same domain/bucket/recovery generation is idempotent.
The API and pane paths must converge before invoking recovery so simultaneous
signals cannot create duplicate swaps or loops.

## Provider Capability Table

| Provider or AI CLI | Usage API | Capacity scopes | Pane fallback | Notes |
| --- | --- | --- | --- | --- |
| Claude | Supported when authenticated | Account/profile, model/route, rolling and weekly buckets | Required as low-latency fallback | Weekly reset menu is confirmed evidence even without a later `resets` banner. |
| Cursor | Supported through existing usage projection where available | Account/profile and model tier as exposed by adapter | Optional | Unknown or stale usage stays diagnostic. |
| Codex | Supported where backend usage service exposes safe quota data | Provider account/profile and model route | Optional | Treat token-only or incomplete data as unknown unless adapter maps mandatory buckets. |
| Antigravity and experimental AI CLIs | Adapter-dependent | Adapter-defined | Optional | Unsupported APIs must not be interpreted as exhausted. |
| Terminal sessions | None | None | None | Not eligible for AI quota recovery. |

Adapters may add providers without changing reconciliation semantics. Provider
bucket names remain opaque and are interpreted only by the adapter/registry pair.

## Freshness And Unknown Rules

A usage response is recovery-authoritative only when all of these are true:

1. The provider request succeeded.
2. The adapter maps the response to a known capacity domain.
3. The mandatory bucket state is known.
4. The snapshot timestamp is within `stale_after`.

Partial data must not zero out prior known capacity. Failed fetches record
health and diagnostics but leave the last successful snapshot intact until
retention pruning. Missing reset times are allowed for diagnostics; reset times
are not required to mark a fresh mandatory bucket exhausted.

When polling is disabled, the daemon must make no provider usage network calls.
Manual `wd usage recover` remains an explicit operator action and may fetch
fresh snapshots for its filtered scope.

## Multi-Account And Profile Behavior

Capacity domains are account/profile isolated. Two agents using the same AI CLI
and model are reconciled together only when their quota bindings resolve to the
same non-secret account/profile fingerprint and required bucket keys.

Provider credentials, raw account identifiers, API tokens, and secret material
must never be persisted in agent/session records, audit logs, API responses, or
TUI views. Safe fingerprints should be stable enough for correlation and opaque
enough for display.

Existing agents without quota bindings remain operable as `unbound_legacy`.
They may acquire a binding on their next safe lifecycle transition, but they
must never be mass-swapped based on guessed account or bucket data.

## Reconciliation Eligibility

Given a fresh exhausted capacity bucket, reconciliation includes live agents
whose quota binding requires the exact domain/bucket. It skips:

- done, archived, terminal, and stopped agents
- unbound legacy agents
- agents already in a recovery generation for the same incident
- agents manually superseded by an operator switch or stop
- agents whose role/tier policy forbids every available candidate

The impact result must include exhausted buckets, affected agents, skipped
agents with reasons, stale or unknown inputs, candidate availability, and
started/waiting outcomes.

## Bulk Recovery Concurrency

Bulk events use a bounded queue controlled by the recovery configuration
(`max_parallel_swaps` in the implementation phase). Before each candidate is
selected, the coordinator must read a fresh capacity snapshot and exclude:

- the exhausted domain/bucket
- candidates under durable cooldown
- disabled models or AI CLIs
- candidates incompatible with the agent role or tier
- candidates whose latest mandatory buckets are exhausted or unknown when policy
  requires known availability

The implementation must reuse the existing backend recovery coordinator. It must
not add a second direct hot-swap path. Existing handoff rails, stabilization
windows, reset scheduling, cooldowns, and manual overrides remain authoritative.

## User-Visible States

| State | Meaning |
| --- | --- |
| `available` | Fresh provider snapshot says every mandatory bucket for the route is available. |
| `exhausted` | Fresh provider snapshot says at least one mandatory bucket is exhausted, or the current agent has confirmed pane/menu hard-limit evidence. |
| `unknown` | Provider data is unsupported, ambiguous, partial, failed, or not yet fetched. |
| `stale` | Last successful snapshot exceeded the configured freshness window. |
| `unbound_legacy` | Agent has no daemon-owned quota binding yet and is not bulk-recovery eligible. |
| `recovering` | Agent has an active backend recovery generation. |
| `waiting_for_capacity` | No eligible replacement currently has known usable capacity. |
| `superseded` | Operator stop/switch or a newer generation replaced the active recovery attempt. |
| `stabilized` | Candidate agent/session completed the recovery stabilization window. |

## Operator Surfaces

### CLI

`wd usage` continues to show provider usage data. A new canonical command is:

```text
wd usage recover [--dry-run] [--ai-cli <id>] [--project <path>] [--max-parallel-swaps <n>]
```

Default invocation is an explicit operator action and may start recovery. Dry-run
must fetch fresh supported snapshots, calculate impact, and print what would
happen without invoking recovery. Output includes snapshot freshness, exhausted
buckets, affected/skipped agents, candidate decisions, and started/waiting
outcomes.

### API

API changes are spec-first. The OpenAPI document must define snapshot, domain,
bucket, impact, recovery attempt, and manual recover response schemas before
code generation. Generated OpenAPI code must not be hand-edited.

### MCP

MCP usage/recovery tools should mirror the API shapes and CLI filters. They
return structured impact and outcome data rather than pane text. Failed or stale
usage fetches are explicit health values, not forced recovery results.

### TUI

The cockpit/recovery views show trigger source (`usage`, `menu`, `banner`,
`manual`), capacity domain, bucket freshness, affected agents, skipped reasons,
candidate history, retry/reset timing, and recovery generation state.

## Audit Vocabulary

All audit and durable agent events use safe domain/bucket identifiers and must
redact credentials:

- `usage_snapshot_received`
- `usage_snapshot_failed`
- `quota_bucket_exhausted`
- `quota_impact_calculated`
- `recovery_started`
- `candidate_attempted`
- `candidate_result`
- `waiting_for_capacity`
- `recovery_stabilized`
- `recovery_superseded`

Each record includes timestamp, source, project/run/agent IDs where applicable,
safe capacity domain, bucket key, recovery generation, reason, and outcome.

## Migration Table

| Existing record | Migration behavior |
| --- | --- |
| Agent/session has explicit quota binding | Use it for reconciliation after schema validation. |
| Agent/session has backend/model only | Mark as `unbound_legacy`; do not guess account or bucket. |
| Restored live agent without binding | Keep operable; attempt binding on next safe route resolution or hot-swap. |
| Historical audit/event rows | Leave unchanged; new events use the vocabulary above. |
| Provider usage rows without domain/bucket keys | Keep as diagnostics; not authoritative recovery input. |

## Regression Scenario

The acceptance fixture for this plan must include the incident that motivated the
work:

1. Two Claude agents share the same account/profile capacity domain and weekly
   mandatory bucket.
2. Both encounter Claude's limit menu.
3. The daemon safely selects `wait for reset`.
4. Claude renders `Requesting rate limit reset for weekly limit`.
5. No parseable `resets` banner appears.
6. A fresh successful Claude usage snapshot reports the shared weekly bucket
   exhausted.
7. Reconciliation identifies both bound agents.
8. Recovery starts once per agent through the existing backend recovery
   coordinator.
9. Simultaneous pane/menu and usage evidence does not create duplicate recovery
   generations.

## Phase Boundaries

This contract intentionally does not change production behavior. Later phases
must implement the binding model, snapshot store, polling loop, reconciliation,
coordinated bulk recovery, pane signal fusion, operator command, observability,
and migration acceptance in DAG order.

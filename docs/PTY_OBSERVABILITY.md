# PTY runtime observability

Operator-facing reference for the PTY runtime (`runtime.pty: off|shadow|on`, see
[the contract](specs/2026-10-19-pty-runtime-contract.md)). The same documents back the
TUI, web cockpit, REST, CLI and MCP; none of them re-derive state.

## Surfaces

| What | REST | CLI | MCP | TUI / web |
|---|---|---|---|---|
| Live stream state, typed waiting reason, evidence, input owner, route, posture, current prompt summary | `GET /api/v1/sessions/{id}/runtime` | `warden inspect pty runtime <id>` | `session_runtime` | cockpit `i` (info) / Agent tab runtime card |
| Per-session decision trace (newest first, `limit`, `outcome` filters) | `GET /api/v1/sessions/{id}/decisions` | `warden inspect pty decisions <id>` | `session_decisions` | Agent tab "Details & history" table |
| Metrics | `GET /api/v1/pty/metrics` (JSON), `/pty/metrics.prom` (Prometheus text) | `warden inspect pty metrics` | `pty_metrics` | — |
| Pending approvals with posture, route, human-readable summary and danger flag | `GET /api/v1/approvals` | `warden approvals` | `list_approvals` | approvals pane / Attention queue |

A session spawned while `runtime.pty` was `off` is reported `supervised: false`; its
remaining runtime fields are empty (tmux polling still supervises it).

### Danger banner

Whenever the effective posture is `dangerously-execute-all`, every surface shows a
conspicuous banner (`DANGER: dangerously-execute-all - ...`): bold red in the TUI, a red
`role="alert"` bar in the web UI, a `danger: true` + `danger_banner` pair in the API.
The posture `allowlisted` is the default when none is set. The accessible human-answer
controls (numbered option buttons, `warden approve`) are unchanged.

## Decision trace

Every prompt decision - answered, escalated, rejected or (in `shadow`) dry-run - appends
one JSON line to `<data_dir>/pty/trace/<session>.jsonl`: fingerprint, shape version,
adapter and CLI version, class, provenance, confidence, the normalised permission
(capabilities/scope/options, chosen option), posture, effective policy, revalidation and
arbitration results, outcome, route, reason and latency. Event sequence and correlation
id join a record to the journal.

- **Append-only, redacted.** Records are built from the already-redacted event stream;
  the chosen *input* is described (`"option 1"`), never stored as raw bytes, and prompt
  text passes the same secret scrubber as the journal.
- **Retention.** `runtime.trace_max_records` (default 5000 per session) and
  `runtime.trace_max_age_days` (default 30; `0`/negative disables age pruning). Compaction
  rewrites the file atomically; both settings hot-reload.
- **Failure is visible, not fatal.** A write error increments
  `warden_pty_decision_trace_errors_total`; decisions and answers continue.

## Metrics

All series are `warden_pty_*`. Label values come from closed vocabularies; anything else
collapses to `other`, so no label can carry prompt text, paths, session names or secrets.

Key series: `events_total{kind}`, `state_transitions_total{adapter,from,to,reason}`,
`state_detection_latency_seconds`, `quiescence_investigations_total`,
`prompts_total`, `unrecognized_prompts_total`, `prompt_confidence`, `fallback_total`,
`reconnects_total`, `backpressure_total` (journaled gaps), `learned_shape_total`,
`decisions_total{outcome,route,...}`, `decision_latency_seconds`, `escalations_total`,
`redactions_total{kind}` (kind only, never the secret),
`decision_trace_records_total`, `decision_trace_errors_total`, `sessions{...}`.

Ready-made Prometheus alerts and a Grafana dashboard are in
[`deploy/observability/`](../deploy/observability/).

## Migration

1. `runtime.pty: off` (default) - nothing changes, no journal, no trace.
2. `shadow` - sessions still launch in tmux; output is mirrored into the journal and the
   engine records what it *would* do (`dry_run` trace records). Pollers stay authoritative.
   Compare traces to real behaviour before moving on.
3. `on` - the engine is authoritative for runtime-backed sessions. Applies to sessions
   created after the change; existing sessions keep their mode. Rolling back is setting it
   to `shadow`/`off` the same way.

## Learned-shape safeguards

Unknown prompt formats are classified by Fast-Brain on a redacted screen. A result becomes
a reusable *learned shape* only after deterministic fixture replay, pinned to adapter and
CLI version, with a confidence floor and an expiry. A CLI upgrade, a failed replay or a
corrupt store disables the shape (fail safe: the prompt escalates to a human). Outcomes are
counted in `learned_shape_total`.

## Recovery

- Daemon restart: the journal is replayed, sessions re-adopted (`reconnects_total`), the
  classifier rebuilds state from the stream; the trace on disk is unaffected.
- A slow consumer gets a journaled gap marker instead of blocking the PTY
  (`backpressure_total`).
- Stale, changed, ambiguous or unrecognised prompts are never answered; they surface as
  needs-attention and appear in the trace as `escalated`/`rejected`.

## Permission (including trust) versus strategic prompts

The engine only answers **permission** prompts - tool/file/command authority and
workspace-trust dialogs - and only within the session's posture. A *strategic* question
("which approach should I take?", plan approval, design choice) is never auto-answered in
any posture, including `dangerously-execute-all`; it goes to the human inbox and shows as
needs-attention with its question text.

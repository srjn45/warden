# 02b-04 operator readiness and runtime observability contract

## Status and compatibility decision

This is an additive, inspect-only contract. It does not authorize a rollout,
change a daemon configuration, or convert any session.

`runtime.pty: off` remains the default. `shadow` remains a tmux launch with a
read-only mirror. `runtime.pty: on` continues to affect only *new plain-shell
terminal* sessions. It does **not** opt AI agents into direct hosting, including
after an upgrade. Existing sessions retain their durable runtime identity.

Direct AI-agent hosting requires both a future explicit operator readiness gate
and an adapter capability declaration. Until both exist, every registered AI
adapter returns a typed unsupported result before creating a pane, holder, child
process, worktree cleanup action, or provider conversation. Test-only seams do
not constitute an operator opt-in.

## Runtime evidence document

`GET /api/v1/sessions/{id}/runtime`, `warden inspect pty runtime <id>`, and
MCP `session_runtime` expose one document. The following additive fields are
the authority for an operator deciding what Warden actually controls:

| Field | Meaning |
| --- | --- |
| `requested_mode` | Current `runtime.pty` configuration, informational only. |
| `actual_kind`, `process_owner`, `authoritative` | Observed session host and control authority; never inferred from requested mode. |
| `evidence_source` | `live`, `record`, `legacy-probe`, or `none`. |
| `evidence_freshness` | `fresh` only for live verification; `recorded` is durable but not liveness proof; `unknown` means no usable evidence. |
| `unsupported_reason` | A safe actionable reason why direct-agent authority is unavailable. Empty means this view has no such capability refusal, not that a future launch is authorized. |

A tmux mirror is always `actual_kind: tmux`, `process_owner: tmux`, and
`authoritative: false`; it must never be labelled direct. A recorded direct
runtime is likewise not authoritative until a live holder/session verifies it.

## Observability and privacy

The existing PTY metric and trace contract is part of readiness evidence:
state-detection and decision latency, unknown prompts, decision outcomes, and
redactions remain available through `pty_metrics`/`inspect pty metrics` and the
redacted decision trace. Metric labels and durable trace fields remain bounded
and redacted; raw keystrokes, prompt bytes, secrets, paths, and session IDs are
never metric labels.


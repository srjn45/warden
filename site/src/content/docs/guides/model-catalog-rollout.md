---
title: AI CLI model-catalog rollout
description: Safely preview, apply, verify, and roll back account-scoped model-catalog policy.
---

Model-catalog reconciliation is additive and explicit. It does not run on
upgrade, daemon startup, ordinary ticks, or schedules; it does not log in,
update an AI CLI, send a prompt, make a paid probe, restart an agent, or change
an existing session.

The operator workflow is manual and daemon-backed:

1. Preview with `warden models refresh [--aicli ID]` (alias `warden backend model
   refresh`). It runs each installed AI CLI's non-generating model discovery,
   submits the evidence to the daemon and prints the proposal. A preview changes
   no routing policy. Account identity is conveyed only as an opaque `scope_fp`.
2. Inspect the operations and blocked entries, then apply with `warden models
   refresh --apply --yes`. Automation can use `POST /api/v1/model-catalog/proposals`,
   `POST /api/v1/model-catalog/proposals/{proposal_id}/apply`, or MCP
   `preview_model_catalog` / `apply_model_catalog_proposal`.
3. Reselect a running agent with `warden switch AGENT --aicli ID` (catalog pick at
   the agent's own role tier), `--aicli ID --tier tier-N`, `--tier tier-N`, or an
   explicit `--aicli ID --model MODEL` pin. Existing, pinned, restored and live
   sessions are otherwise unchanged.
4. Inspect history with `warden models history` and, if needed, roll back the
   exact revision with `warden models rollback REV --yes`. Rollback is
   compare-and-swap policy history, not a live-session restart.

Automatic recovery from an invalid model at launch is deferred until the PTY
migration and state/prompt/error detection redesign; nothing retries or
substitutes a model for you.

Partial, stale, offline, malformed, unsupported, or failed evidence cannot
retire a model. Seed entries are bootstrap/unknown fallback only, not proof of
current availability; accepted negative evidence is not reset by seed code.
Explicit model/tier/quota choices remain operator intent. On an account or
profile switch, create and inspect a fresh scoped preview.

Claude, Aider, and Goose do not have dynamic entitlement discovery in this
release. For a full API example, compatibility/deprecation guidance, downgrade
matrix, stuck-session handling, and release checklist, see the repository's
[model-catalog rollout runbook](https://github.com/srjn45/warden/blob/main/docs/RUNBOOK-aicli-model-catalog.md).

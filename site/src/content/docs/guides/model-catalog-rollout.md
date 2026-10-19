---
title: AI CLI model-catalog rollout
description: Safely preview, apply, verify, and roll back account-scoped model-catalog policy.
---

Model-catalog reconciliation is additive and explicit. It does not run on
upgrade, daemon startup, ordinary ticks, or schedules; it does not log in,
update an AI CLI, send a prompt, make a paid probe, restart an agent, or change
an existing session.

The shipping workflow is API/MCP-first:

1. Submit redacted, structured, operator-reviewed discovery evidence to `POST
   /api/v1/model-catalog/proposals` or MCP `preview_model_catalog`.
2. Inspect the proposal's operations, blocked entries, scope fingerprint, and
   expiry. A preview changes no routing policy.
3. Explicitly apply its exact id with `POST
   /api/v1/model-catalog/proposals/{proposal_id}/apply` or MCP
   `apply_model_catalog_proposal`.
4. Verify a newly-created disposable session routes as expected. Existing,
   pinned, restored, and live sessions are unchanged.
5. Inspect history and, if needed, roll back the exact revision with the API or
   MCP `rollback_model_catalog_revision`. Rollback is compare-and-swap policy
   history, not a live-session restart.

Partial, stale, offline, malformed, unsupported, or failed evidence cannot
retire a model. Seed entries are bootstrap/unknown fallback only, not proof of
current availability; accepted negative evidence is not reset by seed code.
Explicit model/tier/quota choices remain operator intent. On an account or
profile switch, create and inspect a fresh scoped preview.

Claude, Aider, and Goose do not have dynamic entitlement discovery in this
release. For a full API example, compatibility/deprecation guidance, downgrade
matrix, stuck-session handling, and release checklist, see the repository's
[model-catalog rollout runbook](https://github.com/srjn45/warden/blob/main/docs/RUNBOOK-aicli-model-catalog.md).

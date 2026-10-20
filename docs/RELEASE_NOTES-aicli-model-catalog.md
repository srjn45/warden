# Release notes: AI CLI model-catalog reconciliation

This additive release lets an operator submit account-safe model evidence,
preview recommendations, explicitly apply a proposal, and roll back a specific
policy revision. It changes only future unpinned routing after an explicit
apply; existing sessions, explicit model pins, backend/tier/quota choices, and
billing permissions remain unchanged.

After upgrading, no action is required. Nothing discovers or reconciles models
at upgrade, startup, normal daemon ticks, or on a schedule. When you are ready,
follow the [rollout runbook](RUNBOOK-aicli-model-catalog.md): `warden models refresh`, inspect
unknown/blocked recommendations, apply the exact proposal id, and verify one
new disposable session. Do not treat seeds or a public provider catalog as
current entitlement. Offline, stale, partial, unsupported, or failed evidence
cannot retire a model.

The legacy `backend model`/`models` namespace and storage collections remain
supported while the separate `aicli` rename lands. This release does not tag,
deploy, update an AI CLI, log in, run a paid probe, or restart an agent. A
stuck existing session still needs a separately authorized switch, handoff, or
restart. If an accepted policy needs reversal, use the revision-safe rollback;
do not rely on a binary downgrade as a policy undo.

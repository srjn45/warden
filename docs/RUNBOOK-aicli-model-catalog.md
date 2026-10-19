# AI CLI model-catalog rollout runbook

This is an additive, operator-driven release. A catalog preview records evidence
and computes recommendations; only an explicit apply changes routing policy. It
does not update an AI CLI, log in, refresh credentials, run a paid probe, restart
an agent, or reconcile automatically during startup, ordinary daemon ticks, or a
background schedule.

## Before changing policy

1. Upgrade normally. Do not edit the data directory or local configuration as
   part of this feature.
2. Record the current policy with `warden backend model list --json` and the
   current routing expectation. Existing stores without accepted evidence keep
   their existing routing behavior.
3. Inspect the provider evidence you intend to submit. It must be non-secret,
   account-safe metadata from an operator-authorized, non-generating action.
   Never put raw command output, account references, paths containing secrets,
   tokens, or prompts in an API/MCP request.

The legacy `warden backend model discover` command remains a compatibility
surface for listing/importing known models. It is not proof of entitlement and
is not the reconciliation workflow. The planned `warden aicli models ...`
namespace is not required by this release; retain existing `backend model` and
`models` automation until that separately-versioned rename lands.

## Preview, inspect, apply

The current deployable workflow is API/MCP-first. `POST
/api/v1/model-catalog/proposals` accepts *structured evidence*; it never runs a
provider command itself. Use the generated OpenAPI document at
`/api/docs/openapi.yaml` for the complete request schema.

1. Submit a preview with one observation. Required fields are `aicli`,
   `observation.source`, and `observation.completeness`. A complete source may
   make absence meaningful; partial, empty, stale, failed, offline, malformed,
   unauthenticated, or unsupported evidence never authorizes retirement.
2. Inspect the returned proposal's `ops`, `blocked`, `scope_fp`, snapshots, and
   expiry. `ops` are recommendations, not completed changes. Unknown metadata
   stays unclassified/non-auto-assigned rather than being guessed.
3. Apply exactly the returned `id` with `POST
   /api/v1/model-catalog/proposals/{proposal_id}/apply`, or MCP
   `apply_model_catalog_proposal`. A stale/expired proposal returns a conflict;
   start a new preview and inspect it again. Do not retry an old proposal.
4. Read `GET /api/v1/model-catalog/history` (or MCP
   `model_catalog_history`) and `warden backend model list --json` to retain the
   revision and post-apply policy record.

For an MCP client, use `preview_model_catalog` to submit the same structured
evidence. It deliberately cannot invoke provider commands. Then use
`apply_model_catalog_proposal` with the exact returned `proposal_id`. Agents,
autopilot, and background automation must not call the mutating apply tool
without a separate operator instruction.

Example API sequence (substitute an operator-reviewed JSON file; do not copy
sample model IDs as evidence):

```sh
curl --fail-with-body -H "Authorization: Bearer $WARDEN_TOKEN" \
  -H 'Content-Type: application/json' \
  --data-binary @catalog-observation.json \
  "$WARDEN_URL/api/v1/model-catalog/proposals"

# After inspecting the response and recording its exact id:
curl --fail-with-body -X POST -H "Authorization: Bearer $WARDEN_TOKEN" \
  "$WARDEN_URL/api/v1/model-catalog/proposals/<proposal_id>/apply"
curl --fail-with-body -H "Authorization: Bearer $WARDEN_TOKEN" \
  "$WARDEN_URL/api/v1/model-catalog/history"
```

## Verify new-session routing

An accepted revision affects only later unpinned routing decisions. Start one
authorized, disposable **new** session that selects a tier (for example
`warden start "routing smoke check" --tier tier-2`) and inspect its recorded
`ai_cli` and `model`. Do not use a real workload as this check. Existing,
restored, forked, scheduled, or pipeline sessions preserve their recorded pins;
the catalog never moves a live agent.

An explicit `--aicli` + `--model` pin remains the operator's choice and is
launched exactly as supplied. Manual model/tier/quota changes remain protected
operator intent; recommendations do not overwrite them. A pinned model that is
invalid is reported as a failure, not silently substituted. If an existing
session is stuck, an independently authorized `warden switch`, handoff, or
restart is required; catalog apply and rollback are not restart controls.

## Offline, unsupported, and account-switch handling

Treat stale/offline/partial/failed evidence as a reason to stop and inspect,
not as an absence list. Seeds are bootstrap/unknown fallback only: they neither
prove current availability nor re-enable a model contradicted by accepted
negative evidence. An accepted unavailable/retired state cannot be reset by
seed code.

Claude, Aider, and Goose have no dynamic entitlement discovery in this release.
They remain manual/display surfaces. Other adapters can also return partial
results; do not infer capability, tier, quota, or availability from a public
catalog. After changing accounts or profiles, create a fresh scoped preview;
never apply a proposal produced for the previous account scope.

## Revision-safe rollback and downgrade

If a newly accepted policy is wrong, first inspect history, then roll back the
specific revision using `POST /api/v1/model-catalog/revisions/{revision}/rollback`
or MCP `rollback_model_catalog_revision`. Rollback is a forward, compare-and-swap
revision; a conflict means later policy changed, so inspect and resolve manually
instead of forcing it. It changes policy only, not evidence, quota state, prior
routing decisions, or live sessions.

Downgrading to a pre-feature binary is safe for additive records: older code
ignores the new evidence/revision collections and retained conservative routing
fields. Do not rely on a downgrade to undo policy; use the revision rollback
first. Existing custom models and explicit pins remain operator-managed.

## Release gate and deployment checklist

- [ ] `wd check` is green, including the project's configured fast verification.
- [ ] Upgrade fixture confirms no preview/apply means routing is unchanged.
- [ ] Preview → inspect → exact apply → history → disposable new-session routing
      was exercised with fixture evidence; live evidence, if authorized, is
      recorded separately and is not a paid/generating probe.
- [ ] Rollback fixture confirms revision-CAS conflict protection and no live
      session mutation.
- [ ] Race/parallel-reader coverage is green for preview/apply/rollback.
- [ ] Generated CLI/API/MCP references match the shipping surfaces; legacy
      `backend model` aliases remain available.
- [ ] No release tag, deployment, local config change, or "deployed" status is
      produced without separate authorization.

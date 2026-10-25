# Git conflict-detection cleanup: approved inventory

Plan: plan-bb8c6f34, revision 2.
Status: approved inventory. This document is documentation only; it authorizes no implementation by itself.

## Scope and preserve boundary

Remove Warden’s live inter-agent, file-level overlap detector: fsnotify/git-diff worktree scanning, cached overlap results, daemon inbox warnings, detector APIs and user interfaces, detector configuration, and detector-specific spawned-agent guidance.

Preserve all of the following:

- Directed messages/inbox and shared context/CAS.
- Git lifecycle, worktrees, snapshots, rebase/merge conflict handling and rails.
- Branch/CI tracking, including GET /api/v1/collab/branches, workspace branches, get_branch_status, branch_track config, and internal/branchtrack.
- Plan revision conflicts, context-CAS conflicts, Autopilot PR conflict/fix logic, data-store and repair conflicts, schedule validation conflicts, and generic metrics.
- The fsnotify dependency, because internal/config/watch.go independently uses it for configuration reload.

The collab namespace is intentionally broader than the detector. internal/daemon/strict_collab.go also owns context/messaging routes, and the collab HTTP namespace contains the independent branch route. Delete only GET /api/v1/collab/conflicts and related types.

## Approved deletion map

| Path | Approved deletion or surgical edit |
|---|---|
| internal/collab/monitor.go | Delete Monitor, NewMonitor, Run, Conflicts, Conflict, AgentInfo, dirty cache, warning dedup/format/delivery. |
| internal/collab/git.go | Delete detector git reconciliation and index-lock avoidance. |
| internal/collab/watcher.go; budget_linux.go; budget_other.go | Delete detector-only fsnotify watcher/watch-budget code. |
| internal/daemon/{api.go,server.go} | Remove collab monitor fields/construction/start and SetCollabInterval/SetCollabGitReconcileInterval; retain branch tracker. |
| internal/daemon/strict_collab.go | Remove GetConflicts only; retain all context/message/history/search methods and GetBranchStatus. |
| internal/daemon/apidocs/openapi.yaml | Remove GET /api/v1/collab/conflicts, getConflicts, Conflict and ConflictAgent. |
| internal/daemon/oapi/api.gen.go | Regenerate from OpenAPI; do not edit manually. Remove generated conflict type/import/interfaces/wrappers/router/request-response objects. Retain branches endpoint/type. |
| internal/client/client.go | Remove ConflictAgent, Conflict and CollabConflicts; retain BranchStatus/BranchStatuses and other APIs. |
| internal/mcp/server.go | Remove get_collaboration_status and who_is_editing_file only. |
| internal/repl/{daemon.go,registry.go,commands.go,present.go} | Remove CollabConflicts interface/tool, slash-collab command and renderer only. |
| internal/cli/collab.go | Delete command and conflict/who-is-editing leaves. |
| internal/cli/{workspace.go,help.go} | Remove workspace conflicts/who-is-editing and hidden collab compatibility metadata; retain all other workspace commands. |
| internal/lifecycle/{lifecycle.go,runner.go} | Remove collabHintGuidance, collabHint, GetCollabHint and fake config support, from every launch/relaunch path. Preserve shared prompt injection and pipeline/git/memory/persona/peer guidance. |
| internal/agentbackend/backend.go and backend adapters | Revise comments referring to collab/git/pipeline aggregate guidance. Preserve ContextInjector and system-prompt/rules-file infrastructure. |
| internal/config/{config.go,config_test.go} | Retire CollabConfig/Config.Collab/defaults/help/validation/accessors/flat-alias migration and tests; retain branch_track and config watcher. |
| internal/cli/{config.go,daemon.go}; internal/daemon/reconfigure.go | Remove collab display/startup/restart-required handling; retain branch tracking and remaining hints. |
| web/src/components/ConflictsPanel.tsx | Delete. |
| web/src/lib/{api.ts,types.ts}; web/src/components/OthersTab.tsx; web/src/styles/app.css | Remove detector API/types/card/.conflict-* styles only. |
| internal/daemon/legacy_dto_test.go | Remove detector DTO/import only. |

Delete dedicated tests with the feature: internal/collab/{monitor,git,watcher}_test.go, internal/daemon/collab_routes_test.go, and internal/cli/collab_test.go. Update focused client, MCP, REPL, lifecycle, OpenAPI, workspace, web and CLI-golden tests rather than removing their unrelated coverage.

## Retired configuration contract

The removal is backward-compatible for configuration parsing but does not keep dormant detector settings.

Current parser evidence:

- internal/config/config.go defines CollabConfig and Config.Collab with canonical collab.enabled, collab.interval, collab.git_reconcile_interval and collab.hint defaults of true, 10s, 2m and true.
- LoadStrict parses YAML nodes, calls migrateFlatToNamespaced/migrateGroup before struct decode, then validates. Flat collab_enabled, collab_interval and collab_hint migrate in memory; an explicit nested subkey wins. Load does not write.
- Reconcile is the durable config writer behind warden config init. It currently migrates aliases, preserves unknown keys/comments and appends defaults.
- validate and the collab duration accessors normalize empty, zero, negative or invalid duration values to defaults.

Approved retirement strategy:

1. Remove CollabConfig, Config.Collab, defaults, schema/help text, validation/accessors and the flat alias group.
2. Before configuration decode, accept and discard the whole nested collab mapping plus collab_enabled, collab_interval and collab_hint, logging actionable retirement guidance.
3. Do not migrate aliases into a new collab mapping and do not retain a dormant API.
4. Reconcile/config init removes the nested block and all three root aliases while preserving unrelated unknown keys/comments. A fresh render has no collab block and remains idempotent.
5. Retired values are ignored before duration validation. Thus nondefault values and invalid detector values such as 0, -1s and not-a-duration load/reload safely; malformed YAML still fails and the watcher retains last-good configuration.

Required named acceptance tests:

- TestRetiredCollabNestedConfigIsIgnored
- TestRetiredCollabFlatAliasesAreIgnored
- TestRetiredCollabMixedFormsAreIgnored
- TestRetiredCollabInvalidValuesAreIgnored
- TestReconcileRemovesRetiredCollabConfig

They replace TestLoad_Collab_Namespaced and TestLoad_Collab_FlatDeprecated. Test values cover nested nondefault false/45s/7m/false, all flat aliases, nested+flat disagreements, zero/negative/unparsable retired durations, malformed unrelated YAML, and fresh/idempotent canonical rendering.

## Update and migration evidence

There is no collab-specific updater or data migration.

internal/updater performs binary swap, release-manifest/schema validation and health rollback. Its fixture roots are internal/updater/testdata/fixtures/v9.25.0-legacy, v9.26.0-waypoint and v9.27.0-bad-history; none is a configuration-collab fixture. internal/migrate/realistic_cutover_test.go writes config.yaml as an external artifact and asserts it is byte-identical through schema cutover.

Therefore wd update and wd migrate must not rewrite retired detector keys. Only warden config init/config.Reconcile performs the canonical cleanup described above.

## Documentation and live agent prompts

Update live product documentation: README.md, FEATURES.md, docs/FEATURES.md, docs/USAGE.md, site reference env-vars/features/CLI, multi-agent MCP/REPL pages and web-mission-control. Generate site/src/content/docs/reference/cli.md with make gendocs rather than manually editing it. Historical detector design documents may receive a removal-status annotation rather than being silently erased.

The following are live instruction surfaces and must be updated in the surface-removal layer. The canonical skill sources are under `skills/warden/...`; `.agents/skills/warden/...` is their repository projection and must remain aligned rather than being treated as the source of truth.

| Path | Required change |
|---|---|
| AGENTS.md | Remove mandatory who_is_editing_file/get_collaboration_status and conflict-inbox guidance only; retain Warden lifecycle/git/check guidance. |
| skills/warden/SKILL.md (projected at .agents/skills/warden/SKILL.md) | Remove detector tool/claim rows while retaining context/messages/approvals and branch tracking. |
| skills/warden/references/coordination.md (projected at .agents/skills/warden/references/coordination.md) | Remove the File-conflict detection section/commands/config/hint; retain shared context, messages, approvals, Branch & CI tracking. |
| skills/warden/references/operations.md (projected at .agents/skills/warden/references/operations.md) | Remove collab config and alias mentions while retaining branch_track and other operations. |

## Actual user-owned PR stack

The inventory is PR #1067 on `worker-83955ef9`, based on `main`. The user-owned stack is deliberately five layers: inventory -> surface -> runtime -> verification -> release.

The surface-removal branch is forked from the inventory branch head and its PR base is `worker-83955ef9`. The runtime-removal branch is forked from the surface branch head and its PR base is that surface branch. The verification branch is forked from the runtime branch head and its PR base is the runtime branch. The release branch is forked from the verification branch head and its PR base is the verification branch. If an earlier layer is rebased after its parent merges, its direct child must be rebased onto that updated parent; no layer skips a predecessor.

Surface owns public detector surfaces (including prompts, APIs, generated schemas, MCP/CLI/REPL/web and their focused tests) at a compile-clean boundary. Runtime then removes the detector implementation and retired configuration at its own compile-clean boundary. Verification adds the retired-config fixture matrix and acceptance evidence; release adds compatibility/release/operator material. This ordering supersedes earlier atomic-cutover wording.

All PRs are user-owned: do not merge or enable auto-merge.

## Acceptance commands

    go test ./internal/config -run 'Test(RetiredCollabNestedConfigIsIgnored|RetiredCollabFlatAliasesAreIgnored|RetiredCollabMixedFormsAreIgnored|RetiredCollabInvalidValuesAreIgnored|ReconcileRemovesRetiredCollabConfig|ReconcileCreatesFullFile|ReconcileAddsOnlyMissingKeys|ReconcileIsIdempotent|LoadStrict_GoodBadMissing)$' -count=1
    go test ./internal/updater -run '^TestUpgradeFixtures$' -count=1
    go test ./internal/migrate -run '^TestRealisticCutover' -count=1
    make generate-check
    make gendocs-check
    make verify-fast

The inventory PR itself runs documentation-appropriate checks only; the commands above are acceptance requirements for the later implementation and verification stack.

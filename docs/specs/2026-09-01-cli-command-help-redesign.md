# CLI Command and Help Redesign Implementation Plan

**Date:** 2026-09-01

**Status:** Approved design; implementation plan

**Scope:** CLI organization, naming, discovery, generated reference, and completion. No production behavior changes are approved by this document.

## Summary

Replace warden's broad, mostly flat command surface with an intent-oriented Cobra tree whose canonical top-level namespaces are:

`agent`, `pipeline`, `autopilot`, `project`, `backend`, `usage`, `git`, `check`, `workspace`, `context`, `message`, `approval`, `schedule`, `inspect`, `config`, `daemon`, and `completion`.

`setup`, `tutorial`, `doctor`, `tui`, and `version` may remain top-level because they are entry-point or host-level actions rather than domain collections. Existing commands remain callable as hidden compatibility aliases. A small set of high-frequency shortcuts may remain visible and supported permanently where the inventory and telemetry justify them.

The same annotated command tree must drive runtime help, generated documentation, and shell completion. Root help becomes grouped and scannable; deeper help becomes progressively narrower and more detailed.

## Goals and non-goals

### Goals

- Make the CLI discoverable by user intent instead of implementation history.
- Make `wd help`, `wd --help`, and `wd -h` equivalent grouped views of the root command surface.
- Provide progressive nested help: `wd agent --help` and `wd help agent` show the agent domain; `wd agent start --help` and `wd help agent start` show focused action details.
- Establish one canonical spelling for every current command while preserving scripts through compatibility aliases.
- Keep runtime help, generated CLI reference, and completion structurally consistent and deterministically ordered.
- Create explicit tests that freeze behavior while names and parents move.

### Non-goals and semantic freeze

This is an **organizational and naming redesign**, not a semantic redesign. Moving a command beneath a namespace or adding an alias must not alter what the operation means or how it behaves.

**Semantic-freeze rule:** no REST endpoint or payload, MCP tool contract, storage schema or mutation, agent/pipeline/autopilot lifecycle transition, backend-routing decision, JSON field or envelope, or exit-code behavior may change under this redesign without a separate recorded decision and an acceptance test dedicated to that semantic change. The same rule applies to confirmation and destructive-action safety. If canonical wording exposes an inconsistency, record it as follow-up work; do not silently normalize the implementation in a namespace PR.

The implementation may change command paths, help output, completion candidates, and carefully isolated deprecation notices. It must reuse the current option parsing and run logic so canonical commands and aliases issue identical client calls and produce identical operation output.

## User-facing help contract

### Root help

The following invocations are equivalent and must render byte-for-byte identical content after normalizing the executable name:

```text
wd help
wd --help
wd -h
```

Root help uses stable, intent-based groups rather than Cobra's alphabetical flat list. The renderer should show canonical namespaces first, then retained entry points and permanent shortcuts. Hidden compatibility aliases do not appear in normal help.

Suggested groups and order:

1. **Run work:** `agent`, `pipeline`, `autopilot`, `schedule`
2. **Work with a project:** `project`, `git`, `check`, `workspace`
3. **Coordinate:** `context`, `message`, `approval`
4. **Observe and configure:** `inspect`, `usage`, `backend`, `config`
5. **Operate warden:** `daemon`, `completion`
6. **Get started and interact:** `setup`, `tutorial`, `doctor`, `tui`, `version`
7. **Shortcuts:** only the explicitly retained, high-frequency commands

Each row contains the canonical path and a short intent statement. Ordering is metadata-driven and deterministic, not dependent on Go registration order or map iteration.

### Progressive nested help

- Namespace help, for example `wd agent --help` or `wd help agent`, explains the domain, its lifecycle model and safety distinctions, then lists that namespace's canonical actions in stable groups.
- Leaf help, for example `wd agent start --help` or `wd help agent start`, contains the focused synopsis, arguments, domain-specific examples, local flags, inherited flags, safety notes, and aliases/deprecation information only when useful.
- Detail increases with depth while breadth decreases. Root help does not repeat leaf flag manuals; leaf help does not reproduce unrelated namespace navigation.
- `--help` is the canonical form in prose and examples. `-h` is documented as shorthand. `help` is positioned as the exploratory traversal form.
- Do not promote `wd agent help`. Cobra may parse a trailing `help` only if explicitly supported for compatibility, but documentation, examples, hints, and generated reference must use `wd help agent` or `wd agent --help`.

### Full discovery and aliases

Add `wd help --all` to render the complete canonical tree plus hidden compatibility aliases, with aliases visibly marked and linked to their canonical paths. Normal root and namespace help omit hidden aliases.

Legacy command paths remain executable. Compatibility aliases should be hidden from ordinary help and completion unless a completion shell requires them to preserve an already typed legacy path. Deprecation messages go to stderr, never stdout, and are suppressed for JSON output and other machine-readable modes.

Candidates for permanent visible shortcuts are the demonstrably frequent, unambiguous operations such as `wd ls`, `wd start`, `wd status`, `wd send`, `wd commit`, `wd push`, `wd sync`, and `wd check`. The exact list is frozen in the inventory/contract phase using existing documentation and, where available, privacy-safe command-path telemetry. Permanent shortcuts are wrappers over canonical commands, not a second implementation.

## Complete current-to-canonical mapping

The table is the migration inventory for the current tree registered in `internal/cli/root.go`. A row containing a subtree means every listed child moves with the shown spelling unless the row gives a rename. All current paths remain hidden compatibility aliases. Proposed canonical names marked **decision required** must not ship until the ambiguity process below resolves them.

### Agent

| Current command | Canonical command | Notes |
|---|---|---|
| `ls` | `agent list` | `ls` is a likely permanent shortcut. |
| `start` | `agent start` | Share all flags and run logic. |
| `status <agent>` | `agent status <agent>` | Naming ambiguity with `show`; preserve semantics. |
| `digest <agent>` | `agent digest <agent>` | Per-agent accomplishment summary. |
| `fork` | `agent fork` | Preserve Codex-only validation and output. |
| `restore`, `recover`, `adopt`, `attach` | `agent restore`, `agent recover`, `agent adopt`, `agent attach` | Preserve local/daemon distinctions. |
| `stop`, `terminate`, `done`, `delete`, `remove-worktree` | `agent stop`, `agent terminate`, `agent done`, `agent delete`, `agent remove-worktree` | **Decision required:** retain distinct lifecycle semantics until separately resolved. |
| `send`, `tail` | `agent send`, `agent tail` | `send` may also remain a permanent shortcut; do not merge with directed `message send`. |
| `handoff`, `rotate` | `agent handoff`, `agent rotate` | Keep `rotate` as the current retire-self compatibility operation. |
| `switch` | `agent switch` | Backend/model/tier hot swap for one agent. |
| `set-permission-mode` | `agent permission-mode set` | Preserve relaunch and validation behavior. |
| `set-role` | `agent role set` | Preserve compatibility with the flat verb. |
| `role list` | `agent role list` | Built-in roles. |
| `role set-tier`, `role tier`, `role tier list` | `agent role set-tier`, `agent role tier`, `agent role tier list` | Inventory exact existing nested paths before factory extraction. |
| `force-compact` | `agent compact set` | Preserve `on|off|inherit`; canonical wording must not imply a new compact operation. |

### Pipeline

| Current command | Canonical command | Notes |
|---|---|---|
| `pipeline validate` | `pipeline validate` | Already canonical. |
| `pipeline create` | `pipeline create` | Already canonical. |
| `pipeline list-templates` | `pipeline template list` | Hide old nested spelling as an alias. |
| `pipeline list`, `pipeline show` | unchanged | Preserve output and identifiers. |
| `pipeline start`, `pause`, `resume`, `cancel`, `delete` | unchanged | Preserve lifecycle state transitions and safety. |
| `pipeline emit`, `edit-job`, `retry` | unchanged | Preserve payloads and exit behavior. |

### Autopilot

| Current command | Canonical command | Notes |
|---|---|---|
| `autopilot init`, `list`, `register` | unchanged | `init` remains local authoring. |
| `autopilot on`, `off` | provisionally unchanged | **Decision required:** repo-level enablement switch, not run lifecycle. |
| `autopilot start`, `stop`, `pause`, `resume` | provisionally unchanged | **Decision required:** individual registered-run lifecycle. Do not conflate `off` with `stop`. |
| `autopilot status` | provisionally unchanged | **Decision required:** clarify whether aggregate enablement/run state deserves `status`, `show`, or separate paths. |
| `land` | `autopilot land` | Preserve gate checks and integration-branch behavior. |

### Backend and models

| Current command | Canonical command | Notes |
|---|---|---|
| `backends` and `backends list`, `rescan`, `tier`, `default`, `enable`, `disable`, `thinking-mode` | `backend` and `backend list`, `backend rescan`, `backend tier`, `backend default`, `backend enable`, `backend disable`, `backend thinking-mode` | Namespace becomes singular; server registry semantics stay fixed. |
| `models` | `backend model` | Preserve its current default catalog view. |
| `models list` | `backend model list` | Live model/catalog view. |
| `models tier <backend> <model> <tier>` | `backend model tier <backend> <model> <tier>` | Do not merge with backend-level tier assignment. |
| `llm suggest` | `backend suggest` | Preserve local hardware/model recommendation semantics. |
| `llm` | `backend suggest` | The empty legacy parent remains a hidden compatibility path. |
| `repl` | `backend repl` | Its interactive semantics do not change. |

### Git and check

| Current command | Canonical command | Notes |
|---|---|---|
| `commit`, `push`, `sync`, `review` | `git commit`, `git push`, `git sync`, `git review` | High-frequency flat forms may remain permanent shortcuts. |
| `check` | `check run` | `wd check` may remain a permanent shortcut invoking `check run`. |
| `hook git-guard` | `git guard` | Internal/hook-facing alias remains hidden and stable. |
| `hook check-guard` | `check guard` | Preserve hook protocol, JSON, and exit codes exactly. |
| `hook guard`, `hook root-guard` | `check boundary`, `check root-guard` | Preserve fail-open isolation and root-boundary semantics; the hidden `hook` parent also remains compatible. |

### Workspace and project

| Current command | Canonical command | Notes |
|---|---|---|
| `worktree`, `worktree list`, `worktree prune`, `prune` | `workspace`, `workspace list`, `workspace prune` | Preserve the current bare-parent behavior, confirmation, and force guards. |
| `snapshot create`, `snapshot list`, `snapshot restore` | `workspace snapshot create`, `workspace snapshot list`, `workspace snapshot restore` | Snapshot still binds worktree plus transcript. |
| `branches` | `workspace branches` | Per-agent CI and base comparison. |
| `collab conflicts`, `collab who-is-editing` | `workspace conflicts`, `workspace who-is-editing` | Canonical placement emphasizes file/worktree collision checks. |
| `memory` | `project memory` | Preserve `.warden/memory.md` behavior. |
| `preset`, `preset save`, `preset list` | `project preset`, `project preset save`, `project preset list` | Local configuration authoring; preserve bare-parent help behavior. |
| `prompt-template`, `prompt-template save`, `prompt-template list` | `project prompt-template`, `project prompt-template save`, `project prompt-template list` | Preserve template substitution and bare-parent help. |
| `library`, `library list`, `library save-preset`, `library save-prompt` | `project library`, `project library list`, `project preset save`, `project prompt-template save` | Library umbrella remains where it has distinct aggregate behavior; redundant save paths become aliases. |
| `plugin`, `plugin list` | `project plugin`, `project plugin list` | Project extension discovery. |

### Coordination and approvals

| Current command | Canonical command | Notes |
|---|---|---|
| `ctx` and `ctx set`, `cas`, `append`, `get`, `list`, `del` | `context` and `context set`, `context cas`, `context append`, `context get`, `context list`, `context delete` | Keep `ctx` and `del` as hidden aliases. |
| `msg` and `msg send`, `msg inbox`, `msg wait` | `message` and `message send`, `message inbox`, `message wait` | Directed agent mailbox; distinct from terminal `agent send`. |
| `approvals` | `approval list` | Pending prompts. |
| `approve` | `approval answer` | Keep old path hidden; retain option-number behavior. |
| `auto-approve <agent> <on|off>` | `approval auto set <agent> <on|off>` | Preserve policy and output. |
| `auto-approve rules`, `allow`, `deny`, `clear`, `enable`, `disable` | `approval auto rules`, `approval auto allow`, `approval auto deny`, `approval auto clear`, `approval auto enable`, `approval auto disable` | Preserve scopes and policy evaluation. |

### Usage

| Current command | Canonical command | Notes |
|---|---|---|
| `cost` | `usage` | Preserve its current aggregate money view while renaming the namespace. |
| `cost spend`, `spend` | `usage spend` | **Decision required:** distinguish measured currency spend from other usage. |
| `cost savings`, `savings` | `usage savings` | **Decision required:** retain exact estimator and fields. |
| `stats` | `usage resources` provisionally | **Decision required:** current CPU/memory/daemon statistics may belong in `inspect`; do not rename before inventory. |
| `insights` | `usage insights` provisionally | Historical parallelization analysis; inventory may place it under `inspect`. |

### Inspect and operations

| Current command | Canonical command | Notes |
|---|---|---|
| `search` | `inspect search` | Full-text fleet search. |
| `history` | `inspect history` | Archived agents. |
| `audit log` | `inspect audit` | Preserve append-only audit behavior. |
| `export`, `import` | `inspect export`, `inspect import` | These are operational portability actions; import remains clearly mutating in help. |
| `repair`, `repair sessions` | `inspect repair`, `inspect repair sessions` | Preserve offline, backup-first behavior and bare-parent help. |
| `mcp` | `daemon mcp` | Host/process command; old top-level path remains hidden. |
| `token generate`, `token show`, `token rotate` | `daemon token generate`, `daemon token show`, `daemon token rotate` | Remote-access bearer token operations. |

### Schedule, config, daemon, completion, and system commands

| Current command | Canonical command | Notes |
|---|---|---|
| `schedule create`, `list`, `get`, `enable`, `disable`, `delete` | unchanged | Namespace is already canonical; `get` may become `show` only after the naming decision. |
| `config`, `config path`, `config init` | unchanged | Bare `config` continues showing resolved configuration. |
| `daemon` | unchanged | Run the hub process. |
| hidden `hook` subtree | canonical leaves are under `git`/`check`; `hook` remains hidden | Installed hook paths and stdin/stdout/exit contracts are compatibility API. |
| `completion bash`, `zsh`, `fish`, `powershell` | unchanged | Generated from the same canonical tree. |
| `setup`, `tutorial`, `doctor`, `tui`, `version` | unchanged top-level | Explicitly permitted entry points. |

## Ambiguous semantics requiring explicit decisions

Before the relevant namespace PR, write a short decision record and acceptance examples for each ambiguity. Until then, wrappers retain current names beneath the new parent and help text explains the distinction.

1. **Autopilot `on`/`off` versus run `start`/`stop`.** `on` and `off` currently control per-repository enablement; `start` and `stop` address registered runs. Decide whether canonical paths need `autopilot enable|disable` and `autopilot run start|stop`, but do not make `off` stop a run or `stop` disable the repo.
2. **Agent `done`/`stop`/`terminate`/`delete`/`remove-worktree`.** These differ in process termination, record retention, worktree retention, archival, branch deletion, and confirmation. Build a truth table from `internal/cli/lifecycle.go` and `internal/cli/lifecycle_test.go`; approve vocabulary separately. No alias may accidentally select a more destructive flag combination.
3. **`status`/`show`/`stats`.** Define `status` as live state, `show` as one resource's detail, and `stats` or `resources` as aggregates only if current implementations support that distinction. Apply consistently to agents, pipelines, schedules, autopilot, and host resource reporting.
4. **`usage`/`spend`/`savings`/`metrics`.** Decide which data is measured, estimated, financial, token-based, or operational. Preserve current calculations, time windows, JSON, and units while the information architecture changes.

Every decision gets positive examples, a legacy-equivalence example, machine-output expectations, and a statement of what remains out of scope.

## Technical design

### Command factories and compatibility wrappers

Cobra commands are stateful nodes: one `*cobra.Command` cannot be reused beneath two parents. Do not register a factory result under both a canonical namespace and the root.

Refactor each affected command into three layers:

1. Shared option/state construction and a shared `RunE` implementation (or a small runner function).
2. A newly allocated canonical `*cobra.Command` with canonical `Use`, help, examples, annotations, and flags.
3. A separately allocated legacy wrapper command that binds the same options and runner, is hidden from normal help, and carries canonical-path/deprecation annotations.

Factories must return fresh commands and fresh flag storage on every call. Avoid global option variables and avoid invoking a canonical command through `Execute`; wrappers call shared logic directly. Add focused factory tests proving independent flag sets and parentage.

`internal/cli/root.go` remains the construction entry point but delegates namespace creation to focused factories such as `newAgentCmd`, `newUsageCmd`, and `newInspectCmd`. Add `internal/cli/help.go` for annotations, group definitions, ordering, help command behavior, template/renderer installation, alias formatting, and `--all` traversal.

### Annotations

Use command annotations (with constants in `internal/cli/help.go`) for:

- help group identifier and group order;
- command order within a group;
- canonical path;
- alias kind (`compatibility` or `permanent-shortcut`);
- normal-help visibility and `--all` visibility;
- deprecation policy/message identifier;
- whether a node is a namespace, entry point, leaf, or internal hook;
- generated-doc and completion inclusion.

Validate annotations when building the tree: canonical nodes must have a known group/order, compatibility aliases must resolve to a canonical path, and no visible sibling may have a duplicate order.

### Deterministic help renderer

Implement a custom renderer in `internal/cli/help.go` rather than relying on Cobra's default alphabetical template. It should:

- render the approved group order and command order deterministically;
- treat `help`, `--help`, and `-h` consistently;
- traverse an arbitrary canonical path for `wd help <path...>`;
- render namespace overview and leaf details at the appropriate depth;
- omit hidden compatibility aliases normally and include them, labeled, for `help --all`;
- keep output stable across repeated command-tree construction;
- respect `cmd.SetOut`/`SetErr` for tests and embedding;
- avoid daemon calls and first-run tutorial hints while rendering help.

Golden files should cover root, representative namespace, representative leaf, `--all`, unknown paths, and both `warden` and `wd` presentation if executable-name normalization is supported.

### One tree for help, docs, and completion

`cmd/gendocs/main.go` and `internal/cli/gendocs.go` already generate `site/src/content/docs/reference/cli.md` from `newRootCmd()`. Extend that traversal to honor the same annotations and deterministic order as runtime help. The generated reference should lead with canonical paths and have an explicit compatibility-alias appendix sourced from `help --all`; it must never maintain a second hand-written mapping.

Shell completion in `internal/cli/completion.go` must walk the same tree and prefer canonical paths. Test Bash, Zsh, Fish, and PowerShell generation, including canonical nested commands, permanent shortcuts, hidden aliases, and no duplicate candidates.

Update prose that teaches command paths, including `docs/USAGE.md`, `docs/FEATURES.md`, relevant pages under `site/src/content/docs/`, and the operational skill at `skills/warden/SKILL.md` plus `skills/warden/references/{agents,pipelines,git-and-checks,coordination,operations}.md`. The generated site reference is changed only through `go run ./cmd/gendocs`/`make gendocs`, never by hand.

## Delivery phases and bounded PRs

Deliver this as multiple reviewable PRs. Each PR must be independently green, preserve all legacy paths in its scope, and contain no API/storage/lifecycle changes.

### PR 1: Inventory and contract

- Add a machine-readable snapshot test of every current command path, flags, aliases, JSON support, and relevant exit behavior.
- Record permanent-shortcut candidates and the ambiguity decision records.
- Add lifecycle/destructive-operation truth tables from command-specific files and tests.
- Freeze representative stdout/stderr fixtures for compatibility comparison.

### PR 2: Help infrastructure

- Add `internal/cli/help.go`, annotations, deterministic grouping/order, progressive rendering, and `help --all`.
- Make `wd help`, `wd --help`, and `wd -h` equivalent.
- Add command-tree validation and root/namespace/leaf golden tests.
- Do not move domain commands yet; use annotations on the existing tree to prove infrastructure safely.

### PR 3: Agent namespace

- Introduce canonical `agent` factories and independent legacy wrappers.
- Preserve permanent high-frequency shortcuts selected in PR 1.
- Add exhaustive lifecycle alias compatibility and safety tests before moving destructive verbs.
- Update agent-specific docs and skill references.

### PRs 4a–4n: Remaining namespaces in parallel

After the shared help infrastructure lands, split work by non-overlapping command files: pipeline/autopilot; project/workspace; backend/usage/inspect; git/check; context/message/approval; schedule/config/daemon/system. Coordinate ownership of `internal/cli/root.go`, `internal/cli/help.go`, generated reference, and shared goldens to avoid merge conflicts. Each PR includes its subtree mapping and compatibility tests.

### PR 5: Documentation and completion convergence

- Regenerate `site/src/content/docs/reference/cli.md` through `cmd/gendocs/main.go` and `internal/cli/gendocs.go`.
- Update user guides, feature catalog, cross-links, examples, and all warden skill docs.
- Validate completion for every supported shell and generated-doc drift.
- Search the repository for promoted legacy paths and update prose while retaining compatibility notes where useful.

### PR 6: Deprecation and telemetry

- Add isolated, stderr-only legacy notices after canonical paths have shipped.
- Suppress notices in JSON/machine modes and internal hook invocations.
- Add privacy-safe command-path counters only through a separately approved telemetry mechanism; do not collect arguments, prompts, paths, identifiers, or output.
- Use evidence to confirm permanent shortcuts and inform a future removal proposal. This plan does not authorize removal of any legacy alias.

## Testing strategy

### Command-tree tests

- Every approved canonical path exists exactly once and has the expected parent, group, order, visibility, and annotations.
- Every inventoried legacy path resolves to the declared canonical path.
- Factories return distinct command and flag instances.
- `help`, `--help`, and `-h` equivalence; path traversal and unknown-command diagnostics.
- `help --all` is complete, deterministic, and labels compatibility aliases.

### Help goldens

Add goldens for root help, all canonical namespace pages, representative deep leaves (`agent start`, `pipeline edit-job`, `approval auto allow`), hidden alias appendix, inherited flags, examples, and destructive safety warnings. Run each golden twice from newly constructed trees to catch mutable global state.

### Canonical-versus-alias compatibility

For every mapping, execute canonical and legacy commands against the same fake client or fixture and compare:

- REST method/path/body and call count;
- stdout, excluding an approved interactive deprecation line;
- stderr and confirmation behavior;
- returned errors and exit classification;
- flag defaults, validation, mutually exclusive flags, and positional arguments.

Prioritize command-specific files and tests such as `internal/cli/lifecycle.go`/`lifecycle_test.go`, `pipeline.go`/`pipeline_test.go`, `autopilot.go`, `backends.go`/`backends_test.go`, `models.go`/`models_test.go`, `git.go`/`git_test.go`, `worktree.go`/`worktree_test.go`, `approvals.go`/`approvals_test.go`, and the existing `commands*_cli_test.go` suites.

### JSON and deprecation isolation

- JSON stdout is byte-equivalent for canonical and alias paths.
- Deprecation text is never written to stdout and never corrupts JSON, completion, generated docs, hooks, or pipe-oriented output.
- Existing JSON field names, ordering guarantees where tested, and exit codes remain frozen.

### Completion and generated docs

- Generate Bash, Zsh, Fish, and PowerShell completion successfully.
- Assert canonical nested paths complete; hidden aliases are not broadly advertised; permanent shortcuts do complete.
- `make gendocs-check` leaves `site/src/content/docs/reference/cli.md` clean.
- A generated-doc test verifies canonical ordering and the alias appendix from the same annotated tree.

### Safety and lifecycle

- Destructive agent/worktree/pipeline/schedule operations retain prompts, `--force` gates, ownership checks, and keep-record/keep-worktree behavior.
- Autopilot enablement remains distinct from run lifecycle and landing gates.
- Git/check guard entry points retain their machine protocol and exit codes.
- Help paths never contact the daemon, mutate config, emit tutorial hints, or trigger lifecycle operations.

## Comprehensive acceptance criteria

1. The normal root help contains exactly the approved canonical namespaces, permitted top-level entry points, and approved permanent shortcuts, grouped and deterministically ordered.
2. `wd help`, `wd --help`, and `wd -h` are equivalent; `--help` is canonical in docs, `-h` is described as shorthand, and `help` is described as exploratory.
3. `wd agent --help` equals `wd help agent`, and `wd agent start --help` equals `wd help agent start`; the leaf page is more focused and detailed than the namespace page.
4. No maintained documentation promotes `wd agent help`.
5. `wd help --all` includes every canonical command and every inventoried legacy alias, clearly labeled.
6. Every current path in this document remains executable and dispatches the same option/run logic as its canonical path.
7. High-frequency shortcuts selected by the inventory are documented as permanent; all other legacy paths are hidden from ordinary help and normal completion.
8. Canonical and alias invocations have compatible flags, client calls, stdout, stderr, JSON, errors, exit codes, confirmations, and lifecycle effects.
9. No REST, MCP, storage, lifecycle, backend-routing, JSON, or exit-code semantic change appears in a redesign PR without a separate decision and acceptance test.
10. The four ambiguity areas have explicit decisions before affected canonical wording is finalized.
11. Cobra factories create separate `*cobra.Command` objects for canonical and alias parents while sharing option and run logic.
12. Command annotations fully determine help grouping, order, canonical path, and alias visibility; tree validation rejects missing or inconsistent metadata.
13. Runtime help, `site/src/content/docs/reference/cli.md`, and all four shell completions derive from the same tree and agree on canonical paths.
14. Command-tree tests, help goldens, compatibility tests, JSON/deprecation isolation tests, completion tests, generated-doc drift checks, and safety/lifecycle tests pass.
15. `docs/USAGE.md`, `docs/FEATURES.md`, relevant site guides, and `skills/warden/` documentation teach canonical paths and accurately note compatibility aliases.
16. Delivery occurs as bounded PRs in the phased order above; no PR removes legacy behavior, mixes unrelated production semantics, or requires a flag day.

## Implementation checklist

- [ ] Snapshot the current `internal/cli/root.go` tree and resolve every ambiguity decision.
- [ ] Add annotation constants, validators, and deterministic renderer in `internal/cli/help.go`.
- [ ] Install equivalent root help and progressive path traversal, including `help --all`.
- [ ] Extract fresh-command factories and shared runners; add canonical nodes and hidden wrappers.
- [ ] Migrate `agent`, then parallelize the remaining non-overlapping namespaces.
- [ ] Update `internal/cli/gendocs.go`, `cmd/gendocs/main.go` only if needed, and regenerate the site reference.
- [ ] Update `internal/cli/completion.go` and verify all supported shells.
- [ ] Update command-specific tests, goldens, prose docs, generated reference, and `skills/warden/` docs.
- [ ] Run repository checks, generated-doc drift checks, and a repository-wide legacy-path audit.
- [ ] Self-review each PR for semantic freeze, alias completeness, machine-output isolation, safety, test coverage, and scope.

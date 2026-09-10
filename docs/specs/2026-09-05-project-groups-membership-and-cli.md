# Project groups membership fix + projects/groups CLI

- **Status:** Approved for implementation (autopilot run)
- **Issue:** [srjn45/warden#432](https://github.com/srjn45/warden/issues/432) (labels: `bug`, `enhancement`)
- **Repo:** warden (node). Baseline `main` @ `a863361` (v8.25.0).
- **Author:** orch-warden (successor of `agent-560d77d4`)
- **Scope:** Two coupled deliverables — (A) fix silent/failing project-group
  membership over HTTP, (B) add first-class `warden` CLI for projects and
  project groups. Implementation approved — execute via autopilot.

---

## 0. Problem, grounded in the code

### A. The membership bug

The issue reports that `POST /api/v1/project-groups/{id}/members` does nothing
(empty body, or **HTTP 405** with `Content-Type: application/json`), and group
membership stays `[]`.

Root cause: **there is no `/members` sub-resource at all.** It is undeclared end
to end:

- `internal/daemon/apidocs/openapi.yaml` documents membership **only** through
  `PUT /api/v1/project-groups/{id}` (full replace of `name` + `project_ids`).
  There is no `.../members` path.
- The generated strict chi server (`internal/daemon/oapi/api.gen.go`) therefore
  registers no route for it. A `POST` to that URL never reaches a handler — the
  router resolves `{id}` and treats `/members` as an unrouted tail, so the
  response is a router-level 404/405, not a handler error (which is why the body
  is empty and the status flips with method/headers).
- **The store layer is already built for this and is currently dead code.**
  `internal/projectstore/groups.go` has `AddProjectToGroup(groupID, projectID)`
  (line 191) and `RemoveProjectFromGroup(groupID, projectID)` (line 216) — both
  fully implemented (RMW, dedupe, `ErrGroupNotFound`, idempotent) — but **no
  daemon handler and no client method calls them.** They have unit coverage in
  `groups_test.go` but zero wiring above the store.

So the fix is not to repair a broken handler; it is to **expose the store
methods that already exist** through the spec-first API, plus a client method
and CLI. This is the least-surprising fix and matches exactly what the reporter
tried to do.

### B. No CLI for first-class projects or groups

`warden project` (`internal/cli/project.go`) is the **repo-local config**
namespace only (memory, presets, prompt-templates, library, plugins). The
**first-class daemon projects** (`/api/v1/projects…`) and **project groups**
(`/api/v1/project-groups…`) have no CLI surface at all — only raw curl.

The daemon client (`internal/client/client.go`) already has:
`ListProjects`, `CreateProject`, `OpenLocalProject`, `OpenRemoteProject`,
`CloseProject`, `ListProjectGroups`. It is **missing**: register/open-by-id,
get/create/update/delete group, and add/remove member.

---

## 1. Locked design decisions

| Decision | Choice | Why |
|---|---|---|
| Membership API shape | Add an incremental **`/members` sub-resource** (not just document the PUT full-replace) | The store methods already implement incremental add/remove; it is what the reporter attempted; PUT full-replace stays as the bulk-set path. |
| Add member | `POST /api/v1/project-groups/{id}/members` with body `{ "project_id": "…" }` | Mirrors the reporter's exact call. |
| Remove member | `DELETE /api/v1/project-groups/{id}/members` with body `{ "project_id": "…" }` | Project ids are filesystem paths / remote URLs → contain `/` and `:` → **cannot** be a clean path segment. A request body avoids double-percent-encoding traps. oapi-codegen supports a DELETE body. |
| Return value | Both return the **updated `ProjectGroup`** (200) | Same convention as `PUT`; caller sees resulting membership. |
| Errors | `404` unknown group; `400` blank/missing `project_id`, with a **non-empty JSON error body** | Directly addresses the "empty body on failure" complaint. |
| Member existence check | **No** — a member id need not resolve to a registered project | Matches `CreateGroup`/`UpdateGroup` semantics already documented ("need not resolve to existing projects"). Keep it consistent; do not add a new validation asymmetry. |
| CLI namespaces | New top-level `warden projects` (plural) and `warden project-groups` | Mirrors the API paths 1:1. `warden project` (singular) stays the repo-local-config namespace. See §5 for the naming collision note. |
| Alias | `wd` inherits both automatically | It is the same cobra tree. |

Open question surfaced for the maintainer, not blocking the plan: **the
singular/plural `project` vs `projects` split is a known readability hazard.**
Alternative considered and rejected for now: fold groups under
`warden projects groups …`. Rejected because it diverges from the API path
(`/project-groups`) and the issue's own expected shape. Flagged in §5.

---

## 2. Work items (spec-first, in order)

Follow the repo's spec-first rail: **edit `openapi.yaml`, then `make generate`;
never hand-write `api.gen.go` DTOs or the strict-server interface.**

### J1 — OpenAPI: add the `/members` sub-resource
File: `internal/daemon/apidocs/openapi.yaml`

- New path `/api/v1/project-groups/{id}/members`:
  - `post` → `operationId: addProjectGroupMember`, body
    `AddProjectGroupMemberRequest { project_id: string }`, `200 ProjectGroup`,
    `400 BadRequest`, `404 NotFound`.
  - `delete` → `operationId: removeProjectGroupMember`, same body,
    `200 ProjectGroup`, `400`, `404`.
- New request schema `AddProjectGroupMemberRequest` under `components/schemas`
  (reused by both ops).
- Keep `PUT /project-groups/{id}` unchanged; tighten its description to state it
  is the **full-replace / bulk-set** path and point to `/members` for
  incremental edits.
- Do **not** add either op to `oapi/config.yaml` `exclude-operation-ids` — these
  are ordinary JSON routes and must be generated into the strict server. (The
  exclude list is only for SSE/WS/docs/health routes.)

Then `make generate` and commit the regenerated `internal/daemon/oapi/api.gen.go`.

### J2 — Daemon handlers
File: `internal/daemon/strict_project_groups.go`

Add two methods implementing the generated strict interface, following the
existing handlers in this file:

- `AddProjectGroupMember`: nil-body / blank `project_id` → `400`
  ("project_id is required"); call `s.projects.AddProjectToGroup(decodeID(id),
  projectID)`; map `ErrGroupNotFound` → `404`, `ErrInvalidID` → `400`; else
  `200` with the group.
- `RemoveProjectGroupMember`: same wiring against
  `s.projects.RemoveProjectFromGroup`. (Remove of an absent member is a store
  no-op → `200`, matching `RemoveProjectFromGroup`'s documented behaviour.)

No new store code — J1's methods already exist and are tested. This item is the
one that turns the dead store methods live.

### J3 — Client methods
File: `internal/client/client.go`

Add, mirroring the existing project methods:

- `AddProjectGroupMember(ctx, groupID, projectID) (ProjectGroup, error)`
- `RemoveProjectGroupMember(ctx, groupID, projectID) (ProjectGroup, error)`
- `GetProjectGroup(ctx, id) (ProjectGroup, error)`
- `CreateProjectGroup(ctx, name, projectIDs) (ProjectGroup, error)`
- `UpdateProjectGroup(ctx, id, name, projectIDs) (ProjectGroup, error)`
- `DeleteProjectGroup(ctx, id) error`
- `OpenProject(ctx, id, name) (Project, error)` — the register/reopen-by-id
  `POST /projects/open` route (currently unused by any client method).

Reuse existing `ListProjects`, `CreateProject`, `OpenLocalProject`,
`OpenRemoteProject`, `CloseProject`, `ListProjectGroups`. Percent-encode the
group id into the path exactly as `CloseProject` does.

### J4 — CLI: `warden projects`
New file: `internal/cli/projects.go` (+ wire into the root command and
`help.go` metadata table).

Subcommands (thin callers of the client, table/`--json` output like `backends`):

| Command | Client call |
|---|---|
| `warden projects list [--json]` | `ListProjects` |
| `warden projects open <id> [--name]` | `OpenProject` (register/reopen by id) |
| `warden projects open-local <path> [--name]` | `OpenLocalProject` |
| `warden projects open-remote <url> [--name]` | `OpenRemoteProject` |
| `warden projects new <name>` | `CreateProject` |
| `warden projects close <id>` | `CloseProject` |

### J5 — CLI: `warden project-groups`
New file: `internal/cli/project_groups.go`.

| Command | Client call |
|---|---|
| `warden project-groups list [--json]` | `ListProjectGroups` |
| `warden project-groups show <id>` | `GetProjectGroup` |
| `warden project-groups create <name> [--project <id>]…` | `CreateProjectGroup` |
| `warden project-groups update <id> [--name] [--project <id>]…` | `UpdateProjectGroup` (bulk-set) |
| `warden project-groups delete <id>` | `DeleteProjectGroup` |
| `warden project-groups members add <id> <project-id>` | `AddProjectGroupMember` |
| `warden project-groups members remove <id> <project-id>` | `RemoveProjectGroupMember` |

Register both namespaces in the root command builder and add their
`help.go`/`SetCommandHelpMetadata` entries so `warden help` and the generated
CLI reference place them correctly.

### J6 — MCP parity (evaluated → defer)
Checked `internal/mcp/`: **no** project / project-group MCP tools exist today
(FEATURES matrix already marks groups as REST/TUI/web only, MCP column `—`).
Do **not** invent an MCP surface in this issue — that would expand scope past
#432. Keep the FEATURES MCP/CLI parity column honest after the CLI lands
(CLI ✓, MCP still —).

---

## 3. Tests

- `internal/projectstore/groups_test.go` — already covers `AddProjectToGroup`
  / `RemoveProjectFromGroup`; add cases only if a gap shows (e.g. blank id).
- `internal/daemon/project_groups_routes_test.go` — **add** route tests:
  `POST/DELETE /members` happy path, `404` unknown group, `400` blank
  `project_id`, **non-empty error body assertion** (this is the regression guard
  for the reported bug), and dedupe/idempotency (double-add, remove-absent).
- `internal/client` — add client round-trip tests against a test server if the
  package has them for the sibling methods.
- `internal/cli` — extend the command-inventory / cli tests
  (`command_inventory_test.go`, `commands*_cli_test.go`) so the new namespaces
  are registered and their help metadata is present.

## 4. Definition-of-Done checklist (CLAUDE.md)

1. **Codegen:** `make generate` (regenerate `api.gen.go`) and `make gendocs`
   (regenerate `site/src/content/docs/reference/cli.md` from the cobra tree —
   never hand-edit it). CI (`make generate-check`, `make gendocs-check`) fails
   on drift.
2. **Docs:**
   - `README.md` — mention the new CLI surface if the CLI overview lists
     namespaces.
   - `docs/FEATURES.md` (prose) + root `FEATURES.md` (matrix) — add
     projects/groups CLI + `/members` endpoint rows; keep MCP/CLI parity column
     honest.
   - `docs/USAGE.md` — usage examples for the new commands.
   - Website `site/src/content/docs/` — update `guides/project-groups.md`
     (it **already documents** `POST …/members` as if it worked — that example
     is the bug; replace curl-only flows with CLI once J4/J5 land, keep a short
     REST note). Generated `reference/cli.md` via `make gendocs`. Adjust
     `reference/features.md` MCP/CLI columns.
   - `skills/warden/` — note the new CLI verbs if agents should drive
     projects/groups via warden (no MCP verbs in this change — see J6).
3. **CLI help:** covered by J4/J5 `Use`/`Short`/`Long` + `make gendocs`.
4. **Tag & release:** this is a small feature + bugfix → **`patch`** bump, one
   tag. Confirm with the maintainer before pushing the `v*` tag (push cuts the
   public GoReleaser release).

## 5. Risks / call-outs for the maintainer

- **`project` vs `projects` collision.** Singular = repo-local config, plural =
  first-class daemon projects. Documented deliberately; if the maintainer
  prefers, fold groups/projects differently (§1 open question). Decide before J4.
- **DELETE-with-body.** Confirm the daemon's HTTP stack and generated client are
  happy sending a body on `DELETE`. If not, fall back to
  `POST /project-groups/{id}/members/remove` with the same body. (Path-segment
  project ids are ruled out — they contain `/` and `:`.)
- **Member-existence validation intentionally omitted** to stay consistent with
  `CreateGroup`/`UpdateGroup`. If the maintainer wants membership to reject
  unknown project ids, that is a separate, broader semantics change across all
  three write paths — out of scope here.
- **Exact 405 mechanic** is a router artifact of the missing route; adding the
  route resolves it. No separate router change needed.

## 6. Suggested delivery shape

One PR (bugfix + CLI are tightly coupled through the same client/handlers), or a
two-job warden pipeline if the maintainer prefers split review:
1. **J1–J3 + route tests** — the membership fix (self-contained, closes the bug
   half of #432).
2. **J4–J6 + docs** — the CLI/MCP surface (the enhancement half).

Recommend the single PR unless the maintainer wants the bug landed first.

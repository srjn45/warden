# ProjectView Projection and Project Event Contract

**Date:** 2026-10-10  
**Status:** Spec freeze / Projection contract — **design only** (Plan `01-warden-db-architecture-and-contract`, Task `aggregate`)  
**Integration branch:** `autopilot/01-warden-db-architecture-and-contract`  
**Depends on:** [`2026-10-10-warden-db-data-source-inventory.md`](2026-10-10-warden-db-data-source-inventory.md), [`2026-10-10-warden-db-collection-contract.md`](2026-10-10-warden-db-collection-contract.md), [`2026-09-05-project-tree-service.md`](2026-09-05-project-tree-service.md)  
**Scope:** Project read-model, project-scoped event/projection contracts, freshness and revision semantics, bounded reads, rebuild behavior, cache invalidation, and TUI/Cockpit/API response shape for schema-2 `warden-db`. This task does not change production data paths, storage schemas, or writers.

Keywords **MUST**, **MUST NOT**, **MAY** are normative.

---

## 1. Decision

`Project` remains an aggregate/query boundary. It is not a nested mutable document and MUST NOT contain authoritative child-id arrays. `ProjectView` is the read contract that clients use when they want a project plus its children.

`ProjectView` is a derived projection over child-to-parent references:

- `agents.project_id`
- `terminals.project_id`
- `plans.project_id`
- `pipelines.project_id`
- `autopilot_runs.project_id`
- `schedules.project_id`
- `projects.group_id`

The view MAY be materialized in memory, cached on disk, or built on demand, but any materialized copy is disposable. It is never the source of truth and MUST NOT be used as an input to authoritative writes.

This contract supersedes the parent-held membership-list portions of [`2026-09-25-project-entity-hierarchy.md`](2026-09-25-project-entity-hierarchy.md) for schema-2. That older document remains useful for entity vocabulary and UX state names, but schema-2 membership is derived exclusively from child records as required by the collection contract.

---

## 2. Projection Inputs

### 2.1 Authoritative inputs

The projector reads these collections and only these relationship edges:

| Child collection | Project edge | Parent/child edge used inside view | Notes |
|---|---|---|---|
| `projects` | `projects.id` | `projects.group_id` | Project metadata, status, group placement, ordering. |
| `agents` | `agents.project_id` | `agents.parent_id`, `agents.pipeline_id`, `agents.job_id`, `agents.autopilot_run_id`, `agents.autopilot_slot` | Agent/project membership and agent forest are derived from indexed FKs. |
| `terminals` | `terminals.project_id` | none | Plain shell panes; leaf nodes only. |
| `plans` | `plans.project_id` | current `pipeline_id` / `autopilot_run_id` back-pointers | Canonical plan definitions and task progress. |
| `pipelines` | `pipelines.project_id` | `pipelines.parent_agent_id`, `pipelines.plan_id` | Pipeline headers only. |
| `pipeline_jobs` | via `pipeline_jobs.pipeline_id` -> `pipelines.id` | `pipeline_jobs.agent_id`, `depends_on` | Joined only for pipelines already selected by project. |
| `autopilot_runs` | `autopilot_runs.project_id` | `parent_agent_id`, `manager_agent_id`, `brain_agent_id`, `plan_id` | Run state and slots. |
| `schedules` | `schedules.project_id` | none | Optional project automation summary. |

The projector MAY read observed fields such as `agents.status`, `last_pane_excerpt`, `pipelines.status`, and `autopilot_runs.status`, but it MUST label freshness honestly (§3) because observed fields are last-seen snapshots.

### 2.2 Forbidden inputs

The projector MUST NOT read or reconstruct legacy authoritative inverse arrays such as `Project.agents[]`, `Project.pipelines[]`, `Project.terminals[]`, `Project.plans[]`, `Project.autopilots[]`, `Agent.child_agents[]`, `Agent.child_pipelines[]`, `Agent.child_autopilots[]`, or embedded `Pipeline.jobs[]` as membership authority.

Repair/migration MAY inspect legacy arrays only to strip, quarantine, or compare legacy data. Normal `ProjectView` reads do not consult them.

---

## 3. Revision and Freshness Semantics

### 3.1 Collection revisions

Every schema-2 collection has a monotonically increasing `collection_revision` maintained by `warden-db`. The revision increments exactly once for each committed write transaction that changes that collection. Cross-collection transactions expose a single `commit_revision` and a vector of touched collection revisions.

`ProjectView` carries both:

```jsonc
{
  "revision": "pv:projects=42,agents=819,terminals=18,plans=77,pipelines=134,pipeline_jobs=401,autopilot_runs=22,schedules=9",
  "as_of": "2026-10-10T13:48:12Z"
}
```

The string form is opaque to clients. Clients compare it for equality only. Server code MAY keep a structured revision vector internally.

### 3.2 ProjectView revision

A `ProjectView` revision is the stable hash or opaque encoding of the highest relevant collection revisions used to build the view. It changes when any selected input that can affect the view changes, including child create/delete/archive, status changes surfaced in the view, plan task progress, project group changes, and schedule changes.

A `ProjectView` revision MUST NOT change for unrelated writes outside the requested project unless the response includes global buckets such as `no_project`, loose-directory groups, or group summaries that those writes affect.

### 3.3 Freshness classes

Every response declares one freshness class:

| Freshness | Meaning | Client behavior |
|---|---|---|
| `strong` | Built from one consistent `warden-db` read transaction. No known invalidation occurred before the response was sent. | Safe for TUI/Cockpit/API primary render. |
| `bounded-stale` | Served from cache and no invalidation newer than `max_staleness_ms` is known. | Render normally, but keep the revision marker for conditional refresh. |
| `rebuilding` | Cache was invalid or missing; response is either a strong on-demand build or a previous view with `stale_since`. | Render with existing data and subscribe/poll for the next revision. |
| `degraded` | One or more non-authoritative/optional inputs failed; authoritative project membership was still read. | Render available data and show degraded markers. |

`strong` does not mean observed process state is current with the OS. It means the database snapshot is internally consistent. Observed fields still retain their normal `observed_at` semantics from the collection contract.

### 3.4 Conditional reads

API readers MAY pass `If-None-Match` or `?revision=<opaque>`. If the server's current projection revision equals the client revision, the server SHOULD return `304 Not Modified` for HTTP or a small `{ "unchanged": true, "revision": "..." }` envelope for non-HTTP transports.

---

## 4. Bounded Read Contract

Project reads MUST be index-bounded. A ProjectView build MUST NOT scan every agent, pipeline, plan, or terminal except during an explicit full rebuild (§6).

### 4.1 Single-project read

For `project_id=P`, the read plan is:

1. `projects[id=P]`
2. `agents WHERE project_id=P`
3. `terminals WHERE project_id=P`
4. `plans WHERE project_id=P`
5. `pipelines WHERE project_id=P`
6. `autopilot_runs WHERE project_id=P`
7. `schedules WHERE project_id=P`
8. `pipeline_jobs WHERE pipeline_id IN selected_pipeline_ids`

The planner MUST cap fan-out with response limits (§4.3) rather than silently scanning unbounded children.

### 4.2 Multi-project list

Project list surfaces SHOULD use summaries by default:

- project metadata
- counts by child type and status
- active/blocked/need-input rollups
- newest activity timestamps
- `truncated` flags

They MUST NOT include full child arrays for every project unless the caller explicitly requests expansion with limits.

### 4.3 Limits and truncation

Every API response that embeds children MUST accept or apply server defaults for:

| Limit | Default | Applies to |
|---|---:|---|
| `max_projects` | 200 | Multi-project responses. |
| `max_children_per_type` | 500 | Agents, terminals, plans, pipelines, autopilot runs, schedules per project. |
| `max_pipeline_jobs` | 500 | Jobs per pipeline expansion. |
| `max_depth` | 4 | Agent/pipeline/autopilot nesting. |
| `include_archived` | `false` | Archived/terminal lifecycle records. |

When a limit is hit, the response MUST set `truncated: true` and include `truncation` details naming the collection, project id, limit, and returned count. Clients MUST treat omitted children as unknown, not absent.

---

## 5. Project Event Contract

Projection invalidation is event-driven. Events are derived from committed `warden-db` transactions and are emitted after commit. They are at-least-once, ordered by `(commit_revision, sequence)`, and idempotent by `event_id`.

### 5.1 Event envelope

```jsonc
{
  "event_id": "pe:00000000000042:03",
  "commit_revision": 42,
  "sequence": 3,
  "occurred_at": "2026-10-10T13:48:12Z",
  "project_ids": ["/work/warden"],
  "collection": "agents",
  "record_id": "agent-1234abcd",
  "kind": "project.child_changed",
  "change": "upsert|delete|archive|status|membership|rebuild",
  "previous_project_id": "/work/old",
  "project_id": "/work/warden"
}
```

`project_ids` contains every affected project. A child move from one project to another invalidates both old and new projects. An unscoped child changing into or out of the synthetic no-project bucket invalidates the global tree and the synthetic bucket.

### 5.2 Event kinds

| Kind | Emitted when | Invalidates |
|---|---|---|
| `project.metadata_changed` | `projects` metadata/status/group/order changes. | That project and project lists/groups. |
| `project.child_changed` | A child with `project_id` is created, deleted, archived, restored, or changes a displayed field/status. | Child's project view and summaries. |
| `project.child_moved` | Child `project_id` changes. | Old project, new project, global buckets. |
| `project.hierarchy_changed` | `agents.parent_id`, `pipelines.parent_agent_id`, `pipeline_jobs.agent_id`, or autopilot slot/manager links change. | Owning project tree. |
| `project.plan_progress_changed` | Plan definition, revision, task progress, or current executor binding changes. | Project plan section and summaries. |
| `project.group_changed` | `projects.group_id`, `group_order`, or `project_groups` metadata changes. | Project lists and group summaries. |
| `project.rebuild_requested` | Repair, migration, or cache corruption detection requests a rebuild. | Named projects or all projections. |

The event stream does not add new domain authority. It is a projection maintenance mechanism over committed records.

---

## 6. Rebuild Behavior

### 6.1 Incremental rebuild

On each project event, the cache invalidator marks the affected project ids dirty. The next read MAY rebuild synchronously from a strong DB snapshot or return a bounded-stale view while an async rebuild runs.

Incremental rebuild reads only the bounded query set for the affected project (§4.1).

### 6.2 Full rebuild

A full rebuild is allowed only for:

- daemon startup with empty projection cache
- schema upgrade or projection version change
- `warden repair` after integrity fixes
- detected cache corruption or missing dependency index
- explicit operator/admin request

Full rebuild scans projects first, then performs per-project bounded reads. It MUST write projection cache entries only after each project's view validates. A failed project rebuild leaves its previous cache entry intact and marks it `degraded` or `rebuilding`; it does not poison other projects.

### 6.3 Determinism

For the same DB snapshot and limits, ProjectView output MUST be byte-stable modulo map key ordering controlled by the JSON encoder. Ordering is:

1. explicit `group_order`, then project `created_at`, then project `id`
2. active before terminal children
3. `need-input`, `rate_limited`, `busy`, `pending`, `idle`, `orphaned`, `done`
4. newest `updated_at` or observed activity
5. `id` tie-breaker

Clients MAY preserve local collapse/cursor state by stable node ids, but MUST NOT resort server-ordered children.

---

## 7. Cache Invalidation Rules

### 7.1 Cache keys

Cache keys include:

- `project_id` or global/list scope
- requested expansion flags
- limits
- `include_archived`
- projection schema version

Changing any key component requires a separate projection entry.

### 7.2 Invalidation sources

The cache MUST invalidate on:

- project metadata/status/group/order writes
- any selected child write where `project_id` is set
- `project_id` changes on a child, invalidating old and new ids
- hierarchy edges used inside the tree (`parent_id`, `parent_agent_id`, `pipeline_id`, `job_id`, `agent_id`, autopilot slot links)
- plan `revision`, `status`, `task_progress`, current executor bindings, or archive changes
- pipeline job status/agent/dependency changes for selected pipelines
- schedule create/update/delete for a project
- projection version changes
- repair/migration events that touch relevant collections

The cache MUST NOT invalidate a project because an unrelated project changed.

### 7.3 Eviction

Projection caches are capacity-capped derived data. They MAY be evicted at any time. Eviction never deletes authoritative records and never changes `collection_revision`.

---

## 8. Response Shape

### 8.1 API `ProjectView`

`GET /api/v1/projects/{project_id}/view` returns one project view. `GET /api/v1/project-views` returns summaries by default and accepts explicit expansion flags.

```jsonc
{
  "project": {
    "id": "/work/warden",
    "name": "warden",
    "path": "/work/warden",
    "status": "open",
    "group_id": "group-core",
    "group_order": 10,
    "created_at": "2026-10-01T10:00:00Z",
    "updated_at": "2026-10-10T13:48:12Z"
  },
  "revision": "pv:...",
  "freshness": "strong",
  "as_of": "2026-10-10T13:48:12Z",
  "stale_since": "",
  "truncated": false,
  "truncation": [],
  "summary": {
    "agents": { "total": 12, "busy": 4, "need_input": 1, "rate_limited": 0, "done": 7 },
    "terminals": { "total": 2, "busy": 1 },
    "plans": { "total": 3, "in_progress": 1 },
    "pipelines": { "total": 4, "running": 1 },
    "autopilot_runs": { "total": 1, "active": 1 },
    "schedules": { "total": 2, "enabled": 1 }
  },
  "children": {
    "agents": [],
    "terminals": [],
    "plans": [],
    "pipelines": [],
    "autopilot_runs": [],
    "schedules": []
  },
  "tree": {
    "roots": []
  }
}
```

Children arrays are derived projections. They MUST NOT be written back to `projects`.

### 8.2 TUI and Cockpit

TUI and Cockpit SHOULD consume the shared tree shape from `GET /api/v1/tree` or the named `tree` SSE event for navigation. When a project detail panel is needed, they SHOULD request `ProjectView` for that project id and use:

- `summary` for badges and rollups
- `children` for detail tables
- `tree.roots` for nested navigation
- `revision` for conditional refresh
- `freshness`, `degraded`, and `truncated` for subtle state markers

TUI/Cockpit MUST NOT join raw collections client-side to infer membership when a ProjectView/tree endpoint is available.

### 8.3 SSE / watch updates

The watch stream emits:

```jsonc
{
  "type": "project_view",
  "project_id": "/work/warden",
  "revision": "pv:...",
  "freshness": "strong",
  "view": { }
}
```

For high-churn projects the server MAY coalesce events and emit only the latest revision. Coalescing must preserve the invariant that a client receiving revision `R` can fetch `R` or newer with a conditional read.

---

## 9. Integrity and Repair Rules

`warden repair` validates ProjectView rebuildability by checking:

1. every child `project_id` either references an existing project or is explicitly tolerated as unscoped legacy data
2. every selected child can be reached by exactly one project bucket: its `project_id` or the synthetic no-project bucket
3. no project record contains forbidden inverse membership arrays
4. projection cache entries either match the current revision vector or are discarded
5. rebuilding the same project twice from the same snapshot produces the same revision and child order

Repair MAY delete projection cache entries, request `project.rebuild_requested`, and report dangling references. Repair MUST NOT "fix" membership by writing child ids onto parent projects.

---

## 10. Non-Goals

- No production data path or schema changes in this design task.
- No OpenAPI generation in this task.
- No migration from schema-1 arrays in this task.
- No new authoritative project membership fields.
- No client-specific bespoke tree contracts; TUI, Cockpit, API, and SSE share the same projection semantics.

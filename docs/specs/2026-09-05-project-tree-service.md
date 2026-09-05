# Project-tree service (node-side) — Tech spec / RFC

- **Status:** Accepted (locked with hub 2026-09-05; implementation plan in 2026-09-05-node-implementation-plan.md)
- **Author:** warden-node orchestrator (`agent-560d77d4`)
- **Requested by:** warden-hub web-client planner (`docs-b6fed568`) on behalf of the hub orchestrator
- **Date:** 2026-09-05
- **Consumer spec (producer↔consumer pair):** `warden-hub:docs/web-client.md` §0.5, §2, §10
- **Scope:** a daemon-owned service that computes and serves the full typed
  project hierarchy (`GET /api/v1/tree` + a named `tree` SSE event), extracted
  from `internal/tui/list.go`, consumed by the TUI, web and android clients.

> This is the **producer's** view. Where it disagrees with the hub's consumer
> spec, this document wins on anything internal to the daemon, and every
> disagreement is flagged explicitly in §16 so the hub doc can be reconciled
> rather than left to drift.

---

## 0. Why this exists (one paragraph)

Every client — TUI, hub web, android — otherwise reimplements the same
non-obvious joins to draw one tree: the repo/workdir fallback when `project_id`
is empty, the precedence rule that stops pipeline/autopilot children rendering
twice, and the special case that autopilot workers have `parent_id` cleared.
Three implementations is three chances to diverge, silently. The node already
computes this tree once (in `internal/tui/list.go`); we lift the structural core
into a service behind the API and make the TUI its first consumer, so there is
exactly one implementation forever.

---

## 1. Feasibility & sizing (the question the hub asked first)

**Verdict: feasible; a medium refactor, not a lift-and-shift. The hub's read of
`internal/tui/list.go` is correct, and the entanglement it feared is real but
bounded.**

`list.go` is ~1,770 lines. The structural core the service needs is concentrated
in a handful of *pure* functions that already take sessions/pipelines/projects
and return membership + nesting:

| Function | What it computes | Reuse |
|---|---|---|
| `agentForest` (list.go:328) | roots + `parent_id` children map + cross-project backlink map, with the same-project nesting rule and orphan promotion | **Core.** Lift as-is; drop the backlink *label* (view), keep the *edge* (structure) |
| `resolveGroupKey` (list.go:578) | maps `(project_id, dir)` → open-project id \| Ungrouped \| loose-dir key | **Core.** Lift; this is the membership rule |
| `normalizePipelineDir` (list.go:762) / `normalizeAutopilotDir` (list.go:735) / `sourceDir` (list.go:50) | canonicalize repo/worktree/workdir to a project root (abs, `.worktrees/` stripped) | **Core.** The path-canonicalization the fallback depends on |
| `projectGroupedItems` (list.go:612) | the compositor: joins agents + pipelines + autopilot runs under project/loose-dir/Ungrouped groups | **Core structure, view-entangled.** Re-express to emit nested nodes instead of flat rows |
| `groupHeaderFor` (list.go:743) | project vs loose-dir vs Ungrouped header identity | **Core** (identity/label), minus render flags |

Everything else in the file is **view** and stays in the TUI: `buildRows`,
`renderList`, `renderItemLine`, `treePrefix`, `jobBadge`, `contextLabel`,
`cursorRowIndex`, `abbrevHome`, the `collapsed`/`opened`/cursor maps, the
`item`/`listRow` types, and the `"↳ from <parent>"` label strings.

**The entanglement, stated honestly:** the core functions today *return flat,
pre-ordered `[]item` rows*, not a nested tree, and they interleave view state
(collapse map, home-abbreviated paths, backlink label text, `isProject`/render
flags). The refactor is therefore a genuine **structure/view split**: the service
returns a real nested `[]*Node`; the TUI keeps a thin adapter that walks that
tree, applies its collapse/cursor/label layer, and flattens to `[]listRow` for
rendering. Estimate **~300–500 lines** of reusable structural logic moving into
`internal/tree`, plus a **TUI adapter (~150–250 new lines)** that replaces the
direct calls to `projectGroupedItems`/`buildItems`.

**Can the TUI realistically consume it?** Yes, and it should — that is the whole
point (it becomes the client most likely to catch a regression). The one
constraint: the TUI's cursor-pin identity and collapse map are keyed today by
ad-hoc string keys (`projKey`, `secKey`, `dirKey`, `itemKey`). Those must be
re-keyed onto the **service's composite node ids** (§6) so the split is behind a
single identity scheme, not two. This re-keying is the fiddliest part of the TUI
side and is where review attention pays off.

**Net:** no greenfield algorithm work; the hard logic exists and is tested via
the TUI today. The risk is entirely in the split cleanliness, not in the
computation.

---

## 2. Where it lives — the service package

New package: **`internal/tree`** (no daemon or TUI imports; depends only on
`internal/store`, `internal/pipeline`, `internal/projectstore`, and the
autopilot status types).

```go
package tree

// Service computes the typed hierarchy from the daemon's already-in-memory
// entity lists. It is pure: no I/O, no store access, no disk reads. The caller
// (daemon) supplies snapshots; the service joins them.
type Service struct{}

// Inputs is everything needed to build the tree, gathered by the daemon from
// the stores it already reads for /sessions, /projects, /pipelines and
// autopilot.Status(). Passing snapshots (not stores) keeps the service pure and
// trivially testable, and lets the daemon reuse the reads it already does.
type Inputs struct {
    Sessions  []*store.Session            // full fleet (respect ?all handling at the caller — see §11)
    Projects  []projectstore.Project      // open + closed
    Pipelines []*pipeline.Pipeline        // all pipelines
    Autopilot autopilot.Status            // == s.autopilot.Status(); already carries plan tasks + workers
    Groups    []projectstore.ProjectGroup // optional: project-group labels (future node type; ignored in MVP)
}

// Build returns the ordered root nodes. projectID != "" scopes to one project
// subtree (the "No project" bucket is returned only when projectID == "").
func (s *Service) Build(in Inputs, projectID string) *Tree
```

`Build` is `O(n)` over sessions + pipelines + tasks (a few index maps, one
membership pass, one nesting pass, one ordering pass). No per-node store hits, no
disk reads (see §16 BUILD-3 — plan tasks already arrive parsed in `Autopilot`).

The daemon owns the thin HTTP/SSE layer around it (`internal/daemon`), calling
`tree.Service.Build` with snapshots it already gathers.

---

## 3. The envelope schema (the thing three clients code against)

Every node shares one **uniform envelope**. Clients render generically from it;
an unrecognized `type` still renders from `label`/`status`/`children`.

```go
package tree

type Node struct {
    Type      NodeType `json:"type"`                 // see §4
    ID        string   `json:"id"`                   // composite, stable (§6)
    Label     string   `json:"label"`                // display name, node-supplied
    Status    string   `json:"status"`               // normalized (§5); "" if the type has no lifecycle
    SessionID string   `json:"session_id,omitempty"` // REFERENCE only, on session-backed nodes
    Detail    *Detail  `json:"detail,omitempty"`     // small, type-specific, light fields only
    Children  []*Node  `json:"children,omitempty"`   // ordered by the node (§8); never re-sorted by clients
}

// Detail carries the few extra light fields a client needs to render a node
// without a second lookup. It NEVER embeds a full store.Session — session detail
// (repo, branch, backend, model, spend) is joined client-side via SessionID
// against the sessions channel. Every field is omitempty.
type Detail struct {
    Kind      string   `json:"kind,omitempty"`       // session kind: "agent" | "terminal" (session-backed nodes)
    Backend   string   `json:"backend,omitempty"`    // agent nodes: backend id, for an icon before session detail arrives
    DependsOn []string `json:"depends_on,omitempty"` // job nodes: the DAG edges (§ pipelines)
    Repo      string   `json:"repo,omitempty"`       // project/pipeline/run nodes: resolved repo path
    Path      string   `json:"path,omitempty"`       // project nodes: local checkout path
    Slot      string   `json:"slot,omitempty"`       // autopilot lane: "autopilot" | "guardian" | "worker"
    Gate      string   `json:"gate,omitempty"`       // autopilot_run nodes: gate mode
    Synthetic bool     `json:"synthetic,omitempty"`  // true for the "No project" bucket (§7)
    Degraded  bool     `json:"degraded,omitempty"`   // subtree degraded in place (§12)
}
```

**The top-level frame** returned by `GET /api/v1/tree` and carried in the SSE
`tree` event:

```go
type Tree struct {
    Roots     []*Node `json:"roots"`               // project nodes + the "No project" bucket, ordered (§8)
    Degraded  bool    `json:"degraded,omitempty"`  // whole-tree degraded flag (§12)
    Truncated bool    `json:"truncated,omitempty"` // a cap was hit; some children elided (§13)
}
```

Hard constraints (all from the consumer spec, restated as producer guarantees):

1. **Structure, not data.** Nodes carry only light fields and a `session_id`
   reference. No node ever embeds a `store.Session`. This keeps the frame small
   over a relay to a phone.
2. **Uniform envelope.** New node types (schedules, worktrees, project-groups)
   can be added with no client release.
3. **Node-owned order.** `Children` is already in canonical order (§8); clients
   must not re-sort.

---

## 4. Node types, and what `label`/`status` mean for each

Ten types for MVP. Agents, terminals, managers, guardians and workers are all
one underlying entity — a `store.Session` — differing only by `kind` and
orchestration back-refs.

| `type` | Backing entity | `label` | `session_id` |
|---|---|---|---|
| `project` | `projectstore.Project` (or a loose dir / the synthetic bucket) | `Project.Name` (→ `basename(ID)` fallback; `"No project"` for the bucket) | — |
| `agent` | `store.Session` (kind=agent) | session name | ✓ |
| `terminal` | `store.Session` (kind=terminal) | derived terminal name (`terminalDisplayName`) | ✓ |
| `pipeline` | `pipeline.Pipeline` | `Pipeline.Name` | — |
| `job` | `pipeline.Job` | `Job.ID` (or a human name if present) | ✓ when the job has spawned (`Job.SessionID`) |
| `autopilot_run` | `AutopilotRunStatus` | `Run.Name` | — |
| `manager` | `store.Session` (autopilot_slot=autopilot) | session name / `"manager"` | ✓ |
| `guardian` | `store.Session` (autopilot_slot=guardian) | session name / `"guardian"` | ✓ |
| `task` | `AutopilotPlanTask` | `PlanTask.Prompt` truncated (→ `PlanTask.ID`) | — |
| `worker` | `store.Session` (autopilot_slot=worker) | session name | ✓ |

### 5. Status normalization (an underspecified point the hub asked about)

**Recommendation: one small shared enum across every node type, normalized at the
node.** Clients get one palette of colors/icons and never learn per-type
vocabularies. Node-specific lifecycle stays available in the sessions/pipelines/
autopilot detail channels for anyone who wants it; the *tree* status is the
lowest-common-denominator rollup.

Shared enum (7 values):

```
active     — doing work now         (session working/spawning; job/pipeline running; run running)
waiting    — needs a human          (session waiting_for_input; run gate-blocked)
idle       — alive, not working     (session idle)
done       — finished ok            (session done; job done; pipeline done; run completed)
error      — failed / needs attn    (session errored/orphaned; job failed/needs_attention; pipeline stalled)
blocked    — queued / not started   (job pending/skipped; task pending; session rate_limited)
unknown    — no lifecycle / unmapped
```

Per-type mapping tables:

- **Session** (`store.Status`): `working|spawning`→`active`, `waiting_for_input`
  →`waiting`, `idle`→`idle`, `done`→`done`, `errored|orphaned`→`error`,
  `rate_limited`→`blocked`.
- **Job** (`pipeline.JobStatus`): `running`→`active`, `done`→`done`,
  `failed|needs_attention`→`error`, `pending|skipped`→`blocked`.
- **Pipeline** (`pipeline.Status`): `running`→`active`, `paused`→`idle`,
  `done`→`done`, `stalled|canceled`→`error`, `pending`→`blocked`.
- **Autopilot run** (`AutopilotRunStatus.State` + `Gate`): running→`active`,
  gate-blocked→`waiting`, completed→`done`, failed→`error`.
- **Container nodes** (`project`, `task`): a **rollup** of children —
  `error` if any child is `error`, else `waiting` if any child is `waiting`, else
  `active` if any child is `active`, else `done` if all done, else `idle`. A
  `task` with no worker yet is `blocked`.

The exact per-status string constants ship in `internal/tree` as a documented
enum so the three renderers key off identical strings (contract like
`serverCapabilities`: append, never rename).

---

## 6. Composite id scheme (must be identical across implementers)

Ids are **minted from structure, never from a session id**, because jobs and
tasks exist before any session does. Stable across snapshots for the same logical
node.

| Node | Id | Underlying stability |
|---|---|---|
| `agent`, `terminal`, `manager`, `guardian`, `worker` | `session:<session_id>` | session id is stable for life |
| `project` (registered) | `project:<project_id>` | project id == canonical repo path/URL, stable |
| `project` (loose dir) | `project:<abs-dir>` | canonicalized dir, stable |
| `project` (synthetic bucket) | `project:__none__` | fixed sentinel |
| `pipeline` | `pipeline:<pipeline_id>` | pipeline id == name, stable |
| `job` | `pipeline:<pipeline_id>/job:<job_id>` | job id stable within pipeline |
| `autopilot_run` | `run:<run_id>` | run id stable |
| `task` | `run:<run_id>/task:<task_id>` | plan task id stable |

Separators (`:` and `/`) are reserved; the daemon rejects/escapes those bytes in
raw ids if any ever appear (none do today). The id is opaque to clients — they
key view state off it and never parse it.

---

## 7. Exactly-once nesting & the "No project" bucket

Each session appears under **exactly one** parent, by this precedence (highest
first). This is the rule three clients would otherwise each get subtly wrong.

| # | If the session has… | Nest under | Fields (all on `store.Session`) |
|---|---|---|---|
| 1 | `autopilot_run_id` | that run, in the lane from `autopilot_slot` (`autopilot`→manager, `guardian`→guardian, `worker`→the task in `autopilot_task_id`) | `AutopilotRunID`, `AutopilotSlot`, `AutopilotTaskID` |
| 2 | `pipeline_id` + `job_id` | that pipeline's job | `PipelineID`, `JobID` |
| 3 | `parent_id` resolving within the same project | that parent agent | `ParentID` |
| 4 | none of the above | its resolved project | resolved membership (§8) |

Two traps the precedence exists to avoid, both confirmed in code:

- **Autopilot workers have `parent_id` cleared** by the ownership guard, and are
  grouped by `autopilot_run_id`/`autopilot_slot`/`autopilot_task_id`. Any
  implementation reaching for `parent_id` first loses them.
- Pipeline/autopilot children *also* resolve to a project, so without the
  precedence they render twice.

**Cross-project children** (rule 3's "within the same project" clause):
`agentForest` promotes a child whose `sourceDir` differs from its parent's to a
root under its *own* project, today carrying a `"↳ from <parent>"` backlink
label. In the service the **edge** (which project it roots under) is structure
and is kept; the **label** is view and moves to the TUI/clients. For MVP the
cross-project child simply roots under its own project; the "from parent"
affordance is a client view concern reconstructed from the sessions channel
(`parent_id` is on the session). Flagged as a minor open question (Q-T3).

**The "No project" bucket** (`project:__none__`, `Detail.Synthetic=true`): a bare
terminal or an agent with no worktree resolves to no project. It is an expected
state, owned by the node, so every client shows the same bucket. Returned only
when the request is unscoped (no `project_id`).

---

## 8. Membership resolution & canonical ordering

### Membership
`resolveGroupKey` is the rule: prefer `Session.project_id`; else fall back to
canonicalized `repo`→`worktree`→`workdir` (`sourceDir`/`normalizePipelineDir`:
abs path, `.worktrees/` stripped) matched against each project's `id` and `path`.
A match to a **closed/hibernated** project → the item still renders, but under a
node marked closed (the TUI hides these today; the service keeps them and lets
the client decide — see Q-T5). No match → loose-dir project node. No location →
the synthetic bucket.

BUILD-1 (§16) makes `project_id` always-populated so this fallback can eventually
be deleted; until then the fallback is the daemon's single source of truth and
lives only here.

### Canonical ordering (the hub asked for an actual answer — here it is)
The node decides sibling order; clients honor it. Ordering is **per node type,
deterministic, and stable** (stability matters more than the exact key, so that
view state doesn't jump):

- **Roots (projects):** open projects first (alpha by label), then loose-dir
  groups (alpha), then the "No project" bucket **last**.
- **Within a project:** `autopilot_run` nodes, then `pipeline` nodes, then agent
  subtrees, then terminals. Rationale: orchestrators (which contain the most) at
  top, ad-hoc terminals at the bottom — matches the TUI's current reading order.
- **Agents (siblings):** live before terminal-state (`liveStatus` first), then by
  creation time ascending, then by id. This keeps active work at the top and is
  stable as statuses change (creation time is the tiebreaker, not status).
- **Jobs:** topological by `depends_on`, then declaration order (the pipeline's
  own job order). The DAG edges also ship in `Detail.DependsOn` so a client can
  draw the graph.
- **Autopilot lanes:** manager, guardian, then tasks in **ledger order**
  (`AutopilotRunStatus.LedgerTasks` already exists for exactly this), workers
  within a task by creation time.

The exact comparators live in `internal/tree` and are covered by golden tests so
two implementers (and the TUI) can never diverge.

---

## 9. `GET /api/v1/tree` — request / response / errors

Spec-first: the endpoint is added to `internal/daemon/apidocs/openapi.yaml`, then
`make generate` regenerates the strict server + DTOs (never hand-written).

```
GET /api/v1/tree
GET /api/v1/tree?project_id=<id>       # scope to one project subtree
GET /api/v1/tree?all=true              # include system:true sessions (mirrors /sessions, /events)
```

- **200** → `Tree` (§3). Empty fleet → `{"roots":[]}`, never `null`.
- **?project_id=** unknown → **200** with `{"roots":[]}` (not 404 — a project may
  simply have nothing in it yet; the client already knows it exists from the
  projects list). *Open question Q-T2: 404 vs empty; leaning empty.*
- **503** → the underlying session scan is degraded (complete-or-error, matching
  `ListSessions`). The client shows "node degraded", distinct from offline (§12).
  Optionally: return **200 with `degraded:true`** and a best-effort partial tree
  if only a *subsystem* (pipelines/autopilot) errors — see §12.
- **Scope:** a **read** route → `ScopeReadOnly` is sufficient (§14).
- The response is `Cache-Control: no-store` (it's a live snapshot).

OpenAPI addition (sketch — real types generated):

```yaml
/api/v1/tree:
  get:
    operationId: getTree
    parameters:
      - {name: project_id, in: query, schema: {type: string}}
      - {name: all, in: query, schema: {type: boolean}}
    responses:
      '200': {description: the hierarchy, content: {application/json: {schema: {$ref: '#/components/schemas/Tree'}}}}
      '503': {description: session scan degraded}
```

`Tree`, `TreeNode`, `TreeNodeDetail` schemas are added to `components`. Because
the SSE event ships the **same** `Tree` shape (§10), define it once and reference
it from both.

---

## 10. The `tree` SSE event — framing, dedup, back-compat

The tree joins the **existing** `GET /api/v1/events/stream` as a **named event**,
deduplicated independently from the session snapshot. Not a second connection,
not folded into the sessions frame.

**Critical back-compat detail (verified in `sse.go`):** today the stream emits an
**unnamed** event — `data: {sessions,autopilot}\n\n` — and existing clients parse
the default (unnamed) event. Therefore:

- The **sessions snapshot stays unnamed** (`data: …`) — unchanged, so every
  existing client keeps working with zero changes.
- The **tree ships as a named event**: `event: tree\ndata: <Tree JSON>\n\n`.
  Old clients ignore named events they don't register for; new clients add one
  `addEventListener('tree', …)`.

**Independent dedup:** the handler keeps **two** `last` byte buffers — `lastSess`
and `lastTree`. On each hub signal it recomputes both frames; it emits the
sessions frame only if `lastSess` changed, and the tree frame only if `lastTree`
changed. A pure status change re-sends only sessions; a structural change
re-sends only the tree. This is the whole point — it keeps the phone payload
small.

```
event: tree
data: {"roots":[...]}

data: {"sessions":[...],"autopilot":{...}}

: ping           (every 25s, unchanged)
```

- **`?all=true`** applies to both frames identically (system sessions included in
  the tree too).
- **Snapshot, not delta.** Each `tree` frame is a full deduplicated snapshot;
  reconnect recovery is free (first post-reconnect frame is a full resync).
  Deltas are explicitly a v2 concern.
- **Two channels, one render:** structure on `tree`, detail on the sessions
  frame, arriving independently. Clients must render a node whose session they
  haven't seen yet (from the envelope alone) and tolerate a session whose node
  has gone.

The `autopilot` field stays on the sessions frame **as well** for back-compat
(cockpit consumes it today); the tree derives its autopilot subtree from the same
`autopilot.Status()` the daemon already computes, so there's no double
computation — one `Status()` call feeds both.

---

## 11. Default visibility (decided once, at the node)

- `system:true` sessions hidden unless `?all=true` — mirrors `/sessions` and
  `/events/stream` exactly (same predicate).
- **Terminals are NOT hidden** — first-class `terminal` nodes.
- **Terminated sessions stay visible** with `status:done` until deleted; after
  deletion they live behind `GET /api/v1/history` (out of the tree).
- Deciding this at the node is the point: one behavior, three clients.

---

## 12. Partial failure, degraded vs offline

**Recommendation: per-subtree degradation, not all-or-nothing.**

- The **session scan** is the backbone. If it is degraded (`store.IsDegraded`),
  the tree cannot be trusted → the endpoint returns **503** and the SSE handler
  keeps the last good `tree` frame (same complete-or-error contract as
  `ListSessions`/`handleEventsStream` today). This is "**node degraded**".
- If a **subsystem** errors while sessions are fine — the pipeline store or
  autopilot status read fails — the tree is built from what's healthy and the
  affected subtree is **marked in place**: the top-level `Tree.Degraded=true` and
  the specific container node carries `Detail.Degraded=true` with `status:unknown`
  and no (or last-known) children. The client renders "this part couldn't load"
  on that subtree rather than blanking the whole rail.
- **Degraded ≠ offline.** "Offline" is the hub's concept (the daemon isn't
  holding its relay socket) and is invisible to the daemon itself; the daemon only
  ever expresses **degraded** (`503` or `Degraded:true`). The two look identical
  to a user but mean different things, so the client must distinguish: no relay
  socket → offline banner; relay up but `503`/`degraded` → degraded banner. This
  is a **client contract**, noted here so the hub spec states it (it currently
  conflates them in one "node offline" state — see §16 disagreement D3).

---

## 13. Size limits & truncation

Measured expectation: the joins are pure in-memory work the TUI already does many
times per second. For the hub's stated worst case — **~200 sessions across ~15
projects** — the `Tree` JSON is a few tens of KB (light nodes, no embedded
sessions), well under the 1 MiB wire frame cap. **No truncation needed at MVP
scale.**

The guardrails, specced now so they exist before they're needed:

- A soft cap (**default 2,000 nodes**, configurable) after which the service stops
  expanding the largest subtrees and sets `Tree.Truncated=true`; truncated
  container nodes keep their `status` rollup and child *count* in `Detail` but an
  elided `children`. A client renders "N more — open this project to see them"
  honestly rather than silently dropping nodes.
- **Lazy subtrees** (`GET /api/v1/tree?project_id=` already scopes) are the escape
  hatch: a truncated global tree is still fully browsable one project at a time.

The residual cost is the **sessions** snapshot (it re-sends every session on
every change — pre-existing, not introduced here). Independent tree dedup (§10)
contains it; a delta/project-scoped **sessions** stream is a separate warden
follow-up gated on measurement (hub's Q11), out of scope for this spec.

---

## 14. Scope interaction

A `ScopeReadOnly` grant sees the **same tree** as `ScopeFull` — the tree is pure
observation and reveals nothing a read grant shouldn't see (it already sees
`/sessions`). `GET /api/v1/tree` and the `tree` SSE event are **read** routes;
`ScopeReadOnly` suffices. Mutations (spawn/terminate/attach) keep their existing
`ScopeFull` gating unchanged. Stated explicitly per the hub's request rather than
left to assumption.

---

## 15. Capability flag & version/compat

- New flag **`project-tree`** appended to `serverCapabilities` in
  `internal/daemon/strict_core.go` (and thus to `GET /api/v1/capabilities` and
  the relay `Hello.Caps`, which mirrors it). Contract is already documented there:
  **append, never rename or drop**. Clients feature-detect the exact string and,
  if absent, prompt "upgrade this node" — **no client-side assembly fallback**
  (that would preserve the very code this deletes).
- **`GET /api/v1/sessions` is unchanged.** Clients still need it for node detail
  (repo/branch/backend/model/spend) behind the tree's `session_id` references.
- **The existing SSE sessions frame is unchanged** (unnamed, `{sessions,
  autopilot}`) — the tree is strictly additive as a named event (§10). This
  confirms the hub spec's assumption that neither `/sessions` nor the current SSE
  frame changes shape.

---

## 16. How BUILD-1/2/3 fold in (with a correction)

| # | Item | Reality after code check | Recommendation |
|---|---|---|---|
| BUILD-1 | Stamp `project_id` on every spawn path + on `Pipeline` creation | Today only the orchestrator-hook and hibernate paths stamp it; ordinary spawn and pipeline creation don't. The membership **fallback** (§8) covers the gap. | **Follow-up, not a prerequisite.** Ship the service with the fallback (it must exist anyway for old sessions). Do BUILD-1 next to *simplify* — it lets the fallback eventually be deleted. Not a client contract either way. |
| BUILD-2 | `Pipeline.creator_session_id` (and optionally `autopilot_run_id` on escalated pipelines) | Genuinely missing. `Pipeline` has no creator/parent-session field; a pipeline an agent spawned can't be nested under that agent — it roots under the project instead. | **Small, do it with the service** if agent→pipeline nesting is wanted in MVP; otherwise defer and root such pipelines under the project (honest, just flatter). One additive field + stamping at creation. Low risk. |
| BUILD-3 | Embed plan tasks in the autopilot run payload | **Already done.** `AutopilotRunStatus` (client.go:1249) already carries `PlanTasks []AutopilotPlanTask{id,prompt,after,status,landed_pr}`, `Workers map[task_id][]session_id`, `Brain` (manager agent id), `GuardianID`, `ManagerSlotID`, `LedgerTasks` (ordering) — and it's **already on the SSE frame** (`frame.Autopilot = s.autopilot.Status()` in sse.go). | **No work.** The autopilot subtree is fully server-computable from data the daemon already ships. The service just reshapes `AutopilotRunStatus` into tree nodes. This is the strongest evidence the extraction is low-risk. |

**Disagreements with the hub consumer spec (flagged for reconciliation):**

- **D1 — BUILD-3 status.** Hub lists BUILD-3 as "Medium — BUILD-0 needs it." It's
  already shipped; the hub doc should mark it **done** and note task nodes get
  real labels/status for free.
- **D2 — SSE named vs unnamed.** Hub §2.4/§6.1 says the tree is a named event and
  implies the stream has "no named event types" today. Correct that the **sessions
  frame is and stays unnamed** for back-compat; only the tree is named. Behavior
  matches the hub's intent; the wording should be precise so the client's
  `addEventListener('tree')` doesn't also try to name the sessions frame.
- **D3 — degraded vs offline.** Hub §1.5 has a single "node offline" state.
  Recommend splitting client-side into **offline** (no relay socket, hub-observed)
  vs **degraded** (relay up, daemon returns 503/`degraded:true`) — §12. Same
  visual family, different cause and copy.
- **D4 — closed/hibernated projects.** The TUI *hides* items linked to closed
  projects (folds them into Ungrouped). The service will instead **keep** them and
  mark the project node closed, letting each client choose to dim vs hide (the web
  wants to show hibernated projects so they're re-openable). Flagged because it's a
  behavior change from `resolveGroupKey`'s current TUI semantics (Q-T5).

---

## 17. The TUI rewrite (proving the split)

The TUI stops calling `projectGroupedItems`/`buildItems` directly and instead:

1. Calls `tree.Service.Build(inputs, "")` to get `*Tree`.
2. Runs a **view adapter** that walks `Tree.Roots`, applies the collapse map
   (re-keyed onto composite node ids, §6), computes cursor position, adds
   home-abbreviated paths and `"↳ from <parent>"` backlink labels, and flattens to
   the existing `[]listRow` for `renderList`.
3. Keeps every render function (`renderItemLine`, `treePrefix`, `jobBadge`,
   `contextLabel`, …) untouched.

The adapter is new (~150–250 lines) but purely view; the deleted structural code
(~300–500 lines) moves to `internal/tree`. Net line count is roughly flat; the
win is *one* structural implementation. Golden tests that today assert on
`[]item`/`[]listRow` are re-pointed at `tree.Build` output plus a thin adapter
test, so the TUI's existing coverage transfers.

---

## 18. Worked example (a populated tree)

A project with one root agent (+child), one terminal, one pipeline (2 jobs, one
spawned), and one autopilot run (manager + guardian + one task with a worker);
plus a bare terminal in the "No project" bucket.

```jsonc
{
  "roots": [
    {
      "type": "project", "id": "project:/home/u/dev/warden",
      "label": "warden", "status": "active",
      "detail": {"repo": "/home/u/dev/warden", "path": "/home/u/dev/warden"},
      "children": [
        {
          "type": "autopilot_run", "id": "run:ap-42", "label": "recovery-finish",
          "status": "active", "detail": {"repo": "/home/u/dev/warden", "gate": "auto"},
          "children": [
            {"type": "manager",  "id": "session:ap-42-brain",  "label": "manager",
             "status": "active", "session_id": "ap-42-brain",  "detail": {"kind":"agent","slot":"autopilot"}},
            {"type": "guardian", "id": "session:ap-42-guard",  "label": "guardian",
             "status": "idle",   "session_id": "ap-42-guard",  "detail": {"kind":"agent","slot":"guardian"}},
            {"type": "task", "id": "run:ap-42/task:t1", "label": "wire resolver into first-spawn",
             "status": "active",
             "children": [
               {"type": "worker", "id": "session:w-9", "label": "worker-9",
                "status": "active", "session_id": "w-9", "detail": {"kind":"agent","slot":"worker"}}
             ]}
          ]
        },
        {
          "type": "pipeline", "id": "pipeline:cred-inject", "label": "cred-inject",
          "status": "active", "detail": {"repo": "/home/u/dev/warden"},
          "children": [
            {"type": "job", "id": "pipeline:cred-inject/job:implement", "label": "implement",
             "status": "active", "session_id": "impl-1",
             "detail": {"depends_on": []}},
            {"type": "job", "id": "pipeline:cred-inject/job:review", "label": "review",
             "status": "blocked", "detail": {"depends_on": ["implement"]}}
          ]
        },
        {
          "type": "agent", "id": "session:agent-7", "label": "orch-warden",
          "status": "waiting", "session_id": "agent-7", "detail": {"kind":"agent","backend":"claude"},
          "children": [
            {"type": "agent", "id": "session:agent-8", "label": "sub-explorer",
             "status": "active", "session_id": "agent-8", "detail": {"kind":"agent","backend":"claude"}}
          ]
        },
        {"type": "terminal", "id": "session:term-3", "label": "warden ~ main",
         "status": "idle", "session_id": "term-3", "detail": {"kind":"terminal"}}
      ]
    },
    {
      "type": "project", "id": "project:__none__", "label": "No project",
      "status": "idle", "detail": {"synthetic": true},
      "children": [
        {"type": "terminal", "id": "session:term-9", "label": "shell",
         "status": "idle", "session_id": "term-9", "detail": {"kind":"terminal"}}
      ]
    }
  ]
}
```

---

## 19. Open questions

All locked with hub 2026-09-05 (CONFIRM B, no overrides):

- **Q-T1 — BUILD-2 in or out of MVP.** Deferred. Pipelines root under the project.
- **Q-T2 — `?project_id=` unknown.** 200-empty.
- **Q-T3 — cross-project child backlink.** Client view only; no `parent_ref`.
- **Q-T4 — project-groups as a node type.** Label only for MVP.
- **Q-T5 — closed/hibernated projects in the tree.** Include and mark closed (D4).
- **Q-T6 — ordering tiebreakers.** Creation-time ascending.

---

## 20. Non-goals (this spec)

- No delta/incremental SSE (snapshot + dedup only).
- No DAG-authoring, no autopilot plan-tree *editing* — observe only.
- No changes to `/sessions`, the sessions SSE frame shape, or `/attach`.
- No hub-side work (relay, rendezvous, web rendering) — that's the consumer spec.
- No new persistence — the service is pure over existing in-memory snapshots.

---

## 21. Summary for the requester

- **Feasible; medium refactor** (§1). The hub's read of `list.go` is right; the
  entanglement is real but bounded to a structure/view split. ~300–500 structural
  lines move to `internal/tree`; the TUI keeps its view layer via a new adapter
  and becomes the first consumer.
- **Envelope, ids, ordering, status, degradation** are all specified concretely
  (§3–§13) — an implementer can build without coming back, and the hub/android can
  code renderers against §3–§8 in parallel.
- **SSE**: named `tree` event, independent dedup, sessions frame stays unnamed for
  back-compat (§10).
- **Capability**: `project-tree`, append-only (§15).
- **BUILD-3 is already shipped** (§16) — the autopilot subtree needs no new data.
  BUILD-1/2 are follow-ups/optional, not prerequisites.
- **Four flagged disagreements** with the hub doc (D1–D4, §16) to reconcile.
- **Six open questions** (Q-T1–Q-T6, §19), each with a lean.
</content>

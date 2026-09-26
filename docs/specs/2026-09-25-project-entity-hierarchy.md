# Project entity hierarchy — spec freeze

- **Status:** Locked (Phase 0 — spec freeze; docs only, no production code)
- **Repo:** warden (node). Baseline: `autopilot/entity-hierarchy` integration branch.
- **Plan file:** [`plans/project-entity-hierarchy.yaml`](../../plans/project-entity-hierarchy.yaml)
- **Scope:** Freeze the entity model — the four first-class entities, their
  membership and parent/child relationships, their lifecycle and state
  vocabulary — so that the implementation phases (P1+) build against a single,
  agreed contract. This document is normative: the decisions in §2 are **locked**
  and are not to be relitigated during implementation.

---

## 0. Why this exists

Today the relationships between the core entities are **implicit and derived**,
not modelled:

- **Project → members** is reconstructed by scanning every session/pipeline for
  a matching `ProjectID` back-ref (`internal/store` + `internal/projectstore`).
  A project has no first-class list of what it contains.
- **Agent → children** is reconstructed at render time by walking `parent_id`
  edges across the whole session forest (`internal/tree/membership.go`,
  `internal/tui/tree_adapter.go`). An agent record holds no forward child list.
- **Pipeline → owning agent** does not exist at all. A pipeline back-refs a
  project (`Pipeline.ProjectID`) but not the agent that created it, so an
  orchestrator's escalated pipelines cannot be attributed to it as children.
- **State vocabulary** is spread across store constants
  (`spawning/working/waiting_for_input/idle/done/errored/orphaned/rate_limited`)
  with no single documented UX-facing set, and clients (TUI, web, app) each
  re-map it.
- **Open behaviour** auto-spawns an orchestrator (`guaranteeOrchestrator`,
  `internal/daemon/orchestrator_hook.go`) on every project open, so a project is
  never actually empty.

The redesign makes each of these **explicit and stored** so that all clients
render one contract instead of each deriving its own. This spec freezes that
contract.

### Relationship to prior specs

This supersedes the implicit membership model assumed by
[`2026-08-28-project-centric-ui.md`](2026-08-28-project-centric-ui.md) (the
first-class Project entity and IDE-like hibernation) and builds on the
plan-scoped hierarchy seams in
[`2026-09-01-autopilot-plan-scoped-hierarchy.md`](2026-09-01-autopilot-plan-scoped-hierarchy.md).
It does **not** touch the Project Groups organizational layer
([`2026-08-30-project-groups-architecture.md`](2026-08-30-project-groups-architecture.md));
groups remain a set of project ids above the project layer and are out of scope
here.

---

## 1. Entities (locked)

There are **four** first-class entities. **Pipeline stays a separate entity** —
it is *not* folded into Agent and *not* re-expressed as a special agent.

| Entity | What it is | Store today |
|---|---|---|
| **Project** | The repo-level parent (one per checkout root or remote URL). Owns agents, pipelines, and terminals. | `internal/projectstore` `Project` |
| **Agent** | An AI coding session. May have a parent agent and child agents/pipelines. | `internal/store` `Session` (`Kind` == agent) |
| **Terminal** | A plain shell pane (no transcript/cost/state). A tracked session, but never an agent. | `internal/store` `Session` (`Kind` == terminal) |
| **Pipeline** | A multi-job DAG of dependent agent runs. A separate entity, owned by a project and optionally by a parent agent. | `internal/pipeline` `Pipeline` |

---

## 2. Locked design decisions

| # | Decision | Choice |
|---|---|---|
| D1 | **Entity set** | Project, Agent, Terminal, Pipeline. Pipeline stays a **separate** entity (not an agent, not folded into Agent). |
| D2 | **Project membership** | `Project.agents[]`, `Project.pipelines[]`, `Project.terminals[]` are **complete id lists** — the authoritative membership stored *on the project*, not re-derived by scanning back-refs. |
| D3 | **Agent hierarchy** | `Agent.parent_id` (existing) plus a forward `Agent.child_agents[]`. Both directions are stored and kept consistent. |
| D4 | **Agent → pipelines** | `Agent.child_pipelines[]` lists the pipelines an agent owns; the reverse edge is `Pipeline.parent_agent_id`. |
| D5 | **Job agents are not children** | A pipeline's own **job agents are NOT `child_agents`** of the pipeline's owning agent. They belong to the pipeline (`Pipeline.jobs` / `Session.pipeline_id`), not to the agent's `child_agents[]`. Only user-facing spawned sub-agents populate `child_agents[]`. |
| D6 | **Pipeline ownership** | `Pipeline.parent_agent_id` back-refs the agent that created/escalated it (empty = created directly by the operator). Complements the existing `Pipeline.project_id`. |
| D7 | **Lifecycle: create/init vs spawn** | Two distinct lifecycles. **create/init** = an entity record comes into existence (e.g. a project registered, a session record materialized) — internal, may be record-only. **user-facing spawn** = an agent/terminal is actually launched into a live tmux session by an operator or a parent agent. The membership/child lists are populated at spawn, not merely at record creation. |
| D8 | **Recovery** | An entity is **recovered only from the `orphaned` state.** Recovery revives a record whose process died but whose worktree/transcript survive. No other state is a recovery source. |
| D9 | **State set** | `pending`, `busy`, `idle`, `need-input`, `done`, `orphaned`, `rate_limited`. **Aliases are OK** — these map onto the existing store constants (see §4); clients present this seven-state vocabulary. |
| D10 | **Empty project on open** | Opening a project yields an **empty project — no `guaranteeOrchestrator`.** The daemon no longer auto-spawns an `orch-<project>` agent on open; a freshly opened project has empty membership lists until the operator (or a parent agent) spawns into it. |

### Non-goals (explicitly out of scope)

- **NG1 — Pipeline DAG rewrite.** The pipeline job/dependency model
  (`internal/pipeline` `Job`, `after:`) is untouched. This spec adds an
  ownership back-ref (`parent_agent_id`); it does not change how DAGs are
  scheduled or executed.
- **NG2 — Pipeline-as-agent.** A pipeline is never represented as an agent, and
  an agent is never represented as a pipeline. They stay distinct entities
  (reaffirms D1).
- **NG3 — Agent-level `deps[]`.** Agents do not gain a dependency array.
  Cross-agent ordering stays a pipeline concern; agent relationships are
  parent/child only (D3/D4), not a DAG.

---

## 3. Data model (frozen shapes)

The shapes below are the frozen contract. Field names are the JSON wire names;
Go field/casing follows repo convention at implementation time. Existing fields
are marked *(exists)*; new fields are marked **(new)**.

### 3.1 Project

```jsonc
{
  "id":        "…",            // (exists) canonical: local path or remote URL
  "name":      "…",            // (exists)
  "path":      "…",            // (exists)
  "status":    "open|closed",  // (exists) project hibernation (open/closed)
  "agents":    ["…"],          // (new) complete id list of member agents
  "pipelines": ["…"],          // (new) complete id list of member pipelines
  "terminals": ["…"]           // (new) complete id list of member terminals
}
```

- `agents[]`, `pipelines[]`, `terminals[]` are the **authoritative** membership
  (D2). The per-session `ProjectID` / per-pipeline `ProjectID` back-refs remain
  the reverse edge; the two are kept consistent, with the project's lists as the
  membership of record.
- The lists are ordered and de-duplicated. Ids in a list need not still resolve
  to a live record (a member may be `orphaned` or hibernated) — dangling
  membership is tolerated, matching the existing group/back-ref tolerance.

### 3.2 Agent (session, `kind == agent`)

```jsonc
{
  "id":              "…",       // (exists)
  "project_id":      "…",       // (exists) parent project back-ref
  "parent_id":       "…",       // (exists) parent AGENT id; empty = root spawn
  "child_agents":    ["…"],     // (new) user-facing spawned sub-agents (NOT job agents)
  "child_pipelines": ["…"],     // (new) pipelines this agent owns
  "status":          "…"        // (exists) see §4
}
```

- `child_agents[]` is the forward edge of `parent_id` (D3) and holds **only**
  user-facing spawned sub-agents. **Pipeline job agents are excluded** (D5) —
  they are reached through `child_pipelines[]` → `Pipeline.jobs`, never listed
  here.
- `child_pipelines[]` (D4) is the forward edge of `Pipeline.parent_agent_id`.

### 3.3 Terminal (session, `kind == terminal`)

- A terminal is a member of exactly its project (`Project.terminals[]` ↔
  `Session.project_id`). It has no children, no transcript, no cost, and no AI
  state; its status is limited to the liveness-derived subset (`busy` while the
  pane is alive, `orphaned`/`done` when it is not) and it is excluded from every
  AI-centric surface, exactly as today (`Session.IsTerminal`).

### 3.4 Pipeline

```jsonc
{
  "id":              "…",   // (exists) == name
  "project_id":      "…",   // (exists) owning project
  "parent_agent_id": "…",   // (new) owning agent; empty = operator-created
  "jobs":            [ … ]  // (exists) DAG jobs — NOT rewritten (NG1)
}
```

- `parent_agent_id` (D6) is the reverse of `Agent.child_pipelines[]`.
- `jobs[]` and the DAG are unchanged (NG1). Job agents carry `pipeline_id`
  (existing) and are the pipeline's, not the owning agent's `child_agents[]`
  (D5).

---

## 4. State vocabulary (locked, with alias mapping)

The UX-facing state set (D9) is seven states. "Aliases OK" means these are the
presented names; each maps onto an existing `internal/store` `Status` constant,
so no data migration is forced by this spec.

| UX state (this spec) | Store constant (`internal/store`) | Meaning |
|---|---|---|
| `pending` | `spawning` | Record created / launching, not yet doing work. |
| `busy` | `working` | Actively working. |
| `idle` | `idle` | Alive, no active turn. |
| `need-input` | `waiting_for_input` | Blocked on an approval / operator input. |
| `done` | `done` | Finished cleanly. |
| `orphaned` | `orphaned` | Process gone, record + worktree survive — the **only** recovery source (D8). |
| `rate_limited` | `rate_limited` | Paused on a backend rate limit; auto-resumes. |

- `errored` (existing store constant) is **not** in the UX set; it is presented
  under an existing bucket (an errored agent surfaces as `orphaned` when its
  process is gone, or `done` when it exited). Clients must not invent a distinct
  UX state for it under this spec.
- Aliasing is a presentation decision: clients render the left column; the store
  keeps the right column. A later phase may collapse the constants, but this
  spec does **not** require it.

---

## 5. Lifecycle & recovery (locked)

- **create/init** (D7): a record materializes. For a project this is
  registration; for a session it is the store record before/without a live pane.
  This step may be record-only and does not by itself add the entity to a
  parent's membership beyond the direct project link.
- **user-facing spawn** (D7): an agent or terminal is launched into a live tmux
  session — by an operator (CLI/TUI/app/MCP) or by a parent agent. Spawn is what
  wires the parent/child and membership edges: the new agent's `parent_id` is
  set and it is appended to the parent's `child_agents[]` (or the project's
  `agents[]` for a root spawn) and to `Project.agents[]`.
- **Open is empty** (D10): opening a project restores its previously-hibernated
  members (existing hibernation behaviour) but does **not** auto-spawn an
  orchestrator. `guaranteeOrchestrator` is removed from the open path; a project
  with no restorable members opens empty.
- **Recovery** (D8): only an `orphaned` entity is a recovery candidate. Recovery
  re-inserts the record and reconnects its live pane; membership/child edges are
  preserved across recovery (they live on the records, not on the process).

---

## 6. Consistency rules

1. **Two edges, one truth.** Every parent/child and membership relationship is
   stored on both ends (e.g. `Agent.parent_id` ↔ parent's `child_agents[]`;
   `Session.project_id` ↔ `Project.agents[]`). Writers update both ends in the
   same operation; the "complete id list" on the container (D2) is the
   membership of record when they disagree.
2. **Job agents stay off `child_agents[]`.** (D5) A job agent is reachable only
   via its pipeline. Enforced wherever `child_agents[]` is populated.
3. **Dangling ids tolerated.** Membership/child lists may reference records that
   are orphaned or hibernated; lists are not eagerly pruned (matches existing
   group/back-ref semantics).
4. **Terminals are leaf members.** A terminal appears only in
   `Project.terminals[]`; it never appears in any `child_agents[]` /
   `child_pipelines[]` and never owns children.

---

## 7. Out of scope for Phase 0 (this doc)

Phase 0 is a **docs-only spec freeze**. No production code, no schema migration,
no OpenAPI edits land in this phase. The implementation phases that build against
this frozen contract are enumerated in
[`plans/project-entity-hierarchy.yaml`](../../plans/project-entity-hierarchy.yaml).

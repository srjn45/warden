# Feature #5: Project Groups & Orchestrator Collaboration

> **Status: ✅ Complete on `main` (2026-08-31).** Phases 1–4 shipped via PRs
> #363–#368: persisted groups and APIs, TUI group labels, per-project
> orchestrator auto-spawn/wakeup, peer-context injection, and delegation/waiting
> ergonomics. Phase 5 also completed: the obsolete collaboration-group specs were
> removed in PR #370. This document remains as the historical architecture and
> delivery breakdown, not a current “resume here” plan.

## 1. Architectural Vision
Moving away from arbitrary agent swarms, "Collaboration Groups" are now defined as **Project Groups**. 
- A Project Group binds multiple repositories (projects) together.
- Every project in a group is guaranteed to have one persistent agent with the `orchestrator` role (named `orch-<project-name>`).
- **The Orchestrator is the API:** It never writes code itself. It acts as the tech lead, planning work and delegating implementation to local `worker` agents or `pipelines`.
- **Peer-to-Peer Routing:** There is no "Global Orchestrator". If `frontend` needs a change in `backend`, `orch-frontend` sends a message to `orch-backend`. `orch-backend` coordinates its local workers and replies when done.

## 2. Implementation Phases

### Phase 1: Database & Registry (The Project Group)
* **Goal:** Allow users to link multiple projects together in the database.
* **Tasks:**
  1. Update `internal/projectstore` to support a `ProjectGroup` schema (id, name, list of project IDs).
  2. Add daemon HTTP endpoints (e.g., `/api/v1/project-groups`) to create, list, and manage these groups.
  3. Ensure the Cockpit TUI can display which group a project belongs to.

### Phase 2: Orchestrator Daemon Loop & Auto-Spawn
* **Goal:** Guarantee the existence and readiness of the `orch-<project>` agent.
* **Tasks:**
  1. Add a daemon hook: When a project is activated/opened in the Cockpit, check if `orch-<project-name>` is alive. If not, automatically spawn it with the `orchestrator` role in the project root.
  2. Implement an auto-wakeup mechanism: If an orchestrator receives a message via the mailbox, ensure the poller wakes it up if it was idle.

### Phase 3: Peer Awareness & Context Injection
* **Goal:** Orchestrators must know who they can talk to.
* **Tasks:**
  1. Update `internal/lifecycle/poller.go` (or the prompt injection phase) to dynamically inject peer context into the Orchestrator's system prompt at runtime.
  2. Injection format: *"You are part of Project Group X. Your peer orchestrators are: `orch-backend` (manages API), `orch-docs` (manages documentation). You can task them using the `send_message` tool."*

### Phase 4: Subagent & Pipeline Ergonomics
* **Goal:** Ensure the Orchestrator can seamlessly delegate work downward.
* **Tasks:**
  1. Polish the `invoke_subagent` and `create_pipeline` MCP tools to ensure the Orchestrator has frictionless APIs to spin up `planner` and `worker` agents locally.
  2. Ensure the Orchestrator's `wait_for_message` behaves reliably so it can block/sleep while its workers or peers are busy.

### Phase 5: Cleanup & Migration
* **Goal:** Remove technical debt from previous collaboration experiments.
* **Tasks:**
  1. Delete the orphaned `stage-b5`, `stage-b6`, `stage-b7`, and `stage-c2` branches to avoid confusion.
  2. Archive or deprecate the older `2026-08-26-collaboration-groups*.md` specs in favor of this streamlined document.

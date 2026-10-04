---
title: Project tree
description: The shared project-tree hierarchy — GET /api/v1/tree, the SSE tree event, and the TUI consumer.
---

The daemon computes one **project tree** for the whole fleet — projects, plans,
autopilots, pipelines, agents, and terminals — so every UI (TUI, web, remote clients)
renders the same nesting. Clients should **prefer this tree** over joining
`/sessions`, `/pipelines`, and `/autopilot` themselves.

## Surfaces

| Surface | How |
|---|---|
| **REST** | `GET /api/v1/tree` — optional `?project_id=` scope, `?all=true` to include `system:true` sessions (including headless brain) |
| **SSE** | Named `tree` event on `/api/v1/events/stream` (same envelope as the GET) when structure changes |
| **Capability** | `project-tree` in `GET /api/v1/capabilities` |
| **TUI** | The Projects-tab navigator walks `tree.Service.Build` locally (same package the API uses) and applies collapse/cursor on **composite node ids** |

Interactive Swagger UI documents the full schema under
[REST API & OpenAPI](/warden/reference/api-openapi/).

## Shape (summary)

- **Roots** are project nodes (registered projects, loose directories, and a synthetic
  **No project** bucket).
- Children under a project are projected in order: **Plans** (section) → root
  **Autopilots** → root **Pipelines** → root **Agents** → **Terminals** (section).
  There are no Autopilots/Pipelines/Agents section header buckets — those entities
  sit directly under the project. Empty Plans/Terminals sections are omitted. In
  the TUI, empty plan status groups (Pending / In Progress / Completed / Archived)
  under Plans are omitted too.
- Each entity renders **exactly once**: Autopilot managers and workers nest under
  their Autopilot run (not as free Agents); pipeline job agents nest under
  Pipelines; agents may nest child autopilots, then child pipelines, then child
  agents. Plan task evidence lives on Plan detail, never as task groups inside
  Autopilot.
- Autopilot runs render as **Autopilot → {Manager → workers, Brain?}**. Manager
  and Brain are immediate siblings; every worker belonging to the run nests under
  Manager (even when `parent_id` is cleared). Headless brain is hidden unless
  `?all=true` / show-system.
- Plan-bound executors keep their display prefixes: `AP:<plan>` as Autopilot runs,
  `P:<plan>` as Pipeline → DAG jobs, `O:<plan>` / `M:<plan>` as Agents (with
  workers for orchestrator).
- Node ids are **composite and opaque** (`project:…`, `section:…:plans`,
  `plan:…`, `session:…`, `pipeline:…/job:…`, `run:…`) — stable across snapshots;
  key view state (collapse/cursor) off the id, never parse it.

## When to use it

- Building a fleet navigator, mobile client, or dashboard hierarchy → call
  `/api/v1/tree` (or subscribe to the `tree` SSE event).
- Flat triage (`warden ls` / `list_agents`) stays fine for status lists; reach for
  the tree when you need **parent/child structure**.

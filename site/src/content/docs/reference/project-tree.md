---
title: Project tree
description: The shared project-tree hierarchy — GET /api/v1/tree, the SSE tree event, and the TUI consumer.
---

The daemon computes one **project tree** for the whole fleet — projects, pipelines,
autopilot runs, agents, and terminals — so every UI (TUI, web, remote clients)
renders the same nesting. Clients should **prefer this tree** over joining
`/sessions`, `/pipelines`, and `/autopilot` themselves.

## Surfaces

| Surface | How |
|---|---|
| **REST** | `GET /api/v1/tree` — optional `?project_id=` scope, `?all=true` to include `system:true` sessions |
| **SSE** | Named `tree` event on `/api/v1/events/stream` (same envelope as the GET) when structure changes |
| **Capability** | `project-tree` in `GET /api/v1/capabilities` |
| **TUI** | The Projects-tab navigator walks `tree.Service.Build` locally (same package the API uses) and applies collapse/cursor on **composite node ids** |

Interactive Swagger UI documents the full schema under
[REST API & OpenAPI](/warden/reference/api-openapi/).

## Shape (summary)

- **Roots** are project nodes (registered projects, loose directories, and a synthetic
  **No project** bucket).
- Children under a project, in order: **autopilot runs → pipelines → agent
  sub-trees → terminals**.
- Autopilot runs nest **manager → guardian → tasks → workers**.
- Node ids are **composite and opaque** (`project:…`, `session:…`, `pipeline:…/job:…`,
  `run:…/task:…`) — stable across snapshots; key view state (collapse/cursor) off
  the id, never parse it.

## When to use it

- Building a fleet navigator, mobile client, or dashboard hierarchy → call
  `/api/v1/tree` (or subscribe to the `tree` SSE event).
- Flat triage (`warden ls` / `list_agents`) stays fine for status lists; reach for
  the tree when you need **parent/child structure**.

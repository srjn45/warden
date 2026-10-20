---
title: The node API is the client contract
description: Build every client against the daemon's versioned API and streaming contracts, not against a client-specific backend.
---

The daemon's versioned HTTP API is the one client contract for the bundled web
dashboard, CLI-facing integrations, and future native clients. A client should
not depend on a special mobile or Hub-only set of daemon routes. If a future
relay is used, it carries this same API over a different front door.

## Start from the OpenAPI document

Use the shipped OpenAPI document as the authoritative request and response
reference:

| Surface | URL |
| --- | --- |
| Interactive reference | `GET /api/docs` |
| OpenAPI YAML | `GET /api/docs/openapi.yaml` |
| Versioned data API | `/api/v1/...` |

The document is spec-first and the daemon's typed server is generated from it.
That makes the OpenAPI schemas—not UI behavior or internal Go types—the contract
for a network client. See [REST API & OpenAPI](/warden/reference/api-openapi/)
for authentication and route details.

## Read state, then watch it

Fetch the endpoint appropriate to the view, then use
`GET /api/v1/events/stream` to stay current. The stream sends complete snapshots,
not deltas:

- An unnamed event contains the session snapshot envelope.
- A named `tree` event contains the project-tree object; do not parse it as a
  session envelope.
- A named `error` event signals degraded state. Retain the last complete view
  and reconnect rather than replacing it with an empty fleet.
- Unknown named events are forward-compatible and must be ignored.

For interactive terminals, use the documented WebSocket routes and their raw
terminal-byte protocol; do not try to reconstruct terminal state from normal
session snapshots.

## Authentication is transport-specific, not client-specific

For direct remote access, REST requests use `Authorization: Bearer <token>`.
SSE and WebSocket connections carry the token as `?token=` because browser
upgrade/event-source APIs cannot reliably set the header. A future authenticated
relay maps its verified scope into this same authorization model; it does not
create a competing node API.

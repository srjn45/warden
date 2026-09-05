# Node implementation plan — project tree + wire contracts

- **Status:** Ready to execute (contracts locked with hub 2026-09-05)
- **Repo:** warden (this document). Hub work lives in `warden-hub:docs/implementation-plan.md`.
- **Producer spec:** [2026-09-05-project-tree-service.md](2026-09-05-project-tree-service.md) (`d9d8f65`, accepted)
- **Hub companions:** `warden-hub:docs/web-client.md`, `warden-hub:docs/hub-node-contract.md` (0650b7f)
- **Author:** warden-node orchestrator (`agent-560d77d4`)

This is the **node-side** plan only. It is the Track B (warden half) + Track C
work from the hub plan. Hub R1/R2, SPA, DB, and relay ingress are not in this
repo.

---

## 0. Locked decisions (do not relitigate)

| Item | Lock |
|---|---|
| `relay/wire` | Normative. Hub conforms to it. |
| Leg-0 key custody | Daemon-holds-key CSR. No `key_pem`. No migration (pre-deployment). |
| Device-flow types | Live in `relay/wire/device.go` (C14). |
| Conformance fixtures | Live in `relay/wire`. Both repos run them in CI. |
| BUILD-2 (`creator_session_id`) | Deferred. Pipelines root under the project. |
| Unknown `?project_id=` | 200 + empty `roots`. |
| Parent backlink field | No. Client view from `parent_id`. |
| `project_group` node | No. Label only. |
| Closed projects | In the tree, marked closed. |
| Sibling tiebreak | Creation-time ascending. |
| BUILD-1 (stamp `project_id`) | Follow-up, not this plan. |
| BUILD-3 (plan tasks in run) | Already shipped. No work. |
| `/sessions` + unnamed SSE frame | Unchanged. |

Hub-only locks (awareness, no node work): social-auth MVP, dual-write
`user_identities`, `WARDEN_HUB_TOKEN_ENCRYPTION_KEY`, multi-session tabs in MVP
(⌘K deferred), full SSE snapshots for MVP, node-detail folded into workspace
empty state, hub bind `127.0.0.1:9876`.

---

## 1. Jobs (execute in this order)

Two independent chains. Wire work unblocks the hub immediately. Tree work
unblocks web/android/TUI renderers. They may run as two pipelines or one DAG
with a fan-in review.

```
N1 device types ──► N2 fixtures ──► (hub can start R1/R2 / S5)
N3 tree package ──► N4 API+cap ──► N5 SSE ──► N6 TUI ──► N7 docs
N2 + N5 ──► N8 KindWebTerminated ──► N9 warden login
```

N8/N9 can start after N2 even if the tree is still in flight. N9 needs N1 types.

### N1 — Device-flow wire types (`relay/wire/device.go`)

**Why first:** hub Phase 1.2 imports these types. Every day we wait is another
day they can reinvent C14.

Add, matching hub-node-contract § 3.9 exactly (field names and json tags):

- `DeviceStartRequest` / `DeviceStartResponse`
- `DeviceTokenRequest` / `DeviceTokenResponse`

No private-key field. `CSRPEM` on the token request. Package tests for
round-trip JSON. Update `relay/wire/doc.go` to mention Leg-0-adjacent device
authorization as a sibling of enroll, not a fourth relay leg.

**Done when:** `go test ./relay/wire` passes; hub can import the types.

### N2 — Conformance fixtures (`relay/wire`)

Golden / table-driven tests both repos can run:

- Frame codec: known `WriteFrame`/`ReadFrame` byte vectors
- Auth digest: `DomainSepAuth` exact bytes + known nonce → digest
  (signature verification needs a fixture keypair)
- `StreamOpen`: encoded headers including `min(cert-implied, Scope)` narrowing
  cases (`KindWebTerminated`, `KindNativeE2E`, `KindControl`)
- Close-code table
- Enrollment + device JSON fixtures (no `key_pem` in any response)

Export testdata (or a `fixtures` subpackage) so the hub CI can vendor/run the
same vectors. Do not put hub logic here.

**Done when:** `go test ./relay/wire` covers the behavioural clauses in
hub-node-contract § 4.5.

### N3 — `internal/tree` service + goldens

Extract the structural core of `internal/tui/list.go` into a pure package
(`Service.Build(Inputs, projectID) *Tree`). No daemon, no TUI imports.

Lift as specified in the producer RFC §1–§8:

- Membership (`resolveGroupKey` + path canonicalize)
- Exactly-once nesting (autopilot > pipeline+job > parent_id > project)
- Composite ids
- Shared 7-value status enum
- Canonical sibling order
- Synthetic `project:__none__`
- Closed projects kept and marked
- Per-subtree `Degraded` (not all-or-nothing)

Goldens: one populated tree matching RFC §18; one empty; one unknown
`project_id` → empty roots; one closed-project case; one autopilot worker with
cleared `parent_id` (must nest under the task, not the project).

**Done when:** package tests pass without touching HTTP or the TUI.

### N4 — `GET /api/v1/tree` + `project-tree` capability

Spec-first: add `/api/v1/tree` (`project_id`, `all`) and `Tree` / `TreeNode` /
`TreeNodeDetail` to `internal/daemon/apidocs/openapi.yaml`, then `make generate`.
Handler gathers the snapshots the daemon already reads for `/sessions`,
`/projects`, `/pipelines`, `autopilot.Status()`, calls `tree.Service.Build`.

Append `"project-tree"` to `serverCapabilities` in
`internal/daemon/strict_core.go` (copy already feeds `Hello.Caps`).

200 + `Cache-Control: no-store`. 503 only when the session scan is degraded.
`ScopeReadOnly` is enough.

**Done when:** `make generate` + handler tests; `GET /api/v1/capabilities`
lists `project-tree`.

### N5 — Named `tree` SSE event

On the existing `GET /api/v1/events/stream`:

- Sessions frame stays **unnamed** (back-compat).
- Add `event: tree` with the same `Tree` JSON as N4.
- Independent dedup (`lastSess` / `lastTree`).
- `?all=true` applies to both.

**Done when:** SSE tests show an old client still parses unnamed frames, and a
new listener receives `tree` only when structure changes.

### N6 — TUI adapter

TUI stops calling `projectGroupedItems` / `buildItems` directly. New view
adapter walks `Tree.Roots`, applies collapse/cursor (re-keyed onto composite
ids), home-abbreviated paths, optional `"↳ from <parent>"` from session
`parent_id`, flattens to existing `[]listRow`. Render functions stay.

**Done when:** TUI list goldens re-pointed at `tree.Build` + adapter; cursor and
collapse survive a rebuild.

### N7 — Docs, site, skill, gendocs

DoD for a user-facing API:

- `docs/FEATURES.md`, `docs/USAGE.md` as needed
- Site: guide + reference (`site/src/content/docs/`)
- `make gendocs` after any CLI change (N9)
- Skill (`skills/warden/` / `.agents/skills/warden/`) only if agents must call
  the tree (probably a one-liner: prefer `/tree` over client-side joins)

**Done when:** `make gendocs-check` is clean.

### N8 — `KindWebTerminated` on the daemon relay

Accept `wire.StreamOpen` with `KindWebTerminated`. Gate with
`relay.allow_web_terminated` (default off until we want it on for hub web).
Reject with close code 4004 when disabled. Honor hub-asserted `{Grantee, Scope}`.
`ScopeReadOnly` must not attach (`/attach` stays 403).

Depends on N2 (fixtures) and a real `internal/relay` accept path. If the
connector is still unbuilt, this job includes the minimum accept-side to honor
`StreamOpen` — not the full dial-out client (that is N9 + connector).

**Done when:** fixture vectors for narrowing + 4004 reject pass.

### N9 — `warden login [--hub <url>]`

CLI (admin surface, CLI-only is fine): device start → print verification URI +
user code → poll with local CSR → persist cert & key → ready to dial `/relay`.
Uses N1 types. Never sends a private key. Never accepts `key_pem`.

**Done when:** `make gendocs` updated; a dry-run against a fake hub fixture
completes the happy path and the four poll errors
(`authorization_pending`, `slow_down`, `expired_token`, `access_denied`).

---

## 2. Out of scope (this plan)

- Hub R1/R2 (import wire, fix enrollment) — hub repo
- Hub SPA, DB 00002, relay ingress, rendezvous — hub repo
- BUILD-1 / BUILD-2
- Delta/incremental SSE
- DAG-authoring UI, autopilot plan editing
- Changing `/sessions` or the unnamed SSE frame
- Project-group nodes

---

## 3. Suggested pipeline

Two pipelines so wire can land and tag without waiting on the TUI rewrite.

**Pipeline `node-wire-contracts`** (unblocks hub Phase 1):

```yaml
name: node-wire-contracts
repo: /home/srjn45/dev/warden
jobs:
  - id: n1-device-types
    prompt: "Implement N1 from docs/specs/2026-09-05-node-implementation-plan.md — relay/wire/device.go only. Docs-adjacent comments in doc.go. Tests. Commit on this branch."
    worktree: fresh
  - id: n2-fixtures
    prompt: "Implement N2 from docs/specs/2026-09-05-node-implementation-plan.md — conformance fixtures. Depends on N1 types."
    depends_on: [n1-device-types]
    worktree: from:n1-device-types
  - id: review
    prompt: "Review N1+N2 against hub-node-contract §3.9 and §4.5. No key_pem anywhere. Request changes or approve."
    depends_on: [n2-fixtures]
    worktree: from:n2-fixtures
    type: review
```

**Pipeline `node-project-tree`** (BUILD-0):

```yaml
name: node-project-tree
repo: /home/srjn45/dev/warden
jobs:
  - id: n3-tree-pkg
    prompt: "Implement N3 from docs/specs/2026-09-05-node-implementation-plan.md and the producer RFC. Pure internal/tree + goldens. No HTTP."
    worktree: fresh
  - id: n4-api
    prompt: "Implement N4 — OpenAPI + generate + GET /api/v1/tree + project-tree cap. Use the package from n3."
    depends_on: [n3-tree-pkg]
    worktree: from:n3-tree-pkg
  - id: n5-sse
    prompt: "Implement N5 — named tree SSE event, independent dedup, sessions frame unnamed."
    depends_on: [n4-api]
    worktree: from:n4-api
  - id: n6-tui
    prompt: "Implement N6 — TUI adapter onto composite ids. Do not rewrite renderers."
    depends_on: [n5-sse]
    worktree: from:n5-sse
  - id: n7-docs
    prompt: "Implement N7 — FEATURES/USAGE/site/skill + make gendocs if needed. DoD checklist."
    depends_on: [n6-tui]
    worktree: from:n6-tui
  - id: review
    prompt: "Review BUILD-0 against docs/specs/2026-09-05-project-tree-service.md. Goldens, cap flag, SSE back-compat, TUI identity re-key."
    depends_on: [n7-docs]
    worktree: from:n7-docs
    type: review
```

N8/N9 wait until N2 is merged and we decide the connector is in scope for the
same train. Default: **second train**, after wire + tree PRs exist.

---

## 4. Tagging / DoD

- Wire train: **patch** (`v*` after N1+N2 merge) so the hub can import a tagged
  module. Confirm with the maintainer before pushing the tag.
- Tree train: **minor** — new API + cap + TUI consumer.
- Each train updates the docs listed in N7. `make gendocs` only if CLI help
  changed (N9).

---

## 5. What I will not start until you say so

Creating and starting the pipelines. This file is the plan. Hub already has
theirs (`docs/implementation-plan.md` @ `0650b7f`).

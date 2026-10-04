# Plan Modification API & CLI

**Date:** 2026-10-04  
**Status:** Shipped on integration branch `autopilot/plan-modification-api-and-cli`  
**Scope:** First-class mutation of pending Plan definitions (metadata, task DAG)
via daemon API, client SDK, and CLI — with strict immutability for non-pending
plans and optimistic concurrency.

---

## Authority

Canonical Plans live in ScrivaDB (`internal/planstore`). Definition fields
(`name`, `goal`, `constraints`, `done_when`, task DAG) may be mutated **only**
while `Status == pending`. Attempts against `in_progress`, `completed`, or
`archived` plans return **HTTP 409 Conflict** (`planstore.ErrNotPending`).

Every successful mutation bumps `revision` and recomputes `content_hash`.
Callers may pass `expected_revision` for optimistic concurrency; a mismatch
returns 409 with structured fields (`plan_id`, `expected`, `actual`).

DAG rules on every task mutation:

- Declared `after` dependencies must exist in the plan
- The resulting graph must be acyclic (`ValidateTaskDAG`)
- `RemoveTask` is rejected if any remaining task still depends on the target

---

## HTTP surface

| Method | Path | Handler |
|---|---|---|
| `PUT` / existing update | `/api/v1/plans/{plan_id}` | Full/partial definition replace (existing `update_plan`) |
| `POST` | `/api/v1/plans/{plan_id}/tasks` | `AddPlanTask` |
| `PATCH` | `/api/v1/plans/{plan_id}/tasks/{task_id}/definition` | `UpdatePlanTaskDefinition` |
| `DELETE` | `/api/v1/plans/{plan_id}/tasks/{task_id}` | `DeletePlanTask` |

Error mapping: `ValidationError` → 400; `ErrNotFound` → 404;
`ErrNotPending` / `RevisionConflictError` → 409.

OpenAPI: `internal/daemon/apidocs/openapi.yaml` (regenerate with `make generate`).

---

## Client SDK (`internal/client`)

- `PlansUpdate` / `ParsePlanYAML` — YAML file → update request with DAG validation
- `PlansTaskAdd` / `PlansTaskUpdate` / `PlansTaskDelete` — granular task mutations

---

## CLI

| Command | Purpose |
|---|---|
| `wd plan update <id> [--file yaml] [--name] [--goal] [--constraint] [--done-when]` | Patch pending definition (flags overlay `--file`) |
| `wd plan edit <id>` | Open definition in `$EDITOR`, validate, save with revision check |
| `wd plan task add <id> --id <tid> --prompt <p> [--after dep]` | Append a task |
| `wd plan task edit <id> --id <tid> [--prompt] [--after]` | Patch one task |
| `wd plan task rm <id> <tid>` | Remove a task (blocked by dependents) |

Non-pending mutations surface the daemon's 409 Conflict to the operator.

---

## Out of scope

- Mutating definitions of non-pending plans (explicitly forbidden)
- Editing repository YAML replicas as authority (replicas remain inert)
- Background Hub replication of edits (Hub stays explicit Push/Pull/Discover)

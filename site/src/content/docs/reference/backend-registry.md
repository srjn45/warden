---
title: Backend registry reference
description: The full surface of warden's agent-backend registry — data model, tiers, thinking-mode, CLI verbs, MCP tools, REST endpoints, and config keys.
---

The agent-backend registry is warden's persistent, machine-wide record of which
coding-agent CLIs exist, how they're billed, and which is the default. For the
narrative and the mental model, see the [Backend registry
guide](/warden/guides/backend-registry/); this page is the flat reference.

## Data model

One record per backend (keyed by its stable id) plus a reserved settings singleton,
in an embedded ScrivaDB collection at `~/.warden/backends`.

### Backend row

| Field | Type | Notes |
|---|---|---|
| `id` | string | Stable id (`claude`, `codex`, `aider`, …, and the reserved `local`) |
| `installed` | bool | Binary found on `PATH` (or, for `local`, the endpoint reachable) — **detection fact** |
| `binary_path` | string | Resolved `LookPath` (empty for `local`) — **detection fact** |
| `detected_at` | timestamp | Last reconcile — **detection fact** |
| `tier` | string | `free` \| `subscription` \| `pay_per_use` \| `unclassified` \| `local` — **preference** |
| `default` | bool | At most one row is `true` — **preference** |
| `enabled` | bool | Whether the backend may be used — **preference** |
| `is_local` | bool | The reserved local-model row (never limited, never a default) |
| `limited_until` | timestamp | Router-stamped rate-limit expiry; always zero when `is_local` |

**Detection fields** are reconciled by a rescan; **preferences** are preserved by a
rescan.

### Settings singleton

| Field | Type | Notes |
|---|---|---|
| `internal_thinking_mode` | string | `local_only` \| `free_plus_local` (default) |
| `allow_paid_autopilot` | bool | The paid-autopilot gate (migrated from `autopilot.brain.allow_pay_per_use`) |

## Tiers

| Tier | Meaning | Router calls it? |
|---|---|---|
| `free` | `$0` backend (free plan) | ✅ (the only CLI tier the router calls) |
| `subscription` | Flat subscription | ❌ |
| `pay_per_use` | Metered / pay-as-you-go | ❌ |
| `unclassified` | Not yet tiered (a newly detected CLI); treated as **not free** | ❌ |
| `local` | Reserved, system-set (the `local` row only) | ✅ (terminal candidate) |

## Internal-thinking mode

| Mode | Candidate walk |
|---|---|
| `local_only` | `[local]` |
| `free_plus_local` *(default)* | `[eligible free CLIs (default-first, then stable id), …, local]` |

A free CLI backend is **eligible** only when `installed && enabled && tier == "free"
&& limited_until` is in the past. Paid (`subscription` / `pay_per_use`),
`unclassified`, and disabled backends are **never** in the walk. When the walk is
exhausted, the caller degrades gracefully — warden never escalates internal thinking
to a paid backend.

## The reserved `local` row

- **`local`** — a `$0`, never-limited, never-default class for the local model. Its
  tier is the system-set `local`; the terminal candidate of every internal-thinking
  walk.

## CLI — `warden backends`

| Command | Does |
|---|---|
| `warden backends list` (alias `ls`) | Print the registry table (ID, installed, tier, default, enabled, limited) + the thinking mode |
| `warden backends rescan` | Re-detect installed CLIs, reconcile detection, preserve preferences |
| `warden backends tier <id> <tier>` | Assign a tier (`free`\|`subscription`\|`pay_per_use`\|`unclassified`) |
| `warden backends default <id>` | Set the single default (rejects unknown/uninstalled/disabled/`local`) |
| `warden backends enable <id>` | Enable a backend |
| `warden backends disable <id>` | Disable a backend |
| `warden backends thinking-mode <mode>` | Set `local_only` \| `free_plus_local` |

The generated [CLI reference](/warden/reference/cli/) has the full flag/help detail.

## Subscription usage — `warden usage`

`warden usage` asks the daemon for provider-owned usage data from every persisted
row whose tier is exactly `subscription`. Disabled and uninstalled rows remain in
the result; the read never rescans the registry or changes routing, cooldowns, or
the default backend. Use `--json` for the stable v1 document and `--refresh` to
bypass the daemon's 60-second fresh cache.

Each backend has a `usage` array containing zero or more distinct limits, sorted by
stable `id`. Every limit includes `id`, applicability `scope`, a human `label`,
nullable `used_percent` and `resets_at`, plus nullable `model_families` and `models`
selectors. Multiple provider pools are never flattened: Codex primary/secondary
windows remain separate, and an adapter that can safely identify Gemini and
non-Gemini pools returns separate scoped entries. Unknown measurements, restore
times, or model selectors remain JSON `null`; warden never guesses them.

Codex supplies structured percentage limits and reset times through its app-server
protocol. Claude and Cursor currently supply safe plan/login metadata but report
usage as `unsupported`; Antigravity reports `unsupported`. Warden deliberately does
not substitute its local synthetic quota estimates or scrape interactive `/usage`
screens. Account labels, credentials, raw provider output, paths, and request IDs
are excluded.

The command exits 0 when every row is `ok` or truthfully `unsupported`, 2 after
rendering a partial result with an operational provider failure, and 1 when no
consolidated document can be produced.

```json
{
  "id": "antigravity",
  "status": "ok",
  "usage": [
    {"id": "antigravity:gemini", "scope": "gemini", "label": "Gemini models", "model_families": ["gemini"], "models": null, "used_percent": 50, "resets_at": "2026-09-01T12:00:00Z"},
    {"id": "antigravity:non-gemini", "scope": "non-gemini", "label": "Non-Gemini models", "model_families": null, "models": null, "used_percent": null, "resets_at": null}
  ]
}
```

The example illustrates the contract for a future structured Antigravity adapter;
the current installed CLI has no supported structured usage source and therefore
returns `status: "unsupported"` with an empty `usage` array.

## MCP tools

| Tool | Args | Returns |
|---|---|---|
| `list_backends` | — | `{backends[], settings}` (read-only) |
| `rescan_backends` | — | The refreshed `{backends[], settings}` |
| `set_backend_tier` | `{id, tier}` | The updated backend |
| `set_default_backend` | `{id}` | The refreshed `{backends[], settings}` |
| `set_thinking_mode` | `{mode}` | The updated settings |

**Enable/disable is not an MCP tool** — use the CLI, web, TUI, or REST
`PATCH /api/v1/backends/{id}`.

## REST endpoints

| Method + path | Purpose |
|---|---|
| `GET /api/v1/backends` | The registry + settings (read-only) |
| `GET /api/v1/usage?refresh=true` | Provider usage for exact subscription-tier rows (read-only) |
| `POST /api/v1/backends/rescan` | Re-detect and reconcile; returns the registry + settings |
| `PUT /api/v1/backends/default` | `{id}` → set the default; returns the registry + settings |
| `PUT /api/v1/backends/thinking-mode` | `{mode}` → set the thinking mode; returns settings |
| `PATCH /api/v1/backends/{id}` | `{tier?, enabled?}` → update one backend (both fields optional) |

See the [OpenAPI reference](/warden/reference/api-openapi/) (`/api/docs`) for the full
schemas.

## Config

| Key | Default | Description |
|---|---|---|
| `backends.limit_retry` | `15m` | Go duration — how long the internal-thinking router skips a free CLI backend after a rate-limit / spend signal, before retrying it |

Tiers, the default, enabled flags, and the thinking mode live in the **store**, not
the config file — edit them via the surfaces above.

## Autopilot ladder & deprecated config

Autopilot's cost-tier ladder and paid-autopilot gate derive from the registry: only
installed, enabled, non-`local` rows are eligible, bucketed by tier. The deprecated
`autopilot.brain.backends.{free,subscription,pay_per_use}` ladder and
`autopilot.brain.allow_pay_per_use` gate are **imported once** on the first boot after
upgrade (sentinel-guarded, idempotent) and then ignored — the store is authoritative,
and the daemon logs a deprecation warning if the config still carries them. See the
[Autopilot concepts page](/warden/concepts/autopilot/).

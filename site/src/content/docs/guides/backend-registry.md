---
title: Backend registry
description: warden keeps a persistent registry of the coding-agent CLIs on your machine — their billing tier, the default, and an internal-thinking mode. Manage it from the CLI, web, TUI, or MCP; it is the single source of truth for autopilot's cost ladder and the free/local thinking router.
---

Picking a backend per spawn with [`--backend`](/warden/concepts/agent-backends/) is
the *foreground* choice. The **backend registry** is the durable *background* picture
behind it: warden detects the coding-agent CLIs installed on this machine and
remembers, per backend, **how it's billed**, **whether it's enabled**, and **which one
is the default** — plus a machine-wide **internal-thinking mode**.

That store is warden's **single source of truth** for backends. Two subsystems read
from it:

- **[Autopilot](/warden/concepts/autopilot/)'s cost-tier ladder** — cheapest-first
  backend selection for the manager and guardian.
- **The internal free/local thinking router** — warden's own thinking (task
  classification, agent naming, digest narration, memory curation) routed *strictly*
  through free and local backends, **never** a paid call.

## The mental model: detection is a fact, tiering is a preference

The registry keeps two kinds of data per backend, and a **rescan** treats them
differently:

| Kind | Fields | On rescan |
|---|---|---|
| **Detection (fact)** | `installed`, `binary_path`, `detected_at` | **Reconciled** — newly installed CLIs are added; vanished ones are marked uninstalled |
| **Preference (yours)** | `tier`, `default`, `enabled` | **Preserved** — a rescan never overwrites your choices |

So you can `warden backends rescan` freely: it refreshes *what's on disk* without ever
resetting *how you tiered things*.

The store lives in an embedded ScrivaDB collection at `~/.warden/backends` and is
managed by the daemon — every surface below is a thin caller of the same
`/api/v1/backends*` endpoints.

## The reserved `local` row

Alongside the detected CLIs, the registry always carries a reserved **`local`** row —
a `$0`, **never-rate-limited** class representing warden's [local
model](/warden/multi-agent/repl/). It is special:

- Its tier is the system-set **`local`** (you can't re-tier it).
- It can **never** be a user-agent default.
- It is the **terminal candidate** of the internal-thinking walk — the fallback that
  always answers.

## Tiers

Every detected backend carries a billing **tier**:

| Tier | Meaning |
|---|---|
| `free` | A `$0` backend (you run it on a free plan). The **only** CLI tier the internal-thinking router calls. |
| `subscription` | Covered by a flat subscription. |
| `pay_per_use` | Metered / pay-as-you-go. |
| `unclassified` | Not yet tiered. A newly detected CLI starts here, treated as **not free**. |
| `local` | Reserved, system-set — the `local` row only. |

A newly detected CLI is `unclassified` until you tier it, so warden never assumes a
backend is free (and never routes internal thinking to it) without you saying so.

## The internal-thinking mode

warden does a fair amount of its own "thinking" — classifying a task, summarizing
activity, naming an agent, narrating a digest, curating project memory. This is
**internal**, non-user-facing work, and warden routes it **only** through free and
local backends. It **never makes a paid call**. The **thinking-mode** picks the walk:

| Mode | Walk |
|---|---|
| `local_only` | The local model only. |
| `free_plus_local` *(default)* | Eligible **free** CLI backends first (default-first, then stable id order), then the never-limited local model. |

A free CLI backend is eligible only when it is **installed**, **enabled**, tier
**`free`**, and **not currently rate-limited**. On a rate-limit / spend signal warden
stamps that backend limited (config `backends.limit_retry`, default `15m`) and moves
to the next candidate. When the walk is exhausted, the caller **degrades gracefully**
— a deterministic slug, a skipped narration, the default task bucket, no memory
proposal — rather than escalating to a `subscription` / `pay_per_use` backend.

## Managing it

### CLI — `warden backends`

```sh
warden backends list                 # full table incl. the reserved local row (alias: ls)
# ID       INSTALLED  TIER          DEFAULT  ENABLED  LIMITED
# aider    ✓          unclassified  -        ✓        -
# claude   ✓          subscription  ✓        ✓        -
# codex    ✓          free          -        ✓        -
# local    -          local         -        ✓        -
#
# internal thinking mode: free_plus_local

warden backends rescan               # re-detect installed CLIs (preferences preserved)
warden backends tier codex free      # free | subscription | pay_per_use | unclassified
warden backends default claude       # set the single default (rejects local)
warden backends enable codex         # / warden backends disable aider
warden backends thinking-mode local_only   # or free_plus_local
```

`warden backends default <id>` is rejected for an unknown, uninstalled, disabled, or
reserved (`local`) target — the same rules the daemon enforces.

### Web — the 🧩 backends panel

Open the **🧩 backends** button in the web AttentionBar (Esc closes it). It's a table
with a **Tier** dropdown, a **Default** radio, an **Enabled** checkbox, and a live
**Limited** countdown per row, plus a header **thinking-mode** selector and a **⟳
Rescan** button. The reserved `local` row shows a static "Local" tier and a disabled
default radio.

### TUI — the Backends page (`b`)

Press `b` in the [TUI cockpit](/warden/guides/tui-cockpit/) to open the Backends page:

| Key | Action |
|---|---|
| `t` | Cycle the focused backend's tier |
| `d` / `enter` | Make it the default |
| `e` / space | Toggle enabled |
| `m` | Flip the internal-thinking mode |
| `r` | Rescan |
| `esc` / `b` | Back |

### MCP

For an orchestrating agent:

| Tool | Does |
|---|---|
| `list_backends` | The whole registry + settings (read-only) |
| `rescan_backends` | Re-detect and reconcile, return the refreshed registry |
| `set_backend_tier` | Assign a billing tier |
| `set_default_backend` | Set the single default |
| `set_thinking_mode` | Set `local_only` / `free_plus_local` |

**Enabling/disabling a backend is intentionally not an MCP tool** — it is available on
the CLI, web, and TUI, and over REST as `PATCH /api/v1/backends/{id}`.

## Autopilot reads the same registry

[Autopilot](/warden/concepts/autopilot/)'s **cost-tier backend ladder** and its
**paid-autopilot gate** are derived from this registry: only **installed, enabled,
non-`local`** backends are eligible, bucketed by tier, cheapest first. So the way you
steer autopilot's spending is simply how you tier backends here.

:::note[Deprecation]
The registry **supersedes** the old `autopilot.brain.backends.{free,subscription,pay_per_use}`
ladder and the `autopilot.brain.allow_pay_per_use` gate in `~/.warden/config.yaml`.
Those keys are imported into the store **once** on the first boot after upgrade (a
one-time, sentinel-guarded migration), then **ignored** — the store value wins
thereafter, and the daemon logs a deprecation warning if the config still carries
them. Tier autopilot's backends with `warden backends tier` from then on.
:::

## See also

- [Agent backends](/warden/concepts/agent-backends/) — picking a backend per spawn and
  how capabilities degrade gracefully.
- [Backend registry reference](/warden/reference/backend-registry/) — the full endpoint
  / tool / config surface.
- [Autopilot](/warden/concepts/autopilot/) — the cost-tier ladder that reads this
  registry.

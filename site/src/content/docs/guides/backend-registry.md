---
title: Backend registry
description: warden keeps a persistent registry of the coding-agent CLIs on your machine — their billing tier, the enabled flag, and the default. Manage it from the CLI, web, TUI, or MCP; it is the single source of truth for autopilot's cost ladder.
---

Picking a backend per spawn with [`--aicli`](/warden/concepts/agent-backends/) is
the *foreground* choice. The **backend registry** is the durable *background* picture
behind it: warden detects the coding-agent CLIs installed on this machine and
remembers, per backend, **how it's billed**, **whether it's enabled**, and **which one
is the default**.

That store is warden's **single source of truth** for backends. It feeds
**[Autopilot](/warden/concepts/autopilot/)'s cost-tier ladder** — cheapest-first
backend selection for the manager and guardian.

warden's own internal thinking (task classification, summaries, agent naming, commit
messages, digest narration, memory curation, REPL planning) is **not** driven by the
registry: it runs on **Fast-Brain**, a latency-bounded, fail-open gateway over a
headless backend CLI. There is no thinking-mode setting and no reserved `local` row
any more (a legacy `PUT /api/v1/backends/thinking-mode` is a 200 no-op).

## The mental model: detection is a fact, tiering is a preference

The registry keeps two kinds of data per backend, and a **rescan** treats them
differently:

| Kind | Fields | On rescan |
|---|---|---|
| **Detection (fact)** | `installed`, `binary_path`, `detected_at` | **Reconciled** — newly installed CLIs are added; vanished ones are marked uninstalled |
| **Preference (yours)** | `tier`, `default`, `enabled` | **Preserved** — a rescan never overwrites your choices |

So you can `warden backend rescan` freely: it refreshes *what's on disk* without ever
resetting *how you tiered things*.

The store lives in an embedded ScrivaDB collection at `~/.warden/backends` and is
managed by the daemon — every surface below is a thin caller of the same
`/api/v1/backends*` endpoints.

## Tiers

Every detected backend carries a billing **tier**:

| Tier | Meaning |
|---|---|
| `free` | A `$0` backend (you run it on a free plan). The cheapest rung of autopilot's cost ladder. |
| `subscription` | Covered by a flat subscription. |
| `pay_per_use` | Metered / pay-as-you-go. |
| `unclassified` | Not yet tiered. A newly detected CLI starts here, treated as **not free**. |

A newly detected CLI is `unclassified` until you tier it, so warden never assumes a
backend is free without you saying so.

## Managing it

### CLI — `warden backend`

```sh
warden backend list                 # full table of detected backends (alias: ls)
# ID       INSTALLED  TIER          DEFAULT  ENABLED  LIMITED
# aider    ✓          unclassified  -        ✓        -
# claude   ✓          subscription  ✓        ✓        -
# codex    ✓          free          -        ✓        -

warden backend rescan               # re-detect installed CLIs (preferences preserved)
warden backend tier codex free      # free | subscription | pay_per_use | unclassified
warden backend default claude       # set the single default
warden backend enable codex         # / warden backend disable aider
```

`warden backend default <id>` is rejected for an unknown, uninstalled, or disabled target — the same rules the daemon enforces.

### Web — the 🧩 backends panel

Open the **🧩 backends** button in the web AttentionBar (Esc closes it). It's a table
with a **Tier** dropdown, a **Default** radio, an **Enabled** checkbox, and a live
**Limited** countdown per row, plus a header **⟳ Rescan** button.

### TUI — the Backends page (`b`)

Press `b` in the [TUI cockpit](/warden/guides/tui-cockpit/) to open the Backends page:

| Key | Action |
|---|---|
| `t` | Cycle the focused backend's tier |
| `d` / `enter` | Make it the default |
| `e` / space | Toggle enabled |
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

**Enabling/disabling a backend is intentionally not an MCP tool** — it is available on
the CLI, web, and TUI, and over REST as `PATCH /api/v1/backends/{id}`.

## Autopilot reads the same registry

[Autopilot](/warden/concepts/autopilot/)'s **cost-tier backend ladder** and its
**paid-autopilot gate** are derived from this registry: only **installed, enabled**
backends are eligible, bucketed by tier, cheapest first. So the way you
steer autopilot's spending is simply how you tier backends here.

:::note[Deprecation]
The registry **supersedes** the old `autopilot.brain.backends.{free,subscription,pay_per_use}`
ladder and the `autopilot.brain.allow_pay_per_use` gate in `~/.warden/config.yaml`.
Those keys are imported into the store **once** on the first boot after upgrade (a
one-time, sentinel-guarded migration), then **ignored** — the store value wins
thereafter, and the daemon logs a deprecation warning if the config still carries
them. Tier autopilot's backends with `warden backend tier` from then on.
:::

## Integrity, single-daemon rule, and offline repair

The backend registry is opened by **exactly one** warden process at a time. The
daemon takes an exclusive data-directory ownership lock
(`<data_dir>/.warden-owner.lock`) at startup.

:::caution[Single daemon per data directory]
Run exactly one warden daemon per data directory. With the systemd user service,
use `systemctl --user stop warden` and `systemctl --user start warden`, never a
manual `warden daemon` beside it. Multiple concurrent writers can cause revision
regressions in the underlying ScrivaDB store.
:::

Direct CLI operations (`wd models`, `wd role`) route transparently through the
daemon's API when the daemon is running, avoiding split-brain writes. When the
daemon is stopped, CLI commands take a temporary lock.

If the registry suffers revision regressions or damage:
- The daemon refuses to start on unresolvable/ambiguous damage with an actionable
  error pointing to `warden repair backends`.
- `warden doctor` inspects backend registry integrity in read-only mode. Beside a
  running daemon the persisted index legitimately trails its latest writes (it is
  persisted lazily), so `doctor` reports that as clean; only findings that remain
  with the daemon stopped (`warden repair backends --dry-run`) call for a repair.
- `warden update` preflights registry integrity before swapping binaries.

### Offline repair procedure

Offline repair is backup-first, audited, and strictly offline:

```sh
# 1. Stop the daemon
systemctl --user stop warden

# 2. Inspect findings without modifying anything (read-only)
warden repair backends --dry-run

# 3. Repair with backup and confirmation (or pass --yes)
warden repair backends

# 4. Restart the daemon
systemctl --user start warden
```

Each repair takes a verified backup first, removes only provably stale revisions,
and writes an audit report. Ambiguous findings are never guessed.

## See also

- [Agent backends](/warden/concepts/agent-backends/) — picking a backend per spawn and
  how capabilities degrade gracefully.
- [Backend registry reference](/warden/reference/backend-registry/) — the full endpoint
  / tool / config surface.
- [Autopilot](/warden/concepts/autopilot/) — the cost-tier ladder that reads this
  registry.
- [Agent store integrity](/warden/guides/agent-store-integrity/) — integrity and
  recovery for the agent sessions database.

## Perishable quota: reset-aware model selection

When warden picks a backend and model within a tier (initial routing and
reactive rate-limit recovery alike), it prefers capacity that is about to reset
so subscription quota isn't wasted. Candidates fall into two classes:

- **Class A — impending resets:** quota resets within **1 hour** and has at least
  **10% headroom**. Ordered by earliest reset first, then most headroom.
- **Class B — standard:** resets further out, already past, unknown, or with
  under 10% headroom. Ordered by most headroom, with round-robin among ties.

Class A is tried before Class B. The **10% safety floor** means a nearly
exhausted pool is never prioritized just because it resets soon — that would
only cause an immediate rate-limit stall.

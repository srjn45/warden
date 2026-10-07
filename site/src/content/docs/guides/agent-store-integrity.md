---
title: Agent store integrity
description: How warden owns, health-checks and (when supported) repairs the agent store — and the safe daemon-offline procedure when it degrades.
---

import { Aside } from '@astrojs/starlight/components';

The agent store (`<data_dir>/agents-db`) is opened by **exactly one** warden process: the daemon. Everything else — CLI, MCP, TUI, Web Cockpit — goes through the daemon's API, so normal commands are unaffected by any of what follows.

## Ownership

The daemon takes an exclusive lock (`<data_dir>/.agents-store.lock`) before it imports, reconciles or listens. A second daemon — even on another port — is refused with:

```
agent store <dir> is owned by another warden process (lock <file>); stop the running warden daemon …
next step: stop the running warden daemon (including one on another port) that uses this data directory, then retry; do not delete the lock file
```

Offline tools (`warden doctor --reconcile-membership`, `warden inspect repair agents`) follow the same rule and never open a live-owned store. `warden repair agents` remains a compatibility alias. The lock is released by the kernel when the owner exits or dies; never delete it by hand.

## Seeing the state

| Surface | How |
|---|---|
| REST | `GET /api/v1/store/health` — `healthy`, `failures[]`, `repair_available`, `next_step` (always 200) |
| CLI | `warden doctor` — an "agent store" line: healthy / DEGRADED / owned-while-daemon-down |
| MCP | `store_health` tool (read-only) |
| TUI | header chip `degraded ●` and banner "session store degraded — run `warden doctor`"; the last complete fleet stays on screen |
| Web | fleet requests return 503 with the same actionable message |

Reads are complete-or-error: a degraded store returns an error, never a silently shorter fleet. **Running agents are never affected** — tmux sessions are not relaunched, adopted or killed by a read failure.

## Repair — current status

<Aside type="caution">
Automated repair (`repair_available: false`) is **not available yet.** It depends on ScrivaDB Verify/Repair primitives (upstream issue srjn45/scriva#107) that the pinned release does not export, and warden deliberately does not emulate them. `warden inspect repair agents` therefore only checks preconditions and reports this; it modifies nothing.
</Aside>

`warden inspect repair agents` is offline-only and permission-gated: it refuses unless you own the data directory (or are root) and no warden process owns the store. Every attempt — denied, refused, or unavailable — emits an `audit: agent-store repair attempt` log event (`audit=true`, `action`, `outcome`, `data_dir`, `uid`). Daemon startup refusals emit `audit: daemon startup refused`. Repair is intentionally not exposed over REST, MCP, TUI or Web.

## Daemon-offline procedure

1. `warden doctor` — confirm the store is DEGRADED (or owned).
2. Stop the daemon (`systemctl --user stop warden`, or terminate it). Agents' tmux sessions keep running.
3. Back up the whole data directory: `cp -a ~/.warden ~/.warden.bak-$(date +%s)` (keeps 0700/0600 modes; keep it private).
4. Run `warden inspect repair agents` to re-check preconditions. Until repair is available it reports so and stops; the backup and a report to the maintainers (include `warden doctor` output) are the safe end state.
5. Start the daemon again; if the store is still degraded the daemon keeps serving with explicit errors rather than partial data.
6. To roll back, stop the daemon and restore the backup over the data directory.

## Upgrading safely

No data-format migration is required for this release. Before upgrading a daemon
that owns an existing data directory, stop it cleanly and take a private backup
of the whole data directory. Start only one upgraded daemon for each data
directory; a second daemon, `doctor --reconcile-membership`, or the offline
repair preflight will now fail fast while the first daemon owns the store.

The original incident did **not** establish the identity of a second writer.
The plausible paths remain a second daemon and an offline membership reconcile;
the ownership guard prevents both from opening a live store, but does not turn
that hypothesis into a proven root cause. ScrivaDB Verify/Repair remains the
required dependency before Warden can offer a mutating recovery workflow.

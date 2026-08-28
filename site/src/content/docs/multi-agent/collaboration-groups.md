---
title: Collaboration groups
description: Make one developer's per-project orchestrator agents mutually discoverable so they message and delegate across projects — a roster and introduction broker on the existing message bus.
---

A **collaboration group** is a named set of one developer's **per-project orchestrator agents**, made mutually discoverable so they can message and delegate across projects (backend ↔ frontend, a project and a dependency it needs changed, and so on). It is deliberately thin — a **roster + an introduction broker**, not a new communication channel. Peers talk over the existing [directed-message bus](/warden/multi-agent/shared-context-messages/) once they know each other exists.

Scope is the **single developer, single machine, multiple projects** case. (Cluster mode and multi-developer collaboration are future work that needs a hub.)

## Command surface

```sh
warden collaborate group my-team join     # seat this agent as its project's orchestrator
warden collaborate group my-team leave    # remove this agent's seat (soft)
```

Identity defaults to `$WARDEN_SESSION_ID` (set on every agent's tmux session), so an agent runs these with no flags; pass `--as <agent-id>` to act as another. MCP mirror: **`collaborate_group {group, action}`** — CLI/MCP parity.

## Joining

On **join**, warden:

1. **Creates the group** if it does not exist.
2. **Enforces one orchestrator per project.** A "project" is keyed by its **canonical git remote URL** (normalized for scheme / host / `.git` suffix / trailing slash / casing), so two worktrees of one repo are the *same* project and cannot both hold a seat. A repo with **no remote** joins using a **`local:` fallback key** derived from its path (tagged `local:` because it is not portable across machines). A **duplicate join fails with `409`** and returns the already-seated agent's id, so the caller messages the incumbent instead of erroring out.
3. **Switches the agent to the `orchestrator` role.**
4. **Resolves a one-line project summary** (cheapest first): a declared blurb — a `## Summary` or the first paragraph of `CLAUDE.md` / `README.md` — else warden asks the joining agent once (it already has project context loaded), then **caches** the answer on the record so re-joins and daemon restarts never regenerate it.
5. **Brokers introductions both directions.** Warden — not the agents — sends each existing member a templated descriptor of the joiner (*"agent `<name>` orchestrates project `<project>` — `<summary>`; contact it for changes there"*) and sends the joiner the reciprocal roster. Agents spend **zero tokens** on introduction prose.

## Leave vs. terminate

Directed messaging is keyed by **agent-id** and is independent of group membership, so the two operations are cleanly separable:

- **Leave is soft (default, reply-safe).** The agent stops being discoverable and stops accepting *new* inbound delegations, but it does **not** disappear as a message target — replies to work it delegated still route to its inbox, and delegations it received and is mid-way on are drained. Leave means "no new inbound," not "abandon in-flight." Warden notifies the group.
- **Terminate is hard (this is what orphans work).** Tearing down an orchestrator that still holds received delegations orphans them, so **terminating a grouped orchestrator requires explicit confirmation** — a prompt naming the group(s) and any outstanding delegations that would be abandoned — and warden notifies each requester that their delegate was terminated. Prefer `leave` followed by a graceful teardown.

On **`warden recover`**, a recovered orchestrator **auto-rejoins** its groups and re-announces: the group is **durable** (survives daemon restarts) while membership is **live** (tied to a running agent), so groups don't rot after a restart.

## Cross-project delegation

There is no delegation *protocol* in v1 — the group is an **address book**, and delegation is just a directed message to a known peer:

> The frontend orchestrator sends the backend orchestrator: *"I need `POST /x`."* The backend orchestrator reads its inbox, decides, spawns **its own** worker on **its own** worktree, opens a PR in the **backend** repo, and replies with the PR link.

This keeps warden's git-first invariants intact: worktree isolation holds (nobody edits a repo they don't own), cost is attributed to the right project (the target's worker spends the tokens), and every cross-project change is an auditable PR in the target repo.

## Storage

The group record holds **only** the lean roster — per member `{name, id, project (remote key), summary, joined_at}`. Message logs and transcripts stay in the existing inbox store; the group record stays small and mostly static.

## Relationship to Open Project

The TUI's [Open Project panel](/warden/guides/tui-cockpit/) (`o`) and `collaborate group join` are distinct but share two invariants: the **project key** and **one orchestrator per project**. Opening a project auto-spawns (or focuses) its single orchestrator; `join` opts that orchestrator into cross-project **membership**. You can open projects without ever forming a group; you join a group when you want those orchestrators to see and delegate to each other.

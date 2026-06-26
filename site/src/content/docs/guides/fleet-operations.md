---
title: Fleet operations
description: Search, history, presets, batch actions, handoff, export/import, and the audit log — managing a fleet at scale.
---

Once you're running more than a handful of agents, you need to find them, group them, act on them in bulk, and keep a record. These commands cover the fleet-management surface.

## Find & filter

```sh
warden search auth refactor          # AND-ed full-text over active agents
warden search auth --closed          # include archived agents
warden ls --tag backend --tag urgent # only agents carrying every tag
```

`search` matches case-insensitively across each agent's id, name, ticket, type, subject, prompt, branch, tags, and last-pane excerpt. Tag agents at spawn with `warden start --tags backend,urgent` (normalized to lowercase, deduped). The web dashboard carries a live search box, and the Cockpit can group the grid by directory, type, status, or tag.

## History & archive

Every closed session is persisted to the archive (newest-first):

```sh
warden history                    # archived agents, newest first
warden history --since 7d         # 24h / 7d / 2w, a date, or RFC3339
warden history --type development --limit 20
```

The web dashboard surfaces the same data as a 🗄 **Archive** tab with since/type selectors and a text filter.

## Presets

Save a spawn config once and replay it:

```sh
warden preset save backend-dev --type code --tags backend --model opus
warden preset list
warden start --preset backend-dev "implement the rate limiter"   # explicit flags still override
```

## Batch operations (web)

The Cockpit grid has per-tile checkboxes (with Shift-click range select). While ≥1 agent is selected a floating bar offers bulk **Message…**, **Terminate**, and **Delete** (destructive ones need a second click). Actions fan out one agent at a time and report partial success, keeping failures selected for retry.

## Handoff

Delegate a sub-task to *another* agent while the source keeps running (contrast with `rotate`, which retires the source). Phase 1 — writing the handoff package — is driven by the `/warden` skill; the verb delivers it:

```sh
warden handoff --resume-file notes.md --resume-prompt "take the API layer"   # spawn a fresh delegate
warden handoff --to agent-4f2a --resume-file notes.md                        # deliver into a running agent's inbox
```

## Export / import

Serialize session **metadata** for backup or migration between machines. Worktrees, branches, and tmux sessions are **not** serialized and **not** recreated — an imported record just remembers where its (now absent) worktree used to live.

```sh
warden export --all > backup.json     # active + archived records
warden import < backup.json           # idempotent by id (existing ids skipped)
warden import --merge < backup.json   # overwrite colliding records instead
```

## Audit log

The daemon writes an append-only JSON-lines trail to `~/.warden/audit.jsonl` (mode `0600`) — `spawn`, `terminate`, `delete`, `approve`, and pipeline `start`/`cancel`, each with time, actor, target, and a detail map.

```sh
warden audit log                       # recent actions, newest last
warden audit log --tail 100 --action spawn
warden audit log --since 24h --target agent-4f2a --json
```

`warden audit log` reads the file directly, so it works even while the daemon is down.

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

## Prompt templates

Where a preset stores reusable *flags*, a prompt template stores a reusable *prompt body* with `{{VAR}}` placeholders. Save one, then fill it in at spawn time:

```sh
warden prompt-template save bugfix --prompt "Fix the bug in {{FILE}} described by {{TICKET}}"
warden prompt-template list                       # each template + its variables
warden start --prompt-template bugfix --set FILE=server.go --set TICKET=WARD-42
```

Variables are auto-derived from the body. Every declared variable must be supplied (a typo'd `--set` is rejected), and an explicit positional prompt still wins. Templates live in `~/.warden/prompt-templates.yaml`, beside `presets.yaml`. `--prompt-template` is free-form only (no `--type`).

## The library umbrella

`warden library` (alias `wd lib`) is one umbrella over all three reusable launch-config kinds — saved spawn presets, saved prompt templates, **and** the built-in pipeline templates:

```sh
warden library list                            # presets + prompt templates + pipeline templates in labeled sections
warden library save-preset backend-dev --type code --model opus      # delegates to `preset save`
warden library save-prompt bugfix --prompt "Fix {{FILE}}"            # delegates to `prompt-template save`
```

It adds no new storage — it reuses the preset store, the prompt-template store, and the embedded template catalog (also over MCP as `library_list`, which returns `{presets, prompt_templates, templates}`), and `warden preset` / `warden prompt-template` / `warden pipeline list-templates` keep working unchanged.

## Batch operations (web)

The Cockpit grid has per-tile checkboxes (with Shift-click range select). While ≥1 agent is selected a floating bar offers bulk **Message…**, **Terminate**, and **Delete** (destructive ones need a second click). Actions fan out one agent at a time and report partial success, keeping failures selected for retry.

## Handoff (`warden handoff`)

`warden handoff` is the single verb for passing work to another agent, with three modes. Phase 1 — writing the handoff package — is driven by the `/warden` skill; the verb delivers it:

```sh
warden handoff --resume-file notes.md --resume-prompt "take the API layer"   # new delegate (own worktree); source keeps running
warden handoff --to agent-4f2a --resume-file notes.md --resume-prompt "…"    # deliver into a running agent's inbox; source keeps running
warden handoff --retire --confirm --resume-file notes.md --resume-prompt "…" # retire self into a same-worktree successor
```

The first two modes **keep the source running**. `--retire` (requires `--confirm`) is the **self-succession** mode — it spawns a successor in the calling agent's own worktree and reaps the caller, exactly what the `warden rotate` alias runs (see [Self-rotation](/warden/guides/rotation-digests/#self-rotation-warden-handoff---retire-alias-warden-rotate)). `--retire` and `--to` are mutually exclusive.

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

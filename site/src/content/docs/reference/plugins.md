---
title: Plugins
description: Extend warden with custom task types and lifecycle hooks via external JSON-over-stdio plugins.
---

The plugin system lets you extend warden with **custom agent task types** and
**lifecycle hooks** without forking — a thin, **default-off**, fail-open extension
seam. A plugin is an external executable registered in config and invoked over a
documented, versioned **JSON-over-stdio protocol** (request on stdin, response on
stdout, hard timeout), deliberately mirroring warden's existing PreToolUse guard
hooks.

:::caution[Off by default]
Plugins run external code. Enable with `plugins: true` in `~/.warden/config.yaml`
only for plugins you trust.
:::

## What a plugin can do

- **Lifecycle hooks** — subscribe to `pre-spawn`, `post-spawn`, `pre-commit`,
  `post-commit`, `pre-check`, `post-check` (plus a reserved `pre-teardown`). Warden
  invokes the plugin at those points with the agent's session metadata and an event
  payload. Hooks are **advisory and fail-open**: a missing, slow, non-zero-exit, or
  malformed plugin is logged and skipped, and one failing plugin never aborts the
  others. A `pre-` hook cannot veto the action — they observe, they don't gate.
- **Custom task types** — declare new `--type` names, each with its own worktree
  isolation policy. Names that collide with a built-in type or another plugin are
  rejected at config load.

## Registering plugins

Set the `plugins` gate and a `plugin_registry` list in `~/.warden/config.yaml`:

```yaml
plugins: true
plugin_registry:
  - name: notify-commit
    path: /usr/local/bin/warden-notify-commit
    events: [post-commit]
    task_types: []
```

Inspect what's loaded:

```sh
warden plugin list   # paths, custom task types (+ isolation), subscribed events, config errors
```

A worked example (a post-commit notifier) lives under `examples/plugins/` in the
repository.

---
title: Quickstart
description: Spawn your first agent in prompt mode, then watch, talk to, and tear it down — the whole loop.
---

:::tip[New here? Take the guided tour]
Run `warden tutorial` for an interactive walkthrough of the whole loop (spawn →
watch → commit → tear down). Until you've taken or skipped it, warden prints a
one-line nudge. `warden tutorial --skip` dismisses it; `--reset` brings it back.
:::

The fastest path is **prompt mode**: give a plain-English task and let warden handle the rest. No repo, no flags.

```sh
warden start "review the auth module for security issues"
# spawned agent-a1b2 (classifying…) — attach with `warden agent attach agent-a1b2`
```

What just happened:

- A new agent got an ID like `agent-a1b2` and is launched in the directory you ran the command from (your current shell's cwd) — no per-agent directory is created.
- It's running `claude` on your prompt inside a tmux window.
- The type shows as `classifying…` for a moment, then the daemon labels it (e.g. `analysis`) automatically.

Now watch and interact:

```sh
warden ls                         # see it in the list
warden status agent-a1b2          # full detail + event history
warden agent tail agent-a1b2            # recent terminal output
warden send agent-a1b2 "also check the session cookie handling"
warden agent attach agent-a1b2          # drop into its terminal (Ctrl-b d to detach)
warden agent done agent-a1b2            # tear it down when finished
```

That's the whole loop. Everything else is variations on it.

## The lifecycle of an agent

```
start ──▶ spawning ──▶ working ⇄ idle ⇄ waiting_for_input ──▶ done
                                                      └─▶ errored / orphaned
```

Status is driven by Claude Code lifecycle hooks plus the daemon's poller. You don't set it manually.

## Typical workflows

**Ad-hoc investigation (prompt mode):**

```sh
warden start "find and fix the flaky test in the payments suite"
warden ls
warden agent tail <id>
warden send <id> "skip the integration tests for now"
warden agent done <id>
```

**Ticketed development (managed worktree):**

```sh
warden start PROJ-350 --type development     # worktree + branch
warden status PROJ-350
warden agent attach PROJ-350                       # jump in when needed
warden agent done PROJ-350                          # guarded teardown
```

**Reviewing a PR:**

```sh
warden start --type pr-review --pr 1234
warden agent tail prreview-...
warden agent done prreview-...
```

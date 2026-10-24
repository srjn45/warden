---
title: "Autopilot: from a goal to merged PRs, with a pause switch"
description: How warden's Autopilot turns a plan into a manager agent, isolated worker agents, CI-gated PRs and landed branches — and the guardrails that keep unattended runs from touching main.
date: 2026-10-23
authors: srjn45
excerpt: You describe a goal in a plan and start it. A manager breaks it into tasks, workers each take one in their own worktree, and the daemon gates and lands their PRs into an integration branch — never main. Here is the whole loop and the safety rails around it.
tags:
  - autopilot
  - orchestration
  - safety
---

Autopilot is warden's goal-directed, long-running mode. The idea is simple: you
write down a goal, start it, and warden runs the fleet that gets there.

```sh
warden plan run <plan-id> --mode autopilot
```

There is no separate enable step.

## The loop

1. **Plan.** The plan holds your goal and its tasks.
2. **Run.** warden starts an Autopilot executor and a **manager** agent. The
   manager breaks the goal into tasks and delegates each one to a **worker** agent
   running in its own isolated git worktree.
3. **Land.** A worker opens a PR; the daemon gates it through CI and lands it into
   the run's **integration branch**. The workers don't merge their own work.

If the manager gets stuck, a guardian loop heals it. If a backend is
rate-limited, Autopilot escalates down a cost-tier ladder to the next eligible
one, drawing from your [backend registry](/warden/guides/backend-registry/).

## Guardrails

Unattended operation is risky, so the defaults lean conservative:

- **Never `main`.** Work lands in `autopilot/<plan-name>` by default, and you
  review that branch.
- **A pause switch.** `warden plan pause <plan-id>` stops new spawns and landings
  whenever you need it to.
- **An audit log.** Every Autopilot action — manager spawn, worker spawn, land,
  heal — is recorded.

## Try it

Start with a small plan and read the [Autopilot guide](/warden/guides/autopilot/):
it covers preflight warnings, watching status, landing branches and what is kept
when you stop a run.

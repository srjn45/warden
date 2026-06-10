---
title: Self-rotation & digests
description: Retire a context-heavy agent into a fresh successor, and summarize what an agent accomplished.
---

## Self-rotation (`warden rotate`)

Run **inside an agent session** to retire a long-lived, context-heavy agent and hand off to a fresh successor in the same workdir/worktree. Phase 1 (writing the handoff file + resume prompt) is driven by the `/warden` skill; on confirmation the agent spawns its successor and reaps itself.

```sh
warden rotate --confirm \
  --resume-file ./HANDOFF.md \
  --resume-prompt "Continue the migration from where the notes leave off"
```

- **Spawn-before-reap** is fail-safe — if the successor fails to spawn, the current agent keeps running.
- Rotation **reuses the worktree by cwd and never removes it** (a compile-time invariant: the rotator interface omits worktree removal).

## Completion digest (`warden digest`)

Summarize what an agent accomplished — files touched, branch, number of turns, and a short narrative (best-effort, via `claude -p`). Also available as a web **Digest** panel and, in the cockpit, the `d` key (opens a scrollable digest for the selected agent).

```sh
warden digest PROJ-350
warden digest PROJ-350 --json
```

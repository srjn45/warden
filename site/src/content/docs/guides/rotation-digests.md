---
title: Self-rotation & digests
description: Retire a context-heavy agent into a fresh successor, and summarize what an agent accomplished.
---

## Self-rotation (`warden handoff --retire`, alias `warden rotate`)

Run **inside an agent session** to retire a long-lived, context-heavy agent and hand off to a fresh successor in the same workdir/worktree. This is the **retire** mode of the unified [`handoff`](/warden/guides/fleet-operations/#handoff-warden-handoff) verb; `warden rotate` is an exact alias. Phase 1 (writing the handoff file + resume prompt) is driven by the `/warden` skill; on confirmation the agent spawns its successor and reaps itself.

```sh
warden handoff --retire --confirm \
  --resume-file "${TMPDIR:-/tmp}/warden-rotate-handoff-$WARDEN_SESSION_ID.md" \
  --resume-prompt "Continue the migration from where the notes leave off"
# `warden rotate --confirm …` is an exact alias.
```

- The handoff file uses a **unique, per-agent temp path** (`$TMPDIR` keyed on `$WARDEN_SESSION_ID`), so concurrent agents rotating at the same time never overwrite each other's notes. The successor deletes it once it has read it, and `/tmp` self-clears as a backstop.
- **Spawn-before-reap** is fail-safe — if the successor fails to spawn, the current agent keeps running.
- Rotation **reuses the worktree by cwd and never removes it** (a compile-time invariant: the rotator interface omits worktree removal).
- `--retire` is **mutually exclusive** with `--to` — retire reaps the caller, while the delegate / `--to` modes keep it running.

:::tip[Want to keep the conversation, not just the task?]
Rotate/handoff carry the **task** into a fresh agent but **drop the conversation**.
To branch an agent's **recorded session** (its conversation/reasoning) sideways into
a new agent while the source keeps running, use [`warden fork`](/warden/guides/backend-superpowers/#wd-fork--branch-an-agents-session-into-a-new-agent) — a Codex-only superpower.
:::

## Completion digest (`warden digest`)

Summarize what an agent accomplished — files touched, branch, number of turns, and a short narrative (best-effort, via `claude -p`). Also available as a web **Digest** panel and, in the cockpit, the `d` key (opens a scrollable digest for the selected agent).

```sh
warden digest PROJ-350
warden digest PROJ-350 --json
```

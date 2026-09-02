---
title: Snapshots & rollback
description: Checkpoint an agent's worktree changes plus its session transcript, and roll back later.
---

`warden workspace snapshot` captures a checkpoint of an agent's **worktree changes** — its
uncommitted diff, as a stash — **and its session transcript**, so you can roll back
to a known-good point after a risky change. Gated by the `snapshots` config setting
(default on); the daemon owns a JSON snapshot store under `<data_dir>/snapshots/`.

```sh
warden workspace snapshot create [name] -m "before risky refactor"   # capture a checkpoint
warden workspace snapshot list [name] [--all]                        # list checkpoints
warden workspace snapshot restore <id> [--force]                     # re-apply onto its worktree
```

## How restore behaves

Restore re-applies the captured stash onto the snapshot's recorded worktree. Its
rails keep it safe:

- It **refuses a dirty/conflicting tree** rather than clobbering uncommitted work
  (use `--force` to override deliberately).
- It does **not** reset HEAD or drop the snapshot, so a failed/partial apply leaves
  the snapshot usable for a retry.

The same three operations are available as the `snapshot_create`, `snapshot_list`,
and `snapshot_restore` MCP tools.

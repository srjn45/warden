# agentctl self-rotate — design

**Date:** 2026-06-05
**Status:** Approved for planning
**Feature:** `/agentctl rotate` — a long-running agent hands its work off to a fresh successor in the same workspace, then retires itself, bounding its context (and reclaiming its heap) without losing the task.

## Motivation

The freeze investigation ([memory: agentctl-freeze-investigation]) concluded the Mac UI
freezes are most likely **memory-compressor thrash** driven by long-lived **1M-context**
Claude agents — worst when several hit auto-compaction at once. The shipped mitigation was a
*preventive* nudge: decompose big work into short-lived pipeline stages so each agent's context
stays bounded and closing a stage returns its heap to the OS.

This feature adds the *reactive* counterpart for work that is **already running as one
long-lived agent** and has grown too big to want to keep going. Instead of letting the agent
balloon toward an uncontrolled compaction, the human (working with that agent) triggers a
**deliberate, human-timed handoff** to a fresh agent that resumes the work with a small context.

It also formalizes a ritual the user already performs by hand: *"ask the agent to write all
needed context to a scratch file, get a resume prompt, paste it into a new agent."* `rotate`
turns that into one reviewed command.

The token/context **gauge** that was previously shelved ([memory: agentctl-token-cost-shelved])
is **not** part of this feature. Rotation here is human-triggered, not gauge-triggered. A future
gauge could *suggest* rotating, but that is out of scope (see Future work).

## Why this is the safe form of rotation

Earlier in brainstorming we rejected **auto-rotation** (a machine deciding when to rotate a
running agent). Its drawbacks all stem from a machine guessing a safe cut-point: the handoff
summary is itself a heavy context read (the same memory spike we want to avoid), the cut can land
mid-edit / mid-tool-call leaving a half-mutated tree, and concurrent auto-rotations recreate the
very multi-spike freeze.

**Self-rotation triggered by the human removes all of those:** the human has already judged the
agent is at a sane moment, explicitly accepts the lossy handoff, and triggers exactly one agent at
a time — so it cannot cause concurrent-compaction thrash.

## Goals

- `/agentctl rotate` (run **inside** an agent's own session) hands the current agent's work to a
  fresh successor in the **same working directory / worktree**, then retires the current agent.
- The successor inherits the original launch configuration faithfully (cwd, worktree, supervised
  flag) so it lands in the identical environment, including any uncommitted work.
- A **review gate**: the agent writes the handoff and shows it to the human, who can edit it and
  must explicitly confirm before anything irreversible happens.
- Reclaim the retired agent's memory (the freeze motivation) by actually killing its process.
- Nothing is *truly* lost: the retired agent's transcript and the handoff file persist on disk.

## Non-goals (v1)

- **Remote rotation** — `agentctl rotate <id>` targeting *another* agent from a shell/orchestrator.
  That needs prompt-injection-into-target + completion polling and is a separate, harder mechanism.
  Deferred to a fast follow.
- **Auto-rotation** — any machine-decided trigger. Rejected above.
- **A context/token gauge** — measuring how full an agent is. Separate, previously shelved.
- **Worktree migration** — the successor reuses the *existing* worktree by cwd; we never create a
  new worktree or move work between worktrees during rotate.
- **Preserving conversational history** — the handoff is a fresh-context summary, not a transcript
  replay. Lossy by design; the human reviews it precisely because of that.

## User flow

Run from within the agent you want to rotate (the human is attached to it):

**Phase 1 — prepare (the agent, driven by the skill):**
1. The agent resolves its own id from `$AGENTCTL_SESSION_ID`.
2. The agent writes a **handoff file** to its working directory capturing what a fresh agent needs
   to continue (see "Handoff file" below). Because the agent is summarizing *itself*, fidelity is
   as high as it gets — no round-trip, no waiting.
3. The agent composes a **resume prompt** — the initial prompt the successor will receive.
4. The agent presents both (handoff file path + resume prompt) to the human inline and stops.

**Review (the human):** reads the handoff and resume prompt; may edit the handoff file directly on
disk; decides whether to proceed.

**Phase 2 — commit (`agentctl rotate --confirm ...`):** on the human's go-ahead, the agent runs the
confirm step, which deterministically:
1. Reads `$AGENTCTL_SESSION_ID`; looks up the old session's persisted metadata.
2. Validates the handoff file exists and is non-empty.
3. **Spawns the successor** in prompt mode with `Cwd = old.Workdir`, `Supervised = old.Supervised`,
   and a prompt that points the successor at the handoff file plus the resume prompt text.
4. Prints, **loudly**: the new agent id, the handoff file path, and a reminder that the old
   transcript remains on disk for recovery.
5. **Reaps the old agent**: `Terminate` (kill its tmux session + mark it done) — and explicitly
   **does not** remove the worktree.

## Architecture (Approach B: skill + thin CLI verb)

Split by altitude — the agent does the one thing only it can do (summarize its own live context);
the CLI verb does everything that must be deterministic and must not go wrong.

### Skill (`skills/agentctl/SKILL.md`)
A new **`rotate`** section instructing the agent, when the user invokes `/agentctl rotate`, to:
- write the handoff file (with the prescribed structure),
- compose the resume prompt,
- present both and wait for the human,
- on confirmation, invoke `agentctl rotate --confirm --resume-file <path> --resume-prompt <text>`.

The skill owns the *creative* content; it must **not** try to re-derive worktree/supervised/cwd or
order the reap — that is the verb's job.

### CLI verb (`internal/cli/rotate.go`)
Mirrors the `approvals` / `digest` precedent: a thin orchestrator over **existing** client methods,
with pure, unit-testable helpers. **No new daemon endpoint.**

- Resolves self id from `$AGENTCTL_SESSION_ID` (error clearly if unset → "rotate must be run inside
  an agentctl agent session").
- Fetches the old session (existing session-get client call).
- Pure helper: builds the successor `SpawnParams` from the old session
  (`Cwd = old.Workdir`, `Supervised = old.Supervised`, `Type = prompt`, `Prompt = composed`). This
  helper is the heart of "faithful inheritance" and is unit-tested in isolation.
- Pure helper: composes the successor prompt from the resume-prompt text + handoff file path.
- Calls `client.Spawn(successorParams)`.
- Prints the loud summary.
- Calls `client.Terminate(oldID)`. **Does not call `RemoveWorktree`.**

The verb is invoked **only at Phase 2**, and **only with `--confirm`** — there is no Phase-1 CLI
call (Phase 1 is entirely skill-driven: the agent writes the handoff and presents it). `--confirm`
is the gate's enforcement point: the verb refuses to do anything irreversible (spawn + reap)
without it, so the human's explicit go-ahead is structurally required.

### What we deliberately reuse, not rebuild
- **Self-identification:** `$AGENTCTL_SESSION_ID`, injected at every `tmux new-session`
  (`lifecycle.go:594`). Already present; no change.
- **Successor spawn:** existing `client.Spawn` / `SpawnParams`. Prompt mode with `Cwd` set is
  exactly how prompt-spawned agents already launch in a caller's directory.
- **Reap:** existing `client.Terminate`. Because `Terminate` and `RemoveWorktree` are **separate**
  operations (layered teardown), calling only `Terminate` preserves the worktree.

## Key correctness invariants

1. **Worktree is preserved on reap.** The successor lives in `old.Workdir` (which, for a
   worktree-backed agent, *is* the worktree directory). Reaping the old agent must call `Terminate`
   only — never `RemoveWorktree` — or it would delete the directory the successor is now working in
   and destroy uncommitted work. This is the single most important invariant; it gets an explicit
   test.
2. **Spawn-before-reap ordering.** The successor must be confirmed spawned before the old agent is
   terminated. If `Spawn` fails, the verb aborts **without** reaping — the original agent survives
   so no work is stranded.
3. **Self-teardown is safe despite the caller dying.** `Terminate` is a daemon API call that
   completes server-side. The in-session CLI process invoking it is collateral when its own tmux
   session is killed, but the daemon has already performed (and recorded) the termination, so the
   outcome is well-defined. Ordering the `Terminate` call last means the loud summary has already
   been printed before the session goes away.
4. **Successor does not "own" the worktree.** It is launched in prompt mode with a plain `Cwd`, so
   its `Worktree` field is empty; a later `agentctl rm` of the successor will not attempt to delete
   the inherited worktree either. Worktree lifecycle ownership becoming detached from any single
   session is an accepted consequence (see Open questions).

## Handoff file

- **Location:** under the agent's `Workdir` so the successor (same cwd) can find it; default e.g.
  `./.agentctl/rotate-handoff-<timestamp>.md`. The exact path is passed to the verb via
  `--resume-file`, so the agent may choose another location.
- **Forward-looking, not retrospective.** Distinct from the existing `digest` feature, which
  summarizes what an agent *accomplished*. The handoff must capture what the successor needs to
  *continue*:
  - the original task / goal,
  - current working-tree state (branch, what's committed vs. uncommitted, any running background
    work),
  - key decisions made and approaches already ruled out (so the successor doesn't re-explore them),
  - precise next steps,
  - pointers to the relevant files / locations.
- It is a plain Markdown file the human can edit before confirming.

## Error handling

- `$AGENTCTL_SESSION_ID` unset → clear error; do nothing.
- Session not found in store → clear error; do nothing.
- Handoff file missing/empty at `--confirm` → abort before spawn; nothing irreversible.
- `Spawn` fails → abort; **do not reap**; surface the error; original agent intact.
- `Terminate` fails after a successful spawn → the successor is already live and the new id was
  printed; surface a warning that the old agent may still be running and can be removed manually
  with `agentctl rm <old-id>`. (Successor running is the safe-failure direction.)

## Testing strategy

Follows the `approvals` / `digest` pattern — pure helpers carry the logic and are unit-tested; the
thin orchestration layer is exercised with a fake client.
- Pure helper: build successor `SpawnParams` from a `store.Session` — asserts cwd, supervised flag,
  and prompt-mode inheritance; asserts worktree is reused via cwd, not recreated.
- Pure helper: compose successor prompt from resume text + handoff path.
- Orchestration with a fake client:
  - happy path → `Spawn` called with inherited params, then `Terminate(oldID)`, and
    **`RemoveWorktree` never called** (the invariant-1 test).
  - `Spawn` error → `Terminate` not called.
  - missing `$AGENTCTL_SESSION_ID` / missing handoff file → aborts pre-spawn.
- Manual live smoke (left for the user): rotate a real supervised agent in a worktree, confirm the
  successor lands in the same worktree with uncommitted work intact and the old session is gone.

## Future work (explicitly out of scope here)

- **Remote rotation** `agentctl rotate <id>`: inject the handoff-writing prompt into a target
  agent, poll for completion (e.g. handoff file appears), then spawn + reap. Reuses this feature's
  successor-spawn + reap plumbing.
- **Gauge-suggested rotation:** an active extension of stuck-detection / a context gauge that
  *suggests* `/agentctl rotate` when an agent's latest-turn input tokens approach the window. Would
  reuse this mechanism as its action. Still human-confirmed.
- **MCP / web / TUI surfaces** for rotate. v1 is CLI + skill only.

## References

- Freeze investigation & pipeline-decomposition mitigation — memory `agentctl-freeze-investigation`.
- Shelved token gauge & the "only worth it as an active extension" note — memory
  `agentctl-token-cost-shelved`.
- Precedent for skill + thin CLI verb with pure helpers, no daemon change — `approvals`/`approve`
  (`internal/cli/approvals.go`) and `digest` (`internal/cli/digest.go`).
- Layered teardown (`Terminate` separate from `RemoveWorktree`) — `internal/lifecycle/lifecycle.go`.

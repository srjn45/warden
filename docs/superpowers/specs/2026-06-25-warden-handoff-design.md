# warden handoff — design

**Date:** 2026-06-25
**Status:** Shipped
**Feature:** `warden handoff` — an agent (or the operator) delegates a sub-task to a **different** agent (a brand-new one, or an existing one via `--to`), while the source agent keeps running. The cross-agent counterpart to `rotate`.

## Motivation

`warden rotate` (see `2026-06-05-agentctl-self-rotate-design.md`) ships *same-workspace
succession*: a long-lived agent hands its work to a fresh successor **in the same worktree**
and **retires itself**. That spec's *Future work* explicitly defers "Remote rotation" —
handing context to a *different* agent — as a separate, harder mechanism. `handoff` is that
deferred half, reframed as **delegation** rather than succession.

The need: an agent mid-task wants a *self-contained sub-task* carried out by another agent
without giving up its own context — fork off a parallel piece, or brief an already-running
agent on related work. `rotate` can't express this (it retires the caller and reuses the one
worktree); pipelines are heavier (a pre-authored DAG). `handoff` is the lightweight,
ad-hoc, human-reviewed primitive in between.

## Goals

- `warden handoff` delivers a structured context package to a **different** agent.
- **Two modes:** default spawns a **brand-new** delegate in its **own isolated worktree**;
  `--to <id>` delivers into an **existing** agent's inbox (waking it).
- The **source agent keeps running** — handoff is delegation, never succession.
- A skill-driven **human review gate** before delivery (same shape as rotate's Phase 1).
- Thin CLI verb over **existing** client methods; **no new daemon endpoint**.

## Non-goals

- **Retiring the source** — that is `rotate`. (A future `--retire` flag could fold the two,
  but v1 keeps them distinct.)
- **Migrating a worktree / uncommitted work** between agents — the new delegate gets its own
  fresh worktree; cross-agent code flow is pipelines' job (branch chaining).
- **Preserving conversational history** — the handoff is a fresh-context summary, reviewed by
  the human, exactly as in rotate.

## Key design point: inline the handoff *content*, not a path

`rotate`'s successor shares the source's cwd, so `composeSuccessorPrompt` points it at the
temp handoff file *by path*. A handoff recipient runs in a **different worktree/process** and
cannot read that file. So `handoff` **reads the file and inlines its content** into the
delegate's initial prompt (new mode) or the delivered message (`--to` mode). The handoff file
is purely a human-review artifact on the source side.

## Architecture (skill + thin CLI verb — mirrors rotate)

### Skill (`skills/warden/SKILL.md`)
A "Delegating a sub-task (handoff)" section instructs the agent, on `/warden handoff`, to:
write a self-contained handoff file to a unique temp path, compose a resume prompt, present
both and wait for the human, then on go-ahead invoke `warden handoff …`.

### CLI verb (`internal/cli/handoff.go`)
Pure, unit-tested helpers + thin orchestration over a minimal client interface:

- `readHandoffContent(path)` — reuses `validateHandoff` (rotate.go), then reads the body.
- `composeDelegatePrompt(resumePrompt, content)` — new delegate's initial prompt.
- `composeHandoffMessage(resumePrompt, content, fromID)` — `--to` message body (with sender
  provenance).
- `buildDelegateParams(repo, type, name, branch, prompt, force)` — a **managed** spawn (Type set,
  `Worktree`/`InRepo` false) so a write-agent type lands in its own isolated worktree; `force`
  passes through to spawn past the memory-pressure gate (`--force`, mirrors `start`).
- `resolveHandoffRepo(repoFlag, self)` — `--repo` > source session repo > cwd (mirrors `start`).
- `handoffClient { Get; Spawn; MsgSend }` — **omits Terminate** so "source is never reaped" is
  a compile-time guarantee (the same trick rotate uses to omit `RemoveWorktree`).
- `runHandoffNew` → `Spawn`; `runHandoffTo` → `Get` (verify target) then `MsgSend`.

### Reused, not rebuilt
`validateHandoff` (rotate.go), `resolveSender`/`envID` (messages.go), `client.Spawn` /
`SpawnParams`, `client.MsgSend`, `client.Get`. No daemon change.

## Correctness invariants

1. **Source is never terminated** — the interface omits `Terminate`.
2. **`--to` verifies the target exists before sending** — fail fast, no half-delivery.
3. **A new delegate gets its own worktree** — managed spawn; it never shares the source's tree
   (no `Cwd` inheritance).
4. **Content is inlined** — the recipient needs no access to the source's filesystem.

## Error handling

- Missing `--resume-file`/`--resume-prompt` → error before any action.
- Missing/empty handoff file → error before any action (via `validateHandoff`).
- `--to` target not found → error before sending.
- New-mode spawn blocked by the memory-pressure gate (too many live agents) →
  surfaced as the daemon's gate error; re-run with `--force` to spawn anyway
  (threads `SpawnParams.Force`, same as `warden start --force`; ignored with `--to`).
- `Spawn`/`MsgSend` failure → surfaced; source untouched in all cases.

## Testing

Mirrors `rotate_test.go`: pure-helper tests (`readHandoffContent`, the two `compose*`,
`buildDelegateParams`, `resolveHandoffRepo`) plus a `fakeHandoffClient` exercising new-mode
happy path, new-mode spawn error, `--to` happy path (Get-then-MsgSend, woke surfaced), and
`--to` missing target (no send). Manual smoke: spawn a delegate from a repo and confirm it
lands in its own worktree with the inlined context; `--to` a running agent and confirm its
inbox shows the handoff and an idle target is woken.

## References

- Self-rotate design & its deferred "Remote rotation" — `2026-06-05-agentctl-self-rotate-design.md`.
- Skill + thin CLI verb precedent — `internal/cli/rotate.go`, `internal/cli/approvals.go`.
- Mailbox delivery + wake — `internal/cli/messages.go`, `client.MsgSend`.

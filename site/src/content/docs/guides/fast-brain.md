---
title: Fast-Brain decisions
description: How warden makes bounded internal model decisions without making automation less safe.
---

Fast-Brain is warden's internal decision gateway. It handles small, bounded
judgment calls—such as reading an unfamiliar approval menu, triaging a crash, or
classifying a failed CI check—while the daemon keeps ownership of the actual
action. It is not an agent and it cannot edit a repository or run arbitrary
commands.

Every decision has a kind, a fast or thinking tier, a deadline, and a
deterministic fallback. Prompt material is sanitized and bounded before it is
sent to a runner; decision telemetry records operational facts rather than the
prompt itself. You can inspect telemetry, recent redacted traces, and pause or
resume a decision kind with [`warden inspect fastbrain`](/warden/reference/cli/),
whose generated command reference remains the source for exact syntax.

## Fail open is the safety default

Fast-Brain is advisory. A missing runner, timeout, cancellation, malformed
response, or insufficient confidence never turns into a guessed action. The
calling feature either leaves work for a person or uses its existing mechanical
fallback. This is deliberate: losing a convenience decision must not make a
fleet unavailable or silently broaden its authority.

The same rule applies to admission control. When capacity is unavailable,
nonessential work is explicitly deferred rather than starting an unbounded
number of headless AI CLI processes.

## Approval arbiter and learned prompts

In supervised mode, static approval rules run first. If they have no answer,
the optional Fast-Brain arbiter can consider a recognized prompt:

- Tool permissions use the fast tier; strategic questions use the thinking tier.
- A response acts only at confidence 0.8 or above. Otherwise the prompt remains
  in the approvals inbox for a person or resolver.
- The destructive-action guard and repeated-prompt circuit breaker run before
  any model call and cannot be overridden by a model.

For a menu that no backend parser recognizes, warden first tries its
known-prompts store, then asks Fast-Brain to read the visible menu. The reading
is verified against the live pane before it can enter the normal approval path.
After a verified successful answer, warden learns only the prompt *shape*—the
backend, templated question, ordered option labels, and which choices are
one-time or standing grants. It does not retain the concrete command, path, or
other request-specific text.

Later matching menus can be read without a model call. Inspect entries with
`warden approval known list`; remove a bad entry with
`warden approval known forget <id>`. The store bounds itself by age and count,
and drops a learned shape that repeatedly fails to clear its menu. See
[Approvals & supervised mode](/warden/guides/approvals-supervised/) for the full approval
flow and configuration.

## Crash triage and bug reports

On an agent crash, warden captures a bounded pane excerpt and diagnoses it as
an internal bug, transient error, environment error, or task failure. The
fallback is deterministic classification. A suspected internal warden bug gets
a sanitized draft staged locally; secrets are stripped and local paths are
normalized.

Nothing is uploaded automatically. Review a draft with
`warden bug-report <id>` (or use the cockpit's bug-review flow). The command
shows the sanitized report and defaults to **No**; only an operator's explicit
approval can create an issue or open a prefilled issue link. Exact command
behavior is documented in the [CLI reference](/warden/reference/cli/#warden-bug-report).

## Stall and CI diagnosis

Autopilot uses deterministic signals first when an agent appears stuck: a rate
limit, pending approval, or an empty pane with no activity each has a mechanical
recovery. When those signals do not decide the case, Fast-Brain can propose a
bounded recovery such as wait, nudge, resolve a prompt, resume a rate limit, or
redeliver the brief. Manager-only diagnoses may additionally recommend restart,
rotate, or call a resolver. Invalid, timed-out, or low-confidence diagnosis
falls back to the mechanical recovery ladder.

For a red CI check, Fast-Brain may classify the log tail as `flaky_or_infra` or
`real`. If it cannot make a valid classification, warden treats the failure as
real—it never dismisses a failing check on uncertainty.

## Admission control

Each Fast-Brain call passes through one daemon-wide admission controller. It
keeps expensive runner processes bounded and gives interactive safety work a
path through a busy fleet:

- Safety decisions (approval arbitration and prompt recognition) have the
  highest priority and reserved capacity.
- Interactive work, including REPL turns and stall diagnosis, runs ahead of
  operational diagnosis and summaries.
- Operational work has bounded queues; best-effort work such as activity
  summaries, naming, and curation is shed rather than queued when capacity is
  reserved for higher-priority work.
- Calls are fairly scheduled across agents, duplicate in-flight requests may
  share one runner, and short-lived cached results avoid repeat work where it
  is safe to do so.

This policy improves responsiveness under load without changing approval
authority: destructive guards, confidence requirements, and fail-open behavior
remain in force at every priority.

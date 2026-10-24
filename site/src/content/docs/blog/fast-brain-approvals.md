---
title: "Approvals that read the prompt, not just match it"
description: How warden answers agent permission prompts safely — static rules and a destructive deny-list first, then an opt-in Fast-Brain model for the prompts the rules can't read, with verification before any key is pressed.
date: 2026-10-24
authors: srjn45
excerpt: Coding-agent CLIs reword their permission prompts between releases. warden answers them with rules and a hard deny-list first, and uses a small fast model only to read what no parser recognizes — it recognizes, it never decides, and it checks the screen before pressing anything.
tags:
  - approvals
  - fast-brain
  - safety
---

When you run many agents, permission prompts pile up. warden's auto-approve is
**off by default**; when you turn it on, a prompt is answered only if it clears
every layer below.

## Decision order

1. **The destructive deny-list always wins.** Delete, `rm -rf`, force, push,
   deploy, `reset --hard` and friends are checked first, and that list is not
   configurable.
2. **Your rules.** The prompt must match an allow rule and no deny rule. Deny wins.
3. **A real yes/no prompt.** warden presses the least-privilege affirmative —
   "yes" rather than "yes, and don't ask again" — unless you opted into sticky grants.

## Where Fast-Brain comes in

Each backend's parser keys on that CLI's prompt wording, and vendors reword
prompts between releases. With `recognize_prompts: true` (the default), a menu that
sits unchanged at the bottom of a pane and that no parser matches is read by the
**Fast-Brain** model, which transcribes the question, the command and the options.

The model only recognizes. It never decides, and it never presses a key:

- Its reading is **discarded** unless every option label is really on screen, in order.
- The destructive guard reads the **pane text**, not the model's summary.
- The answer then goes through the same decision order as any parsed prompt, and a
  key is sent only after a re-capture confirms the cursor is on the chosen option.

A prompt shape that works is remembered (its shape, never the concrete command),
so later occurrences are read from a local store with no model call and re-verified
against the pane each time. `warden approval known list` shows what was learned,
and `forget` removes it.

## Opt-in arbitration

Separately, `use_fast_brain: true` (default `false`) lets a fast model answer
prompts your static rules can't — with a confidence threshold, and failing open to
you rather than guessing. The deny-list and the circuit breaker always run before it.

See the [supervised approvals guide](/warden/guides/approvals-supervised/) for the
config.

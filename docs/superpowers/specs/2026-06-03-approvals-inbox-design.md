# Approvals Inbox — Design

**Date:** 2026-06-03
**Status:** Approved (brainstorm), pending implementation plan

## Problem

When agentctl-spawned agents block on a prompt they enter `waiting_for_input`.
Today the only way to answer is to **attach** to each agent's tmux session
("see + jump"). Running a fleet, the user hits a *mix* of prompts: lots of
routine tool-permission approvals (`Yes / Yes-don't-ask / No`) plus the
occasional real question that genuinely needs reading context and typing a
reply.

"See + jump" already works and is fine for the real questions. The unmet need
is the **routine approvals**: clearing six agents' permission prompts means six
tmux round-trips. The feature must let the user answer the routine prompts from
one place **without jumping** — while safely degrading to today's behavior for
anything that isn't a recognized, routine prompt.

## Non-goals

- No attempt to parse or answer freeform questions, multi-selects, or the
  "tell Claude what to do differently" text field. Those route to attach.
- No change to how prompts are *detected* — the poller already classifies
  `waiting_for_input` by pane matching. This feature parses and answers; it does
  not re-implement detection.

## Scope line (the safety valve)

Inline answering applies **only** to the recognized tool-permission grammar:

```
<action context, e.g. Bash(rm -rf node_modules) / Edit(src/auth/middleware.ts)>
Do you want to proceed?
❯ 1. Yes
  2. Yes, and don't ask again for <tool> in <dir>
  3. No, and tell Claude what to do differently (esc)
```

Anything that does not match this grammar confidently → marked **unrecognized**
→ rendered as "⚠ unrecognized — attach to answer." We never guess at a menu.
Worst case for an unparseable prompt is exactly today's behavior.

## Feature toggle

Gated behind `AGENTCTL_APPROVALS`, mirroring the existing `AGENTCTL_NOTIFY`
pattern in `internal/config/config.go` (off by default; on only for
`1` / `on` / `true`). When off:

- `GET /approvals` returns disabled (the queue is empty / the route reports the
  feature off).
- TUI hides the pinned `⏳ Approvals` row entirely.
- Web falls back to the existing read-only `AttentionQueue` (see + jump), with
  no answer buttons.

This lets us disable the feature cleanly if parsing/injection misbehaves in the
field, with zero impact on existing flows.

## Architecture

A daemon-owned **approval engine** plus two thin UI renderers. All parsing and
injection logic lives in Go, behind HTTP endpoints, so the TUI and web UI are
both just renderers of the same queue.

### New package `internal/approval`

Two pure, independently-testable functions (no I/O):

**`Parse(pane string) (Approval, bool)`** — recognizer over a fresh tmux pane
capture.

- Scans captured lines for the permission-box grammar: the action context, a
  question line, and numbered options matching `^\s*❯?\s*(\d+)\.\s+(.+)$`.
- Returns `Approval{Action, Question, Options []string, SelectedIdx int}` and
  `ok=true` **only** on a confident match.
- Returns `ok=false` for freeform questions, multi-selects, a text-entry field,
  a mid-redraw/partial box, or no box at all.

`Approval` also carries a **fingerprint** (a cheap hash of the option texts)
used for the re-verify guard below.

**`Fingerprint(opts []string) string`** — stable hash of the rendered options,
so the client can prove it is answering the prompt it actually saw.

### Endpoints (`internal/daemon`)

**`GET /approvals`** — the live queue. For every `waiting_for_input` session,
returns its `Parse` result: recognized `{action, question, options,
fingerprint}` or the `unrecognized` flag. Display can reuse the per-tick
`last_pane_excerpt` already in the store to keep load low. SSE (existing wiring)
drives live updates. When the toggle is off, returns empty/disabled.

**`POST /sessions/{id}/approve {option: N, fingerprint: "<hash>"}`** — answer
with a re-verify guard:

1. **Re-capture the pane fresh** and `Parse` it again.
2. Inject **only if** the prompt is still the same — the request's `fingerprint`
   must equal the freshly-parsed `Fingerprint`.
3. If it changed or is gone → `409 Conflict` "prompt changed, reopen." Never
   inject a digit into a different prompt.
4. Injection reuses the existing `tmux send-keys` path: send the chosen digit
   (for Claude's select prompts the number key both selects and confirms). The
   exact keystroke will be verified against a real capture during implementation
   rather than assumed.

Destructive-looking actions (`rm -rf`, `git push --force`, etc.) are **not**
specially gated — one-click is always one-click; the re-verify guard is the only
safeguard. (Decision: the user already approves these in the terminal today.)

## UI

Both surfaces are thin renderers of `GET /approvals`; SSE drives live updates.

### TUI (`internal/tui`)

- A **synthetic pinned row** at the top of the agents list: `⏳ Approvals (N)`.
  Always present, even at `N=0` (greyed), so the list never shifts under the
  cursor. Hidden entirely when the toggle is off.
- Selecting it makes the **detail pane** render the queue instead of a single
  agent's output. Each waiting agent shows `<id> · <action>`, the question, and
  `[1] Yes  [2] …  [3] No`, or `⚠ unrecognized — press a to attach`.
- A **sub-cursor** (↑/↓) moves between waiting agents; number keys answer the
  highlighted one; `a` attaches.
- **Passive live behavior:** SSE repaints the queue and the count live, but
  selection never moves on its own. On a successful answer the agent transitions
  `working` and drops off; the sub-cursor lands on the next. A `409` surfaces in
  the footer status line ("prompt changed — reopened").

### Web (`web/src` + `internal/daemon` static)

- **Extend** the existing `AttentionQueue` / `AttentionBar` rather than add a new
  panel. Each queue item gains parsed option **buttons** (recognized) or an
  **"Attach to answer"** button (unrecognized). Clicking a button calls
  `POST /sessions/{id}/approve`; existing SSE wiring drops the item on
  transition. The `AttentionBar` count becomes actionable.
- With the toggle off, the queue renders read-only as today.

## Data flow

1. Agent blocks → poller classifies `waiting_for_input` → SSE notifies UIs.
2. UI fetches/holds `GET /approvals`; renders recognized options or the
   unrecognized fallback.
3. User clicks/keys an option → `POST /sessions/{id}/approve {option,
   fingerprint}`.
4. Daemon re-captures + re-parses; fingerprint match → inject digit; mismatch →
   `409`.
5. Agent transitions `working` → poller/SSE → item drops off the queue live.

## Error handling

- **Stale prompt:** fingerprint mismatch → `409`, UI reopens/refreshes the item.
- **Unrecognized prompt:** never answerable inline; always routes to attach.
- **Toggle off:** endpoints disabled; UIs fall back to see + jump.
- **Injection failure** (tmux send-keys error): surfaced as a `5xx` / footer
  error; queue item remains.

## Testing

- **`internal/approval`** — table-driven `Parse` tests over captured-pane
  fixtures: recognized Bash/Edit/Write permission prompts (the positives) **and**
  the negatives that must return `ok=false` (freeform question, multi-select,
  mid-redraw, no box). `Fingerprint` stability tests. This suite protects the
  safety valve.
- **Daemon** — `GET /approvals` shape (recognized + unrecognized + toggle-off);
  `POST .../approve` happy path, `409` on fingerprint mismatch, unrecognized
  rejected.
- **TUI** — pinned row present at `N=0` and hidden when toggle off; selecting it
  renders the queue; number key issues the correct `approve` call (fake
  lifecycle); passive behavior — an incoming approval bumps the count without
  moving selection.

## Decisions (locked during brainstorm)

- **Both surfaces** (web + TUI), with shared daemon-owned engine.
- **Inline answer** for the recognized permission grammar only; unrecognized →
  attach (the see + jump we have today).
- **TUI placement:** pinned `⏳ Approvals (N)` row → queue in the detail pane;
  always visible (even at 0); passive live updates.
- **Web placement:** extend existing `AttentionQueue`/`AttentionBar`.
- **Safety:** re-verify fingerprint guard on answer; one-click always (no
  destructive-action gate).
- **Feature toggle:** `AGENTCTL_APPROVALS`, off by default.

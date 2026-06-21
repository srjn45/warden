# Rate-Limit Auto-Resume Hardening — Design

**Date:** 2026-06-21
**Status:** Approved (brainstorm), pending implementation plan

## Problem

warden's rate-limit auto-resume feature detects when an agent has hit a Claude
usage/rate limit, parks the session as `rate_limited`, schedules a resume for when
the limit is expected to reset, and then nudges the agent back to work. Three
defects make the feature fire when it shouldn't, resume in a way that corrupts the
agent's task, and miscompute the reset time:

1. **False positives.** Detection is a case-insensitive substring scan of the *whole*
   tmux scrollback for `rate limit` / `usage limit` / `session limit` /
   `quota exceeded`. Any agent whose own output contains those words — one writing,
   reviewing, or discussing rate-limit code (e.g. this very feature) — is
   misclassified as `rate_limited` mid-work, parked, and "resumed."

2. **Blunt resume.** When the tmux session still exists, resume types the literal
   English string `continue` into the pane as a fresh user turn. This injects a
   spurious instruction, assumes the agent's task is the kind that "continue" makes
   sense for, and pollutes the transcript permanently.

3. **Time parsing.** `ParseRestoreTime` has no date rollover: a reset clock-time
   earlier than the current wall-clock returns `now`, producing an immediate resume
   that finds the limit still active and loops until the *real* reset. It also
   carries redundant am/pm logic (a layout-selection branch *and* a separate
   `detectAmPm` scan) and a zone-less generic fallback that parses in local time with
   no timezone awareness.

This spec hardens all three. **No production code changes are made here — this is a
design document only.**

## Goals

- Detection fires only on Claude's real limit banner, only when that banner is the
  *trailing* state of the pane, and never while the agent is actively streaming.
- Resume defaults to the least-invasive action that un-pauses the agent without
  inventing a user turn; any textual nudge is opt-in and configurable.
- Reset-time parsing always biases *later* (never earlier) than the true reset, so a
  resume never fires into a still-active limit and loops.

## Non-Goals

- Detecting limits from sources other than the agent's tmux pane (e.g. API error
  bodies surfaced elsewhere) — out of scope beyond the existing
  `attemptResume`/`Restore` error-string path.
- Changing the scheduler's timer/persistence model (`SetRateLimit`,
  `ReconstructTimers`, retry-count bookkeeping) — untouched except where noted.
- Localizing/translating the banner. We assume the English Claude Code UI; non-English
  banners are a future enhancement.

## Current Behavior

### Detection — `internal/poller/detect.go`

- `detectRateLimit(pane)` (`detect.go:14-39`) lowercases the **entire** pane and
  returns `true` on the first keyword substring hit anywhere in scrollback
  (`detect.go:16-30`). On a hit it calls `ParseRestoreTime` and returns
  `(true, restoreTime, ok)`.
- `ParseRestoreTime(pane)` (`detect.go:49-132`):
  - Pattern 1 (`detect.go:52-94`): `resets\s+(\d{1,2}:\d{2})(?:am|pm)?\s*\(([^)]+)\)`.
    Loads the captured IANA zone, then **selects a layout by scanning the whole pane**
    for `pm`/`am` (`detect.go:65-72`) *and* separately appends `detectAmPm(pane,
    timeStr)` to the time string (`detect.go:76`) — two independent am/pm
    determinations for one value. Builds the result from *today's* date in the zone
    (`detect.go:82-85`). If the result is before `now`, **returns `now`**
    (`detect.go:89-91`) → immediate retry.
  - Pattern 2 (`detect.go:97-129`): generic `(?:at|again at)\s+(\d{1,2}:\d{2})\s*(am|pm|AM|PM)?`.
    Parses in **local tz** with no zone (`detect.go:111-120`). Same "before now →
    return now" behavior (`detect.go:124-126`).
- `detectAmPm(pane, timeStr)` (`detect.go:136-158`): locates `timeStr` in the pane and
  reads up to 10 trailing chars for an `am`/`pm` prefix. Redundant with the Pattern-1
  layout branch.

### Classification — `internal/poller/poller.go`

- `classify()` checks `detectRateLimit(pane)` **first** (`poller.go:25-29`), *before*
  the `esc to interrupt` "working" check (`poller.go:31-33`). So a rate-limit keyword
  anywhere in scrollback wins over an actively streaming agent.
- The poller already records only the trailing pane via `lastLines(pane, 20)`
  (`poller.go:261`, helper at `poller.go:406-412`), but `classify` is handed the
  **full** captured pane (`poller.go:276`), and `detectRateLimit` scans all of it.

### Resume — `internal/daemon/ratelimit.go`

- `OnTransition` (`ratelimit.go:42-66`) fires on `→ rate_limited`, parses
  `sess.LastPaneExcerpt` via `ParseRestoreTime` (`ratelimit.go:50`), schedules at
  `restoreTime + buffer` or falls back to `now + retryInterval` (`ratelimit.go:52-59`).
- `attemptResume` (`ratelimit.go:90-211`): on `ErrAlreadyRunning` (tmux session still
  alive), it sets `resumePrompt := "continue"` and calls
  `r.life.Input(ctx, sess.TmuxSession, resumePrompt)` (`ratelimit.go:115-119`) —
  the literal injected user turn.

## Proposed Design

### Defect 1 — False positives

**Anchor on the real banner, require trailing position, and let "working" win.**

1. **Match Claude's actual limit banner, not loose keywords.** Replace the four loose
   substrings with a banner matcher anchored on the real Claude Code limit message.
   The matcher should require the structural shape of the banner (the limit phrasing
   *together with* its reset clause), not any one word in isolation. The exact banner
   string must be confirmed against a live limit hit (see Open Questions) before the
   regex/anchors are finalized; until then the spec defines the *shape*:
   - a limit phrase (e.g. the Claude Code wording for usage/session limits), AND
   - in the same trailing region, the reset clause that `ParseRestoreTime` already
     keys on (`resets …`).

   Requiring both halves co-located is what distinguishes the banner from an agent
   merely printing the words "rate limit".

2. **Anchor on the *trailing* pane state.** `detectRateLimit` must inspect only the
   tail of the pane — `lastLines(pane, ~6)` — not the whole scrollback. A genuine
   limit banner is the last thing Claude renders before pausing; it is the terminal
   state of the pane. Output that merely *mentions* a limit scrolls up and away and
   must not match. (The exact line count, ~6, is tuned to the banner's height once the
   live string is confirmed.)

3. **Reorder `classify` so "working" short-circuits.** Move the `esc to interrupt`
   check (`poller.go:31-33`) **above** the rate-limit check (`poller.go:25-29`). An
   agent that is actively streaming displays `esc to interrupt`; a real limit banner
   only appears once streaming has stopped, so the two never legitimately coexist. If
   `esc to interrupt` is present, classify `working` and never evaluate rate-limit
   detection this tick. This makes "currently working" an authoritative veto over a
   stray keyword.

The combination is defense-in-depth: even if banner text appeared mid-scrollback
while the agent works, the trailing-only window (point 2) and the working short-circuit
(point 3) each independently prevent misclassification.

### Defect 2 — Blunt resume

**Default to a bare resume keypress; make any textual nudge opt-in and gated.**

1. **Bare keypress, no injected turn.** When the tmux session still exists
   (`ErrAlreadyRunning`), the default resume action is a single keypress that un-pauses
   Claude **without** submitting a user message — no `continue`, no synthetic turn in
   the transcript. (The specific key to un-pause a limit-paused Claude Code pane is
   confirmed alongside the banner string — see Open Questions.)

2. **Make the nudge configurable, default empty.** Introduce a config setting
   `rate_limit_resume_prompt` (string, default `""`). When empty (the default), resume
   is keypress-only as in point 1. When non-empty, its value is the text injected via
   `life.Input` after the limit clears — restoring today's behavior only for users who
   explicitly opt in (and letting them choose wording other than `continue`).

3. **Gate the nudge on anchored detection.** Any textual nudge is sent only when the
   hardened Defect-1 detection still confirms the agent is genuinely limit-paused at
   resume time (banner present in the trailing window). This prevents a
   stale/misfired schedule from typing a prompt into an agent that has since moved on.

Behavior matrix:

| `rate_limit_resume_prompt` | tmux session state | Action |
|---|---|---|
| `""` (default) | alive, limit-paused | bare resume keypress, no transcript turn |
| `""` (default) | alive, banner gone | no-op (agent already moved on); clear schedule |
| non-empty | alive, limit-paused | keypress or `Input(prompt)` per implementation, gated on detection |
| any | not running | `Restore` creates a fresh session (unchanged) |

### Defect 3 — Time parsing

**Roll past times forward, collapse am/pm, bias the zone-less path later.**

1. **Date rollover instead of "return now".** When the computed reset time is before
   `now` in the target zone, **add 24h** (roll to tomorrow) rather than returning `now`
   (replaces `detect.go:89-91` and `detect.go:124-126`). A reset clock-time earlier
   than the current time means the reset is the *next* occurrence of that clock-time,
   not one that already happened today. This removes the immediate-retry loop.

2. **Collapse am/pm into the regex group.** Capture the am/pm marker directly in
   Pattern 1's regex (make `(am|pm)` a real capture group adjacent to the time) and
   choose the layout from that single captured value. Delete the whole-pane `pm`/`am`
   scan (`detect.go:65-72`) and the `detectAmPm` helper (`detect.go:136-158`) and its
   call site (`detect.go:76`). One source of truth for am/pm, parsed from the same
   match as the time.

3. **Bias the zone-less fallback later, never earlier.** Pattern 2 has no timezone, so
   a parsed local time can land before the true (remote-zone) reset. After applying the
   rollover from point 1, the scheduler's existing `+buffer` (`ratelimit.go:55`) is the
   safety margin; the zone-less path must round/bias **up** so the scheduled resume is
   never earlier than the real reset. Concretely: never return a time `< now`, and let
   `+buffer` absorb cross-zone skew. A resume that fires a little late is harmless (the
   limit has cleared); one that fires early loops.

These changes keep `ParseRestoreTime`'s signature `(time.Time, bool)` intact, so
`OnTransition` (`ratelimit.go:50`) and the error-string reparse (`ratelimit.go:184`)
are unaffected.

## Config Additions

One new setting, threaded through the existing config-file model
(`2026-06-17-config-file-design.md`) alongside the current
`rate_limit_retry_interval` / `rate_limit_buffer` / `rate_limit_auto_resume` keys:

| YAML key | Type | Default | Meaning |
|---|---|---|---|
| `rate_limit_resume_prompt` | string | `""` | Text to inject when resuming a limit-paused agent. Empty = bare keypress, no injected user turn (recommended). Non-empty = send this text via `Input` after the limit clears. |

- Add `RateLimitResumePrompt string` to `config.Config` with the matching YAML tag and
  a schema-table entry (key → default `""` → hint), so the reflection drift-guard test
  stays green.
- Thread it into `NewRateLimitScheduler` (a new parameter, mirroring how
  interval/buffer/auto-resume are already passed) and store it on the struct for
  `attemptResume` to consult.
- Example hint comment in the generated file:
  ```yaml
  # Text to send when resuming a rate-limited agent. Empty = bare keypress (no injected
  # user turn). Set a value only if you want an explicit nudge. Values: any string
  rate_limit_resume_prompt: ""
  ```

No other config keys change. Detection-window size (~6 lines) and the banner anchor are
implementation constants, not user config.

## Test Plan

### Detection (`internal/poller/detect_test.go`)

- **True positive:** a captured pane whose trailing lines are the real limit banner
  (with a `resets …` clause) → `detectRateLimit` returns `true` and `ParseRestoreTime`
  succeeds.
- **False positive — agent output:** a pane where `rate limit` / `usage limit` etc.
  appears in mid-scrollback agent output but the trailing lines are normal work →
  `detectRateLimit` returns `false`.
- **False positive — banner scrolled away:** the banner appears earlier in scrollback
  but newer output has pushed it out of the trailing window → `false`.
- **Trailing-window boundary:** banner exactly at the edge of `lastLines(pane, ~6)` →
  matches; one line beyond → does not.

### Classification (`internal/poller/poller_test.go`)

- **Working vetoes limit:** a pane containing *both* `esc to interrupt` and limit-banner
  text → `classify` returns `working` (reordered check short-circuits).
- **Real limit, not working:** banner present, no `esc to interrupt` → `rate_limited`.
- **Regression:** the existing waiting-for-input and stuck→idle cases still classify as
  before.

### Time parsing (`internal/poller/detect_test.go`)

- **Rollover:** `resets 1:30am (Europe/Madrid)` evaluated when local-equivalent is
  already past 1:30am → result is *tomorrow* 1:30am, strictly after `now` (not `now`).
- **Future same-day:** reset later today → returns today's time, after `now`.
- **am/pm from group:** `1:30pm`, `1:30am`, and 24-hour `13:30` each parse to the
  correct hour using only the captured group (no reliance on a whole-pane scan); a pane
  containing an unrelated `am`/`pm` elsewhere does not flip the result.
- **Zone-less fallback biases later:** Pattern-2 inputs never return a time `< now`;
  with `+buffer` the scheduled instant is ≥ the true reset.
- **Signature unchanged:** `(time.Time, bool)` contract holds for parse-failure inputs
  (returns `false`).

### Resume (`internal/daemon/ratelimit_test.go`)

- **Default keypress:** `rate_limit_resume_prompt == ""` and `ErrAlreadyRunning` →
  scheduler issues the bare resume keypress and does **not** call `Input` with any text
  turn; status moves `rate_limited → spawning`; `rate-limit-resumed` event recorded.
- **Configured nudge:** `rate_limit_resume_prompt == "continue"` → `Input` is called
  with that text (opt-in path), gated on detection still confirming the banner.
- **Gating:** schedule fires but the trailing window no longer shows the banner (agent
  moved on) → no nudge/keypress beyond what's safe; schedule cleared.
- **Restore path:** session not running → `Restore` creates a new session (unchanged
  behavior, regression guard).

### Config (`internal/config/config_test.go`)

- `rate_limit_resume_prompt` loads from file, defaults to `""` when absent.
- Drift-guard: the new `Config` field's YAML tag is present in the schema table.

## Open Questions

- **Confirm the exact Claude limit banner string.** The banner anchor (Defect 1) and
  the un-pause keypress (Defect 2) both depend on the precise text and interaction
  Claude Code renders when a usage/session/rate limit is hit. This must be captured
  from a **live limit hit** (or an authoritative fixture) before the regex anchors,
  the `lastLines` window size, and the resume keypress are finalized. Until confirmed,
  the implementation should keep the matcher behind the trailing-window + working-veto
  guards so a wrong guess fails closed (misses a real limit) rather than open
  (misclassifies working agents).

## Out of Scope / Future Enhancements

- Non-English / localized banners.
- Detecting limits from API error payloads outside the tmux pane.
- Per-agent override of `rate_limit_resume_prompt` (global-only for now).
- Surfacing resume actions (keypress vs. nudge) on the metrics endpoint.

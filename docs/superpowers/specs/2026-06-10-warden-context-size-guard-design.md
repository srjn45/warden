# Context-Size Guard — Design

**Date:** 2026-06-10
**Status:** Approved (brainstorming → spec)

## Problem

A long-lived agent's context window fills as it works. Past a point, the model
degrades (lost-in-the-middle), the next compaction spikes harder, and the agent
holds an ever-larger heap. Today warden has no visibility into how full any
agent's context is, and no way to act on it. The user has no signal short of
attaching and running `/context` by hand.

This is the "active extension of stuck-detection" that a prior token-cost
brainstorm (shelved 2026-06-05 as a vanity metric) explicitly left open as the
*only* version worth building: not a passive spend counter, but a gauge that
**drives an action** — warn the user to compact, and at a hard ceiling compact
automatically.

## Goal

Passively track each live agent's **context-window occupancy** and act on two
configurable thresholds:

| State | Condition | Behavior |
|---|---|---|
| `ok` | tokens < warn (default **200k**) | tracked + displayed green |
| `warning` | warn ≤ tokens < crit (200k–400k) | displayed orange + alert "consider /compact" |
| `critical` | tokens ≥ crit (default **400k**) | displayed red + alert + auto-send `/compact` (only when idle) |

All measurement is passive — read from the transcript JSONL. warden never
injects `/context` and never sends keystrokes to a *busy* agent.

## Key decisions (from brainstorming)

- **Gauge = latest-turn context fill, read passively from the transcript.**
  Each assistant turn writes a `message.usage` block; the most recent turn's
  `input_tokens + cache_read_input_tokens + cache_creation_input_tokens` is the
  context-window fill — the same quantity `/context`'s total reports, since that
  total *is* what gets sent to the model as input. Rejected alternatives:
  cumulative-tokens (never drops after compaction → can't gauge compaction need)
  and scraping live `/context` (intrusive keystroke injection into possibly-busy
  agents + brittle TUI-grid parsing, the class of bug that already bit the
  approvals-box parser).
- **Critical action = auto `/compact`, not auto-rotate.** Compaction keeps the
  same session and is the lowest-risk auto-action. Auto-rotate was *deliberately
  rejected* (the handoff summary is itself a context spike + mid-flight cut
  hazard; a human picks the cut-point). Rotate stays the manual `warden rotate`
  path.
- **Compact only when idle/waiting.** Never `/compact` an agent in the `working`
  ("esc to interrupt") state — defer and retry when it next goes idle, so we
  never interrupt active work or land a keystroke mid-tool-flow.
- **Everything ON by default, each piece independently toggleable.** Disable the
  whole feature, just the warning alert, or just the auto-compact action,
  independently, via env flags (all default on; off with `0`/`off`/`false`).

The thresholds 200k/400k are reachable because spawned agents run the
1M-context model (`opus[1m]`).

## Components & boundaries

### a. `internal/ctxtokens` (new, pure)

Single purpose: turn a transcript JSONL into the live context fill + a state.

- `LatestContextTokens(r io.Reader) (tokens int, ok bool)` — scans the JSONL for
  the **last** `assistant` record carrying a `message.usage` block and returns
  `input_tokens + cache_read_input_tokens + cache_creation_input_tokens`.
  `ok=false` when the transcript has no model turn yet (a just-spawned agent →
  not near threshold). Malformed lines skipped (not fatal); same 16 MiB scanner
  cap convention as `internal/digest/parse.go`.
- `Classify(tokens, warn, crit int) State` — pure mapping to
  `ok | warning | critical`. Table-tested.

Kept separate from `digest` (which is on-demand, parses different facts, and
re-reads the whole file): the poller needs a cheap, focused per-tick reader.

### b. `store.Session` (new fields)

```go
ContextTokens    int       `json:"context_tokens"`     // latest context fill, 0 if unknown
ContextState     string    `json:"context_state"`      // "" | ok | warning | critical
ContextCheckedAt time.Time `json:"context_checked_at"`
LastCompactAt    time.Time `json:"last_compact_at"`     // re-fire / cooldown guard
```

- New store method `UpdateContext(ctx, id, tokens int, state string)` persists
  the gauge each check.
- An `Event` is appended on a state **transition** (e.g. `context: ok→warning
  (210k)`), not every check.
- `LastCompactAt` set when a `/compact` is sent; used as the cooldown guard.

### c. `internal/poller` (extend)

Each tick, for **live, non-terminal** agents, throttled to ~every 20s (reuse the
existing `SummarizeAfter`-style cadence — transcripts grow, scanning every tick ×
every agent is wasteful):

1. Resolve the transcript path (`lifecycle.TranscriptPath`), read tokens via
   `ctxtokens.LatestContextTokens`. Skip on `ok=false`.
2. `state = ctxtokens.Classify(tokens, warn, crit)`. Persist via `UpdateContext`.
3. On an **edge transition** (state changed from the stored value):
   - → `warning`: fire the warning alert once (gated by `WARDEN_TOKEN_WARN_ALERT`).
   - → `critical`: fire the critical alert; then if the agent's status is
     `idle`/`waiting_for_input` **and** `WARDEN_TOKEN_AUTO_COMPACT` is on **and**
     the cooldown has elapsed, send `/compact` and stamp `LastCompactAt`. If the
     agent is `working`, defer — retry the compact on a later tick when it goes
     idle (the state is still `critical`, so the "pending compact" intent
     persists; we just gate the *send* on idleness, not the alert).

Edge-triggering: alerts fire once per crossing. After a successful `/compact`,
tokens drop → state returns toward `ok`; a later climb re-crosses and re-fires.
The cooldown (e.g. 2 min, var-overridable in tests) prevents re-sending
`/compact` while a just-sent compaction's effect hasn't yet shown up in the
transcript.

New poller fields: `tokenWarn`, `tokenCrit int`; `warnAlert`, `autoCompact
bool`; an `OnContextAlert func(sess, state, tokens)` hook (daemon wires to the
notifier) and a `Compact func(ctx, sess) error` closure (daemon wires to
`lifecycle.Input(ctx, sess.ID, "/compact")`). Throttle state (`lastCtxCheck
map[string]time.Time`) pruned alongside the existing `lastSummary` map.

### d. Action send

Reuse `lifecycle.Input(ctx, sess.ID, "/compact")` — the existing
bracketed-paste-then-separate-Enter path (lines ~926–959). `/compact` is a slash
command; bracketed paste + a settled Enter submits it cleanly without the
fuse-two-keystrokes hazard.

### e. Daemon wiring

`daemon` reads the new config fields, constructs the poller with thresholds +
flags, and provides the `OnContextAlert`/`Compact` closures. The notifier is the
existing `internal/notify` package. No new endpoints required for the core loop;
the gauge fields ride along on the existing session JSON already served to web
and `warden ls`.

## Config — independent flags, all ON by default

Added to `config.Config` + `Load()`, following the `WARDEN_<name>` (legacy
`AGENTCTL_<name>`) convention. Booleans default on; disabled only by
`0`/`off`/`false`.

| Env var | Default | Effect when off / value |
|---|---|---|
| `WARDEN_TOKEN_GUARD` | on | master switch — disables tracking, alerts, and compaction entirely |
| `WARDEN_TOKEN_WARN_ALERT` | on | suppresses the warning/critical notification (still tracked + displayed) |
| `WARDEN_TOKEN_AUTO_COMPACT` | on | suppresses the critical auto-compact (still tracked, displayed, alerted) |
| `WARDEN_TOKEN_WARN` | `200000` | warning threshold in tokens (unparseable → default) |
| `WARDEN_TOKEN_CRITICAL` | `400000` | critical threshold in tokens (unparseable → default) |

Independence: e.g. keep monitoring + colored badges but kill the auto-action
(`WARDEN_TOKEN_AUTO_COMPACT=off`), or kill just the popups
(`WARDEN_TOKEN_WARN_ALERT=off`), or the whole thing (`WARDEN_TOKEN_GUARD=off`).
If `crit <= warn` after parsing, fall back to defaults (guard against an
inverted config that would make `warning` unreachable).

## Surfacing — context size with state-colored display

Every agent row shows its context size, colored by state, everywhere agents are
listed. The color is purely the rendered form of `ContextState`, so it stays
correct automatically and shows **regardless of the alert/notify flags** (it is
display, not an alert).

| State | Color | Example |
|---|---|---|
| `ok` (< 200k) | green | `145k` |
| `warning` (200k–400k) | orange | `210k ⚠` |
| `critical` (≥ 400k) | red | `410k ⛔` |

- **TUI**: colored token figure on each agent row (lipgloss/ANSI per state).
- **Web**: colored badge/cell per agent (CSS class per state).
- **CLI `warden ls`**: a token column, ANSI-colored when stdout is a TTY, plain
  otherwise.
- Agents with no model turn yet (`ok=false`, `ContextTokens==0`,
  `ContextState==""`) render blank/`—` — not a green zero — so a just-spawned
  agent isn't mislabeled.

**Desktop popup**: the warning/critical *notification* fires only when
`WARDEN_NOTIFY` is on (existing notifier) and `WARDEN_TOKEN_WARN_ALERT` is on.
The colored display does not depend on either.

## Accepted risk

Auto-compact ON by default means warden will send `/compact` to any agent that
crosses 400k while idle, without an explicit per-agent opt-in. `/compact` can
still land at an awkward point in a task even when the agent is idle. Mitigations
(all in this design): the only-when-idle gate, the cooldown, edge-triggering, and
the independent `WARDEN_TOKEN_AUTO_COMPACT=off` flag. The user reviewed and
accepted this default.

## Out of scope (YAGNI)

- **Auto-rotate** at critical — manual `warden rotate` stays the path; auto-rotate
  was deliberately rejected.
- **Live `/context` scraping** — passive transcript read covers it.
- **Per-agent threshold overrides** — global env only for v1.
- **Historical token graph** — the `metrics` Resources panel covers trends; this
  is a single live gauge.

## Testing

- **Pure unit tests** (`internal/ctxtokens`): `LatestContextTokens` over fixtures
  — no-usage transcript (`ok=false`), single turn, multi-turn (picks the last),
  malformed lines skipped, an oversized line degrading gracefully; `Classify`
  table across the three bands incl. exact boundary values.
- **Poller transition tests** (fake deps): ok→warning fires the alert exactly
  once; →critical fires the alert and compacts **only when idle**, **defers when
  working**, and **respects the cooldown**; each flag off suppresses exactly its
  piece (guard off → nothing; warn-alert off → no notify but state still
  persisted; auto-compact off → alert but no `/compact`).
- **Config tests**: defaults, override parsing, `crit <= warn` fallback.
- **Lifecycle**: `/compact` send issues the expected bracketed-paste + Enter.
- **Store**: `UpdateContext` round-trips fields; transition appends one event.

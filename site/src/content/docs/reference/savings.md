---
title: Token-savings ledger
description: The append-only ledger of tokens warden's lifecycle features keep out of agents' context — and the warden savings report.
---

Every time one of warden's lifecycle features avoids dumping output into an agent's
transcript, it records the saving to a real, **append-only ledger** under
`<data_dir>/savings/`. `warden savings` reads it back — a measured proof point, not
an estimate. Gated by the `savings` config setting (default on); the daemon serves
it at `GET /api/v1/savings` (403 when off).

## Two axes, never blended

The report keeps two honest claims separate:

- **Context axis** — how much leaner agent context stayed: the raw output that
  *would have* entered Claude vs. what actually did, as a reduction % and dollars.
- **Offload axis** — Claude work moved off entirely onto the local LLM
  (classify/summarize calls that never hit Claude), in dollars. It keeps nothing
  in-context, so it is never folded into the context percentage.

## What records a saving

- `warden check` — raw build/test output kept out of the transcript (only failures returned).
- `warden commit` / `push` / `sync` — git plumbing output the agent never sees.
- Auto-/`​/compact` context reclaim — tokens dropped when the guard compacts a critical agent.
- Local-LLM offload — classify/summarize work routed to the local model instead of Claude.

## Reading the report

```sh
warden savings                    # per-feature table (saved/raw tokens, events)
warden savings --benchmark        # headline A/B proof, built to screenshot
warden savings --since 7d         # scope to a window (24h/7d/2w) or a date
warden savings --json             # structured summary for tooling
warden savings --audit            # raw-vs-kept provenance samples (needs savings_samples)
warden savings --calibrate        # measure this workload's true bytes/token ratio
```

`--benchmark` is the persuasive view: *without warden* (raw tokens that would have
entered Claude) vs. *with warden* (what actually did), the reduction %, a leaner
factor, dollars saved, a per-day saved-tokens **sparkline**, and — when transcript
spend was observed — the cut as a share of real measured Claude spend.

## Honesty knobs

- **Basis line.** Every figure states whether it rests on a `CALIBRATED`
  (workload-measured) or `HEURISTIC` (4 bytes/token) bytes-per-token ratio.
- **`--calibrate`.** Measures the real ratio against Claude's `count_tokens`
  endpoint (needs `ANTHROPIC_API_KEY` and retained samples), then persists it.
  **Forward-only:** it prices events recorded after it runs; earlier rows keep their
  heuristic counts. `--calibrate-max` caps the paid calls.
- **`--audit` + `savings_samples`.** Off by default. When on, the ledger retains a
  truncated raw-vs-kept sample on a fraction of events so a skeptic can eyeball the
  actual bytes. The samples hold substrings of real build/test/git output, which may
  be sensitive — hence opt-in.

Dollars are priced at the Opus input/output rates. Also exposed as the `savings`
MCP tool.

## API: the saved-tokens trend

`GET /api/v1/savings` accepts:

- `since` — a window (`24h`/`7d`/`2w`) or an RFC3339 timestamp; absent ⇒ all time.
- `bucket` — `day` or `hour`. Attaches a **zero-filled, contiguous** saved-tokens
  trend (`buckets[]`) at that granularity, oldest first. Each bucket carries `ts`
  (unix seconds at the interval start), a `date` label, `saved_tokens`, `events`,
  the running `cumulative`, and a per-feature `by_feature` split. Because the trend
  is zero-filled up to *now*, an idle interval is a real zero and the line stays
  continuous — so even a ledger only hours old plots a curve rather than a single
  point. `bucket_granularity` echoes the chosen width. An unknown `bucket` value
  yields no trend (not an error).
- `samples=1` — attaches retained provenance pairs (needs `savings_samples`).

The web **Metrics** tab uses this: short windows (`24h`/`48h`) request `bucket=hour`,
longer ones `bucket=day`, and the card plots per-bucket saved tokens, the cumulative
line, and a per-feature stacked breakdown. `warden savings --benchmark` requests the
daily buckets for its sparkline.

## Cost governance — `warden spend` + the budget gate

Savings reports what warden kept **out** of context; **cost governance** reports what
agents **actually billed** Claude. The daemon already reads each agent's REAL
input/output tokens from its transcript; `warden spend` prices that per model into
dollars and rolls it up. Gated by the same `savings` switch; served at
`GET /api/v1/spend` (403 when off) and exposed as the `spend` MCP tool.

```sh
warden spend                      # total / today / this week, then per-agent/repo/day $ tables
warden spend --by agent           # show just one rollup: agent, repo, or day
warden spend --json               # structured rollup for tooling
```

Per-model pricing (`$/Mtok`, in/out): **Opus** `$5/$25`, **Sonnet** `$3/$15`,
**Haiku** `$0.8/$4`. An unrecognized model is priced at the Opus tier, so spend is
never silently under-counted. Opus rates match the savings ledger.

### Budget gate

A **soft** spawn gate on cost, sibling to the memory-pressure gate. Set the caps and
turn it on:

```yaml
tokens:
  budget_gate: true
  budget_daily_usd: 25      # today's measured spend cap (0 disables this axis)
  budget_weekly_usd: 100    # trailing-7-day cap (0 disables this axis)
```

When today's or the trailing week's measured spend reaches a cap, a non-forced spawn
returns **428** with a confirmation payload naming the figures; re-run with `--force`
(API/MCP `force: true`) to proceed — exactly like the memory-pressure gate. It never
hard-blocks.

### Where else cost shows up

- **`warden ls`** gains a **COST** column (also in `warden search` / `warden history`).
- The web **Metrics** tab adds a **Cost per agent** card: the `total / today / this
  week` headline plus a live per-agent cost table beside the RSS/CPU charts.

## One umbrella — `warden cost`

`warden cost` gathers both financial views under a single discoverable command,
mirroring how `warden library` unifies presets, prompt templates, and pipeline
templates:

```sh
warden cost                       # combined: a SPEND section + a SAVINGS section
warden cost spend --by agent      # same as `warden spend --by agent`
warden cost savings --benchmark   # same as `warden savings --benchmark`
```

`warden cost spend` and `warden cost savings` are the very same commands as the
top-level `warden spend` / `warden savings` — every flag wired straight through —
and the top-level commands remain available as aliases. With no subcommand,
`warden cost` prints a combined at-a-glance summary that reuses both reports'
render logic, so the figures never disagree. Purely additive: no new storage,
pricing, or MCP tool (both axes already ship the `spend` and `savings` MCP tools).
Resource footprint — memory/CPU/pressure — is a **different axis** and stays under
[`warden stats`](/warden/reference/observability/), deliberately not folded in.

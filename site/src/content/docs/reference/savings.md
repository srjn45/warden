---
title: Token-savings ledger
description: The append-only ledger of tokens warden's lifecycle features keep out of agents' context — and the warden savings report.
---

Every time one of warden's lifecycle features avoids dumping output into an agent's
transcript, it records the saving to a real, **append-only ledger** under
`<data_dir>/savings/`. `warden savings` reads it back — a measured proof point, not
an estimate. Gated by the `savings` config setting (default on); the daemon serves
it at `GET /savings` (403 when off).

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

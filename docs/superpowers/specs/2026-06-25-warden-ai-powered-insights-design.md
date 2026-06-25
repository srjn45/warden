# warden AI-powered insights — design

**Status:** shipped
**Feature:** FUTURE_ENHANCEMENTS #48 ("Analyze historical patterns; suggest parallelization / hints")
**Surfaces:** `wd insights` · MCP `insights`

## Motivation

warden already records a lot about its own runs — every agent session carries a
type, a status, a repo, created/updated timestamps, and (live) resource metrics.
That history is sitting in the store unused. An operator running many agents over
days wants to know the things only the aggregate can tell them: which task types
run long (and which individual runs blew past the norm), how often a type errors,
when the fleet is busiest, which files keep getting edited together, and — the
headline — **which sequential runs could have been run in parallel** because they
touched disjoint files and never overlapped in time.

`wd insights` mines exactly that. Like the orchestrator (#50) and digest, it follows
warden's core principle: a **deterministic statistics core that needs no LLM**, with
an **optional local-LLM narration layer** that degrades gracefully to the
deterministic output whenever the model is off, unreachable, errors, or returns an
empty reply. The numbers are always real and reproducible; the model only ever
rephrases them.

## Architecture

Three layers, all in a self-contained, pure `internal/insights` package, plus a
shared aggregator and the two thin surfaces:

1. **Deterministic core** (`insights.go`, `parallel.go`) — pure functions over an
   `Input` value, no I/O. This is the load-bearing half and is fully unit-tested
   with hand-built fixtures (no model, no network, no daemon).
2. **Narration seam** (`narrator.go`) — `Narrate(ctx, llm.Completer, Report)`,
   mirroring `digest/narrator.go` precisely, including the rule that an **empty
   reply is not trusted** for a summarization task and falls back to the
   deterministic floor.
3. **Aggregator** (`client.Insights`) — the one place that does I/O: it gathers
   sessions + history + metrics from the daemon, reconstructs file sets
   best-effort, and calls `insights.Analyze`. Both the CLI verb and the MCP tool
   call it, so they can never drift.

### Why the aggregator lives in `client`

`internal/mcp` imports `internal/client`, and `internal/cli` imports both. The
shared aggregation logic therefore lives in `client` — the lowest common
denominator both surfaces already depend on — rather than being duplicated or
forced into a new daemon endpoint. This also keeps the daemon surface unchanged:
`Insights` is composed from existing client calls (`List`, `History`,
`GetAgentHistory`, and best-effort `Digest`).

## Data model

```
Input  { Sessions []SessionRecord; Agents []metrics.AgentSummary; Now time.Time }
Report { GeneratedAt; Sessions, ActiveSessions int;
         Durations []TypeDuration; CoEdits []CoEditPair; ErrorRates []TypeErrorRate;
         BusiestPeriods []HourBucket; Parallelizable []ParallelSuggestion;
         Anomalies []AgentAnomaly }
```

A `SessionRecord` is the engine's normalized view of a session: id, name, type,
status, repo, a start/end window, and the set of files it edited.
`FromSession(s, files)` adapts a `store.Session`: a **terminal** status
(done/errored/orphaned) closes the window at `UpdatedAt`; an open session stays
open-ended and is treated as active (excluded from duration stats, which need a
finished run to be meaningful).

## Deterministic computations

- **Durations by type** — group finished runs by normalized type; report count,
  **median / p90 / max** (nearest-rank percentile), and flag any run longer than
  `durationOutlierFactor` (2.0) × the type median as an **outlier**. Active runs
  are skipped — an unfinished run has no duration.
- **Parallelization suggester** (`SuggestParallelization`) — the headline. Over
  finished, repo-scoped, file-attributed sessions, suggest a pair when they are in
  the **same repo**, their `[Start,End)` windows do **not** overlap (half-open), and
  their edited file sets are **disjoint**. The saving is the shorter of the two
  runs (the time that would have been recovered by overlapping them). Sorted by
  saving desc then ids; capped at `maxParallelSuggestions` (10). Overlapping or
  file-sharing pairs are deliberately **not** suggested — shared files imply a
  possible dependency, and already-overlapping runs were effectively parallel.
- **Co-edited files** — file pairs edited together in at least `coEditMinSessions`
  (2) sessions, capped at `maxCoEditPairs` (10). A coupling hint.
- **Error rates by type** — errored+orphaned over total, per type.
- **Busiest periods** — session counts bucketed by UTC hour, top `maxBusyBuckets`
  (5).
- **Anomalies** — passed straight through from `metrics.AgentSummary.Anomalies`
  for live agents.

## Narration

`Narrate` builds a blunt, no-preamble prompt (`NarratorPrompt`) carrying the
structured facts and asks the local model for a short summary. The contract is the
digest/lifecycle one exactly:

- nil completer ⇒ deterministic-only (`DeterministicSummary`).
- any model error ⇒ deterministic floor.
- empty/whitespace reply ⇒ **not trusted** ⇒ deterministic floor.
- otherwise the cleaned model line, with the deterministic detail still printed
  below it.

`DeterministicSummary` is always a valid standalone answer; an empty report yields
`"No agent session history to analyze yet."`.

## Surfaces

- **`wd insights`** — thin cobra verb. Gated on `cfg.GetInsights()`. Builds an
  Ollama `Completer` only when `local_llm` is enabled (else nil ⇒ deterministic).
  Flags mirror the history/digest neighbors: `--since`, `--limit`, `--session`
  (scopes the parallelization suggestions to one id/name), `--json`.
- **MCP `insights`** — registered in `internal/mcp`, calls `client.Insights` and
  returns the compact `Report` as a JSON result.

## Config gate

A new `insights` boolean (default **on**), added following the
`local_llm`/`isolation_guard`/`snapshots` pattern: the field + yaml tag on
`Config`, a schema entry, a `defaults()` value, a description entry, and a
`GetInsights()` accessor. The schema drift-guard test keeps these in lockstep.

## Invariants & known limitations

- Every git/exec/model call runs under a `context` with a timeout; errors are
  **returned**, never panicked on. A best-effort file-set fetch that fails just
  yields fewer co-edit/parallelization hints, never an error.
- Duration / error-rate / busy-period analysis covers **all** sessions. The
  file-set-dependent analysis (co-edits, parallelization) is strongest on active or
  digestible sessions: archived file sets are reconstructed best-effort from
  digests, and the digest endpoint only resolves **active** sessions, so very old
  archived runs may contribute to the time/error stats but not to the file-pair
  analysis. This is an intentional surface-area trade-off (no new daemon endpoint);
  the report is still correct, just less file-rich for cold history.

## Testing

Mirrors `rotate_test.go` / `history_test.go`: pure fixtures, no real model or
network. `insights_test.go` covers the aggregations and outlier/percentile edges;
`parallel_test.go` covers the overlap/disjoint matrix (suggested vs. not);
`narrator_test.go` uses a fake `llm.Completer` to assert the nil/error/empty
fallbacks and the model-preferred path; `cli/insights_test.go` covers session
scoping, the formatter sections, and error propagation with a fake client.
`go test ./...` and `make verify-fast` pass.

## Deferred

The optional **web Cockpit insights card** is deferred — the CLI + MCP surfaces
deliver the feature, and a card would balloon scope into the web app without a
driving use case yet. The shared `client.Insights` aggregator already gives a web
route a single call to build on when that demand appears.

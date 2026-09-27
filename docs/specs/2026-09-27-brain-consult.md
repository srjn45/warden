# Shared Need-Based Brain Consult

**Date:** 2026-09-27  
**Status:** Frozen — spec-only, no production code in this PR  
**Feature branch:** `autopilot/brain-consult`

---

## Problem

Today the `PipelineWatcher` responds to a stuck job (`JobNeedsAttention` after
`stuckRetryAfter`) with at most one deterministic auto-retry
(`AutoRetryCount == 0`). If the retry fails again, the watcher logs a warning
and stops: a human must intervene. Autopilot's manager (role `autopilot`) has a
`brain` resolver it spawns for high-level decisions, but that brain is
manager-scoped; it is not reachable from the pipeline layer.

There is no shared, pipeline-accessible mechanism to ask a reasoning model
"given what we know, what should happen to this stuck job?" — and no standard
way for the autopilot manager to consult the same resolver for ad-hoc decisions
without re-spawning a full brain.

The result is unnecessary human-in-the-loop moments (stuck pipelines that could
be unblocked by a quick AI triage) and duplicated spawn/teardown logic scattered
across call sites.

---

## Goals

1. **Single Consultor abstraction** — one interface that spawns a short-lived
   `role=brain` agent, injects a structured prompt, waits for a single structured
   reply, and tears the agent down.
2. **PipelineWatcher integration** — after the one deterministic auto-retry is
   exhausted, the watcher may consult the brain once per stuck episode rather than
   stopping dead.
3. **Autopilot manager reuse** — the autopilot resolver path routes through the
   same Consultor, eliminating duplicate spawn/teardown logic.  The manager's
   liveness model and Guardian are untouched.
4. **Audit trail** — every consult writes an `audit.Event` so operators can see
   what the brain decided and why.
5. **Config opt-out** — the feature can be turned off globally or per-pipeline.

---

## Non-Goals (D8)

- **Long-lived pipeline manager**: the brain spawned by Consultor is
  short-lived (one prompt → one reply → teardown). Warden does not grow a
  durable pipeline-orchestration brain.
- **Brain free-form repo edits**: the brain may only select an action from the
  closed enum (D2). It issues no file edits, git commits, or spawns of its own.
- **Orphaned-agent AutoRestart**: Consultor does not replace or modify the
  existing `AutoRestart` supervisor in `internal/daemon/autorestart.go`.
- **Changing `emit`/`Reconcile` happy path**: the Executor's normal
  `Reconcile → Plan → spawn` loop is untouched. Consultor is called only in the
  abnormal stuck-job branch.

---

## D1 — Package and Public API

### Package placement

New package: **`internal/brainconsult`**

Rationale: the Consultor is shared between `internal/daemon` (pipeline) and
`internal/autopilot` (manager resolver). Neither is an appropriate parent.
Placing it in `internal/daemon` would create an import cycle from `autopilot`.
A sibling package keeps the API visible to both without coupling.

### Public API

```go
package brainconsult

import "context"

// Action is the closed set of actions a brain may recommend.
// Consumers may offer subsets via Request.Allowed.
type Action string

const (
    ActionWait       Action = "wait"
    ActionNudgeAgent Action = "nudge_agent"
    ActionRetryJob   Action = "retry_job"
    ActionMarkFailed Action = "mark_failed"
    ActionSkipJob    Action = "skip_job"
    ActionEscalate   Action = "escalate"
    ActionNoop       Action = "noop"
)

// Request is the input to a consultation.
type Request struct {
    Intent      string   // one-line summary, e.g. "stuck pipeline job"
    Situation   string   // free-form description of the current state
    Goal        string   // what outcome is desired
    AlreadyTried []string // actions already attempted (matched against Action constants)
    Evidence    string   // log excerpts, error messages, agent output snippets
    Allowed     []Action // subset of actions the caller accepts; nil = all of D2
}

// Result is what the brain decided.
type Result struct {
    Action  Action // one of the D2 enum values
    Reason  string // one-line explanation from the brain
    BrainID string // agent id of the spawned brain (for tracing/audit)
}

// Consultor is the single interface for all brain consult call sites.
type Consultor interface {
    // Consult spawns a role=brain agent, injects the structured prompt (D3),
    // waits for a single structured reply, tears the agent down, and returns
    // the result. It honours ctx cancellation and the configured timeout (D7).
    Consult(ctx context.Context, req Request) (Result, error)
}
```

### Reply format (frozen)

The brain replies with a **single JSON line** on stdout:

```json
{"action": "<id>", "reason": "<one-line>"}
```

This is the frozen reply format. The Consultor reads the agent's output,
extracts the first line matching `{"action":...,"reason":...}`, and returns it.
Single-line JSON is chosen over `ACTION=<id>` because it carries the reason in
a structured field without a second parse step, and JSON is already the lingua
franca of warden's inter-agent protocol.

### Lifecycle contract

- `Consult` spawns a new `role=brain` agent via the existing `Lifecycle.Spawn`
  / `SpawnRequest` path (mirroring `autopilotRuntime.SpawnBrain`), using
  `Role: "autopilot"` (the `autopilotBrainRole` constant in
  `internal/daemon/autopilot_runtime.go`).
- The spawned agent is **headless** (non-interactive).
- After the brain replies (or after timeout / ctx cancel), the Consultor calls
  `Lifecycle.Teardown` unconditionally.
- A configurable **timeout** (default `10m`, D7) is applied via
  `context.WithTimeout` wrapping the inner consult.
- If the brain does not produce a parseable reply within the timeout, `Consult`
  returns `ErrNoBrainReply` and teardown still runs.

---

## D2 — Closed Action Enum v1

The frozen action set (callers may expose a strict subset via `Request.Allowed`):

| Action id | Meaning |
|---|---|
| `wait` | Do nothing this tick; re-evaluate on the next watcher cycle |
| `nudge_agent` | Send a brief wake-up message to the stuck job's agent |
| `retry_job` | Retry the job via `Executor.Retry` (increment `AutoRetryCount`) |
| `mark_failed` | Mark the job `JobFailed` directly (unblocks descendants via Reconcile) |
| `skip_job` | Mark the job `JobSkipped` (unblocks descendants via Reconcile) |
| `escalate` | Record an audit event and surface a human-readable message to the operator |
| `noop` | No action; the brain could not determine a useful step |

Consumers MUST treat an unrecognized action string as `noop` and log a warning,
since the set may grow in future versions.

---

## D3 — Prompt Template

The exact prompt injected into the brain agent (interpolated from `Request`):

```
Scenario: {{.Situation}}
Goal: {{.Goal}}
Already tried: {{join .AlreadyTried ", "}}
Evidence: {{.Evidence}}
Allowed next steps: [{{join .Allowed ", "}}]

Reply with exactly one action id from the allowed list and a one-line reason,
as a single JSON object on a line by itself:
{"action": "<id>", "reason": "<one-line reason>"}
```

The `Scenario:` and `Goal:` lines are trimmed to avoid injecting empty keys.
`Evidence` may be a multi-line block; the prompt template does not truncate it,
but the Consultor implementation SHOULD cap it at 8 KB before injection to
prevent runaway token usage.

---

## D4 — PipelineWatcher Integration Point

### Where in the tick loop

Current stuck-job logic in `PipelineWatcher.tick` (line 102–130 of
`internal/daemon/pipeline_watcher.go`):

```
if w.autoRetry && j.AutoRetryCount == 0 {
    w.exec.Retry(...)               // one-shot deterministic retry
} else {
    slog.Warn(...)                  // currently stops here
}
```

The consult fires in the `else` branch — after the deterministic auto-retry has
already been attempted at least once (`AutoRetryCount >= 1`) and the job is
still in `JobNeedsAttention`:

```
if w.autoRetry && j.AutoRetryCount == 0 {
    w.exec.Retry(...)               // unchanged — deterministic one-shot retry
} else if w.consultor != nil && w.brainConsultEnabled(p) && !w.consulted[key] {
    go w.consultBrain(ctx, p, j)    // async, capped by max_concurrent
}
```

### Dedupe key

`consulted` is a `map[string]bool` on `PipelineWatcher`. The key is
`"<pipelineID>/<jobID>+<generation>"` where generation is `j.AutoRetryCount`
(the epoch of the current stuck episode). When a job is retried and becomes
stuck again, the generation increments and a fresh consult fires.

On job resolution (status leaves `JobNeedsAttention`), the watcher deletes
`consulted[key]`.

### Execution

The Consultor is called **asynchronously** (goroutine) but is subject to a
**max-concurrent cap** (semaphore, default 1 from D7). The result is executed
synchronously within the goroutine:

| Action | Executor call |
|---|---|
| `retry_job` | `e.exec.Retry(ctx, pid, jobID)` |
| `mark_failed` | `e.exec.markJob(pid, jobID, func(j){j.Status = JobFailed})` + `Reconcile` |
| `skip_job` | `e.exec.markJob(pid, jobID, func(j){j.Status = JobSkipped})` + `Reconcile` |
| `nudge_agent` | `e.life.SendMessage(agentID, "continue")` (best-effort) |
| `wait` / `noop` | no-op |
| `escalate` | audit event + `slog.Warn` — no structural change |

### New fields on `PipelineWatcher`

```go
consultor          brainconsult.Consultor    // nil = feature off
brainConsultMaxCon int                       // semaphore capacity (default 1)
consulted          map[string]bool           // dedupe: key = "pid/jobID+generation"
consultSem         chan struct{}              // buffered semaphore
```

`PipelineConfig` is extended with:

```go
BrainConsult bool `yaml:"brain_consult"` // default true (D7)
```

`PipelineWatcher.brainConsultEnabled(p)` checks both the global
`brain_consult.enabled` flag and `p.BrainConsult` (pipeline-level override).

---

## D5 — Autopilot Manager Resolver Reuse

The autopilot Controller's resolver path (`selectBrain` / `spawnBrain`, see
`internal/autopilot/controller.go`) currently spawns a full brain agent and
keeps it alive for the run's lifetime. This spec does **not** change that path.

For **ad-hoc design calls** from the manager (e.g. "which action should I take
for this stuck worker?"), the manager may call the `Consultor` through a
**thin wrapper** that satisfies `brainconsult.Consultor` and is injected at
daemon wiring time. This wrapper:

- shares the same `Lifecycle` surface (`Spawn`/`Teardown`) already used by
  `autopilotRuntime.SpawnBrain`;
- uses `Role: "autopilot"` (i.e. `autopilotBrainRole`);
- does **not** write to the autopilot ledger or touch `r.brain`, so the
  manager's liveness model is unchanged;
- does **not** interact with the `Guardian` or `GuardianRuntime`.

The manager accesses the Consultor via an optional interface it receives at
construction — if the field is nil, it falls back to its existing resolver
behavior. There is no change to `Controller.SetRuntime`, `GuardianRuntime`, or
the `OverwatchRuntime` interfaces.

---

## D6 — Audit Trail

Every consult call writes one `audit.Event` via `audit.Writer.Log`:

```go
audit.Event{
    Action: "brain_consult",          // new ActionBrainConsult constant
    Target: brainID,                  // the spawned brain agent id
    Detail: map[string]string{
        "intent":      req.Intent,
        "action":      string(result.Action),
        "reason":      result.Reason,
        "pipeline_id": pipelineID,    // when called from PipelineWatcher
        "job_id":      jobID,         // when called from PipelineWatcher
        "run_id":      runID,         // when called from autopilot manager
        "task_id":     taskID,        // when called from autopilot manager
    },
}
```

Fields not applicable to a given call site are omitted (empty string →
`omitempty` drops them from JSON).

The `"brain_consult"` action name is added to the `audit` package alongside the
existing `ActionSpawn`, `ActionAutopilotOn`, etc. constants in
`internal/audit/audit.go`.

---

## D7 — Config Names (Frozen)

### New config block: `brain_consult`

```yaml
brain_consult:
  enabled: true          # global kill-switch; default true
  timeout: 10m           # per-consult deadline; Go duration string
  max_concurrent: 1      # max simultaneous brain consult agents
```

### Extension to `pipeline` block

```yaml
pipeline:
  keep_done: false       # (existing)
  hint: false            # (existing)
  brain_consult: true    # new; per-pipeline override; default true
```

### Go struct additions

`internal/config/config.go`:

```go
// BrainConsultConfig is the brain_consult config block.
type BrainConsultConfig struct {
    Enabled       bool   `yaml:"enabled"`
    Timeout       string `yaml:"timeout"`        // Go duration, default "10m"
    MaxConcurrent int    `yaml:"max_concurrent"` // default 1
}

// Added to PipelineConfig:
BrainConsult bool `yaml:"brain_consult"` // default true

// Added to Config:
BrainConsult BrainConsultConfig `yaml:"brain_consult"`
```

Default wiring (mirrors the frictionless-safeguards philosophy: generous
defaults, never pace normal work):

| Key | Default | Notes |
|---|---|---|
| `brain_consult.enabled` | `true` | Opt-out: set `false` to disable globally |
| `brain_consult.timeout` | `10m` | Generous; consults are infrequent |
| `brain_consult.max_concurrent` | `1` | One consult at a time across all pipelines |
| `pipeline.brain_consult` | `true` | Per-pipeline override; `false` to silence one pipeline |

---

## Open Questions (not blocking this spec)

1. **Brain model selection**: should the Consultor use the same `Resolver` as
   the autopilot controller (picking the cheapest enabled tier) or should it
   hard-code a model tier?  Recommendation: reuse the existing `Resolver` for
   consistency; defer to implementation PR.
2. **Reply timeout vs. agent liveness**: if the brain agent crashes before
   replying, the Consultor will wait until its own timeout elapses.  A faster
   path (polling `store.Get` for a terminal session state) could cut the wait;
   defer to implementation.
3. **PipelineSpec YAML opt-out field**: `pipeline.brain_consult` is a
   daemon-level config override, not a per-pipeline YAML field in the spec file.
   If per-pipeline YAML opt-out is needed, it should be added as a `Job.BrainConsult`
   field — defer to implementation.

---

## Summary of Decisions

| ID | Decision |
|---|---|
| D1 | New package `internal/brainconsult`; `Action`, `Request`, `Result`, `Consultor` interface; frozen JSON reply `{"action":…,"reason":…}` |
| D2 | Action enum v1: `wait`, `nudge_agent`, `retry_job`, `mark_failed`, `skip_job`, `escalate`, `noop` |
| D3 | Template: `Scenario / Goal / Already tried / Evidence / Allowed next steps / JSON reply instruction` |
| D4 | After `AutoRetryCount >= 1` still `JobNeedsAttention`; once per episode (dedupe `pid/jobID+generation`); executed via `Executor`; capped at `max_concurrent` (default 1) |
| D5 | Autopilot manager uses thin wrapper over same `Lifecycle`; no liveness / Guardian changes |
| D6 | `audit.ActionBrainConsult = "brain_consult"` event with `intent/action/reason/pipeline_id/job_id/run_id/task_id` |
| D7 | `brain_consult.{enabled,timeout,max_concurrent}` + `pipeline.brain_consult` (all defaulting to on/generous) |
| D8 | Non-goals: long-lived pipeline brain, free-form edits, AutoRestart, emit/Reconcile happy path |

# Memory-pressure-aware soft spawn gate

**Date:** 2026-06-05
**Status:** Approved (brainstorming complete)

## Problem

The freeze investigation ([[agentctl-freeze-investigation]]) concluded that the Mac
"freeze / forced-restart" while running agentctl is **not** a daemon leak — the daemon
itself is tiny (24 MB / 22 FDs). The leading cause is **memory-compressor thrash from
many long-lived 1M-context Claude agents compacting at once**.

agentctl currently has zero in-process resource awareness: nothing reads system memory
pressure or agent load, and nothing stops you from spawning a 9th heavy agent onto an
already-strained machine. The external `~/.agentctl/memlog` sampler is a passive shell
script that only helps *post-mortem*.

This feature adds **active, preventive backpressure**: when memory pressure (or agent
load) is high and you try to spawn, agentctl warns you and asks you to confirm — turning
the known freeze failure mode into an informed choice.

Token/cost tracking was deliberately shelved as a vanity metric
([[agentctl-token-cost-shelved]]); this feature deliberately does **not** revive it. We
gate on memory pressure, which has a real track record of causing harm.

## Goals

- Warn (never hard-block) before a spawn that would add load to an already-pressured machine.
- Cover **every** spawn path — interactive (CLI/TUI/web) and non-interactive (MCP/pipeline).
- Be cheap: one `sysctl` read, cached; no per-agent RSS accounting.
- Be disableable via a single env toggle the moment it feels restrictive.

## Non-goals (YAGNI)

- No CPU or network monitoring (network bandwidth is trivial; CPU is I/O-bound waiting on the API).
- No per-agent RSS tracking or historical pressure graphing.
- No "crossing into warn/critical" OS notification (explicitly deselected for v1).
- No hard blocking — the gate is always soft (confirm-anyway).
- No reviving token/cost metrics.

## Decisions (from brainstorming)

| Decision | Choice |
|---|---|
| Gate behavior | **Soft warn + confirm-anyway.** Never hard-blocks. |
| Pressure signal | **macOS native pressure level** (sysctl) as primary, **live agent count** for context/co-trigger. |
| Trigger | `level >= Warn(2)` **OR** `liveAgentCount >= N` (N configurable, default 5). |
| Non-interactive spawns | Daemon returns a structured "confirmation required" response; MCP/CLI retry with `force=true`. **Pipelines pass `force=true` always** (advisory — never deadlock a DAG). |
| v1 surfaces | Web confirm dialog, TUI confirm prompt, always-on pressure gauge (web + TUI). |
| Toggle | `AGENTCTL_SPAWN_GATE` env, **default on**, disableable. Gauge stays live even when gate is off. |

## Architecture

### 1. `internal/pressure` — pure core

Mirrors the `internal/approval` and `internal/digest` precedent: parsing + decision logic
are pure and unit-testable; the OS exec lives elsewhere.

```go
type Level int
const (
    Normal   Level = 1
    Warn     Level = 2
    Critical Level = 4
)
func (l Level) String() string // "normal" | "warn" | "critical" | "unknown"

// ParseSysctl parses `kern.memorystatus_vm_pressure_level: 2` (value-only or full line).
// Returns an error on malformed input; caller degrades to Normal.
func ParseSysctl(raw string) (Level, error)

type Verdict struct {
    Elevated   bool
    Level      Level
    AgentCount int
    MaxAgents  int
    Reason     string // human text, e.g. "pressure: warn" or "6 agents live ≥ 5"
}

// Evaluate is the truth table. Elevated when level >= Warn OR agentCount >= maxAgents.
func Evaluate(level Level, agentCount, maxAgents int) Verdict
```

### 2. lifecycle — OS exec helper

Exactly how `internal/digest` gets `RunClaudeP` / `GitNumstat` from lifecycle, lifecycle
exposes the side-effecting read:

```go
// MemoryPressure runs `sysctl -n kern.memorystatus_vm_pressure_level` and parses it.
// On non-macOS (sysctl missing / key absent) it returns pressure.Normal, nil —
// the gate degrades to count-only.
func (l *Lifecycle) MemoryPressure(ctx context.Context) (pressure.Level, error)
```

### 3. Daemon wiring

- **Sampler loop** — a sibling goroutine to the existing poller ticker
  (`internal/poller`), sampling `MemoryPressure` every ~5s and caching the latest `Level`
  on a mutex-guarded `Server` field. Spawn checks and gauge polls read the cache, never
  exec sysctl on the hot path.
- **`GET /pressure`** — returns
  `{level: int, levelName: string, agentCount: int, maxAgents: int, elevated: bool, gateEnabled: bool}`.
  Feeds the gauge and lets UIs hide the warning path when the gate is off.
- **`handleSpawn`** (`internal/daemon/lifecycle_routes.go`) — after existing validation,
  before `s.life.Spawn`:
  1. If gate disabled → proceed unchanged.
  2. Count live non-terminal sessions via `store.List`.
  3. `v := pressure.Evaluate(cachedLevel, count, maxAgents)`.
  4. If `v.Elevated && !req.Force` → respond **HTTP 409** with
     `{confirmationRequired: true, verdict: {...}}`.
  5. Else proceed unchanged.

### 4. Surfaces — all consume the one verdict

- **Client** (`internal/client`): `SpawnParams` gains `Force bool`. `Spawn` maps the 409
  into a typed `ErrConfirmationRequired{Verdict}`. New `Client.Pressure(ctx)` → `GET /pressure`.
- **CLI** (`internal/cli/lifecycle.go`, `start`): add `--force`. On
  `ErrConfirmationRequired`: if stdin is a TTY, print the verdict and prompt `y/N`
  (yes → retry with force); otherwise print it and instruct `--force`.
- **MCP** (`internal/mcp/server.go`, `spawn_agent`): add `force` bool param. On
  `ErrConfirmationRequired`, return the verdict text instructing the orchestrator to retry
  with `force: true` — the LLM decides.
- **Pipeline executor**: job spawns always pass `force=true` (advisory; never deadlocks a DAG).
- **Web**: `web/src/lib/api.ts` `spawn` passes `force`; on 409, `NewAgentModal.tsx` renders
  an inline warning (`pressure: warn · 6 agents live`) + **"Spawn anyway"** button that
  re-POSTs with `force=true`. Fleet header polls `GET /pressure` and renders a passive
  **gauge** (level + live count). New pure helper `web/src/lib/pressure.ts` for formatting.
- **TUI**: new-agent flow (`n` key) shows the warning and requires a confirm keypress
  (→ force). Header renders the **gauge**. (`internal/tui/list.go` / `list_pane.go`.)

### 5. Toggle + config

Env, matching the `AGENTCTL_APPROVALS` precedent:

- `AGENTCTL_SPAWN_GATE` — default **on**; `off` / `0` / `false` disables enforcement.
  When off: `handleSpawn` skips the verdict (spawns always proceed); the gauge still
  reports current pressure (visibility is never restrictive).
- `AGENTCTL_SPAWN_GATE_MAX_AGENTS` — the count co-trigger `N`, default **5**.
- Pressure trigger level is fixed at `Warn` (2).

## Data flow

```
spawn (CLI / MCP / web / TUI / pipeline)
   → client.Spawn(SpawnParams{..., Force})
   → POST /spawn
   → handleSpawn:
        gate off?            → proceed
        force=true?          → proceed
        Evaluate(cachedLevel, liveCount, N).Elevated?
            no  → proceed
            yes → 409 {confirmationRequired, verdict}
                    CLI    → y/N prompt (TTY) or "--force" hint
                    MCP    → return text "retry with force:true"
                    web    → "Spawn anyway" button → re-POST force=true
                    TUI    → confirm keypress → force=true
                    pipe   → (never hits this; always force=true)

sampler loop (≈5s) → MemoryPressure() → cache Level
GET /pressure → {level, levelName, agentCount, maxAgents, elevated, gateEnabled}
   → web fleet header gauge + TUI header gauge
```

## Testing

- **`internal/pressure`** (pure): `ParseSysctl` incl. malformed/empty input; `Evaluate`
  truth table — every `Level` × (count above/below N) → expected `Elevated` + `Reason`.
- **Daemon** `handleSpawn`: injected cached level + force flag → 409 vs proceed; gate-off
  bypass; count-at-threshold boundary.
- **Client**: 409 body → `ErrConfirmationRequired{Verdict}` mapping.
- **Web**: `lib/pressure.ts` formatting unit test.
- Non-macOS degradation: `MemoryPressure` returns `Normal` when sysctl is absent (gate
  becomes count-only) — covered by the lifecycle helper test with a stubbed runner.

## Rollout notes

- Default-on but soft, so no behavior change beyond an occasional confirm prompt.
- Daemon must be rebuilt + reinstalled (binary embeds `web/dist`) to serve `/pressure`
  and the new UI — same as prior web-touching features.
- Manual smoke left for user: spawn under induced pressure (or low `MAX_AGENTS`) and
  confirm the warning fires across CLI/web/TUI, then `AGENTCTL_SPAWN_GATE=off` disables it.

## Build approach

Repo convention is subagent-driven TDD in a worktree (`.claude/worktrees/<topic>`,
baseRef=head, no origin). The natural dependency order forms a short pipeline:
`internal/pressure` (pure) → lifecycle helper → daemon (sampler + endpoint + gate) →
client → CLI + MCP + pipeline → web → TUI → review. See the implementation plan for batch
breakdown.

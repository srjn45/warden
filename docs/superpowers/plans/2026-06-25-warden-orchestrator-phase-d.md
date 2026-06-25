# Warden Orchestrator — Phase D: Monitoring Verbs Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Deliver the second half of the orchestrator's value — **supervision**. First-class natural-language handling of fleet questions ("what's running / what's stuck", "what's agent X doing", "anything waiting on me", "clean up"), implemented as read-only registry calls plus a local-model summarization pass. These are the high-frequency, low-stakes reads where a local model is good enough and Claude tokens would be wasted.

**Design spec:** `docs/superpowers/specs/2026-06-25-warden-orchestrator-design.md` (Phase D)

**Depends on:** Phase B (the registry's read-only tools + the loop). Brain spec Phase 1b (`lifecycle.Summarize`, already routed through the local model) — reused for the condensation pass. Independent of Phase C — build in either order.

**Architecture:** Phase B already exposes the read tools to the model and auto-executes them. Phase D is **mostly prompt + summarization scaffolding on top**, not new mechanics: a set of monitoring "intents" the loop recognizes, each a fixed read-call recipe followed by a condensation pass, so the operator gets a digest rather than raw JSON. The only mutating monitoring verb — "clean up" — proposes `terminate`/`delete` of terminal agents through the **existing confirm gate** (Phase B), not a new path.

```
  "what's stuck?"     ─▶ list_agents (+per-agent status) ─▶ condense ─▶ digest
  "what's X doing?"   ─▶ get_agent_output(X) tail        ─▶ condense ─▶ digest
  "anything for me?"  ─▶ list_approvals + read_inbox      ─▶ condense ─▶ digest
  "clean up"          ─▶ list_agents → terminal ones      ─▶ confirm gate (Phase B) ─▶ terminate/delete
  internal/orchestrator/monitor.go — the intent recipes + condensation
```

**Tech Stack:** Go 1.26+, reuses `internal/orchestrator` (Phase B), `internal/llm` (`Completer` for the condensation pass), `lifecycle.Summarize`. No new dependencies.

**Scope guard:** Monitoring reads are auto-execute (no gate); the **only** gated verb here is "clean up" and it reuses the Phase B gate verbatim — do **not** add a second confirm path. No new mutating capability beyond `terminate`/`delete` (already in the registry). If a task here proposes auto-reaping without confirmation, stop — teardown is always operator-confirmed.

---

## File Structure

### New Files
- `internal/orchestrator/monitor.go` — monitoring intent recipes + the condensation pass
- `internal/orchestrator/monitor_test.go` — per-intent tests against `fakeDaemon` + a fake condenser

### Modified Files
- `internal/orchestrator/session.go` — register the monitoring intents/tools with the loop (a thin hook; the loop machinery is unchanged)
- `internal/orchestrator/registry.go` — only if a read verb the monitors need isn't already exposed (e.g. a combined fleet-status read); otherwise untouched

---

## Task 1: Fleet digest — "what's running / what's stuck"

**Files:** New `internal/orchestrator/monitor.go`, `internal/orchestrator/monitor_test.go`

The flagship monitoring verb. `list_agents` + per-agent status, condensed to a short digest the operator can scan — *not* the raw session JSON the model would otherwise echo.

- [ ] **Step 1: Tests first**

  - **Running vs. stuck partition:** given a `fakeDaemon` returning a mix of active / waiting-on-approval / errored / idle sessions, the digest groups them correctly and names what's blocked.
  - **Condensation is used, not raw dump:** assert the output goes through the condenser (a digest sentence), and that a stuck agent's blocking reason surfaces.
  - **Empty fleet:** "nothing running" — clean, no error.
  - **Condenser failure degrades gracefully:** if the local-model condensation errors, fall back to a deterministic one-line-per-agent table (never a stack trace) — same fallback posture as the brain spec.

```go
func TestMonitor_Stuck(t *testing.T) {
    fd := &fakeDaemon{sessions: []*store.Session{
        active("a1"), waitingApproval("a2"), errored("a3")}}
    m := NewMonitor(fd, fakeCondenser{line: "1 running; a2 awaiting approval; a3 errored"})
    out, err := m.FleetDigest(context.Background())
    require.NoError(t, err)
    require.Contains(t, out, "a2 awaiting approval")
}

func TestMonitor_CondenserFailureFallsBackToTable(t *testing.T) {
    m := NewMonitor(&fakeDaemon{sessions: []*store.Session{active("a1")}}, failingCondenser{})
    out, err := m.FleetDigest(context.Background())
    require.NoError(t, err) // degrades, never errors out
    require.Contains(t, out, "a1")
}
```

- [ ] **Step 2: Implement**

```go
// Condenser turns a compact fact list into a short operator digest. Backed by
// the local model (reuse lifecycle.Summarize / the llm.Completer seam); any
// failure falls back to a deterministic table, never an error to the operator.
type Condenser interface{ Condense(ctx context.Context, facts string) (string, error) }

type Monitor struct { d Daemon; c Condenser }

func (m *Monitor) FleetDigest(ctx context.Context) (string, error) {
    sessions, err := m.d.List(ctx)
    if err != nil { return "", err }
    facts := summarizeFleet(sessions) // one compact line per agent: id, state, blocking reason
    if out, err := m.c.Condense(ctx, facts); err == nil && strings.TrimSpace(out) != "" {
        return out, nil
    }
    return facts, nil // deterministic fallback
}
```

`summarizeFleet` extracts the same compact one-line-per-agent shape the Phase B fleet snapshot uses (reuse it) — keep the model's input small. The condenser wraps `lifecycle.Summarize` (or a thin `Completer` call) with the brain spec's truncation discipline.

- [ ] **Step 3: Run → fail → implement → pass → commit**

Commit: `feat(orchestrator): fleet digest — condensed what's-running/what's-stuck`.

---

## Task 2: Per-agent + inbox monitoring — "what's X doing" / "anything waiting on me"

**Files:** `internal/orchestrator/monitor.go` (+ tests)

- [ ] **Step 1: Tests first**

  - **`AgentDigest(id)`:** pulls `get_agent_output(id)` tail, condenses to "what X is doing"; unknown id returns a clean not-found message, not an error.
  - **`PendingForMe()`:** combines `list_approvals` + `read_inbox`, lists what's awaiting the operator; empty → "nothing waiting on you."
  - Both reuse the same `Condenser` + deterministic fallback as Task 1.

- [ ] **Step 2: Implement**

```go
func (m *Monitor) AgentDigest(ctx context.Context, id string) (string, error) {
    out, err := m.d.Output(ctx, id, 200) // tail; bounded
    if err != nil { return "", err }
    return m.condenseOr(ctx, out, /*fallback*/ tail(out, 20)), nil
}

func (m *Monitor) PendingForMe(ctx context.Context) (string, error) {
    _, approvals, _ := m.d.Approvals(ctx)
    inbox, _ := m.d.MsgInbox(ctx, sessionID(), /*unreadOnly*/ true)
    facts := summarizePending(approvals, inbox)
    return m.condenseOr(ctx, facts, facts), nil
}
```

`condenseOr` is the shared "try the model, else deterministic" helper from Task 1.

- [ ] **Step 3: Run → pass → commit**

Commit: `feat(orchestrator): per-agent digest + "anything waiting on me" inbox/approval summary`.

---

## Task 3: "Clean up" — gated teardown of terminal agents

**Files:** `internal/orchestrator/monitor.go` (+ tests); reuses the Phase B gate

The one mutating monitoring verb. It **proposes** `terminate`/`delete` of terminal (done/errored/exited) agents and routes the proposal through the **existing confirm gate** — never an automatic reap.

- [ ] **Step 1: Tests first**

  - **Proposes only terminal agents:** active agents are never in the proposed teardown set.
  - **Goes through the gate:** assert the gate is consulted; reject ⇒ `fakeDaemon.terminateCalls == 0`.
  - **Approve ⇒ terminates the proposed set** and reports what was reaped.
  - **Nothing terminal ⇒ "nothing to clean up"** with no gate prompt.

```go
func TestMonitor_CleanupProposesOnlyTerminalAndGates(t *testing.T) {
    fd := &fakeDaemon{sessions: []*store.Session{active("a1"), done("a2"), errored("a3")}}
    spy := &spyGate{decision: Decision{Action: Reject}}
    m := NewMonitorWithGate(fd, fakeCondenser{}, spy)
    _, _ = m.CleanUp(context.Background())
    require.Equal(t, 1, spy.confirmCalls)
    require.ElementsMatch(t, []string{"a2", "a3"}, spy.proposedIDs())
    require.Zero(t, fd.terminateCalls, "reject reaps nothing")
}
```

- [ ] **Step 2: Implement**

```go
func (m *Monitor) CleanUp(ctx context.Context) (string, error) {
    sessions, err := m.d.List(ctx)
    if err != nil { return "", err }
    terminal := filterTerminal(sessions) // done / errored / exited
    if len(terminal) == 0 { return "nothing to clean up — no terminal agents", nil }
    calls := toTerminateCalls(terminal) // []ToolCall, all mutating
    if d := m.gate.Confirm(calls); d.Action != Approve {
        return "left them as-is", nil
    }
    return m.runTeardown(ctx, d.Calls)
}
```

Reuse `Gate.Confirm` and `Registry.Dispatch` from Phase B verbatim — `CleanUp` only *selects* the candidates and hands them to the existing machinery. Wire the three monitoring entrypoints into the loop/REPL so the natural-language forms reach them (either as explicit registry tools the model can call, or as recognized intents — prefer registry tools so the existing loop/gate handle them with no special-casing).

- [ ] **Step 3: Full suite + commit**

```bash
cd /home/srjn45/dev/warden && go test ./internal/orchestrator/... && go build ./...
```

Commit: `feat(orchestrator): "clean up" — gated teardown of terminal agents`.

---

## Summary

Three TDD tasks, almost entirely composition over Phase B's registry, loop, and gate.

1. ✅ Fleet digest — condensed "what's running / what's stuck", deterministic fallback when the model is unavailable.
2. ✅ Per-agent digest + "anything waiting on me" (approvals + inbox).
3. ✅ "Clean up" — proposes terminal-agent teardown through the **existing** confirm gate; never auto-reaps.

**Invariants held:** monitoring reads auto-execute (low-stakes); the sole mutating verb reuses the Phase B gate; condensation always degrades to a deterministic table rather than erroring; teardown is always operator-confirmed.

**With D landed, the design is complete:** composition (B) + operator's-shell hosting (C) + supervision (D), all under the thin-translator scope — the local model conducts and summarizes; it never writes code, and every mutation is confirmed.

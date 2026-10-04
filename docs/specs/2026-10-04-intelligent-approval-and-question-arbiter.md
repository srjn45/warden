# Fast-Brain: Unified Gateway Architecture & Intelligent Arbiter

**Date:** 2026-10-04  
**Status:** Approved design spec  
**Scope:** Reusable Fast-Brain Gateway port (`internal/fastbrain`) for sub-second micro-decisions and deep reasoning across Warden, featuring the Intelligent Auto-Approval & Question Arbiter as the flagship capability.

---

## 1. Unified Brain Gateway Philosophy

Instead of siloed ad-hoc AI calls, all non-deterministic micro-decisions across Warden flow through a single **Brain Gateway Port** (`fastbrain.Engine`).

The engine provides:
1. **Tiered Model Routing**: Dispatches to **Fast-tier** (sub-second Haiku/Flash) or **Thinking-tier** (deep reasoning Sonnet/GPT-4o).
2. **Strict Timeouts & Fail-Open Resilience**: Hard context timeouts ($\le 1.5$s for Fast, $\le 10$s for Thinking) with zero-downtime deterministic fallbacks.
3. **Universal JSON Sanitization**: Reliable stripping of markdown backticks, conversational prefixes, and trailing commas into clean JSON.
4. **Centralized Observability**: Uniform structured logging, latency tracking, and token spend telemetry.

---

## 2. Core Port Interface (`internal/fastbrain`)

```go
package fastbrain

import (
    "context"
    "time"
)

// DecisionKind identifies which capability is requesting a decision.
type DecisionKind string

const (
    KindArbitrateApproval  DecisionKind = "arbitrate_approval"
    KindResolveAgentName   DecisionKind = "resolve_agent_name"
    KindDiagnoseFailure    DecisionKind = "diagnose_failure"
    KindSummarizeActivity  DecisionKind = "summarize_activity"
    KindGenerateCommit     DecisionKind = "generate_commit"
    KindRouteProfile       DecisionKind = "route_profile"
)

// Tier determines whether to route to a sub-second model or a reasoning model.
type Tier string

const (
    TierFast     Tier = "fast"     // Sub-second (Haiku, Flash, 4o-mini)
    TierThinking Tier = "thinking" // High-reasoning (Sonnet, o3-mini, extended thinking)
)

// Request defines the universal input envelope.
type Request struct {
    Kind         DecisionKind
    Tier         Tier
    Timeout      time.Duration // 0 defaults to tier default (1.5s for Fast, 10s for Thinking)
    SystemPrompt string
    UserContent  string
    JSONSchema   bool          // whether response must be parsed as JSON
}

// Response is the structured envelope returned by all Brain decisions.
type Response struct {
    RawOutput       string
    StructuredJSON  []byte
    ModelUsed       string
    Duration        time.Duration
    FallbackApplied bool
}

// Engine is the central Brain Port implemented in internal/fastbrain.
type Engine interface {
    Decide(ctx context.Context, req Request) (Response, error)
    
    // Domain helper methods built on top of Decide:
    ArbitrateApproval(ctx context.Context, in ArbiterInput) (ArbiterDecision, error)
    ResolveAgentName(ctx context.Context, prompt string) (string, error)
}
```

---

## 3. Flagship Capability: Intelligent Auto-Approval & Question Arbiter

### Universal Prompt Categorization
All four supported AI CLIs (Claude Code, Codex CLI, Cursor CLI, Antigravity CLI) already normalize prompts into `agentbackend.Approval`:

```go
type PromptCategory string

const (
    CategoryToolPermission    PromptCategory = "tool_permission"
    CategoryStrategicQuestion PromptCategory = "strategic_question"
    CategoryUnrecognized      PromptCategory = "unrecognized"
)

func ClassifyPrompt(a *agentbackend.Approval) PromptCategory {
    if a.AffirmativeIdx > 0 && (a.Action != "" || isPermissionQuestion(a.Question)) {
        return CategoryToolPermission
    }
    if a.AffirmativeIdx == 0 && len(a.Options) >= 2 {
        return CategoryStrategicQuestion
    }
    return CategoryUnrecognized
}
```

### Routing & Decision Flow
1. **Tool Permissions (`CategoryToolPermission`)**:
   - `Engine.Decide` invoked with `TierFast` ($\le 1.5$s timeout).
   - Prompt: Evaluates whether command (e.g. `Bash(go test ./...)`) is safe, non-destructive, and within the scope of the agent's task.
   - Output: `{"approve": true|false, "confidence": 0.0-1.0, "reason": "..."}`.
   - On `approve == true` and `confidence >= 0.8`: Dispatches `a.AffirmativeIdx`.
   - On `approve == false` or low confidence: Returns `DecisionEscalate` (leaves for human in `waiting_for_input`).

2. **Strategic Questions (`CategoryStrategicQuestion`)**:
   - `Engine.Decide` invoked with `TierThinking` ($\le 10$s timeout).
   - Prompt: Analyzes the agent's active plan goal, constraints, question, and numbered options `1..N`.
   - Output: `{"selected_option": <int>, "confidence": 0.0-1.0, "reason": "..."}`.
   - On valid option (1..len(options)) and `confidence >= 0.8`: Dispatches option number.
   - On ambiguity or low confidence: Returns `DecisionEscalate` (leaves for human).

---

## 4. Non-Overridable Safety Invariants

1. **Destructive Guard Runs First**: `approval.IsDestructive(a)` (`rm -rf`, `drop table`, `push --force`) runs **before** any AI model evaluation. Destructive actions are immediately blocked.
2. **Circuit Breaker**: `approveBreaker.Allow(...)` halts identical recurring prompts to prevent looping.
3. **Fail-Open to Human**: Any timeout, network error, malformed JSON, or low confidence ($< 0.8$) returns `DecisionEscalate`, gracefully leaving the prompt in `waiting_for_input` for the human.

---

## 5. Future Capability Extensions (Reusing the Same Port)

Because `Engine.Decide` is fully generic:
- **Crash Triage & Bug Reporting**: Invokes `Decide` with `KindDiagnoseFailure` on internal panics; prompts user for bug report approval.
- **TUI Live Activity Badge**: Invokes `Decide` with `KindSummarizeActivity` every 10–15s to produce a 3–5 word progress badge.
- **Conventional Commits**: Invokes `Decide` with `KindGenerateCommit` to synthesize task prompts and git diffs upon task completion.

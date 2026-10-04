# Fast-Brain: Intelligent Auto-Approval & Question Arbiter

**Date:** 2026-10-04  
**Status:** Approved design spec  
**Scope:** Autonomous inline decision-making for tool permissions and multiple-choice questions across all supported agent backends (Claude, Codex, Cursor, Antigravity).

---

## 1. Context & Motivation

Warden's auto-approval engine (`internal/approval`) currently relies on deterministic regexes and keyword matching (`approval.Policy`). While effective for known scripts, this creates two major points of friction in autonomous workflows (Autopilot, DAG Pipelines, long-running background workers):

1. **Rule Brittleness**: Legitimate, non-destructive commands (e.g. `Bash(pytest tests/)`, `FileEdit(config.go)`) that do not match an explicit regex stall in `waiting_for_input`.
2. **Question Paralysis**: When agents hit an architectural fork and present multiple-choice questions:
   ```text
   Which approach would you like to take?
   1. Maintain backward-compatible adapter
   2. Refactor schema directly
   ```
   Warden sees `AffirmativeIdx == 0` (no "yes/allow" token) and stops. Unattended pipelines stall indefinitely until a human attaches to the tmux pane.

The **Fast-Brain Arbiter** solves this by evaluating prompt safety and alignment against the active task goal and plan constraints, routing between **Fast-tier** models (sub-second tool permissions) and **Tier-1 Thinking** models (strategic forks).

---

## 2. Universal Prompt Categorization

All four supported AI CLIs (Claude Code, Codex CLI, Cursor CLI, Antigravity CLI) already normalize their interactive prompts into `agentbackend.Approval`:

```go
type Approval struct {
    Action            string   // e.g. "Bash(git status)", "$ curl ...", "echo ..."
    Question          string   // e.g. "Do you want to proceed?", "Which approach...?"
    Options           []string // 1-indexed list of choices on screen
    SelectedIdx       int      // 1-based currently selected option
    AffirmativeIdx    int      // 1-based index of "yes/run/allow"; 0 if none
    AffirmativeSticky bool     // standing grant flag
}
```

Prompts are deterministically categorized into two distinct decision paths:

```
                            Approval Prompt
                                   │
                    ┌──────────────┴──────────────┐
                    ▼                             ▼
       [ Tool Permission (y/n) ]       [ Strategic Question ]
       • Action != ""                  • AffirmativeIdx == 0
       • AffirmativeIdx > 0            • len(Options) >= 2
                    │                             │
                    ▼                             ▼
            Fast Model (~400ms)         Tier-1 Thinking Model (~6s)
          (Haiku / Flash / 4o-mini)       (Sonnet / Extended Thinking)
                    │                             │
          Is it safe & aligned?           Which option best satisfies
          (Sub-second response)             the plan constraints?
```

### Deterministic Categorization Function

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

---

## 3. Two-Tier Decision Architecture

### Tier 1: Fast-Tier Model (Tool Permissions)
- **Target Models**: Claude 3.5 Haiku, Gemini 1.5 Flash, GPT-4o-mini.
- **Latency Budget**: Hard timeout $\le 1.5$ seconds.
- **Task**: Evaluates:
  1. Does this command fit the scope of the agent's task?
  2. Is it safe and non-destructive?
- **Response Format**: Compact JSON:
  ```json
  {
    "approve": true,
    "confidence": 0.95,
    "reason": "Running unit tests aligns with verification task"
  }
  ```

### Tier 2: Thinking / High-Reasoning Model (Strategic Questions)
- **Target Models**: Claude 3.5 Sonnet / Extended Thinking, GPT-4o, or Autopilot Brain.
- **Latency Budget**: Timeout $\le 10$ seconds.
- **Task**: Evaluates:
  1. Plan goal, constraints, and current task prompt.
  2. Analyzes trade-offs between `Option 1..N`.
  3. Selects the option that best fulfills the user's architectural intent.
- **Response Format**: Compact JSON:
  ```json
  {
    "selected_option": 1,
    "confidence": 0.92,
    "reason": "Option 1 maintains backwards compatibility per plan constraint #2"
  }
  ```

---

## 4. Safety & Invariants

1. **Destructive Guard Runs First**: `approval.IsDestructive(a)` (e.g. `rm -rf`, `drop table`, `git reset --hard`) is evaluated **before** Fast-Brain and is completely non-overridable. Destructive commands are blocked immediately.
2. **Circuit Breaker**: `approveBreaker.Allow(...)` tracks identical prompt signatures. If the same action or question is answered repeatedly without unblocking the agent, the breaker trips and forces human escalation.
3. **Fail-Open to Human**: If Fast-Brain encounters a timeout, network failure, malformed JSON, or low confidence ($< 0.8$), it gracefully returns `Escalate` (leaving the agent in `waiting_for_input` for the operator).
4. **Audit Logging**: Every Fast-Brain decision (category, model used, latency, confidence, rationale) is written to daemon structured logs (`slog.Info`).

---

## 5. Implementation Structure

```text
internal/fastbrain/
├── runner.go           // Unified Runner interface + sub-second timeout wrappers
├── classify.go         // PromptCategory classifier (ToolPermission vs StrategicQuestion)
├── arbiter.go          // ArbitrateApproval: fast-tier tool evaluation & thinking question arbiter
├── templates.go        // Compact zero-shot system prompts with JSON schema constraints
├── arbiter_test.go     // Mock-based unit tests for all 4 backends & error cases
```

---

## 6. Poller Integration

In `internal/poller/poller.go` (`tryAutoApprove`):
1. Run deterministic checks: `IsDestructive`, `approveBreaker`, explicit `deny` rules.
2. If unresolved by static allow rules, call `fastbrain.Arbitrate(...)`.
3. If `Approve`: send `a.AffirmativeIdx`.
4. If `SelectOption`: send `decision.SelectedOption`.
5. If `Escalate`: log reason and leave as `waiting_for_input` (or forward to Autopilot Brain if in autopilot run).

package repl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/config"
	"github.com/srjn45/warden/internal/llm"
)

// escalationTimeout bounds the single planning call to the escalation model.
const escalationTimeout = 45 * time.Second

// NewRouterFromConfig builds a Router from warden config: the model's planning
// tier comes from local_llm_tier (or the model name when "auto"), and escalation
// from local_llm_escalate. Classification is the deterministic heuristic (no
// model call) by default, or — when local_llm_classifier is "model" and a
// Completer is available — a one-shot local-model classification that falls back
// to the heuristic on any error. Escalation shells one bounded `claude -p`.
//
// comp may be nil (no local model wired); then classification stays heuristic
// regardless of the config value.
func NewRouterFromConfig(cfg config.Config, comp llm.Completer) *Router {
	tier, ok := ParseTier(cfg.GetLocalLLMTier())
	if !ok {
		tier = modelTier(cfg.LocalLLM.Model)
	}
	// Escalation planning is internal thinking, so it must never make a paid call
	// (docs/specs/2026-08-06-backend-registry.md §7). The CLI repl cannot safely
	// share the daemon's backend-registry store (ScrivaDB is single-process), so it
	// escalates to the local model — the terminal candidate of every free/local
	// walk — instead of the former headless `claude -p`. With no local model the
	// escalator degrades cleanly (planning fails, the confirm gate stays the
	// backstop) rather than paying.
	var esc Escalator
	if cfg.GetLocalLLMEscalate() {
		esc = &completerEscalator{comp: comp}
	}
	var cls Classifier = heuristicClassifier{}
	if comp != nil && strings.EqualFold(cfg.GetLocalLLMClassifier(), "model") {
		cls = modelClassifier{comp: comp, fallback: heuristicClassifier{}}
	}
	return NewRouter(tier, cfg.GetLocalLLMEscalate(), cls, esc)
}

// heuristicClassifier buckets a request's needed tier from cheap surface signals
// (conjunctions, list separators, the word "pipeline") — no model call, so it
// never blocks the operator. Conservative: when in doubt it returns a low tier,
// which routes to a local plan, and the confirm gate is still the backstop.
type heuristicClassifier struct{}

func (heuristicClassifier) NeededTier(_ context.Context, line string) (Tier, error) {
	l := strings.ToLower(line)
	score := strings.Count(l, " and ") + strings.Count(l, ",") + strings.Count(l, " then ")
	if strings.Contains(l, "pipeline") {
		score += 2
	}
	switch {
	case score >= 2:
		return T2, nil
	case score == 1:
		return T1, nil
	default:
		return T0, nil
	}
}

// completerEscalator drafts a plan with an internal-thinking completer when the
// local model is under-tier. It asks for a strict JSON array of {name, args} tool
// calls (the same calls a local plan would produce) and parses it; the
// orchestrator then runs them locally through the confirm gate. It routes through
// a free/local completer (the local model in the CLI repl), never a paid backend —
// the §7 free/local rail applies to escalation too.
type completerEscalator struct {
	comp    llm.Completer
	timeout time.Duration
}

func (e *completerEscalator) Plan(ctx context.Context, line string, tools []llm.ToolSchema) ([]ToolCall, error) {
	if e.comp == nil {
		return nil, errors.New("escalation unavailable: no local model configured")
	}
	timeout := e.timeout
	if timeout <= 0 {
		timeout = escalationTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out, err := e.comp.Complete(cctx, buildEscalationPrompt(line, tools))
	if err != nil {
		return nil, fmt.Errorf("escalation planning: %w", err)
	}
	return parsePlanJSON(out)
}

func buildEscalationPrompt(line string, tools []llm.ToolSchema) string {
	var b strings.Builder
	b.WriteString("You are planning warden tool calls. You never write code; ")
	b.WriteString("to do code work, plan a spawn_agent call. Output ONLY a JSON array of ")
	b.WriteString("{\"name\":<tool>,\"args\":{...}} objects — no prose, no markdown.\n\n")
	if len(tools) > 0 {
		b.WriteString("Available tools:\n")
		for _, t := range tools {
			fmt.Fprintf(&b, "- %s: %s\n", t.Name, t.Description)
		}
		b.WriteString("\n")
	}
	b.WriteString("Operator request: ")
	b.WriteString(line)
	return b.String()
}

// parsePlanJSON extracts the JSON tool-call array from Claude's reply, tolerating
// surrounding prose or a ```json fence.
func parsePlanJSON(s string) ([]ToolCall, error) {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '['); i >= 0 {
		if j := strings.LastIndexByte(s, ']'); j > i {
			s = s[i : j+1]
		}
	}
	var raw []struct {
		Name string         `json:"name"`
		Args map[string]any `json:"args"`
	}
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, fmt.Errorf("parse escalated plan: %w", err)
	}
	calls := make([]ToolCall, 0, len(raw))
	for _, r := range raw {
		if r.Name == "" {
			continue
		}
		args := r.Args
		if args == nil {
			args = map[string]any{}
		}
		calls = append(calls, ToolCall{Name: r.Name, Args: args})
	}
	return calls, nil
}

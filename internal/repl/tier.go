package repl

import (
	"context"
	"strings"

	"github.com/srjn45/warden/internal/llm"
)

// Tier is a request's (or model's) capability bucket. T0 is cheap classification
// and trivial single-verb asks; T1 is routine multi-step composition; T2 is
// hard, ambiguous, multi-agent planning where a small local model is unreliable.
type Tier int

const (
	T0 Tier = iota
	T1
	T2
)

// modelTier maps an Ollama model name to the planning tier it can be trusted at.
// Conservative by default (unknown ⇒ T0). The 3b→T1 / 7b→T2 boundaries are a
// starting calibration (a spec open question), tunable via local_llm_tier.
func modelTier(model string) Tier {
	m := strings.ToLower(model)
	switch {
	case containsAny(m, "32b", "70b", "72b", "14b", "13b", "8b", "7b", "9b"):
		return T2
	case containsAny(m, "3b", "4b", "2b"):
		return T1
	default:
		return T0
	}
}

// ParseTier reads an explicit local_llm_tier override ("auto"|"t0"|"t1"|"t2").
// An empty/"auto"/unknown value returns (0, false) so the caller derives the
// tier from the model name instead.
func ParseTier(s string) (Tier, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "t0":
		return T0, true
	case "t1":
		return T1, true
	case "t2":
		return T2, true
	default:
		return T0, false
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// Classifier buckets a request's needed tier with one cheap T0 call (any model
// can do it). Best-effort: an error means "couldn't tell".
type Classifier interface {
	NeededTier(ctx context.Context, line string) (Tier, error)
}

// Escalator drafts a plan with headless Claude (`claude -p`), returning the SAME
// confirm-gate tool calls a local plan would. Only the rare hard plan spends
// tokens; execution stays local warden calls.
type Escalator interface {
	Plan(ctx context.Context, line string, tools []llm.ToolSchema) ([]ToolCall, error)
}

// RouteMode is the planning route chosen for a request.
type RouteMode int

const (
	// PlanLocal: the local model plans this turn (needed tier ≤ model tier).
	PlanLocal RouteMode = iota
	// Escalate: draft the plan with headless Claude, then execute locally.
	Escalate
	// Degrade: tell the operator honestly rather than attempt over-tier.
	Degrade
)

// Route is the routing decision for one operator line.
type Route struct {
	Mode            RouteMode
	OperatorMessage string     // set on Degrade
	Calls           []ToolCall // set on a successful Escalate
}

// Router decides, before the expensive planning turn, whether the local model
// can handle a request, whether to escalate the single planning step to Claude,
// or whether to degrade honestly.
type Router struct {
	modelTier Tier
	escalate  bool
	cls       Classifier
	esc       Escalator
}

// NewRouter builds a router. esc may be nil when escalate is off.
func NewRouter(model Tier, escalate bool, cls Classifier, esc Escalator) *Router {
	return &Router{modelTier: model, escalate: escalate, cls: cls, esc: esc}
}

// Route compares the request's needed tier to the model's tier. Best-effort: a
// classifier error defaults to attempting locally (the gate is still the
// backstop) rather than blocking the operator.
func (r *Router) Route(ctx context.Context, line string) Route {
	if r == nil || r.cls == nil {
		return Route{Mode: PlanLocal}
	}
	needed, err := r.cls.NeededTier(ctx, line)
	if err != nil {
		return Route{Mode: PlanLocal} // can't tell ⇒ try locally
	}
	if needed <= r.modelTier {
		return Route{Mode: PlanLocal}
	}
	if !r.escalate || r.esc == nil {
		return Route{Mode: Degrade, OperatorMessage: degradeMessage(needed, r.modelTier)}
	}
	calls, err := r.esc.Plan(ctx, line, nil)
	if err != nil {
		return Route{Mode: Degrade, OperatorMessage: "tried to escalate to Claude but it failed: " + err.Error()}
	}
	return Route{Mode: Escalate, Calls: calls}
}

func degradeMessage(needed, have Tier) string {
	return "this needs a more capable model than the one configured (needs " +
		tierName(needed) + ", have " + tierName(have) +
		"); enable local_llm_escalate to draft it with Claude, run a bigger local model, or break it into smaller steps"
}

func tierName(t Tier) string {
	switch t {
	case T2:
		return "T2"
	case T1:
		return "T1"
	default:
		return "T0"
	}
}

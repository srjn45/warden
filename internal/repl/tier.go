package repl

import "context"

// RouteMode is the planning route chosen for a request.
//
// Fast-Brain is the planner: every turn goes through the injected llm.Chatter
// (a fastbrain.FastBrainChatter in the CLI). The former T0/T1/T2 under-tier
// escalation to a heavier model is gone, so the only live route is PlanLocal.
// The type and the Router are kept for one release so existing call sites
// (NewRouterFromConfig in the CLI) keep compiling.
type RouteMode int

const (
	// PlanLocal: the injected Chatter plans this turn. The only route produced.
	PlanLocal RouteMode = iota
)

// Route is the routing decision for one operator line.
type Route struct {
	Mode RouteMode
}

// Router is a compat shim: it never escalates, degrades, or refuses a request.
type Router struct{}

// NewRouter returns the always-PlanLocal router.
func NewRouter() *Router { return &Router{} }

// Route always plans via the Chatter.
func (r *Router) Route(context.Context, string) Route { return Route{Mode: PlanLocal} }

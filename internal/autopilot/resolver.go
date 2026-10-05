package autopilot

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Resolver agent (run-to-final-pr spec §D). When the mechanical rungs and the
// fix loop cannot clear a blocker, SpawnResolver starts a short-lived worker on
// the exact PR branch, tagged "resolver", with a strict attempt-a-fix-and-exit
// brief. It never touches gh or run state: the resolver's push/exit is picked up
// by the normal landing loop.

// MaxResolverAttempts caps resolver spawns per PR (branch) for a run. Past it the
// run fails open by parking as needs-attention.
const MaxResolverAttempts = 3

// KindResolverExhausted: the resolver was tried MaxResolverAttempts times for one
// blocker and could not clear it.
const KindResolverExhausted FailureKind = "resolver_exhausted"

// Blocker classes passed to the resolver.
const (
	BlockerRedGate      = "red_gate"
	BlockerManagerStall = "manager_stall"
)

// ResolverSpawn describes a resolver worker to start on a PR branch.
type ResolverSpawn struct {
	RunID   string
	Repo    string
	TaskID  string
	Branch  string
	Class   string
	Attempt int
	Prompt  string
}

// ResolverRuntime is the optional daemon seam that actually starts the resolver
// worker (new worktree on Branch, tag "resolver") and returns its session id.
// It must return promptly: it does not wait for the resolver.
type ResolverRuntime interface {
	SpawnResolver(ctx context.Context, spec ResolverSpawn) (string, error)
}

// ResolverRequest is one blocker handed to the resolver.
type ResolverRequest struct {
	RunID  string
	TaskID string
	Branch string // the exact PR branch; "" falls back to the run's integration branch
	Class  string
	// Detail is the stall diagnosis or failure reason/evidence.
	Detail string
}

// SpawnResolver hands the blocker to a resolver agent. It reports whether one was
// started; false with a nil error means the run was parked because the per-PR cap
// was exhausted (fail-open). Acquires c.mu — use spawnResolverLocked when held.
func (c *Controller) SpawnResolver(ctx context.Context, req ResolverRequest) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.runs[req.RunID]
	if !ok {
		return false, fmt.Errorf("resolver: unknown run %q", req.RunID)
	}
	return c.spawnResolverLocked(ctx, r, req)
}

func (c *Controller) spawnResolverLocked(ctx context.Context, r *run, req ResolverRequest) (bool, error) {
	rt, ok := c.runtime.(ResolverRuntime)
	if !ok {
		return false, errors.New("resolver: runtime cannot spawn resolvers")
	}
	gr, _ := c.runtime.(GuardianRuntime)
	branch := firstNonEmpty(req.Branch, r.integrationBranch)
	key := firstNonEmpty(branch, "run:"+r.runID)

	if r.resolverAttempts[key] >= MaxResolverAttempts {
		// Park only once per exhausted blocker (park is idempotent state-wise but
		// would re-notify on every landing tick).
		if r.needsAttention == "" && gr != nil {
			c.park(gr, r, KindResolverExhausted, fmt.Errorf("the resolver tried %d times and could not clear %s on %s", MaxResolverAttempts, req.Class, key))
		}
		return false, nil
	}
	attempt := r.resolverAttempts[key] + 1
	spec := ResolverSpawn{
		RunID: r.runID, Repo: r.repo, TaskID: req.TaskID, Branch: branch,
		Class: req.Class, Attempt: attempt,
		Prompt: composeResolverPrompt(r, req, branch, attempt),
	}
	id, err := rt.SpawnResolver(ctx, spec)
	if err != nil {
		return false, err // not counted: a failed start is not an attempt
	}
	if r.resolverAttempts == nil {
		r.resolverAttempts = map[string]int{}
	}
	r.resolverAttempts[key] = attempt
	if gr != nil {
		gr.AuditRunEvent(ctx, r.runID, "autopilot.resolver_spawned", id,
			fmt.Sprintf("class=%s task=%s branch=%s attempt=%d/%d", req.Class, req.TaskID, branch, attempt, MaxResolverAttempts))
	}
	return true, nil
}

// composeResolverPrompt builds the brief: task constraints, the blocker, and the
// strict attempt-a-fix-and-exit instruction.
func composeResolverPrompt(r *run, req ResolverRequest, branch string, attempt int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are a RESOLVER agent for autopilot run %s (attempt %d of %d), working on branch %s.\n\n", r.runID, attempt, MaxResolverAttempts, branch)
	if g := strings.TrimSpace(r.plan.Goal); g != "" {
		fmt.Fprintf(&b, "Plan goal: %s\n", g)
	}
	if len(r.plan.Constraints) > 0 {
		b.WriteString("Constraints you must respect:\n")
		for _, c := range r.plan.Constraints {
			fmt.Fprintf(&b, "- %s\n", c)
		}
	}
	fmt.Fprintf(&b, "\nBlocker (%s", req.Class)
	if req.TaskID != "" {
		fmt.Fprintf(&b, ", task %s", req.TaskID)
	}
	fmt.Fprintf(&b, "):\n%s\n\n", strings.TrimSpace(req.Detail))
	b.WriteString("Instructions: attempt a fix for this blocker on this branch, run `wd check`, commit and push, then exit with `wd job done`. " +
		"Do this autonomously and NEVER wait for a human or ask questions — if you cannot fix it, push whatever progress is safe, state why in one sentence, and exit anyway. " +
		"Do NOT merge any PR, touch the default branch, change the plan goal or constraints, or spawn other agents; autopilot lands the PR after you exit.\n")
	return b.String()
}

package autopilot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Usage-limit recovery owned by the guardian (spec 2026-10-05 §J.1). A run agent
// — the manager or any worker — that is rate-limited or out of quota is first
// moved in place onto another selectable backend (the daemon's existing
// hot-swap / usage-recovery path does the swap; autopilot only adds the policy
// and the trigger). When nothing is selectable the earliest known reset is
// recorded as the agent's resting_until and the agent is resumed at that
// instant through the existing rate-limit resume path. Nothing here waits for
// the manager to notice a limited worker.
const (
	// limitResumeBuffer is added to a known reset instant before resuming, so the
	// provider has actually cleared the window.
	limitResumeBuffer = 30 * time.Second
	// limitFallbackRetry is the wait when no reset time is known (a monthly spend
	// banner carries none). Generous: the limit will not clear sooner.
	limitFallbackRetry = 30 * time.Minute
	// limitRecheckDelay is how long after a resume the agent gets to prove the
	// limit cleared before the guardian re-evaluates it once.
	limitRecheckDelay = 2 * time.Minute
	// limitSwitchGrace is how long an in-flight backend switch gets to settle
	// before the guardian asks again.
	limitSwitchGrace = 5 * time.Minute
)

// ErrNoAlternateBackend is returned (wrapped) by LimitRuntime.SwitchLimited when
// no other backend is selectable right now.
var ErrNoAlternateBackend = errors.New("no alternate backend selectable")

// LimitSwitch describes a SwitchLimited outcome. On success From/To name the
// backends; on ErrNoAlternateBackend Reset carries the earliest known reset of the
// limited backends (zero when none is parseable).
type LimitSwitch struct {
	From  string
	To    string
	Reset time.Time
}

// LimitRuntime is the optional runtime slice that moves a rate-limited agent onto
// another selectable backend in place, continuing the same task from its
// handoff. The daemon implements it by delegating to BackendRecoveryCoordinator —
// there is no second swap path. A runtime without it leaves limit handling to the
// daemon's own recovery exactly as before.
type LimitRuntime interface {
	SwitchLimited(ctx context.Context, agentID string) (LimitSwitch, error)
}

// restingAgent is one limited run agent the guardian is tending.
type restingAgent struct {
	until     time.Time // when the guardian next acts on this agent
	switching bool      // a backend switch was dispatched; until is its settle deadline
	resumed   bool      // a timed resume was already issued; until is the re-check
	fallback  bool      // until is the no-reset-time fallback, not a provider reset
}

// restingNow reports whether any run agent is resting until a future instant.
func (r *run) restingNow(now time.Time) bool {
	for _, e := range r.resting {
		if now.Before(e.until) {
			return true
		}
	}
	return false
}

// managerResting reports whether the manager itself is resting on a limit, in
// which case its silence is the limit's doing, not a wedge.
func (r *run) managerResting(now time.Time) bool {
	if r.brain == nil || r.brain.AgentID == "" {
		return false
	}
	e, ok := r.resting[r.brain.AgentID]
	return ok && now.Before(e.until)
}

// earliestResting returns the soonest instant the guardian acts on a limited agent.
func (r *run) earliestResting() (time.Time, bool) {
	var at time.Time
	found := false
	for _, e := range r.resting {
		if !found || e.until.Before(at) {
			at, found = e.until, true
		}
	}
	return at, found
}

// limitTick tends every live run's limited agents once. Like the overwatch it
// no-ops when the runtime lacks the optional seams, honors paused/stopped runs and
// holds c.mu for the pass (the runtime calls only dispatch; the swap itself runs
// on the coordinator's own goroutine).
func (c *Controller) limitTick(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	lr, ok1 := c.runtime.(LimitRuntime)
	ow, ok2 := c.runtime.(OverwatchRuntime)
	ev, ok3 := c.runtime.(EvidenceRuntime)
	gr, ok4 := c.runtime.(GuardianRuntime)
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return
	}
	now := c.now()
	for _, r := range c.runs {
		if r.state == StatePaused || r.state == StateStopped || r.state == StateComplete ||
			r.state == StateRegistered || r.state == StateDisabled {
			continue
		}
		c.limitRun(ctx, lr, ow, ev, gr, r, now)
	}
}

func (c *Controller) limitRun(ctx context.Context, lr LimitRuntime, ow OverwatchRuntime, ev EvidenceRuntime, gr GuardianRuntime, r *run, now time.Time) {
	roster, err := ow.RunAgents(ctx, r.runID)
	if err != nil {
		return // keep what we know; try again next tick
	}
	limited := map[string]bool{}
	for _, a := range roster {
		if a.State == "rate_limited" {
			limited[a.ID] = true
		}
	}
	for id := range r.resting { // cleared, swapped away or gone: nothing left to tend
		if !limited[id] {
			delete(r.resting, id)
		}
	}
	for _, a := range roster {
		if !limited[a.ID] {
			continue
		}
		ent := r.resting[a.ID]
		if ent != nil && now.Before(ent.until) {
			continue // resting: a scheduled action is pending
		}
		if ent != nil && !ent.switching && !ent.resumed {
			// Reset reached: resume in place through the existing rate-limit path,
			// then give it one re-check window.
			if err := ev.ResumeRateLimit(ctx, a.ID); err != nil {
				slog.Warn("autopilot limits: resume failed", "run", r.runID, "agent", a.ID, "err", err)
			}
			ent.resumed = true
			ent.until = now.Add(limitRecheckDelay)
			gr.AuditRunEvent(ctx, r.runID, "autopilot_limit_resumed", a.ID, "reset reached; resumed in place")
			continue
		}
		// New limit, a settled/failed switch, or still limited after the resume:
		// look for an alternate backend (one is re-checked once the limit is still up).
		sw, err := lr.SwitchLimited(ctx, a.ID)
		switch {
		case err == nil:
			r.setResting(a.ID, &restingAgent{until: now.Add(limitSwitchGrace), switching: true})
			gr.AuditRunEvent(ctx, r.runID, "autopilot_limit_switched", a.ID,
				fmt.Sprintf("from=%s to=%s; continuing the same task from its handoff", sw.From, sw.To))
		default:
			if !errors.Is(err, ErrNoAlternateBackend) {
				slog.Warn("autopilot limits: switch failed", "run", r.runID, "agent", a.ID, "err", err)
			}
			c.scheduleResume(ctx, gr, r, a.ID, sw.Reset, now)
		}
	}
}

// scheduleResume records the earliest known reset (the daemon's reading, else the
// tierstate's) as the agent's resting_until, falling back to a fixed retry when no
// reset time is parseable, so a limited agent always carries a next action.
func (c *Controller) scheduleResume(ctx context.Context, gr GuardianRuntime, r *run, agentID string, reset, now time.Time) {
	if reset.IsZero() || !reset.After(now) {
		if t, ok := c.tierstate.earliestReset(); ok {
			reset = t
		}
	}
	ent := &restingAgent{}
	if reset.After(now) {
		ent.until = reset.Add(limitResumeBuffer)
	} else {
		ent.until = now.Add(limitFallbackRetry)
		ent.fallback = true
	}
	r.setResting(agentID, ent)
	detail := "no alternate backend; resume at " + ent.until.UTC().Format(time.RFC3339)
	if ent.fallback {
		detail += " (no reset time known — fallback retry)"
	}
	gr.AuditRunEvent(ctx, r.runID, "autopilot_limit_resume_scheduled", agentID, detail)
}

func (r *run) setResting(id string, e *restingAgent) {
	if r.resting == nil {
		r.resting = map[string]*restingAgent{}
	}
	r.resting[id] = e
}

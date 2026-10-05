package autopilot

import (
	"fmt"
	"time"
)

// NextStep is the guardian's scheduled next action for a run and its owner
// (spec 2026-10-05 §J.2). A run that is not complete, paused, stopped, parked
// (needs-attention) or merely registered always carries one.
type NextStep struct {
	Action string `json:"action"`
	// At is the RFC3339 instant the action fires; empty only when the action
	// waits for an operator (a parked run).
	At string `json:"at,omitempty"`
	// Owner is who performs it: guardian | overwatch | operator.
	Owner string `json:"owner"`
}

// nextStepLocked derives r's next scheduled action from its state, heal stage and
// resting agents; the caller holds c.mu. nil means the run has none by design
// (complete, paused, stopped, registered, disabled).
func (c *Controller) nextStepLocked(r *run, now time.Time) *NextStep {
	switch r.state {
	case StateComplete, StatePaused, StateStopped, StateRegistered, StateDisabled:
		return nil
	}
	if r.needsAttention != "" {
		return &NextStep{Action: "waiting for you: " + r.needsAttention, Owner: "operator"}
	}
	tick := now.Add(c.guardian.Interval)
	step := func(action string, at time.Time) *NextStep {
		if at.IsZero() {
			at = tick
		}
		return &NextStep{Action: action, At: rfc3339OrEmpty(at), Owner: "guardian"}
	}
	if at, ok := r.earliestResting(); ok {
		return step("resume or switch a rate-limited agent", at)
	}
	switch r.healStage {
	case stageNudged:
		return step("restart the manager if it shows no heartbeat", r.healNextAt)
	case stageRestarted:
		return step("rotate the manager to the next backend if it shows no heartbeat", r.healNextAt)
	case stageRotated:
		return step("back off (or park) if the rotated manager shows no heartbeat", r.healNextAt)
	case stageBackoff:
		return step("retry the backend ladder from the top", r.backoffNextRetry)
	}
	switch {
	case r.brain == nil || r.brain.AgentID == "":
		return step("spawn the manager", r.healNextAt)
	case r.state == StateStarting:
		return step("verify the manager is alive", r.brainSpawnedAt.Add(c.guardian.HeartbeatTimeout))
	case r.state == StateHealing || r.state == StateDegraded:
		return step("verify the manager recovered", r.healNextAt)
	}
	return step("supervise: landing, overwatch and stall triage", tick)
}

// nextStepViolation names a missing schedule, or "" when r honors the invariant:
// every degraded, healing or backoff run carries an explicit next action time.
func (c *Controller) nextStepViolation(r *run, now time.Time) string {
	ns := c.nextStepLocked(r, now)
	if ns == nil || ns.Owner == "operator" {
		return ""
	}
	if r.restingNow(now) {
		return ""
	}
	switch {
	case r.healStage == stageBackoff && r.backoffNextRetry.IsZero():
		return "backoff without a retry time"
	case (r.healStage == stageNudged || r.healStage == stageRestarted || r.healStage == stageRotated) && r.healNextAt.IsZero():
		return fmt.Sprintf("heal stage %d without a next-step time", r.healStage)
	}
	return ""
}

// invariantHook, when set (tests), receives every nextStepViolation the guardian
// tick observes.
var invariantHook func(runID, violation string)

func (c *Controller) checkNextStep(r *run, now time.Time) {
	if invariantHook == nil {
		return
	}
	if v := c.nextStepViolation(r, now); v != "" {
		invariantHook(r.runID, v)
	}
}

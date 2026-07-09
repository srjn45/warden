package autopilot

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// healStage is the brain's position on the guardian heal ladder (autopilot.md
// §2.3). It escalates on each wedge that outlives the previous step's grace, and
// resets to healthy the moment a fresh heartbeat proves the brain recovered.
type healStage int

const (
	stageHealthy   healStage = iota // brain alive and heartbeating
	stageNudged                     // sent a steering message (stage 1)
	stageRestarted                  // restarted on the same backend, fresh context (stage 2)
	stageRotated                    // rotated onto another backend down the ladder (stage 3)
	stageBackoff                    // ladder exhausted: capped-exponential wait, forever (stage 4)
)

// guardianNudge is the steering message the guardian sends as its cheapest heal
// step. It nudges a quiet brain back to its loop without disrupting its context.
const guardianNudge = "autopilot guardian: you have gone quiet past the heartbeat timeout — " +
	"re-read the plan, reconcile the ledger against list_agents, and continue the run."

// RunGuardian is the daemon-launched heartbeat guardian loop (autopilot.md §2.3),
// built in the worktree_prune.go / scheduler.go pattern: it ticks on the
// configured interval until ctx is cancelled. A runtime that does not implement
// GuardianRuntime (the S1 inert core, the S3 lifecycle fakes) makes every tick a
// no-op, so wiring it in unconditionally is safe. Launched from server.go.
func (c *Controller) RunGuardian(ctx context.Context) {
	interval := c.guardian.Interval
	if interval <= 0 {
		interval = 60 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.guardianTick(ctx)
		}
	}
}

// guardianTick supervises every live run once. It honors the kill switch (a
// disabled controller does nothing) and no-ops when the runtime cannot support a
// guardian. It holds c.mu for the whole pass — matching Enable, so run state stays
// consistent — which is fine at the guardian's generous cadence.
func (c *Controller) guardianTick(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.enabled {
		return
	}
	gr, ok := c.runtime.(GuardianRuntime)
	if !ok {
		return
	}
	now := c.now()
	for _, r := range c.runs {
		c.superviseRun(ctx, gr, r, now)
	}
}

// superviseRun runs the heal state machine for one run on this tick (§2.3). A
// fresh heartbeat clears the ladder (and may trigger a planned rotation on context
// pressure); a stale one escalates once the current step's grace has elapsed.
func (c *Controller) superviseRun(ctx context.Context, gr GuardianRuntime, r *run, now time.Time) {
	if r.tried == nil {
		r.tried = map[string]bool{}
	}

	hb := r.brainSpawnedAt // a cold-started brain heartbeats from its spawn instant
	if act, ok := gr.BrainActivity(ctx, r.runID); ok && act.After(hb) {
		hb = act
	}
	r.lastHeartbeat = hb

	level := ""
	if r.brain != nil && r.brain.AgentID != "" {
		level = gr.BrainContextLevel(ctx, r.brain.AgentID)
	}
	r.contextLevel = level

	alive := r.brain != nil && r.brain.AgentID != ""
	fresh := alive && !hb.IsZero() && now.Sub(hb) < c.guardian.HeartbeatTimeout

	if fresh {
		if r.healStage != stageHealthy {
			c.recover(r)
		}
		r.state = StateActive
		// Planned rotation: a healthy brain whose context has reached the configured
		// level is cold-started on a freshly selected backend (§2.3, §7). A cooldown
		// stops it thrashing while the fresh brain's context settles.
		if contextTriggersRotation(level, c.guardian.RotateAtContext) && !now.Before(r.plannedRotateNextAt) {
			c.plannedRotate(ctx, gr, r, now)
		}
		return
	}

	// Wedged (stale heartbeat, or the brain is gone). Wait out the current step's
	// grace so an escalation gets a full heartbeat window to prove itself.
	if now.Before(r.healNextAt) {
		return
	}
	r.state = StateHealing
	c.escalate(ctx, gr, r, now)
}

// recover clears the heal ladder after a brain proves alive again: the cycle
// restarts from healthy, the tried-backend set and backoff are reset.
func (c *Controller) recover(r *run) {
	r.healStage = stageHealthy
	r.healNextAt = time.Time{}
	r.backoffStage = 0
	r.backoffNextRetry = time.Time{}
	r.backoffLastErr = ""
	r.tried = map[string]bool{}
}

// escalate advances one rung up the heal ladder (§2.3). Backoff expiry retries the
// whole ladder from stage 1 (forever); a brain that is entirely gone jumps
// straight to (re)spawn via the rotate step.
func (c *Controller) escalate(ctx context.Context, gr GuardianRuntime, r *run, now time.Time) {
	// Backoff elapsed → retry the ladder from the top (§2.3 stage 4 loops forever).
	// The tried set is cleared so a backend freed during the wait re-qualifies; the
	// backoff exponent is deliberately kept so repeated full-ladder failures keep
	// widening the wait up to the cap.
	if r.healStage == stageBackoff {
		r.tried = map[string]bool{}
		r.healStage = stageHealthy
	}

	// No brain at all (a prior rotate could not spawn): the only meaningful step is
	// to (re)spawn on a freshly selected backend.
	if r.brain == nil || r.brain.AgentID == "" {
		c.rotateStep(ctx, gr, r, now)
		return
	}

	switch r.healStage {
	case stageHealthy:
		// Stage 1 — nudge the existing brain.
		if err := gr.NudgeBrain(ctx, r.brain.AgentID, guardianNudge); err != nil {
			slog.Warn("autopilot guardian: nudge failed", "run", r.runID, "err", err)
		}
		r.healStage = stageNudged
		r.healNextAt = now.Add(c.guardian.HeartbeatTimeout)
		c.escalated(gr, r, "nudge", "brain quiet past heartbeat timeout — sent a steering nudge")
	case stageNudged:
		// Stage 2 — restart on the same backend with a fresh context (cold-start).
		cur := brainBackend(r)
		r.tried[cur] = true
		if err := c.rotateBrain(ctx, r, cur); err != nil {
			slog.Warn("autopilot guardian: restart failed", "run", r.runID, "err", err)
			r.state = StateDegraded
		}
		r.healStage = stageRestarted
		r.healNextAt = now.Add(c.guardian.HeartbeatTimeout)
		c.escalated(gr, r, "restart", "nudge did not revive the brain — restarted it (same backend, fresh context)")
	default:
		// Stage 3 — rotate down the ladder to the next available backend.
		c.rotateStep(ctx, gr, r, now)
	}
}

// rotateStep rotates the brain onto the next selectable backend not yet tried this
// cycle, cold-starting from the digest (§7). When nothing is selectable it enters
// backoff (§2.3 stage 4). Used both to walk down the ladder (stage 3) and to
// (re)spawn a brain that is entirely gone.
func (c *Controller) rotateStep(ctx context.Context, gr GuardianRuntime, r *run, now time.Time) {
	sel := c.selectBrain(r.tried)
	if !sel.OK {
		c.enterBackoff(gr, r, now, sel.GateOnly)
		return
	}
	r.tried[sel.Backend] = true
	if err := c.rotateBrain(ctx, r, sel.Backend); err != nil {
		// The selected backend failed to spawn despite qualifying — degrade and back
		// off; the next tick re-selects with this backend already marked tried.
		slog.Warn("autopilot guardian: rotate spawn failed", "run", r.runID, "backend", sel.Backend, "err", err)
		r.state = StateDegraded
		c.enterBackoff(gr, r, now, false)
		return
	}
	r.tier = sel.Tier
	r.healStage = stageRotated
	r.healNextAt = now.Add(c.guardian.HeartbeatTimeout)
	c.escalated(gr, r, "rotate", fmt.Sprintf("rotated brain to backend %s (tier %s)", backendLabel(sel.Backend), sel.Tier))
}

// enterBackoff parks the run in capped-exponential backoff (§2.3 stage 4). It
// NEVER gives up: a next-retry instant is always scheduled, capped by
// guardian.backoff_max and floored by the earliest known backend reset so the run
// climbs back up the ladder the moment a backend frees (§7). gateOnly emits the
// distinct "flip allow_pay_per_use" notification.
func (c *Controller) enterBackoff(gr GuardianRuntime, r *run, now time.Time, gateOnly bool) {
	r.healStage = stageBackoff
	r.state = StateDegraded
	r.backoffStage++

	wait := c.guardian.BackoffMin << uint(r.backoffStage-1)
	if wait <= 0 || wait > c.guardian.BackoffMax { // overflow or past the cap ⇒ clamp
		wait = c.guardian.BackoffMax
	}
	next := now.Add(wait)
	if reset, ok := c.tierstate.earliestReset(); ok && reset.After(now) && reset.Before(next) {
		next = reset // a backend frees sooner than the backoff — wake then
	}
	r.backoffNextRetry = next
	r.healNextAt = next

	if gateOnly {
		r.backoffLastErr = "only pay-per-use backends remain; set autopilot.brain.allow_pay_per_use to continue"
		c.notify(gr, r, "autopilot brain stalled", r.backoffLastErr)
		return
	}
	r.backoffLastErr = "all backends rate-limited — backing off until " + next.Format(time.RFC3339)
	c.notify(gr, r, "autopilot brain stalled", r.backoffLastErr)
}

// plannedRotate cold-starts a healthy brain whose context has reached the rotate
// threshold (§2.3 planned rotation). It selects a fresh backend FIRST and only
// rotates when one is available, so a working brain is never torn down without a
// replacement. A cooldown floor prevents thrashing while the new brain's context
// settles.
func (c *Controller) plannedRotate(ctx context.Context, gr GuardianRuntime, r *run, now time.Time) {
	sel := c.selectBrain(nil)
	if !sel.OK {
		return // nothing to rotate onto — leave the working brain in place
	}
	if err := c.rotateBrain(ctx, r, sel.Backend); err != nil {
		slog.Warn("autopilot guardian: planned rotation failed", "run", r.runID, "err", err)
		r.state = StateDegraded
		return
	}
	r.tier = sel.Tier
	r.state = StateActive
	r.plannedRotateNextAt = now.Add(c.guardian.HeartbeatTimeout)
	c.escalated(gr, r, "planned-rotation", fmt.Sprintf("context %s — cold-started a fresh brain on %s", r.contextLevel, backendLabel(sel.Backend)))
}

// escalated logs every heal step and, when guardian.notify_each is set, surfaces
// it to the owner. The always-notify stall/gate states go through notify directly.
func (c *Controller) escalated(gr GuardianRuntime, r *run, kind, msg string) {
	slog.Info("autopilot guardian: heal step", "run", r.runID, "step", kind, "detail", msg)
	if c.guardian.NotifyEach {
		c.notify(gr, r, "autopilot heal: "+kind, msg)
	}
}

// notify surfaces an owner-facing escalation through the runtime's operator
// notifier (best-effort; a nil-ish runtime just logs).
func (c *Controller) notify(gr GuardianRuntime, r *run, title, body string) {
	slog.Warn("autopilot guardian: "+title, "run", r.runID, "detail", body)
	gr.NotifyEscalation(r.runID, title, body)
}

// backoffStatus returns the run's backoff snapshot for AutopilotStatus, or nil
// unless the run is currently parked in backoff (§2.3).
func (r *run) backoffStatus() *Backoff {
	if r.healStage != stageBackoff {
		return nil
	}
	return &Backoff{
		Stage:       r.backoffStage,
		NextRetryAt: rfc3339OrEmpty(r.backoffNextRetry),
		LastError:   r.backoffLastErr,
	}
}

// brainBackend returns the run's current brain backend ("" ⇒ the daemon default).
func brainBackend(r *run) string {
	if r.brain == nil {
		return ""
	}
	return r.brain.Backend
}

// backendLabel renders a backend for a message, naming the daemon default when
// the selection resolved to "".
func backendLabel(b string) string {
	if b == "" {
		return "(daemon default)"
	}
	return b
}

// contextLevelRank maps a context-window level to an ordered severity so the
// guardian can compare a live level against the configured rotate threshold.
// Unknown values rank 0 (never triggers), so a missing reading is inert.
func contextLevelRank(level string) int {
	switch level {
	case "warning", "warn":
		return 1
	case "critical":
		return 2
	default:
		return 0
	}
}

// contextTriggersRotation reports whether a live context level has reached the
// configured rotate-at threshold. A level of 0 (unknown/ok) never triggers.
func contextTriggersRotation(level, threshold string) bool {
	lr := contextLevelRank(level)
	return lr > 0 && lr >= contextLevelRank(threshold)
}

// rfc3339OrEmpty formats t as RFC3339, or "" for the zero time.
func rfc3339OrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

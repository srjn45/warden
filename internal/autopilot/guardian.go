package autopilot

import (
	"context"
	"errors"
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
// configured interval until ctx is cancelled. Each tick runs the guardian heal
// pass (manager liveness) and then the overwatch pass (§2.4, worker tending) —
// the overwatch's own nudge cadence is time-gated per run, so sharing the
// guardian's ticker just gives it a chance to evaluate. A runtime that does not
// implement GuardianRuntime / OverwatchRuntime (the S1 inert core, the lifecycle
// fakes) makes each pass a no-op, so wiring both in unconditionally is safe.
// Launched from server.go.
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
			c.limitTick(ctx)
			c.guardianTick(ctx)
			c.overwatchTick(ctx)
			c.landingTick(ctx)
			c.completionTick(ctx)
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
	gr, ok := c.runtime.(GuardianRuntime)
	if !ok {
		return
	}
	now := c.now()
	for _, r := range c.runs {
		// Per-repo kill switch: a run whose repo has been switched off is supervised
		// by nothing (disabled repos already have their runs torn down, so this is
		// belt-and-suspenders).
		if r.state == StatePaused || r.state == StateStopped || r.state == StateComplete || r.state == StateRegistered {
			continue
		}
		c.superviseRun(ctx, gr, r, now)
		c.checkNextStep(r, now)
		c.persistRunLocked(r)
	}
}

// superviseRun runs the heal state machine for one run on this tick (§2.3). A
// fresh heartbeat clears the ladder (and may trigger a planned rotation on context
// pressure); a stale one escalates once the current step's grace has elapsed.
func (c *Controller) superviseRun(ctx context.Context, gr GuardianRuntime, r *run, now time.Time) {
	if r.tried == nil {
		r.tried = map[string]bool{}
	}

	// A run parked as needs-attention is left alone until its plan definition
	// changes in ScrivaDB; the operator (pause+resume) and a daemon restart clear
	// it elsewhere.
	if r.needsAttention != "" {
		if !c.parkExpired(ctx, r) {
			return
		}
		slog.Info("autopilot guardian: plan definition changed — leaving needs-attention", "run", r.runID)
		gr.AuditRunEvent(ctx, r.runID, "autopilot.unparked", brainAgentID(r), "plan definition changed; retrying the heal ladder")
		c.clearParked(r)
	}

	// A manager whose session no longer exists is gone: drop the stale record and
	// respawn now — no heartbeat wait, no nudge of a ghost. Only a definitive
	// "missing" counts; an unknown answer (daemon restart, store error) is ignored.
	if r.brain != nil && r.brain.AgentID != "" && gr.BrainSession(ctx, r.brain.AgentID) == SessionMissing {
		cause := "missing"
		if lc, ok := gr.(BrainLossCauser); ok {
			if cc := lc.BrainLossCause(ctx, r.brain.AgentID); cc != "" {
				cause = cc
			}
		}
		id := r.brain.AgentID
		c.managerLost(ctx, gr, r, "session "+cause)
		c.rotateStep(ctx, gr, r, now)
		auditRespawn(ctx, gr, r, id, cause)
		return
	}

	// A manager whose initial digest never reached its pane is re-fed once, then
	// escalated as a spawn failure (seed.go).
	if c.superviseSeed(ctx, gr, r, now) {
		return
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

	// Progress watchdog bookkeeping runs on the run's state BEFORE the fresh
	// branch below promotes it to active.
	roster, rosterOK := c.trackProgress(ctx, r, now)

	if fresh {
		// A ladder the watchdog is climbing is cleared only by progress, never by a
		// heartbeat (a manager can heartbeat forever while nothing moves).
		if r.healStage != stageHealthy && !r.wdActive {
			c.recover(r)
		}
		if !r.wdActive {
			r.state = StateActive
		}
		if !c.watchdogDue(r, roster, rosterOK, now) {
			c.resetTriageEpisode(r) // a heartbeat with nothing stalled ends the episode
		}
		c.superviseWatchdog(ctx, gr, r, roster, rosterOK, now)
		// Planned rotation: a healthy brain whose context has reached the configured
		// level is cold-started on a freshly selected backend (§2.3, §7). A cooldown
		// stops it thrashing while the fresh brain's context settles.
		if contextTriggersRotation(level, c.guardian.RotateAtContext) && !now.Before(r.plannedRotateNextAt) {
			c.plannedRotate(ctx, gr, r, now)
		}
		return
	}

	// A manager resting on a usage limit is silent because of the limit: the
	// limit pass owns it until its resume time.
	if r.managerResting(now) {
		return
	}

	// Wedged (stale heartbeat, or the brain is gone). Wait out the current step's
	// grace so an escalation gets a full heartbeat window to prove itself.
	if now.Before(r.healNextAt) {
		return
	}
	tr := c.triageStall(ctx, gr, r, nil, false, now, guardianNudge)
	if tr.deferred {
		return
	}
	r.state = StateHealing
	if !tr.acted {
		c.escalateWith(ctx, gr, r, now, tr.nudge)
	}
}

// managerLost clears the run's stale manager record after the manager session was
// found missing, resets the heal ladder, reports the run as healing while the slot
// is empty (a failed respawn then moves it to degraded/backoff), and records the
// event in the audit log. The caller follows with rotateStep (the respawn path).
func (c *Controller) managerLost(ctx context.Context, gr GuardianRuntime, r *run, why string) {
	id := ""
	if r.brain != nil {
		id = r.brain.AgentID
	}
	slog.Warn("autopilot guardian: manager session missing — replacing", "run", r.runID, "agent", id, "why", why)
	gr.AuditRunEvent(ctx, r.runID, "autopilot.manager_missing", id, why+"; clearing stale manager record and respawning")
	r.brain = nil
	r.contextLevel = ""
	r.state = StateHealing
	r.healStage = stageHealthy
	r.healNextAt = time.Time{}
	r.tried = map[string]bool{}
	r.wdActive = false
}

// auditRespawn records autopilot.manager_respawned once a lost manager's slot has
// a live successor (nothing is recorded when the respawn failed and the run went
// to backoff).
func auditRespawn(ctx context.Context, gr GuardianRuntime, r *run, lostID, cause string) {
	if r.brain == nil || r.brain.AgentID == "" {
		return
	}
	gr.AuditRunEvent(ctx, r.runID, "autopilot.manager_respawned", r.brain.AgentID,
		"cause="+cause+" (lost "+lostID+"); successor takes the same slot and reconciles ledger, open PRs and live workers first")
}

// recover clears the heal ladder after a brain proves alive again: the cycle
// restarts from healthy, the tried-backend set and backoff are reset.
func (c *Controller) recover(r *run) {
	r.healStage = stageHealthy
	r.healNextAt = time.Time{}
	r.backoffStage = 0
	r.backoffNextRetry = time.Time{}
	r.backoffLastErr = ""
	r.backoffKind = ""
	r.failStreak, r.failStreakText = 0, ""
	r.tried = map[string]bool{}
}

// escalateWith advances one rung up the heal ladder (§2.3) with a caller-chosen
// stage-1 nudge text (the watchdog names stalled tasks, triage supplies model
// text). Backoff expiry retries the whole ladder from stage 1 (forever); a brain
// that is entirely gone jumps straight to (re)spawn via the rotate step.
func (c *Controller) escalateWith(ctx context.Context, gr GuardianRuntime, r *run, now time.Time, nudge string) {
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
		if err := gr.NudgeBrain(ctx, r.brain.AgentID, nudge); err != nil {
			if errors.Is(err, ErrAgentNotFound) {
				lost := r.brain.AgentID
				c.managerLost(ctx, gr, r, "nudge target not found")
				c.rotateStep(ctx, gr, r, now)
				auditRespawn(ctx, gr, r, lost, "missing")
				return
			}
			slog.Warn("autopilot guardian: nudge failed", "run", r.runID, "err", err)
		}
		r.healStage = stageNudged
		r.healNextAt = now.Add(c.guardian.HeartbeatTimeout)
		c.escalated(gr, r, "nudge", "brain quiet past heartbeat timeout — sent a steering nudge")
	case stageNudged:
		// Stage 2 — restart on the same backend with a fresh context (in-place HotSwap).
		c.restartStep(ctx, gr, r, now)
	default:
		// Stage 3 — rotate down the ladder to the next available backend.
		c.rotateStep(ctx, gr, r, now)
	}
}

// restartStep is ladder stage 2: restart the manager in place (same backend,
// fresh context) and give it one heartbeat window to prove itself.
func (c *Controller) restartStep(ctx context.Context, gr GuardianRuntime, r *run, now time.Time) {
	cur := brainBackend(r)
	r.tried[cur] = true
	if err := c.rotateBrain(ctx, r, cur, RotateReasonHeal); err != nil {
		slog.Warn("autopilot guardian: restart failed", "run", r.runID, "err", err)
		r.state = StateDegraded
	}
	r.healStage = stageRestarted
	r.healNextAt = now.Add(c.guardian.HeartbeatTimeout)
	c.escalated(gr, r, "restart", "nudge did not revive the brain — restarted it (same backend, fresh context)")
}

// rotateStep rotates the brain onto the next selectable backend not yet tried this
// cycle, hot-swapping into the same manager slot (§7). When nothing is selectable
// it enters backoff (§2.3 stage 4). Used both to walk down the ladder (stage 3)
// and to (re)spawn a brain that is entirely gone.
func (c *Controller) rotateStep(ctx context.Context, gr GuardianRuntime, r *run, now time.Time) {
	sel := c.selectBrain(r.tried)
	if !sel.OK {
		c.enterBackoff(gr, r, now, KindNoBackendSelectable, sel.GateOnly, nil)
		return
	}
	r.tried[sel.Backend] = true
	if err := c.rotateBrain(ctx, r, sel.Backend, RotateReasonHeal); err != nil {
		// The selected backend failed to spawn despite qualifying — degrade and back
		// off; the next tick re-selects with this backend already marked tried.
		slog.Warn("autopilot guardian: rotate spawn failed", "run", r.runID, "backend", sel.Backend, "err", err)
		r.state = StateDegraded
		c.enterBackoff(gr, r, now, classifySpawnError(err), false, err)
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
func (c *Controller) enterBackoff(gr GuardianRuntime, r *run, now time.Time, kind FailureKind, gateOnly bool, cause error) {
	if c.trackFailure(r, kind, cause) {
		c.park(gr, r, kind, cause)
		return
	}
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
	r.backoffKind = kind

	switch {
	case cause != nil:
		// A real spawn/rotate error: report it verbatim, never as a rate limit.
		r.backoffLastErr = fmt.Sprintf("%s: %v — backing off until %s", kind, cause, next.Format(time.RFC3339))
	case gateOnly:
		r.backoffLastErr = "only pay-per-use backends remain; set autopilot.brain.allow_pay_per_use to continue"
	default:
		r.backoffLastErr = "all backends rate-limited — backing off until " + next.Format(time.RFC3339)
	}
	slog.Warn("autopilot guardian: entering backoff", "run", r.runID, "kind", string(kind), "detail", r.backoffLastErr)
	gr.AuditRunEvent(context.Background(), r.runID, "autopilot.backoff", brainAgentID(r), string(kind)+": "+r.backoffLastErr)
	c.notify(gr, r, "autopilot brain stalled ("+string(kind)+")", r.backoffLastErr)
}

// trackFailure updates the run's consecutive-identical-failure streak and reports
// whether the failure is hopeless: a definition error always is; an unknown spawn
// error is once the same text has repeated guardian.max_identical_failures times.
// Transient kinds (backend unavailable, nothing selectable) reset the streak and
// never park — they keep the capped-exponential backoff forever.
func (c *Controller) trackFailure(r *run, kind FailureKind, cause error) bool {
	switch kind {
	case KindDefinitionError:
		return cause != nil
	case KindSpawnError:
		if cause == nil {
			return false
		}
		text := cause.Error()
		if text == r.failStreakText {
			r.failStreak++
		} else {
			r.failStreak, r.failStreakText = 1, text
		}
		return r.failStreak >= c.guardian.MaxIdenticalFailures
	}
	r.failStreak, r.failStreakText = 0, ""
	return false
}

// park stops the retry loop for a failure that cannot succeed on retry: the run
// stays degraded with a distinct needs-attention reason, the operator is notified
// and the audit log written exactly once, and later ticks do nothing until the
// run is unparked (plan change, pause+resume, daemon restart).
func (c *Controller) park(gr GuardianRuntime, r *run, kind FailureKind, cause error) {
	r.healStage = stageHealthy
	r.state = StateDegraded
	r.backoffKind = kind
	r.backoffNextRetry = time.Time{}
	r.healNextAt = time.Time{}
	r.needsAttention = fmt.Sprintf("%s: %v — retries stopped; fix the plan/config, then `wd plan resume` (or pause+resume)", kind, cause)
	r.parkedPlanKey = c.planKey(context.Background(), r)
	slog.Warn("autopilot guardian: parked as needs-attention", "run", r.runID, "kind", string(kind), "detail", r.needsAttention)
	gr.AuditRunEvent(context.Background(), r.runID, "autopilot.needs_attention", brainAgentID(r), r.needsAttention)
	c.notify(gr, r, "autopilot needs attention ("+string(kind)+")", r.needsAttention)
}

// planKey identifies the plan definition revision a plan-bound run last saw
// ("" for legacy file-only runs or when the plan source is unavailable).
func (c *Controller) planKey(ctx context.Context, r *run) string {
	if r.planID == "" || c.planSource == nil {
		return ""
	}
	p, err := c.planSource.Get(ctx, r.planID)
	if err != nil || p == nil {
		return ""
	}
	return fmt.Sprintf("%d:%s", p.Revision, p.ContentHash)
}

// parkExpired reports whether the plan definition changed since the run parked.
func (c *Controller) parkExpired(ctx context.Context, r *run) bool {
	if r.parkedPlanKey == "" {
		return false
	}
	key := c.planKey(ctx, r)
	return key != "" && key != r.parkedPlanKey
}

// clearParked leaves the needs-attention condition and restarts the heal ladder
// from the top with a clean failure counter and backoff exponent.
func (c *Controller) clearParked(r *run) {
	r.needsAttention, r.parkedPlanKey = "", ""
	r.failStreak, r.failStreakText = 0, ""
	r.backoffStage = 0
	r.backoffKind = ""
	r.backoffLastErr = ""
	r.backoffNextRetry = time.Time{}
	r.healStage = stageHealthy
	r.healNextAt = time.Time{}
	r.tried = map[string]bool{}
	// Leaving needs-attention (plan change, pause+resume) restarts the watchdog
	// window so the run is not immediately re-parked.
	r.wdActive, r.wdParked = false, false
	r.lastProgressAt = c.now()
}

func brainAgentID(r *run) string {
	if r.brain == nil {
		return ""
	}
	return r.brain.AgentID
}

// plannedRotate hot-swaps a healthy brain whose context has reached the rotate
// threshold (§2.3 planned rotation) into the same manager slot. It selects a
// fresh backend FIRST and only rotates when one is available, so a working
// brain is never swapped without a replacement. A cooldown floor prevents
// thrashing while the successor's context settles.
func (c *Controller) plannedRotate(ctx context.Context, gr GuardianRuntime, r *run, now time.Time) {
	sel := c.selectBrain(nil)
	if !sel.OK {
		return // nothing to rotate onto — leave the working brain in place
	}
	if err := c.rotateBrain(ctx, r, sel.Backend, RotateReasonContext); err != nil {
		slog.Warn("autopilot guardian: planned rotation failed", "run", r.runID, "err", err)
		r.state = StateDegraded
		return
	}
	r.tier = sel.Tier
	r.state = StateActive
	r.plannedRotateNextAt = now.Add(c.guardian.HeartbeatTimeout)
	c.escalated(gr, r, "planned-rotation", fmt.Sprintf("context %s — hot-swapped a fresh brain on %s", r.contextLevel, backendLabel(sel.Backend)))
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
		Kind:        string(r.backoffKind),
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

func restingUntil(r *run) time.Time {
	t, _ := r.earliestResting()
	return t
}

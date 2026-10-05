package autopilot

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
)

// Progress watchdog (docs/specs/2026-10-05-plan-restart-and-watchdog.md §5).
//
// The heartbeat guardian treats a run as healthy whenever its manager session
// exists and heartbeats. A manager can heartbeat forever while the run makes no
// headway (every worker gone, nothing landing). The watchdog tracks the last time
// the run made real PROGRESS and, once that exceeds the window with nothing
// working, climbs the SAME heal ladder (nudge → restart → rotate) as the
// heartbeat guardian; a ladder that runs out without progress parks the run as
// needs-attention (no_progress).
//
// Progress = a change in the run's progress fingerprint: ledger task states, the
// landing count, the plan's task statuses, or a new run-tagged worker. Heartbeats
// and overwatch nudges never touch the fingerprint, so they are not progress.

// Watchdog states reported on RunStatus.Watchdog.
const (
	WatchdogDisabled   = "disabled"   // watchdog switched off in config
	WatchdogIdle       = "idle"       // recent progress (inside the window)
	WatchdogArmed      = "armed"      // window elapsed but no escalation yet (agent busy / grace)
	WatchdogEscalating = "escalating" // climbing the heal ladder
	WatchdogParked     = "parked"     // ladder exhausted — parked as needs-attention
)

// KindNoProgress is the needs-attention reason for a run the watchdog could not
// revive.
const KindNoProgress FailureKind = "no_progress"

// DefaultWatchdogWindow is the no-progress window before the watchdog acts.
const DefaultWatchdogWindow = 2 * time.Hour

// progressFingerprint summarises everything that counts as progress.
func (c *Controller) progressFingerprint(ctx context.Context, r *run, roster []AgentInfo) string {
	var parts []string
	if c.runtime != nil {
		if l := c.runtime.NewLedger(r.runID); l != nil {
			ids := map[string]bool{}
			if tasks, err := l.Tasks(); err == nil {
				for _, t := range tasks {
					parts = append(parts, "L:"+t.ID+"="+string(t.State))
					ids[t.ID] = true
				}
			}
			for _, t := range r.plan.Tasks {
				ids[t.ID] = true
			}
			// Per-task overlay (WriteTaskState) is a ledger state change too.
			for id := range ids {
				if st, err := l.TaskState(id); err == nil {
					parts = append(parts, "O:"+id+"="+string(st))
				}
			}
			if lands, err := l.Landings(); err == nil {
				parts = append(parts, fmt.Sprintf("landings=%d", len(lands)))
			}
		}
	}
	if c.planSource != nil && r.planID != "" {
		if p, err := c.planSource.Get(ctx, r.planID); err == nil && p != nil {
			for k, v := range p.TaskProgress {
				parts = append(parts, "P:"+k+"="+v)
			}
			if p.ActiveExecution != nil {
				for k, v := range p.ActiveExecution.TaskProgress {
					parts = append(parts, "PE:"+k+"="+v)
				}
			}
		}
	}
	for _, a := range roster {
		if r.brain != nil && a.ID == r.brain.AgentID {
			continue // the manager is not a spawned worker
		}
		parts = append(parts, "A:"+a.ID)
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
}

// watchdogRoster returns the run's live roster; ok=false when the runtime cannot
// provide one or the read failed.
func (c *Controller) watchdogRoster(ctx context.Context, r *run) ([]AgentInfo, bool) {
	ow, ok := c.runtime.(OverwatchRuntime)
	if !ok {
		return nil, false
	}
	roster, err := ow.RunAgents(ctx, r.runID)
	if err != nil {
		return nil, false
	}
	return roster, true
}

// trackProgress refreshes the run's last-progress clock from the fingerprint and
// clears a watchdog escalation when real progress is observed. It reports whether
// the roster was readable (needed by the busy gate).
func (c *Controller) trackProgress(ctx context.Context, r *run, now time.Time) (roster []AgentInfo, rosterOK bool) {
	roster, rosterOK = c.watchdogRoster(ctx, r)
	if !rosterOK {
		// Cannot fingerprint spawns reliably (or tell whether anything is busy):
		// keep the last fingerprint and clock, only seeding a missing clock.
		if r.lastProgressAt.IsZero() {
			r.lastProgressAt = now
		}
		return nil, false
	}
	fp := c.progressFingerprint(ctx, r, roster)

	switch {
	case r.lastProgressAt.IsZero():
		r.lastProgressAt = now // first sight (or a pre-watchdog record): start the window
		r.progressFP = fp
	case r.progressFP == "":
		r.progressFP = fp // record carried a clock but no fingerprint: adopt it
	case fp != r.progressFP:
		r.progressFP = fp
		r.lastProgressAt = now
		c.clearWatchdog(r)
	case r.state != StateActive && !r.wdActive:
		// A run that is starting / healing / degraded for other reasons is not
		// stalled: hold the clock so the window only measures active time.
		r.lastProgressAt = now
	}
	return roster, rosterOK
}

// clearWatchdog drops an in-flight watchdog escalation after progress, resetting
// the heal ladder the watchdog itself climbed.
func (c *Controller) clearWatchdog(r *run) {
	if !r.wdActive {
		return
	}
	r.wdActive = false
	c.recover(r)
	slog.Info("autopilot watchdog: progress observed — cleared", "run", r.runID)
}

// watchdogDue reports whether the watchdog should act for r this tick.
func (c *Controller) watchdogDue(r *run, roster []AgentInfo, rosterOK bool, now time.Time) bool {
	if c.guardian.WatchdogDisabled || !rosterOK {
		return false
	}
	if r.state != StateActive && !r.wdActive {
		return false
	}
	if r.needsAttention != "" {
		return false
	}
	if now.Sub(r.lastProgressAt) < c.guardian.WatchdogWindow {
		return false
	}
	for _, a := range roster {
		if isAgentBusy(a.State) {
			return false
		}
	}
	return true
}

// superviseWatchdog runs the watchdog for a run whose manager heartbeat is fresh.
// It shares healNextAt grace with the heartbeat ladder, so the two can never both
// climb a rung inside the same window.
func (c *Controller) superviseWatchdog(ctx context.Context, gr GuardianRuntime, r *run, roster []AgentInfo, rosterOK bool, now time.Time) {
	if !c.watchdogDue(r, roster, rosterOK, now) {
		return
	}
	if now.Before(r.healNextAt) {
		return
	}
	// Ladder exhausted: the last rung (rotate) — or the backoff past it — had its
	// grace and still nothing progressed.
	if r.wdActive && (r.healStage == stageRotated || r.healStage == stageBackoff) {
		c.parkNoProgress(gr, r, now)
		return
	}
	r.wdActive = true
	r.state = StateHealing
	c.escalateWith(ctx, gr, r, now, c.watchdogNudge(r, now))
	if r.healStage == stageBackoff {
		c.parkNoProgress(gr, r, now)
	}
}

// watchdogNudge is the stage-1 steering text, naming the stalled tasks.
func (c *Controller) watchdogNudge(r *run, now time.Time) string {
	var stalled []string
	if l := c.ledgerTasksLocked(r.runID); len(l) > 0 {
		for _, t := range l {
			if t.State != LedgerLanded {
				stalled = append(stalled, fmt.Sprintf("%s (%s)", t.ID, t.State))
			}
		}
	} else {
		for _, t := range r.plan.Tasks {
			if t.Status != TaskStatusDone {
				stalled = append(stalled, fmt.Sprintf("%s (%s)", t.ID, t.Status))
			}
		}
	}
	const maxNamed = 8
	extra := 0
	if len(stalled) > maxNamed {
		extra = len(stalled) - maxNamed
		stalled = stalled[:maxNamed]
	}
	list := strings.Join(stalled, ", ")
	if extra > 0 {
		list += fmt.Sprintf(" and %d more", extra)
	}
	if list == "" {
		list = "none unfinished — verify done_when"
	}
	return fmt.Sprintf("autopilot watchdog: this run has made no progress for %s and no agent is working. "+
		"Stalled tasks: %s. Reconcile the ledger against list_agents, then spawn workers for pending tasks, "+
		"unblock gated ones, or mark the run complete.", now.Sub(r.lastProgressAt).Round(time.Minute), list)
}

// parkNoProgress parks a run whose watchdog ladder was exhausted without progress.
// The operator is notified exactly once; later ticks skip the parked run.
func (c *Controller) parkNoProgress(gr GuardianRuntime, r *run, now time.Time) {
	r.wdActive = false
	r.healStage = stageHealthy
	r.state = StateDegraded
	r.backoffKind = ""
	r.backoffNextRetry = time.Time{}
	r.healNextAt = time.Time{}
	r.wdParked = true
	r.needsAttention = fmt.Sprintf("%s: no progress for %s despite nudge, restart and rotate — retries stopped; run `wd plan restart` to start it again with fresh agents",
		KindNoProgress, now.Sub(r.lastProgressAt).Round(time.Minute))
	r.parkedPlanKey = c.planKey(context.Background(), r)
	slog.Warn("autopilot watchdog: parked as needs-attention", "run", r.runID, "detail", r.needsAttention)
	gr.AuditRunEvent(context.Background(), r.runID, "autopilot.needs_attention", brainAgentID(r), r.needsAttention)
	c.notify(gr, r, "autopilot needs attention ("+string(KindNoProgress)+")", r.needsAttention)
}

// watchdogState is the RunStatus.Watchdog value for r at instant now.
func (c *Controller) watchdogState(r *run, now time.Time) string {
	switch {
	case c.guardian.WatchdogDisabled:
		return WatchdogDisabled
	case r.wdParked && r.needsAttention != "":
		return WatchdogParked
	case r.wdActive:
		return WatchdogEscalating
	case !r.lastProgressAt.IsZero() && r.state == StateActive && now.Sub(r.lastProgressAt) >= c.guardian.WatchdogWindow:
		return WatchdogArmed
	}
	return WatchdogIdle
}

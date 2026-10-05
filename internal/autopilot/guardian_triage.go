package autopilot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/srjn45/warden/internal/fastbrain"
)

// Guardian triage (docs/specs/2026-10-05-autopilot-run-to-final-pr.md §C).
//
// Before the guardian (stale heartbeat) or the progress watchdog escalates a
// run, a Fast-Brain diagnosis may pick a more precise recovery than "the next
// rung by time". The tick snapshots the evidence under c.mu, a goroutine runs the
// diagnosis OFF the lock (at most one in flight per run), and a LATER tick applies
// the result — only when the run object, the manager id and the heal stage are
// all unchanged. Everything that is not a clean, bounded model answer falls open
// to the existing ladder step, so triage can only be more precise, never more
// stuck, than the ladder.

// triageMinConfidence gates the destructive rungs (restart / rotate).
const triageMinConfidence = 0.8

// triageState is the per-run triage bookkeeping.
type triageState struct {
	inFlight bool
	result   *triageResult // a finished diagnosis awaiting a later tick
	waits    int           // consecutive "wait" decisions this episode
	waited   time.Duration // total deferral granted this episode
}

// triageResult is a diagnosis plus the identity it was made for.
type triageResult struct {
	diag    fastbrain.StallDiagnosis
	run     *run
	manager string
	stage   healStage
}

// triageOutcome tells the caller what triage did this tick.
type triageOutcome struct {
	deferred bool   // do nothing this tick (waiting, in flight, or applied a hold)
	acted    bool   // an action ran in place of the ladder step
	nudge    string // nudge text for the ladder step when !deferred && !acted
}

func stageName(s healStage) string {
	switch s {
	case stageHealthy:
		return "healthy"
	case stageNudged:
		return "nudged"
	case stageRestarted:
		return "restarted"
	case stageRotated:
		return "rotated"
	case stageBackoff:
		return "backoff"
	}
	return fmt.Sprintf("stage-%d", int(s))
}

// resetTriageEpisode ends a stall episode (progress or a heartbeat was observed).
func (c *Controller) resetTriageEpisode(r *run) {
	r.triage.waits, r.triage.waited = 0, 0
}

// triageStall is called by the guardian/watchdog right before an escalation. It
// runs under c.mu and never blocks on the model.
func (c *Controller) triageStall(ctx context.Context, gr GuardianRuntime, r *run, _ []AgentInfo, watchdog bool, now time.Time, generic string) triageOutcome {
	mech := triageOutcome{nudge: generic}
	if !c.guardian.UseFastBrain || r.brain == nil || r.brain.AgentID == "" {
		return mech
	}
	ev, ok := c.runtime.(EvidenceRuntime)
	if !ok {
		return mech
	}

	// A finished diagnosis from an earlier tick: apply it iff nothing moved.
	if res := r.triage.result; res != nil {
		r.triage.result = nil
		if res.run != c.runs[r.runID] || res.run != r || res.manager != r.brain.AgentID || res.stage != r.healStage {
			c.auditDiagnosis(ctx, gr, r, res.diag, "discarded", "stale: run, manager or heal stage changed")
			return triageOutcome{deferred: true}
		}
		return c.applyDiagnosis(ctx, gr, ev, r, res.diag, now, generic)
	}
	if r.triage.inFlight {
		return triageOutcome{deferred: true}
	}

	in, err := c.stallInput(ctx, ev, r, watchdog, now)
	if err != nil {
		slog.Warn("autopilot guardian: triage evidence unavailable — running the ladder", "run", r.runID, "err", err)
		return mech
	}
	fn := c.triageFn
	if fn == nil {
		eng := c.fastBrain
		fn = func(ctx context.Context, in fastbrain.StallInput) fastbrain.StallDiagnosis {
			return fastbrain.DiagnoseStall(ctx, eng, in)
		}
	}
	snap := &triageResult{run: r, manager: r.brain.AgentID, stage: r.healStage}
	r.triage.inFlight = true
	go c.runTriage(ctx, fn, in, snap)
	return triageOutcome{deferred: true}
}

// runTriage is the off-lock diagnosis goroutine. It takes c.mu only to deliver.
func (c *Controller) runTriage(ctx context.Context, fn func(context.Context, fastbrain.StallInput) fastbrain.StallDiagnosis, in fastbrain.StallInput, snap *triageResult) {
	var diag fastbrain.StallDiagnosis
	func() {
		defer func() {
			if p := recover(); p != nil {
				diag = fastbrain.StallDiagnosis{Action: fastbrain.ActionMechanical, Source: "failopen",
					FailOpen: fastbrain.FailOpenRunnerError, Rationale: fmt.Sprintf("panic: %v", p)}
			}
		}()
		diag = fn(ctx, in)
	}()
	snap.diag = diag
	c.mu.Lock()
	defer c.mu.Unlock()
	snap.run.triage.inFlight = false
	snap.run.triage.result = snap
}

// stallInput builds the diagnosis input from the manager's evidence.
func (c *Controller) stallInput(ctx context.Context, ev EvidenceRuntime, r *run, watchdog bool, now time.Time) (fastbrain.StallInput, error) {
	e, err := ev.AgentEvidence(ctx, r.brain.AgentID)
	if err != nil {
		return fastbrain.StallInput{}, err
	}
	trigger := "stale heartbeat"
	if watchdog {
		trigger = "no progress"
	}
	header := fmt.Sprintf("run %s; trigger %s; state %s; heal stage %s; %s since last activity; %s since last progress; waits so far %d",
		r.runID, trigger, r.state, stageName(r.healStage), since(now, r.lastHeartbeat), since(now, r.lastProgressAt), r.triage.waits)
	facts := fmt.Sprintf("backend %s; tier %s; status %s; context %s; rate limited %t",
		backendLabel(brainBackend(r)), r.tier, e.Status, e.ContextLevel, e.RateLimited)
	return fastbrain.StallInput{
		AgentID:            r.brain.AgentID,
		Header:             header,
		Facts:              facts,
		Pane:               e.PaneTail,
		RateLimited:        e.RateLimited,
		PendingApproval:    e.PendingApproval != "",
		ActivitySinceSpawn: r.lastHeartbeat.After(r.brainSpawnedAt),
	}, nil
}

func since(now, t time.Time) string {
	if t.IsZero() {
		return "unknown time"
	}
	return now.Sub(t).Round(time.Minute).String()
}

// applyDiagnosis maps a (still valid) diagnosis onto the existing guardian code.
func (c *Controller) applyDiagnosis(ctx context.Context, gr GuardianRuntime, ev EvidenceRuntime, r *run, d fastbrain.StallDiagnosis, now time.Time, generic string) triageOutcome {
	id := r.brain.AgentID
	hold := func() triageOutcome {
		r.healNextAt = now.Add(c.guardian.HeartbeatTimeout) // one heartbeat window to prove itself
		return triageOutcome{deferred: true}
	}
	failOpen := func(why string) triageOutcome {
		c.auditDiagnosis(ctx, gr, r, d, "mechanical", why)
		return triageOutcome{nudge: generic}
	}

	switch d.Action {
	case fastbrain.ActionWait:
		remain := c.guardian.MaxWaitTotal - r.triage.waited
		if r.triage.waits >= c.guardian.MaxWaits || remain <= 0 {
			// Past the bounds: rewritten to the generic nudge (the ladder step).
			return failOpen("wait bounds exceeded")
		}
		delay := min(c.guardian.HeartbeatTimeout, remain)
		r.triage.waits++
		r.triage.waited += delay
		r.healNextAt = now.Add(delay)
		c.auditDiagnosis(ctx, gr, r, d, "applied", fmt.Sprintf("deferred %s (wait %d/%d)", delay, r.triage.waits, c.guardian.MaxWaits))
		return triageOutcome{deferred: true}

	case fastbrain.ActionNudge:
		c.auditDiagnosis(ctx, gr, r, d, "applied", "nudge with model text")
		return triageOutcome{nudge: d.Text}

	case fastbrain.ActionRestart, fastbrain.ActionRotate:
		if d.Confidence < triageMinConfidence {
			return failOpen(fmt.Sprintf("confidence %.2f below %.1f", d.Confidence, triageMinConfidence))
		}
		c.auditDiagnosis(ctx, gr, r, d, "applied", string(d.Action))
		r.state = StateHealing
		if d.Action == fastbrain.ActionRestart {
			r.tried = orEmpty(r.tried)
			c.restartStep(ctx, gr, r, now)
		} else {
			r.tried = orEmpty(r.tried)
			c.rotateStep(ctx, gr, r, now)
		}
		return triageOutcome{acted: true}

	case fastbrain.ActionResolvePrompt, fastbrain.ActionResumeRateLimit, fastbrain.ActionRedeliverPrompt:
		var err error
		switch d.Action {
		case fastbrain.ActionResolvePrompt:
			err = ev.ResolvePrompt(ctx, id)
		case fastbrain.ActionResumeRateLimit:
			err = ev.ResumeRateLimit(ctx, id)
		default:
			err = ev.RedeliverPrompt(ctx, id)
		}
		if err != nil {
			if errors.Is(err, ErrAgentNotFound) {
				return failOpen("manager not found: " + err.Error())
			}
			return failOpen(string(d.Action) + " failed: " + err.Error())
		}
		c.auditDiagnosis(ctx, gr, r, d, "applied", string(d.Action))
		hold()
		return triageOutcome{acted: true}

	case fastbrain.ActionCallResolver:
		started, err := c.spawnResolverLocked(ctx, r, ResolverRequest{
			RunID: r.runID, Class: BlockerManagerStall, Detail: "manager stall: " + d.Rationale})
		if err != nil {
			return failOpen("resolver start failed: " + err.Error())
		}
		if !started {
			c.auditDiagnosis(ctx, gr, r, d, "applied", "resolver cap exhausted; run parked")
			return triageOutcome{deferred: true}
		}
		c.auditDiagnosis(ctx, gr, r, d, "applied", "resolver started")
		hold()
		return triageOutcome{acted: true}
	}
	// ActionMechanical (heuristics found nothing, or any fail-open status).
	return failOpen("ladder step")
}

func orEmpty(m map[string]bool) map[string]bool {
	if m == nil {
		return map[string]bool{}
	}
	return m
}

// auditDiagnosis records every triage decision (autopilot.guardian_diagnosis).
func (c *Controller) auditDiagnosis(ctx context.Context, gr GuardianRuntime, r *run, d fastbrain.StallDiagnosis, outcome, note string) {
	detail := fmt.Sprintf("action=%s outcome=%s source=%s confidence=%.2f failopen=%q rationale=%q note=%q",
		d.Action, outcome, d.Source, d.Confidence, d.FailOpen, d.Rationale, note)
	c.recordDiagnosis(r, d, outcome)
	gr.AuditRunEvent(ctx, r.runID, "autopilot.guardian_diagnosis", brainAgentID(r), detail)
}

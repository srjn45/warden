package autopilot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/srjn45/warden/internal/fastbrain"
)

// Worker triage in the overwatch pass (docs/specs/2026-10-05-autopilot-run-to-final-pr.md
// §C, applied to workers). For needy workers (not spawning or working) the
// overwatch gathers evidence and runs a worker-scoped Fast-Brain diagnosis OFF
// c.mu, at most once per overwatchMinGap per run and for at most
// overwatchTriageMax workers per pass. A LATER tick applies the results:
//
//   - resolve_prompt / resume_rate_limit / redeliver_prompt run directly through
//     the evidence runtime, so a worker at an approval prompt or a rate-limit
//     banner resumes without the manager noticing;
//   - nudge becomes the specific finding that replaces the generic line for that
//     worker in the manager's nudge (composeOverwatchNudge);
//   - wait keeps the worker out of the nudge, bounded by guardian.MaxWaits.
//
// The overwatch never restarts, terminates or removes a worker (the worker action
// set has no such action). Any fail-open result leaves the worker in the generic
// nudge exactly as before, and with triage off none of this runs.
const overwatchTriageMax = 4

// workerTriageState is the per-run worker-triage bookkeeping (mutated under c.mu).
type workerTriageState struct {
	inFlight bool
	lastAt   time.Time                  // when the last diagnosis pass launched
	result   *workerTriageResult        // a finished pass awaiting a later tick
	findings map[string]string          // worker id -> specific finding for the manager
	handled  map[string]bool            // worker id -> resolved directly or waiting; omitted from the nudge
	waits    map[string]int             // worker id -> consecutive wait decisions
	diags    map[string]workerDiagEntry // last diagnosis per worker (for audit/tests)
}

type workerDiagEntry struct {
	diag fastbrain.StallDiagnosis
	at   time.Time
}

// workerTriageResult is a finished pass plus the identity it was made for.
type workerTriageResult struct {
	run     *run
	manager string
	diags   map[string]fastbrain.StallDiagnosis
}

// workerTriageEnabled reports whether the overwatch should triage workers.
func (c *Controller) workerTriageEnabled() (EvidenceRuntime, bool) {
	if !c.guardian.UseFastBrain {
		return nil, false
	}
	ev, ok := c.runtime.(EvidenceRuntime)
	return ev, ok
}

// pruneWorkerTriage forgets state for workers that are no longer needy.
func (r *run) pruneWorkerTriage(needy []AgentInfo) {
	w := &r.wtriage
	keep := map[string]bool{}
	for _, a := range needy {
		keep[a.ID] = true
	}
	for id := range w.handled {
		if !keep[id] {
			delete(w.handled, id)
		}
	}
	for id := range w.findings {
		if !keep[id] {
			delete(w.findings, id)
		}
	}
	for id := range w.waits {
		if !keep[id] {
			delete(w.waits, id)
		}
	}
}

// workerTriage runs under c.mu from overwatchRun. It applies a finished pass and
// launches a new one when due. It returns the needy workers the manager should
// still hear about and whether a diagnosis is in flight (the caller then defers
// the event nudge one tick so the specific finding can replace the generic line).
func (c *Controller) workerTriage(ctx context.Context, ow OverwatchRuntime, r *run, needy []AgentInfo, now time.Time) (remaining []AgentInfo, pending bool) {
	ev, ok := c.workerTriageEnabled()
	if !ok || r.state != StateActive {
		return needy, false
	}
	w := &r.wtriage
	if w.findings == nil {
		w.findings = map[string]string{}
		w.handled = map[string]bool{}
		w.waits = map[string]int{}
		w.diags = map[string]workerDiagEntry{}
	}
	r.pruneWorkerTriage(needy)

	if res := w.result; res != nil {
		w.result = nil
		if res.run != c.runs[r.runID] || res.run != r || r.brain == nil || res.manager != r.brain.AgentID {
			c.auditWorkerDiagnosis(ctx, r, "", fastbrain.StallDiagnosis{}, "discarded", "stale: run or manager changed")
		} else {
			c.applyWorkerDiagnoses(ctx, ev, r, res.diags, needy, now)
		}
	}

	if !w.inFlight && len(needy) > 0 && (w.lastAt.IsZero() || now.Sub(w.lastAt) >= overwatchMinGap) {
		batch := needy
		if len(batch) > overwatchTriageMax {
			batch = batch[:overwatchTriageMax]
		}
		w.inFlight = true
		w.lastAt = now
		snap := &workerTriageResult{run: r, manager: r.brain.AgentID, diags: map[string]fastbrain.StallDiagnosis{}}
		go c.runWorkerTriage(ctx, ev, r.runID, string(r.state), append([]AgentInfo(nil), batch...), snap)
	}

	for _, a := range needy {
		if !w.handled[a.ID] {
			remaining = append(remaining, a)
		}
	}
	// A hung diagnosis must not hold the nudge forever: pending lapses after a gap.
	return remaining, w.inFlight && now.Sub(w.lastAt) < overwatchMinGap
}

// runWorkerTriage is the off-lock goroutine: it reads each worker's evidence and
// diagnoses it, then takes c.mu only to deliver.
func (c *Controller) runWorkerTriage(ctx context.Context, ev EvidenceRuntime, runID, state string, batch []AgentInfo, snap *workerTriageResult) {
	fn := c.triageFn
	if fn == nil {
		eng := c.fastBrain
		fn = func(ctx context.Context, in fastbrain.StallInput) fastbrain.StallDiagnosis {
			return fastbrain.DiagnoseStall(ctx, eng, in)
		}
	}
	for _, a := range batch {
		snap.diags[a.ID] = c.diagnoseWorker(ctx, ev, fn, runID, state, a)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	snap.run.wtriage.inFlight = false
	snap.run.wtriage.result = snap
}

func (c *Controller) diagnoseWorker(ctx context.Context, ev EvidenceRuntime, fn func(context.Context, fastbrain.StallInput) fastbrain.StallDiagnosis, runID, state string, a AgentInfo) (diag fastbrain.StallDiagnosis) {
	defer func() {
		if p := recover(); p != nil {
			diag = fastbrain.StallDiagnosis{Action: fastbrain.ActionMechanical, Source: "failopen",
				FailOpen: fastbrain.FailOpenRunnerError, Rationale: fmt.Sprintf("panic: %v", p)}
		}
	}()
	e, err := ev.AgentEvidence(ctx, a.ID)
	if err != nil {
		return fastbrain.StallDiagnosis{Action: fastbrain.ActionMechanical, Source: "failopen",
			FailOpen: fastbrain.FailOpenRunnerError, Rationale: "evidence unavailable: " + err.Error()}
	}
	label := a.Name
	if label == "" {
		label = a.ID
	}
	return fn(ctx, fastbrain.StallInput{
		AgentID:         a.ID,
		Worker:          true,
		Header:          fmt.Sprintf("run %s; worker %s; state %s; status %s", runID, label, state, a.State),
		Facts:           fmt.Sprintf("status %s; context %s; rate limited %t", e.Status, e.ContextLevel, e.RateLimited),
		Pane:            e.PaneTail,
		RateLimited:     e.RateLimited,
		PendingApproval: e.PendingApproval != "",
		// A worker that is idle/waiting has by definition produced activity.
		ActivitySinceSpawn: true,
	})
}

// applyWorkerDiagnoses applies a still-valid finished pass under c.mu.
func (c *Controller) applyWorkerDiagnoses(ctx context.Context, ev EvidenceRuntime, r *run, diags map[string]fastbrain.StallDiagnosis, needy []AgentInfo, now time.Time) {
	w := &r.wtriage
	stillNeedy := map[string]bool{}
	for _, a := range needy {
		stillNeedy[a.ID] = true
	}
	ids := make([]string, 0, len(diags))
	for id := range diags {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		d := diags[id]
		if !stillNeedy[id] {
			c.auditWorkerDiagnosis(ctx, r, id, d, "discarded", "worker no longer idle or waiting")
			continue
		}
		w.diags[id] = workerDiagEntry{diag: d, at: now}
		delete(w.findings, id)
		delete(w.handled, id)
		switch d.Action {
		case fastbrain.ActionWait:
			if w.waits[id] >= c.guardian.MaxWaits {
				c.auditWorkerDiagnosis(ctx, r, id, d, "mechanical", "wait bounds exceeded")
				continue
			}
			w.waits[id]++
			w.handled[id] = true
			c.auditWorkerDiagnosis(ctx, r, id, d, "applied", fmt.Sprintf("wait %d/%d", w.waits[id], c.guardian.MaxWaits))
		case fastbrain.ActionNudge:
			w.findings[id] = d.Text
			c.auditWorkerDiagnosis(ctx, r, id, d, "applied", "finding reported to the manager")
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
				if !errors.Is(err, ErrAgentNotFound) {
					slog.Warn("autopilot overwatch: worker action failed", "run", r.runID, "worker", id, "action", d.Action, "err", err)
				}
				c.auditWorkerDiagnosis(ctx, r, id, d, "mechanical", string(d.Action)+" failed: "+err.Error())
				continue
			}
			w.handled[id] = true
			c.auditWorkerDiagnosis(ctx, r, id, d, "applied", string(d.Action))
		default:
			c.auditWorkerDiagnosis(ctx, r, id, d, "mechanical", "generic nudge")
		}
	}
}

// auditWorkerDiagnosis records a worker triage decision (autopilot.overwatch_diagnosis).
func (c *Controller) auditWorkerDiagnosis(ctx context.Context, r *run, workerID string, d fastbrain.StallDiagnosis, outcome, note string) {
	gr, ok := c.runtime.(GuardianRuntime)
	if !ok {
		return
	}
	detail := fmt.Sprintf("worker=%s action=%s outcome=%s source=%s confidence=%.2f failopen=%q rationale=%q note=%q",
		workerID, d.Action, outcome, d.Source, d.Confidence, d.FailOpen, d.Rationale, note)
	gr.AuditRunEvent(ctx, r.runID, "autopilot.overwatch_diagnosis", workerID, detail)
}

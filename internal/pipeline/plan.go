package pipeline

// Decision is the pure output of Plan: which pending jobs are ready to spawn,
// which pending jobs must be skipped (their run_if condition can't be met), and
// the resulting pipeline status. It performs no side effects.
type Decision struct {
	Spawn  []string // job ids ready to spawn (all deps terminal, run_if satisfied)
	Skip   []string // job ids to mark skipped (run_if condition unmet)
	Status Status
}

// jobRunIf returns a job's run_if, defaulting an empty value to "success" so a
// pipeline constructed in code (not via ParseSpec) still gates correctly.
func jobRunIf(j *Job) string {
	if j.RunIf == "" {
		return "success"
	}
	return j.RunIf
}

// Plan computes the next reconcile decision from current job statuses.
//
// A pending job is decided only once ALL its dependencies have settled into a
// terminal state (done/failed/skipped). Its run_if then chooses spawn vs skip:
//
//	success (default): run iff every dep is done; else skip.
//	failure:           run iff at least one dep failed; else skip.
//	always:            run regardless of dep outcomes.
//
// Skips cascade: a skipped dep is terminal, so it settles its own dependents,
// which may in turn be skipped (success) or run (failure/always). The skip set
// is therefore computed to a fixpoint before spawns are read off.
func Plan(p *Pipeline) Decision {
	status := map[string]JobStatus{}
	for i := range p.Jobs {
		status[p.Jobs[i].ID] = p.Jobs[i].Status
	}

	skip := map[string]bool{}
	// settled reports whether a dep has reached a terminal state: a real terminal
	// status or a skip already decided in this pass.
	settled := func(id string) bool {
		s := status[id]
		return s == JobDone || s == JobFailed || s == JobSkipped || skip[id]
	}
	// notDone reports a settled-but-unsuccessful dep (failed or skipped) — the
	// condition that dooms a success job, decidable before its other deps settle.
	notDone := func(id string) bool {
		return status[id] == JobFailed || status[id] == JobSkipped || skip[id]
	}
	depsSettled := func(j *Job) bool {
		for _, dep := range j.DependsOn {
			if !settled(dep) {
				return false
			}
		}
		return true
	}

	// Fixpoint over the skip set. A success job is skipped eagerly the moment any
	// dep can't succeed (matching the original fail-fast behavior); a failure job
	// is skipped only once all deps settle with none having failed. Each new skip
	// settles a dep, which can cascade into more skips.
	changed := true
	for changed {
		changed = false
		for i := range p.Jobs {
			j := &p.Jobs[i]
			if status[j.ID] != JobPending || skip[j.ID] {
				continue
			}
			switch jobRunIf(j) {
			case "always":
				// runs once deps settle; never skipped
			case "failure":
				if !depsSettled(j) {
					continue
				}
				failed := false
				for _, dep := range j.DependsOn {
					if status[dep] == JobFailed {
						failed = true
						break
					}
				}
				if !failed {
					skip[j.ID] = true
					changed = true
				}
			default: // success
				for _, dep := range j.DependsOn {
					if notDone(dep) {
						skip[j.ID] = true
						changed = true
						break
					}
				}
			}
		}
	}

	var d Decision
	for id := range skip {
		d.Skip = append(d.Skip, id)
	}
	// Spawn: a pending, un-skipped job whose deps have all settled has, by
	// definition, a satisfied run_if (a success job with a non-done dep would have
	// been skipped above; a failure job survives only with a failed dep).
	for i := range p.Jobs {
		j := &p.Jobs[i]
		if status[j.ID] != JobPending || skip[j.ID] || !depsSettled(j) {
			continue
		}
		d.Spawn = append(d.Spawn, j.ID)
	}

	d.Status = pipelineStatus(p, status, skip, len(d.Spawn))
	return d
}

// failureHandled reports whether a failed job has a downstream conditional step
// designed to react to its failure — a direct dependent whose run_if is failure
// or always. Such a failure is being recovered, not stalling the pipeline.
func failureHandled(p *Pipeline, failedID string) bool {
	for i := range p.Jobs {
		j := &p.Jobs[i]
		for _, dep := range j.DependsOn {
			if dep != failedID {
				continue
			}
			switch jobRunIf(j) {
			case "failure", "always":
				return true
			}
		}
	}
	return false
}

func pipelineStatus(p *Pipeline, status map[string]JobStatus, skip map[string]bool, spawnable int) Status {
	anyUnhandledFailed, anyRunning, allTerminal := false, false, true
	for i := range p.Jobs {
		id := p.Jobs[i].ID
		s := status[id]
		// A failure stalls the pipeline only when nothing downstream is set up to
		// handle it; an unhandled failure needs a human (retry/cancel).
		if s == JobFailed && !failureHandled(p, id) {
			anyUnhandledFailed = true
		}
		if s == JobRunning || s == JobNeedsAttention {
			anyRunning = true
		}
		// "terminal" for completion purposes = done/failed/skipped (incl. about-to-skip).
		if !(s == JobDone || s == JobFailed || s == JobSkipped || skip[id]) {
			allTerminal = false
		}
	}
	switch {
	case anyUnhandledFailed:
		return StatusStalled
	case allTerminal:
		return StatusDone
	case anyRunning || spawnable > 0:
		return StatusRunning
	default:
		return StatusPending
	}
}

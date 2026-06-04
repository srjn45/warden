package pipeline

// Decision is the pure output of Plan: which pending jobs are ready to spawn,
// which pending jobs must be skipped (a failed ancestor), and the resulting
// pipeline status. It performs no side effects.
type Decision struct {
	Spawn  []string // job ids ready to spawn (deps all done)
	Skip   []string // job ids to mark skipped (descendant of a failed job)
	Status Status
}

// Plan computes the next reconcile decision from current job statuses.
func Plan(p *Pipeline) Decision {
	status := map[string]JobStatus{}
	for i := range p.Jobs {
		status[p.Jobs[i].ID] = p.Jobs[i].Status
	}

	// Transitively mark descendants of any failed job for skipping (only those
	// still pending — a job already running/done is left as-is).
	skip := map[string]bool{}
	changed := true
	for changed {
		changed = false
		for i := range p.Jobs {
			j := &p.Jobs[i]
			if status[j.ID] != JobPending || skip[j.ID] {
				continue
			}
			for _, dep := range j.DependsOn {
				if status[dep] == JobFailed || skip[dep] {
					skip[j.ID] = true
					changed = true
					break
				}
			}
		}
	}

	var d Decision
	for id := range skip {
		d.Skip = append(d.Skip, id)
	}
	for i := range p.Jobs {
		j := &p.Jobs[i]
		if status[j.ID] != JobPending || skip[j.ID] {
			continue
		}
		ready := true
		for _, dep := range j.DependsOn {
			if status[dep] != JobDone {
				ready = false
				break
			}
		}
		if ready {
			d.Spawn = append(d.Spawn, j.ID)
		}
	}

	d.Status = pipelineStatus(p, status, skip, len(d.Spawn))
	return d
}

func pipelineStatus(p *Pipeline, status map[string]JobStatus, skip map[string]bool, spawnable int) Status {
	anyFailed, anyRunning, allTerminal := false, false, true
	for i := range p.Jobs {
		s := status[p.Jobs[i].ID]
		if s == JobFailed {
			anyFailed = true
		}
		if s == JobRunning || s == JobNeedsAttention {
			anyRunning = true
		}
		// "terminal" for completion purposes = done/failed/skipped (incl. about-to-skip).
		if !(s == JobDone || s == JobFailed || s == JobSkipped || skip[p.Jobs[i].ID]) {
			allTerminal = false
		}
	}
	switch {
	case anyFailed:
		return StatusStalled
	case allTerminal:
		return StatusDone
	case anyRunning || spawnable > 0:
		return StatusRunning
	default:
		return StatusPending
	}
}

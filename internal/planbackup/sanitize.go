package planbackup

import "github.com/srjn45/warden/internal/planstore"

// sanitizePlan strips fields that must never ride in a portable backup:
// credentials are not stored on Plan today, and disposable worktree/executor
// handles are cleared so restore cannot imply local filesystem state exists.
// Audit evidence (ExecutionHistory, TaskOutcomes, BranchSummaries, events,
// summaries) is retained. ActiveExecution is kept as historical metadata but
// cleared of live executor IDs that would point at missing agents/worktrees.
func sanitizePlan(p *planstore.Plan) *planstore.Plan {
	if p == nil {
		return nil
	}
	// Deep-ish copy via field assignment so we never mutate the store's live record.
	out := *p
	out.Constraints = append([]string(nil), p.Constraints...)
	out.DoneWhen = append([]string(nil), p.DoneWhen...)
	if p.Tasks != nil {
		out.Tasks = make([]planstore.PlanTask, len(p.Tasks))
		copy(out.Tasks, p.Tasks)
		for i := range out.Tasks {
			out.Tasks[i].After = append([]string(nil), p.Tasks[i].After...)
		}
	}
	if p.TaskProgress != nil {
		out.TaskProgress = make(map[string]string, len(p.TaskProgress))
		for k, v := range p.TaskProgress {
			out.TaskProgress[k] = v
		}
	}
	out.Branches = append([]string(nil), p.Branches...)
	if p.RepoExport != nil {
		re := *p.RepoExport
		out.RepoExport = &re
	}
	if p.ActiveExecution != nil {
		ae := *p.ActiveExecution
		ae.ExecutorID = "" // disposable — agent/pipeline may not exist on target machine
		ae.PlanBranches = append([]string(nil), p.ActiveExecution.PlanBranches...)
		if p.ActiveExecution.TaskProgress != nil {
			ae.TaskProgress = make(map[string]string, len(p.ActiveExecution.TaskProgress))
			for k, v := range p.ActiveExecution.TaskProgress {
				ae.TaskProgress[k] = v
			}
		}
		if p.ActiveExecution.Snapshot != nil {
			snap := *p.ActiveExecution.Snapshot
			snap.Constraints = append([]string(nil), p.ActiveExecution.Snapshot.Constraints...)
			snap.DoneWhen = append([]string(nil), p.ActiveExecution.Snapshot.DoneWhen...)
			snap.Tasks = append([]planstore.PlanTask(nil), p.ActiveExecution.Snapshot.Tasks...)
			ae.Snapshot = &snap
		}
		out.ActiveExecution = &ae
	}
	if p.ExecutionHistory != nil {
		out.ExecutionHistory = make([]planstore.PlanExecution, len(p.ExecutionHistory))
		copy(out.ExecutionHistory, p.ExecutionHistory)
	}
	if p.TaskOutcomes != nil {
		out.TaskOutcomes = make(map[string]planstore.TaskOutcome, len(p.TaskOutcomes))
		for k, v := range p.TaskOutcomes {
			out.TaskOutcomes[k] = v
		}
	}
	out.BranchSummaries = append([]planstore.BranchSummary(nil), p.BranchSummaries...)
	if p.ExecutionSummary != nil {
		es := *p.ExecutionSummary
		out.ExecutionSummary = &es
	}
	// CleanupEvidence often names local worktree paths — drop it; Finalize can
	// re-derive cleanup on a fresh machine if needed.
	out.CleanupEvidence = nil
	// Hub / remote credentials seam — never copy opaque remote tokens.
	out.RemoteID = ""
	out.SyncedAt = nil
	// Top-level live executor links are disposable on the destination.
	out.AutopilotRunID = ""
	out.PipelineID = ""
	out.OrchestratorID = ""
	return &out
}

func cloneEvents(in []*planstore.PlanExecutionEvent) []*planstore.PlanExecutionEvent {
	if len(in) == 0 {
		return nil
	}
	out := make([]*planstore.PlanExecutionEvent, 0, len(in))
	for _, ev := range in {
		if ev == nil {
			continue
		}
		cp := *ev
		if ev.Payload != nil {
			pl := *ev.Payload
			cp.Payload = &pl
		}
		out = append(out, &cp)
	}
	return out
}

func cloneNotes(in []*planstore.ExecutionNote) []*planstore.ExecutionNote {
	if len(in) == 0 {
		return nil
	}
	out := make([]*planstore.ExecutionNote, 0, len(in))
	for _, n := range in {
		if n == nil {
			continue
		}
		cp := *n
		out = append(out, &cp)
	}
	return out
}

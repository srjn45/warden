package daemon

import (
	"context"

	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/planstore"
)

// planExecutorStatus builds the live executor block for GET /plans/{plan_id}.
// It returns nil when the plan has no executor (pending, completed without a
// linked executor) or the executor record can no longer be resolved, so such
// plans render exactly as before.
func (s *Server) planExecutorStatus(ctx context.Context, p *planstore.Plan) *oapi.PlanExecutorStatus {
	switch {
	case p.AutopilotRunID != "" && s.autopilot != nil:
		// Status() (not LookupRun) carries the brain and backoff blocks.
		var rs *autopilot.RunStatus
		st := s.autopilot.Status()
		for i := range st.Runs {
			if st.Runs[i].RunID == p.AutopilotRunID {
				rs = &st.Runs[i]
				break
			}
		}
		if rs == nil {
			return nil
		}
		out := &oapi.PlanExecutorStatus{
			Kind:              "autopilot",
			Id:                rs.RunID,
			State:             string(rs.State),
			IntegrationBranch: rs.IntegrationBranch,
		}
		if rs.Brain != nil {
			out.ManagerAgentId = rs.Brain.AgentID
		}
		if b := rs.Backoff; b != nil {
			out.Backoff = &oapi.PlanExecutorBackoff{Stage: b.Stage, NextRetryAt: b.NextRetryAt, LastError: b.LastError}
		}
		for _, lt := range rs.LedgerTasks {
			out.Tasks = append(out.Tasks, oapi.PlanExecutorTask{
				Id: lt.ID, State: string(lt.State), WorkerAgentId: lt.WorkerID, Branch: lt.Branch, Pr: lt.PR,
			})
		}
		return out
	case p.PipelineID != "" && s.exec != nil && s.exec.pstore != nil:
		pl, err := s.exec.pstore.Get(p.PipelineID)
		if err != nil || pl == nil {
			return nil
		}
		out := &oapi.PlanExecutorStatus{Kind: "pipeline", Id: pl.ID, State: string(pl.Status)}
		for _, j := range pl.Jobs {
			out.Tasks = append(out.Tasks, oapi.PlanExecutorTask{
				Id: j.ID, State: string(j.Status), WorkerAgentId: j.AgentRef(), Branch: j.Branch,
			})
		}
		return out
	}
	agentID := p.OrchestratorID
	if agentID == "" && p.ActiveExecution != nil {
		agentID = p.ActiveExecution.ExecutorID
	}
	if agentID == "" || s.store == nil {
		return nil
	}
	sess, err := s.store.Get(ctx, agentID)
	if err != nil || sess == nil {
		return nil
	}
	return &oapi.PlanExecutorStatus{Kind: "agent", Id: agentID, State: string(sess.Status)}
}

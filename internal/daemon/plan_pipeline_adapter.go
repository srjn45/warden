package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/planstore"
)

const planPipelineNamePrefix = "P:"

var planPipelineSlugSanitizer = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

// sanitizePlanPipelineSlug forces planName into the P:<slug> charset and
// truncates to 64 chars so the display name matches ValidName.
func sanitizePlanPipelineSlug(planName string) string {
	slug := planPipelineSlugSanitizer.ReplaceAllString(strings.TrimSpace(planName), "-")
	slug = strings.Trim(slug, "-_")
	if slug == "" {
		return "unnamed"
	}
	if len(slug) > 64 {
		slug = slug[:64]
		slug = strings.TrimRight(slug, "-_")
	}
	if slug == "" {
		return "unnamed"
	}
	return slug
}

// pipelineDisplayName returns the canonical P:<plan-name> display name.
func pipelineDisplayName(planName string) string {
	name := strings.TrimSpace(planName)
	if strings.HasPrefix(name, planPipelineNamePrefix) {
		return planPipelineNamePrefix + sanitizePlanPipelineSlug(strings.TrimPrefix(name, planPipelineNamePrefix))
	}
	return planPipelineNamePrefix + sanitizePlanPipelineSlug(name)
}

// buildPlanPipeline constructs a plan-bound pipeline: Name=P:<plan-name>,
// ID=plan ID (SafeID-clean for branch/agent derivation), PlanID set, one Job
// per canonical Plan task with the same stable task ID and after→depends_on
// edges. Tasks are read from ScrivaDB only (snapshot source at run start).
func buildPlanPipeline(p *planstore.Plan, root string) (*pipeline.Pipeline, map[string]string, error) {
	taskJob := map[string]string{}
	var jobs []pipeline.Job
	snap := planDefinitionForExecution(p)
	if snap != nil {
		for _, t := range snap.Tasks {
			id := strings.TrimSpace(t.ID)
			if id == "" {
				continue
			}
			jobs = append(jobs, pipeline.Job{
				ID:        id,
				Prompt:    t.Prompt,
				DependsOn: append([]string(nil), t.After...),
				Worktree:  "fresh",
				Type:      "development",
				Status:    pipeline.JobPending,
			})
			taskJob[id] = id
		}
	}
	if len(jobs) == 0 {
		// Fall back to a single job with the plan name as the prompt.
		jobs = []pipeline.Job{{
			ID:       "run",
			Prompt:   "Execute the plan: " + p.Name,
			Worktree: "fresh",
			Type:     "development",
			Status:   pipeline.JobPending,
		}}
		taskJob["run"] = "run"
	}
	pl := &pipeline.Pipeline{
		// ID stays the plan ID (SafeID) so git branch / SpawnJob ids stay valid.
		// Name is the operator-facing P:<plan-name> label.
		ID:        p.ID,
		Name:      pipelineDisplayName(p.Name),
		Repo:      root,
		ProjectID: p.ProjectID,
		PlanID:    p.ID,
		Status:    pipeline.StatusPending,
		Jobs:      jobs,
	}
	if err := pipeline.Validate(pl); err != nil {
		return nil, nil, err
	}
	return pl, taskJob, nil
}

// beginPlanPipelineExecution stamps ActiveExecution (with task→job map) and
// appends execution_started + executor_created events.
func (s *Server) beginPlanPipelineExecution(ctx context.Context, p *planstore.Plan, pl *pipeline.Pipeline, taskJob map[string]string) error {
	if s.plans == nil || p == nil || pl == nil {
		return nil
	}
	execID := planstore.NewPlanExecutionID()
	now := time.Now().UTC()
	progress := make(map[string]string, len(taskJob))
	for taskID := range taskJob {
		progress[taskID] = "pending"
	}
	pe := planstore.PlanExecution{
		ID:             execID,
		PlanID:         p.ID,
		ExecutionMode:  planstore.PlanModePipeline,
		ExecutorID:     pl.ID,
		StartedAt:      now,
		TerminalStatus: planstore.ExecutionStatusRunning,
		TaskProgress:   progress,
		TaskJobMap:     taskJob,
		Snapshot:       planstore.SnapshotFromPlan(p),
	}
	if err := s.plans.Update(ctx, p.ID, func(up *planstore.Plan) error {
		up.ActiveExecution = &pe
		up.ExecutionMode = planstore.PlanModePipeline
		up.PipelineID = pl.ID
		if up.TaskProgress == nil {
			up.TaskProgress = map[string]string{}
		}
		for taskID := range taskJob {
			if _, ok := up.TaskProgress[taskID]; !ok {
				up.TaskProgress[taskID] = "pending"
			}
		}
		return nil
	}); err != nil {
		return errStatus(http.StatusInternalServerError, "record plan pipeline execution: "+err.Error())
	}

	events := []*planstore.PlanExecutionEvent{
		{
			DedupKey:    p.ID + ":" + execID + ":execution_started",
			PlanID:      p.ID,
			ExecutionID: execID,
			Kind:        planstore.EventKindExecutionStarted,
			OccurredAt:  now,
			Payload: &planstore.EventPayload{
				ExecutorID:    pl.ID,
				ExecutionMode: string(planstore.PlanModePipeline),
				PlanName:      p.Name,
				TasksTotal:    len(taskJob),
			},
		},
		{
			DedupKey:    p.ID + ":" + execID + ":executor_created",
			PlanID:      p.ID,
			ExecutionID: execID,
			Kind:        planstore.EventKindExecutorCreated,
			OccurredAt:  now,
			Payload: &planstore.EventPayload{
				ExecutorID:    pl.ID,
				ExecutionMode: string(planstore.PlanModePipeline),
			},
		},
	}
	for _, ev := range events {
		if err := s.plans.AppendEvent(ctx, ev); err != nil {
			return errStatus(http.StatusInternalServerError, "append plan pipeline event: "+err.Error())
		}
	}
	return nil
}

// startPlanPipeline creates the P:<plan> pipeline, records ActiveExecution
// evidence, membership, and starts reconcile. Independent CreatePipeline is
// unchanged — this path is only reached from run_plan mode=pipeline.
func (s *Server) startPlanPipeline(ctx context.Context, p *planstore.Plan, root string) (string, error) {
	if s.exec == nil {
		return "", errStatus(http.StatusServiceUnavailable, "pipeline executor not configured")
	}
	pl, taskJob, buildErr := buildPlanPipeline(p, root)
	if buildErr != nil {
		return "", errStatus(http.StatusInternalServerError, "build pipeline: "+buildErr.Error())
	}
	if err := s.exec.pstore.Create(pl); err != nil {
		if errors.Is(err, pipeline.ErrExists) {
			return "", err
		}
		return "", errStatus(http.StatusInternalServerError, "create pipeline: "+err.Error())
	}
	s.addPipelineMembership(pl)
	s.addPlanMembership(p.ID, p.ProjectID)
	if err := s.beginPlanPipelineExecution(ctx, p, pl, taskJob); err != nil {
		return "", err
	}
	_ = s.exec.pstore.Update(pl.ID, func(up *pipeline.Pipeline) { up.Status = pipeline.StatusRunning })
	_ = s.exec.Reconcile(context.Background(), pl.ID)
	return pl.ID, nil
}

// planForPipeline looks up the Plan linked to a plan-bound pipeline. Returns
// nil when the pipeline is planless or has no ActiveExecution (hooks are no-ops).
func (s *Server) planForPipeline(ctx context.Context, pl *pipeline.Pipeline) *planstore.Plan {
	if s.plans == nil || pl == nil || strings.TrimSpace(pl.PlanID) == "" {
		return nil
	}
	p, err := s.plans.Get(ctx, pl.PlanID)
	if err != nil || p == nil || p.ActiveExecution == nil || p.ActiveExecution.ID == "" {
		return nil
	}
	return p
}

// resolvePlanTaskID maps a pipeline job ID back to a plan task ID via
// ActiveExecution.TaskJobMap (identity when the adapter stored it that way).
func resolvePlanTaskID(p *planstore.Plan, jobID string) string {
	if p == nil || p.ActiveExecution == nil {
		return jobID
	}
	for taskID, mapped := range p.ActiveExecution.TaskJobMap {
		if mapped == jobID {
			return taskID
		}
	}
	if p.ActiveExecution.TaskJobMap != nil {
		if _, ok := p.ActiveExecution.TaskJobMap[jobID]; ok {
			return jobID
		}
	}
	return jobID
}

func (s *Server) appendPlanPipelineEvent(ctx context.Context, p *planstore.Plan, kind planstore.EventKind, payload *planstore.EventPayload, dedupSuffix string) {
	if p == nil || p.ActiveExecution == nil {
		return
	}
	if payload == nil {
		payload = &planstore.EventPayload{}
	}
	if payload.ExecutorID == "" {
		payload.ExecutorID = p.ActiveExecution.ExecutorID
	}
	if payload.ExecutionMode == "" {
		payload.ExecutionMode = string(planstore.PlanModePipeline)
	}
	dedup := fmt.Sprintf("%s:%s:%s", p.ID, p.ActiveExecution.ID, string(kind))
	if dedupSuffix != "" {
		dedup += ":" + dedupSuffix
	}
	ev := &planstore.PlanExecutionEvent{
		DedupKey:    dedup,
		PlanID:      p.ID,
		ExecutionID: p.ActiveExecution.ID,
		Kind:        kind,
		OccurredAt:  time.Now().UTC(),
		Payload:     payload,
	}
	if err := s.plans.AppendEvent(ctx, ev); err != nil {
		slog.Debug("plan pipeline event append failed", "plan", p.ID, "kind", kind, "err", err)
	}
}

func (s *Server) updatePlanTaskEvidence(ctx context.Context, p *planstore.Plan, taskID, status, agentID string) {
	if p == nil || taskID == "" {
		return
	}
	if svc := s.planSvc(); svc != nil {
		if _, err := svc.UpdateTaskStatus(ctx, p.ID, taskID, status); err != nil {
			slog.Debug("plan task status update failed", "plan", p.ID, "task", taskID, "status", status, "err", err)
		}
	}
	now := time.Now().UTC()
	_ = s.plans.Update(ctx, p.ID, func(up *planstore.Plan) error {
		if up.ActiveExecution != nil {
			if up.ActiveExecution.TaskProgress == nil {
				up.ActiveExecution.TaskProgress = map[string]string{}
			}
			up.ActiveExecution.TaskProgress[taskID] = status
		}
		if status == "done" || status == "skipped" {
			if up.TaskOutcomes == nil {
				up.TaskOutcomes = map[string]planstore.TaskOutcome{}
			}
			out := up.TaskOutcomes[taskID]
			out.TaskID = taskID
			out.Status = status
			if agentID != "" {
				out.AssignedAgent = agentID
			}
			out.CompletedAt = now
			up.TaskOutcomes[taskID] = out
		} else if agentID != "" {
			if up.TaskOutcomes == nil {
				up.TaskOutcomes = map[string]planstore.TaskOutcome{}
			}
			out := up.TaskOutcomes[taskID]
			out.TaskID = taskID
			if out.Status == "" {
				out.Status = status
			}
			out.AssignedAgent = agentID
			up.TaskOutcomes[taskID] = out
		}
		return nil
	})
}

// --- PlanPipelineHook (Executor → Plan service) ---

// OnJobAssigned records task_assigned + agent_spawned and marks the task in_progress.
func (s *Server) OnJobAssigned(pl *pipeline.Pipeline, jobID, agentID string) {
	ctx := context.Background()
	p := s.planForPipeline(ctx, pl)
	if p == nil {
		return
	}
	taskID := resolvePlanTaskID(p, jobID)
	s.updatePlanTaskEvidence(ctx, p, taskID, "in_progress", agentID)
	s.appendPlanPipelineEvent(ctx, p, planstore.EventKindTaskAssigned, &planstore.EventPayload{
		TaskID:  taskID,
		AgentID: agentID,
	}, taskID)
	if agentID != "" {
		s.appendPlanPipelineEvent(ctx, p, planstore.EventKindAgentSpawned, &planstore.EventPayload{
			TaskID:  taskID,
			AgentID: agentID,
		}, agentID)
	}
}

// OnJobCompleted records task_evidence_verified and marks the task done.
func (s *Server) OnJobCompleted(pl *pipeline.Pipeline, jobID, agentID string) {
	ctx := context.Background()
	p := s.planForPipeline(ctx, pl)
	if p == nil {
		return
	}
	taskID := resolvePlanTaskID(p, jobID)
	s.updatePlanTaskEvidence(ctx, p, taskID, "done", agentID)
	s.appendPlanPipelineEvent(ctx, p, planstore.EventKindTaskEvidenceVerified, &planstore.EventPayload{
		TaskID:  taskID,
		AgentID: agentID,
	}, taskID)
	if agentID != "" {
		s.appendPlanPipelineEvent(ctx, p, planstore.EventKindAgentFinished, &planstore.EventPayload{
			TaskID:  taskID,
			AgentID: agentID,
		}, agentID)
	}
}

// OnJobFailed records agent_finished for the failed job without marking the
// task done (UpdateTaskStatus has no "failed" status).
func (s *Server) OnJobFailed(pl *pipeline.Pipeline, jobID, agentID string) {
	ctx := context.Background()
	p := s.planForPipeline(ctx, pl)
	if p == nil {
		return
	}
	taskID := resolvePlanTaskID(p, jobID)
	s.appendPlanPipelineEvent(ctx, p, planstore.EventKindAgentFinished, &planstore.EventPayload{
		TaskID:        taskID,
		AgentID:       agentID,
		FailureReason: "pipeline job failed",
	}, "failed:"+jobID)
}

// OnJobSkipped marks the corresponding plan task skipped.
func (s *Server) OnJobSkipped(pl *pipeline.Pipeline, jobID string) {
	ctx := context.Background()
	p := s.planForPipeline(ctx, pl)
	if p == nil {
		return
	}
	taskID := resolvePlanTaskID(p, jobID)
	s.updatePlanTaskEvidence(ctx, p, taskID, "skipped", "")
	s.appendPlanPipelineEvent(ctx, p, planstore.EventKindTaskEvidenceVerified, &planstore.EventPayload{
		TaskID: taskID,
	}, "skipped:"+taskID)
}

// OnPipelineTerminal seals the ActiveExecution when the pipeline reaches a
// terminal status (done / stalled / canceled).
func (s *Server) OnPipelineTerminal(pl *pipeline.Pipeline) {
	ctx := context.Background()
	p := s.planForPipeline(ctx, pl)
	if p == nil {
		return
	}
	var (
		kind    planstore.EventKind
		term    planstore.ExecutionStatus
		payload *planstore.EventPayload
	)
	switch pl.Status {
	case pipeline.StatusDone:
		kind = planstore.EventKindCompletionVerified
		term = planstore.ExecutionStatusCompleted
		payload = &planstore.EventPayload{ExecutorID: pl.ID, ExecutionMode: string(planstore.PlanModePipeline)}
	case pipeline.StatusCanceled:
		kind = planstore.EventKindExecutionStopped
		term = planstore.ExecutionStatusCancelled
		payload = &planstore.EventPayload{
			ExecutorID: pl.ID, ExecutionMode: string(planstore.PlanModePipeline),
			StopReason: "pipeline canceled",
		}
	case pipeline.StatusStalled:
		kind = planstore.EventKindExecutionFailed
		term = planstore.ExecutionStatusFailed
		payload = &planstore.EventPayload{
			ExecutorID: pl.ID, ExecutionMode: string(planstore.PlanModePipeline),
			FailureReason: "pipeline stalled",
		}
	default:
		return
	}
	s.appendPlanPipelineEvent(ctx, p, kind, payload, string(pl.Status))
	now := time.Now().UTC()
	_ = s.plans.Update(ctx, p.ID, func(up *planstore.Plan) error {
		if up.ActiveExecution == nil {
			return nil
		}
		up.ActiveExecution.TerminalStatus = term
		up.ActiveExecution.CompletedAt = &now
		return nil
	})
}

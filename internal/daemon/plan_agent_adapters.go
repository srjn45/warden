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

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/planstore"
)

const (
	planOrchestratorNamePrefix = "O:"
	planManualNamePrefix       = "M:"
)

var planSlugSanitizer = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

// sanitizePlanSlug forces planName into the ValidateName slug charset and
// truncates to 64 chars so O:/M:<slug> stays within planExecutorNamePattern.
func sanitizePlanSlug(planName string) string {
	slug := planSlugSanitizer.ReplaceAllString(strings.TrimSpace(planName), "-")
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

// orchestratorDisplayName returns the canonical O:<plan-name> display name.
func orchestratorDisplayName(planName string) string {
	name := strings.TrimSpace(planName)
	if strings.HasPrefix(name, planOrchestratorNamePrefix) {
		return planOrchestratorNamePrefix + sanitizePlanSlug(strings.TrimPrefix(name, planOrchestratorNamePrefix))
	}
	return planOrchestratorNamePrefix + sanitizePlanSlug(name)
}

// manualDisplayName returns the canonical M:<plan-name> display name.
func manualDisplayName(planName string) string {
	name := strings.TrimSpace(planName)
	if strings.HasPrefix(name, planManualNamePrefix) {
		return planManualNamePrefix + sanitizePlanSlug(strings.TrimPrefix(name, planManualNamePrefix))
	}
	return planManualNamePrefix + sanitizePlanSlug(name)
}

// planTasksTotal returns the number of tasks on the canonical Plan (or its
// execution snapshot), never from repository YAML.
func planTasksTotal(p *planstore.Plan, _ string) int {
	snap := planDefinitionForExecution(p)
	if snap == nil {
		return 0
	}
	return len(snap.Tasks)
}

// spawnPlanBoundAgent launches a free-form plan-bound agent (orchestrator or
// manual), persists it, and joins project membership. Autopilot is never
// created — Plan owns task evidence; the agent is only a disposable executor.
func (s *Server) spawnPlanBoundAgent(ctx context.Context, p *planstore.Plan, root, role, name, prompt string) (*agentstore.Agent, error) {
	if s.life == nil {
		return nil, errStatus(http.StatusServiceUnavailable, "lifecycle not configured")
	}
	if s.store == nil {
		return nil, errStatus(http.StatusServiceUnavailable, "agent store not configured")
	}
	req := SpawnRequest{
		Name:      name,
		Repo:      root,
		Cwd:       root,
		Role:      role,
		ProjectID: p.ProjectID,
		PlanID:    p.ID,
		Prompt:    prompt,
	}
	if code, msg := s.validateSpawnRequest(ctx, req); code != 0 {
		return nil, errStatus(code, msg)
	}
	sess, err := s.life.Spawn(ctx, req)
	if err != nil {
		return nil, errStatus(http.StatusInternalServerError, "spawn plan agent: "+err.Error())
	}
	s.stampProjectMembership(sess)
	if err := s.store.Insert(ctx, sess); err != nil {
		tctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if terr := s.life.Teardown(tctx, sess); terr != nil {
			slog.Warn("plan agent spawn rollback failed", "agent", sess.ID, "err", terr)
		}
		return nil, errStatus(http.StatusInternalServerError, "persist plan agent: "+err.Error())
	}
	s.addProjectMembership(sess)
	s.notify()
	return sess, nil
}

// beginPlanAgentExecution stamps ActiveExecution on the plan and appends the
// standard start events (execution_started, executor_created, agent_spawned).
// orchestratorID is recorded on Plan.OrchestratorID when non-empty (orchestrator
// mode only); manual mode leaves that field empty and uses ActiveExecution only.
func (s *Server) beginPlanAgentExecution(ctx context.Context, p *planstore.Plan, mode planstore.PlanExecutionMode, executorID, agentID, root string) error {
	if s.plans == nil || p == nil {
		return nil
	}
	execID := planstore.NewPlanExecutionID()
	now := time.Now().UTC()
	tasksTotal := planTasksTotal(p, root)
	pe := planstore.PlanExecution{
		ID:             execID,
		PlanID:         p.ID,
		ExecutionMode:  mode,
		ExecutorID:     executorID,
		StartedAt:      now,
		TerminalStatus: planstore.ExecutionStatusRunning,
		Snapshot:       planstore.SnapshotFromPlan(p),
	}

	if err := s.plans.Update(ctx, p.ID, func(pl *planstore.Plan) error {
		pl.ActiveExecution = &pe
		pl.ExecutionMode = mode
		if mode == planstore.PlanModeOrchestratorWorker && executorID != "" {
			pl.OrchestratorID = executorID
		}
		return nil
	}); err != nil {
		return errStatus(http.StatusInternalServerError, "record plan execution: "+err.Error())
	}

	events := []*planstore.PlanExecutionEvent{
		{
			DedupKey:    p.ID + ":" + execID + ":execution_started",
			PlanID:      p.ID,
			ExecutionID: execID,
			Kind:        planstore.EventKindExecutionStarted,
			OccurredAt:  now,
			Payload: &planstore.EventPayload{
				ExecutorID:    executorID,
				ExecutionMode: string(mode),
				PlanName:      p.Name,
				TasksTotal:    tasksTotal,
				AgentID:       agentID,
			},
		},
		{
			DedupKey:    p.ID + ":" + execID + ":executor_created",
			PlanID:      p.ID,
			ExecutionID: execID,
			Kind:        planstore.EventKindExecutorCreated,
			OccurredAt:  now,
			Payload: &planstore.EventPayload{
				ExecutorID:    executorID,
				ExecutionMode: string(mode),
				AgentID:       agentID,
			},
		},
	}
	if agentID != "" {
		events = append(events, &planstore.PlanExecutionEvent{
			DedupKey:    p.ID + ":" + execID + ":agent_spawned:" + agentID,
			PlanID:      p.ID,
			ExecutionID: execID,
			Kind:        planstore.EventKindAgentSpawned,
			OccurredAt:  now,
			Payload: &planstore.EventPayload{
				AgentID:    agentID,
				ExecutorID: executorID,
			},
		})
	}
	for _, ev := range events {
		if err := s.plans.AppendEvent(ctx, ev); err != nil {
			return errStatus(http.StatusInternalServerError, "append plan event: "+err.Error())
		}
	}
	return nil
}

// stampPlanSpawnBackRefs inherits PlanID from a plan-bound parent when the spawn
// request left it empty. Autopilot-owned workers keep their AutopilotRunID path
// (stampAutopilotSpawnBackRefs); this only fills PlanID so orchestrator workers
// remain plan-bound for task evidence and event attribution.
func (s *Server) stampPlanSpawnBackRefs(ctx context.Context, sr *SpawnRequest) {
	if sr == nil || strings.TrimSpace(sr.PlanID) != "" {
		return
	}
	caller := s.callerSession(ctx)
	if caller == nil || strings.TrimSpace(caller.PlanID) == "" {
		return
	}
	sr.PlanID = caller.PlanID
}

// recordPlanBoundAgentEvent appends a PlanExecutionEvent for a plan-bound agent
// action (spawn / check / commit / push / PR). Best-effort: missing plan or
// ActiveExecution is a no-op so planless agents are unaffected.
func (s *Server) recordPlanBoundAgentEvent(sess *agentstore.Agent, kind planstore.EventKind, payload *planstore.EventPayload, dedupSuffix string) {
	if s.plans == nil || sess == nil || strings.TrimSpace(sess.PlanID) == "" {
		return
	}
	ctx := context.Background()
	p, err := s.plans.Get(ctx, sess.PlanID)
	if err != nil || p == nil || p.ActiveExecution == nil || p.ActiveExecution.ID == "" {
		return
	}
	if payload == nil {
		payload = &planstore.EventPayload{}
	}
	if payload.AgentID == "" {
		payload.AgentID = sess.ID
	}
	if payload.ExecutorID == "" {
		payload.ExecutorID = p.ActiveExecution.ExecutorID
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
		slog.Debug("plan event append failed", "plan", p.ID, "kind", kind, "err", err)
	}
}

// planningAgentPrompt builds the opening prompt for orchestrator/manual plan
// agents from ScrivaDB only, including a heuristic related-plan block when
// siblings exist. Repository YAML is never read.
func (s *Server) planningAgentPrompt(ctx context.Context, p *planstore.Plan, root, mode string) string {
	_ = root
	var base string
	switch mode {
	case "orchestrator":
		base = orchestratorPlanPrompt(p, root)
	default:
		base = manualPlanPrompt(p, root)
	}
	if s.plans == nil || p == nil {
		return base
	}
	candidates, err := s.plans.ListByProject(ctx, p.ProjectID)
	if err != nil {
		return base
	}
	related := planstore.FindRelatedPlans(p, candidates, 5)
	return base + planstore.FormatRelatedPlansContext(related)
}

// orchestratorPlanPrompt builds the opening prompt for an O:<plan> agent from
// the canonical ScrivaDB Plan definition (never repository YAML).
func orchestratorPlanPrompt(p *planstore.Plan, _ string) string {
	snap := planDefinitionForExecution(p)
	body := formatCanonicalPlanBody(snap)
	rev := int64(0)
	hash := ""
	var tasks []planstore.PlanTask
	if snap != nil {
		rev = snap.Revision
		hash = snap.ContentHash
		tasks = snap.Tasks
	}
	progress := map[string]string{}
	if p != nil && p.TaskProgress != nil {
		progress = p.TaskProgress
	}
	ready := planstore.ReadyTaskIDs(tasks, progress)
	blocked := planstore.BlockedTaskIDs(tasks, progress)
	return fmt.Sprintf("You are an orchestrator executing the following plan.\n\n"+
		"Plan ID: %s (revision %d, %s)\n\n%s\n\n"+
		"The tasks form a DAG (after: edges). Only start a task when every dependency "+
		"listed in after: is done. Ready now: [%s]. Blocked: [%s]. "+
		"Each worker you spawn must present its output for human approval before you "+
		"proceed. Workers are role=worker with you as ParentID; task assignment and "+
		"evidence remain on the Plan (do not create an Autopilot run).",
		p.ID, rev, hash, body, strings.Join(ready, ", "), strings.Join(blocked, ", "))
}

// manualPlanPrompt builds the opening prompt for an M:<plan> agent from the
// canonical ScrivaDB Plan definition (never repository YAML).
func manualPlanPrompt(p *planstore.Plan, _ string) string {
	snap := planDefinitionForExecution(p)
	body := formatCanonicalPlanBody(snap)
	rev := int64(0)
	hash := ""
	var tasks []planstore.PlanTask
	if snap != nil {
		rev = snap.Revision
		hash = snap.ContentHash
		tasks = snap.Tasks
	}
	progress := map[string]string{}
	if p != nil && p.TaskProgress != nil {
		progress = p.TaskProgress
	}
	ready := planstore.ReadyTaskIDs(tasks, progress)
	blocked := planstore.BlockedTaskIDs(tasks, progress)
	return fmt.Sprintf("You are driving this plan manually (execution mode=manual).\n\n"+
		"Plan ID: %s (revision %d, %s)\n\n%s\n\n"+
		"The tasks form a DAG (after: edges). Only work a task when every after "+
		"dependency is done. Ready now: [%s]. Blocked: [%s]. "+
		"Mark task progress on the Plan; do not create an Autopilot run. "+
		"Completion is via `wd plan complete`.",
		p.ID, rev, hash, body, strings.Join(ready, ", "), strings.Join(blocked, ", "))
}

// sealPlanAgentExecution appends completion_verified for orchestrator/manual
// ActiveExecutions that have started but not yet reached a terminal event.
// Operator `wd plan complete` is the authoritative completion signal for these
// modes; sealing lets EvalCompletionFromEvents treat NoLiveAgents as satisfied
// without requiring a separate executor teardown step first.
func (s *Server) sealPlanAgentExecution(ctx context.Context, planID string) error {
	if s.plans == nil || strings.TrimSpace(planID) == "" {
		return nil
	}
	p, err := s.plans.Get(ctx, planID)
	if err != nil {
		if errors.Is(err, planstore.ErrNotFound) {
			return nil
		}
		return err
	}
	if p.ActiveExecution == nil || p.ActiveExecution.ID == "" {
		return nil
	}
	mode := p.ActiveExecution.ExecutionMode
	if mode == "" {
		mode = p.ExecutionMode
	}
	if mode != planstore.PlanModeOrchestratorWorker && mode != planstore.PlanModeManual {
		return nil
	}
	events, err := s.plans.ListEvents(ctx, p.ID, p.ActiveExecution.ID)
	if err != nil {
		return err
	}
	for _, ev := range events {
		switch ev.Kind {
		case planstore.EventKindCompletionVerified, planstore.EventKindExecutionFailed, planstore.EventKindExecutionStopped:
			return nil
		}
	}
	now := time.Now().UTC()
	return s.plans.AppendEvent(ctx, &planstore.PlanExecutionEvent{
		DedupKey:    p.ID + ":" + p.ActiveExecution.ID + ":completion_verified",
		PlanID:      p.ID,
		ExecutionID: p.ActiveExecution.ID,
		Kind:        planstore.EventKindCompletionVerified,
		OccurredAt:  now,
		Payload: &planstore.EventPayload{
			ExecutorID:    p.ActiveExecution.ExecutorID,
			ExecutionMode: string(mode),
		},
	})
}

package workflows

import (
	"context"
	"fmt"

	"github.com/srjn45/warden/internal/dbtx"
)

// --- 1. project + child creation -------------------------------------------

// CreateProjectWithAgent creates the project (if absent) and its agent child in
// one transaction; if the project already exists it adds the agent to it
// (fenced on the project's revision, which also carries the live counter).
// Participants: projects, agents, [project_views].
func CreateProjectWithAgent(ctx context.Context, e *Env, projectID, agentID string) error {
	_, err := e.run(ctx, intent("create-project-agent", projectID+"/"+agentID), func(tx *dbtx.Tx) error {
		p, pok, err := e.Projects.ByKey(projectID)
		if err != nil {
			return err
		}
		a, aok, err := e.Agents.ByKey(agentID)
		if err != nil {
			return err
		}
		if _, arch, err := e.Archive.ByKey(agentID); err != nil {
			return err
		} else if arch {
			return fmt.Errorf("%w: agent %s is archived", ErrExists, agentID)
		}
		switch {
		case aok && pok && a.Value.ProjectID == projectID:
			return dbtx.ErrNoChange
		case aok:
			return fmt.Errorf("%w: agent %s", ErrExists, agentID)
		case !pok:
			if _, err := e.Projects.Insert(tx, Project{ID: projectID, Name: projectID, Status: "open", Version: 1, LiveAgents: 1}); err != nil {
				return err
			}
			_, err := e.Agents.Insert(tx, Agent{ID: agentID, ProjectID: projectID, Status: "running"})
			return err
		}
		if p.Value.Status != "open" {
			return fmt.Errorf("%w: %s", ErrProjectClosed, projectID)
		}
		return e.stageAgent(tx, p, Agent{ID: agentID, ProjectID: projectID, Status: "running"})
	})
	return err
}

// --- 7 (helpers). plan creation and audit append -----------------------------

// CreatePlan creates a draft plan at revision 1 with its creation event.
// Participants: plans, events, projects (fence).
func CreatePlan(ctx context.Context, e *Env, planID, projectID string) error {
	_, err := e.run(ctx, intent("create-plan", planID), func(tx *dbtx.Tx) error {
		if _, ok, err := e.Plans.ByKey(planID); err != nil {
			return err
		} else if ok {
			return dbtx.ErrNoChange
		}
		p, err := e.openProject(projectID)
		if err != nil {
			return err
		}
		if err := e.fenceProject(tx, p); err != nil {
			return err
		}
		if _, err := e.Plans.Insert(tx, Plan{ID: planID, ProjectID: projectID, Status: "draft", Revision: 1}); err != nil {
			return err
		}
		_, err = e.Events.Insert(tx, Event{ID: eventID(planID, 1), Kind: "plan.created", Subject: planID, SubjectRev: 1, To: "draft"})
		return err
	})
	return err
}

// MarkPlanReady moves a draft plan to ready and appends its audit event in the
// same transaction (workflow 7). Participants: plans, events.
func MarkPlanReady(ctx context.Context, e *Env, planID string) error {
	_, err := e.run(ctx, intent("mark-plan-ready", planID), func(tx *dbtx.Tx) error {
		p, ok, err := e.Plans.ByKey(planID)
		if err != nil {
			return err
		}
		switch {
		case !ok:
			return fmt.Errorf("%w: %s", ErrPlanMissing, planID)
		case p.Value.Status == "ready":
			return dbtx.ErrNoChange
		case p.Value.Status != "draft":
			return fmt.Errorf("%w: %s is %s", ErrPlanNotReady, planID, p.Value.Status)
		}
		return e.stagePlan(tx, p, "plan.ready", func(pl *Plan) { pl.Status = "ready" })
	})
	return err
}

// --- 2. autopilot startup ---------------------------------------------------

func launchOpID(agentID string) string { return "tmux.create:" + agentID }

// StartAutopilot starts a run for a ready plan: the run record, the plan's
// status flip (+event), the manager agent slot (+project counter) and the
// operation-journal INTENT for launching the manager all commit together. The
// launch itself is an external effect and is not performed; FinishAgentLaunch
// records its outcome in a second transaction.
// Participants: autopilot_runs, plans, events, agents, projects, operations,
// [project_views].
func StartAutopilot(ctx context.Context, e *Env, runID, planID, managerID string) error {
	_, err := e.run(ctx, intent("start-autopilot", runID), func(tx *dbtx.Tx) error {
		pl, ok, err := e.Plans.ByKey(planID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: %s", ErrPlanMissing, planID)
		}
		if pl.Value.RunID == runID {
			return dbtx.ErrNoChange
		}
		if pl.Value.Status != "ready" || pl.Value.RunID != "" {
			return fmt.Errorf("%w: %s is %s", ErrPlanNotReady, planID, pl.Value.Status)
		}
		for _, k := range []struct {
			ok  func() (bool, error)
			who string
		}{
			{func() (bool, error) { _, f, err := e.Runs.ByKey(runID); return f, err }, "run " + runID},
			{func() (bool, error) { _, f, err := e.Agents.ByKey(managerID); return f, err }, "agent " + managerID},
		} {
			if f, err := k.ok(); err != nil {
				return err
			} else if f {
				return fmt.Errorf("%w: %s", ErrExists, k.who)
			}
		}
		proj, err := e.openProject(pl.Value.ProjectID)
		if err != nil {
			return err
		}
		if _, err := e.Runs.Insert(tx, Run{ID: runID, ProjectID: proj.Value.ID, PlanID: planID,
			ManagerAgentID: managerID, Status: "running", LivePlan: planID}); err != nil {
			return err
		}
		if err := e.stagePlan(tx, pl, "plan.run_started", func(p *Plan) { p.Status = "running"; p.RunID = runID }); err != nil {
			return err
		}
		if err := e.stageAgent(tx, proj, Agent{ID: managerID, ProjectID: proj.Value.ID, Status: "starting", Role: "manager", RunID: runID}); err != nil {
			return err
		}
		_, err = e.Ops.Insert(tx, Operation{ID: launchOpID(managerID), Kind: "tmux.create",
			SubjectType: "agent", SubjectID: managerID, State: "pending"})
		return err
	})
	return err
}

// FinishAgentLaunch is the outcome transaction of the launch intent written by
// StartAutopilot: operation pending→succeeded and agent starting→running.
func FinishAgentLaunch(ctx context.Context, e *Env, runID string) error {
	_, err := e.run(ctx, intent("finish-launch", runID), func(tx *dbtx.Tx) error {
		r, ok, err := e.Runs.ByKey(runID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("workflows: run %s not found", runID)
		}
		a, ok, err := e.Agents.ByKey(r.Value.ManagerAgentID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: %s", ErrAgentMissing, r.Value.ManagerAgentID)
		}
		op, ok, err := e.Ops.ByKey(launchOpID(a.Value.ID))
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("workflows: launch intent for %s missing", a.Value.ID)
		}
		if op.Value.State == "succeeded" && a.Value.Status == "running" {
			return dbtx.ErrNoChange
		}
		op.Value.State = "succeeded"
		a.Value.Status = "running"
		if err := e.Ops.Update(tx, op.ID, op.Value, op.Rev); err != nil {
			return err
		}
		return e.Agents.Update(tx, a.ID, a.Value, a.Rev)
	})
	return err
}

// --- 3. pipeline linkage ----------------------------------------------------

// LinkPipeline creates a pipeline with its jobs and links it to a plan on both
// sides (pipeline.plan_id and plan.pipeline_id, +event) atomically.
// Participants: pipelines, pipeline_jobs, plans, events, projects (fence).
func LinkPipeline(ctx context.Context, e *Env, pipelineID, planID string, jobs []string) error {
	_, err := e.run(ctx, intent("link-pipeline", pipelineID), func(tx *dbtx.Tx) error {
		pl, ok, err := e.Plans.ByKey(planID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: %s", ErrPlanMissing, planID)
		}
		switch pl.Value.PipelineID {
		case pipelineID:
			return dbtx.ErrNoChange
		case "":
		default:
			return fmt.Errorf("%w: %s -> %s", ErrPlanLinked, planID, pl.Value.PipelineID)
		}
		if _, exists, err := e.Pipelines.ByKey(pipelineID); err != nil {
			return err
		} else if exists {
			return fmt.Errorf("%w: pipeline %s", ErrExists, pipelineID)
		}
		proj, err := e.openProject(pl.Value.ProjectID)
		if err != nil {
			return err
		}
		if err := e.fenceProject(tx, proj); err != nil {
			return err
		}
		if _, err := e.Pipelines.Insert(tx, Pipeline{ID: pipelineID, ProjectID: proj.Value.ID,
			PlanID: planID, Status: "running", Jobs: len(jobs)}); err != nil {
			return err
		}
		for _, j := range jobs {
			if _, err := e.Jobs.Insert(tx, PipelineJob{ID: pipelineID + ":" + j, PipelineID: pipelineID, JobID: j, Status: "pending"}); err != nil {
				return err
			}
		}
		return e.stagePlan(tx, pl, "plan.pipeline_linked", func(p *Plan) { p.PipelineID = pipelineID })
	})
	return err
}

// --- 4. archive / restore ---------------------------------------------------

// ArchiveAgent moves an agent from the live collection to the archive and
// adjusts both project counters in one transaction.
// Participants: agents, agents_archive, projects, [project_views].
func ArchiveAgent(ctx context.Context, e *Env, agentID string) error {
	_, err := e.run(ctx, intent("archive-agent", agentID), func(tx *dbtx.Tx) error {
		a, ok, err := e.Agents.ByKey(agentID)
		if err != nil {
			return err
		}
		if !ok {
			if _, arch, err := e.Archive.ByKey(agentID); err != nil {
				return err
			} else if arch {
				return dbtx.ErrNoChange
			}
			return fmt.Errorf("%w: %s", ErrAgentMissing, agentID)
		}
		p, ok, err := e.Projects.ByKey(a.Value.ProjectID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: %s", ErrProjectMissing, a.Value.ProjectID)
		}
		if err := e.Agents.Delete(tx, a.ID, a.Rev); err != nil {
			return err
		}
		arch := a.Value
		arch.ArchivedFrom, arch.Status = a.Value.Status, "archived"
		if _, err := e.Archive.Insert(tx, arch); err != nil {
			return err
		}
		return e.stageProject(tx, p, func(p *Project) { p.LiveAgents--; p.ArchivedAgents++ }, false)
	})
	return err
}

// RestoreAgent is the exact inverse of ArchiveAgent. The project must be open
// (a restored agent returns to its prior, possibly running, status).
func RestoreAgent(ctx context.Context, e *Env, agentID string) error {
	_, err := e.run(ctx, intent("restore-agent", agentID), func(tx *dbtx.Tx) error {
		arch, ok, err := e.Archive.ByKey(agentID)
		if err != nil {
			return err
		}
		if !ok {
			if _, live, err := e.Agents.ByKey(agentID); err != nil {
				return err
			} else if live {
				return dbtx.ErrNoChange
			}
			return fmt.Errorf("%w: %s", ErrAgentMissing, agentID)
		}
		p, err := e.openProject(arch.Value.ProjectID)
		if err != nil {
			return err
		}
		if err := e.Archive.Delete(tx, arch.ID, arch.Rev); err != nil {
			return err
		}
		live := arch.Value
		live.Status, live.ArchivedFrom = arch.Value.ArchivedFrom, ""
		if _, err := e.Agents.Insert(tx, live); err != nil {
			return err
		}
		return e.stageProject(tx, p, func(p *Project) { p.LiveAgents++; p.ArchivedAgents-- }, false)
	})
	return err
}

// --- 5. project closure -----------------------------------------------------

// CloseProject closes a project and, in the same transaction, stops every open
// agent and cancels every running pipeline of it. A project with a live
// autopilot run is refused (restricted, contract R1). More children than one
// transaction can hold (MaxOps) fails with a typed dbtx.ClassTooLarge and
// changes nothing; draining such a project is a caller-driven series of
// ordinary per-agent transactions followed by CloseProject.
// Participants: projects, agents, pipelines, [project_views].
func CloseProject(ctx context.Context, e *Env, projectID string) error {
	_, err := e.run(ctx, intent("close-project", projectID), func(tx *dbtx.Tx) error {
		p, ok, err := e.Projects.ByKey(projectID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: %s", ErrProjectMissing, projectID)
		}
		if p.Value.Status == "closed" {
			return dbtx.ErrNoChange
		}
		runs, err := e.Runs.All()
		if err != nil {
			return err
		}
		for _, r := range runs {
			if r.Value.ProjectID == projectID && r.Value.Status == "running" {
				return fmt.Errorf("%w: %s", ErrProjectBusy, r.Value.ID)
			}
		}
		agents, err := e.Agents.All()
		if err != nil {
			return err
		}
		for _, a := range agents {
			if a.Value.ProjectID == projectID && isOpen(a.Value.Status) {
				next := a.Value
				next.Status = "stopped"
				if err := e.Agents.Update(tx, a.ID, next, a.Rev); err != nil {
					return err
				}
			}
		}
		pipes, err := e.Pipelines.All()
		if err != nil {
			return err
		}
		for _, pp := range pipes {
			if pp.Value.ProjectID == projectID && pp.Value.Status == "running" {
				next := pp.Value
				next.Status = "canceled"
				if err := e.Pipelines.Update(tx, pp.ID, next, pp.Rev); err != nil {
					return err
				}
			}
		}
		return e.stageProject(tx, p, func(p *Project) { p.Status = "closed" }, false)
	})
	return err
}

// --- 6. projection update ---------------------------------------------------

// RenameProject renames a project and writes/refreshes its persisted view in the
// same transaction, so view.source_version == project.version at every commit.
// Participants: projects, project_views.
func RenameProject(ctx context.Context, e *Env, projectID, name string) error {
	_, err := e.run(ctx, intent("rename-project", projectID+"/"+name), func(tx *dbtx.Tx) error {
		p, ok, err := e.Projects.ByKey(projectID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: %s", ErrProjectMissing, projectID)
		}
		if v, vok, err := e.Views.ByKey(projectID); err != nil {
			return err
		} else if vok && p.Value.Name == name && v.Value.SourceVersion == p.Value.Version {
			return dbtx.ErrNoChange
		}
		return e.stageProject(tx, p, func(p *Project) { p.Name = name }, true)
	})
	return err
}

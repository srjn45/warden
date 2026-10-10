package workflows

import (
	"context"
	"fmt"
)

// Fixed ids: each workflow owns a distinct project so all of them can run in
// one database.
const (
	W1Project, W1Agent = "proj-w1", "agent-w1a"

	W2Project, W2Agent, W2Plan, W2Run, W2Manager = "proj-w2", "agent-w2a", "plan-w2", "run-w2", "agent-w2m"

	W3Project, W3Agent, W3Plan, W3Pipeline = "proj-w3", "agent-w3a", "plan-w3", "pipe-w3"

	W4Project, W4AgentA, W4AgentB = "proj-w4", "agent-w4a", "agent-w4b"

	W5Project, W5AgentA, W5AgentB, W5Plan, W5Pipeline = "proj-w5", "agent-w5a", "agent-w5b", "plan-w5", "pipe-w5"

	W6Project, W6Agent, W6Name = "proj-w6", "agent-w6a", "renamed-w6"

	W7Project, W7Plan = "proj-w7", "plan-w7"
)

// W3Jobs are the job ids LinkPipeline creates in workflow 3 and 5.
var W3Jobs = []string{"build", "test", "ship"}

// touch makes a benign competing write: it rewrites a record with identical
// data, which bumps its revision and so invalidates any ExpectedRev fence.
func touch[T any](r *Repo[T], key string) error {
	v, ok, err := r.ByKey(key)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("workflows: touch: %s/%s missing", r.name, key)
	}
	rec, err := r.col.Get(v.ID)
	if err != nil {
		return err
	}
	_, err = r.col.Update(v.ID, rec.Data)
	return err
}

func seedProject(ctx context.Context, e *Env, project string, agents ...string) error {
	for _, a := range agents {
		if err := CreateProjectWithAgent(ctx, e, project, a); err != nil {
			return err
		}
	}
	return nil
}

// All returns the seven representative workflows, in a stable order.
func All() []Workflow {
	return []Workflow{
		{
			Name:        "project-child-creation",
			Seed:        func(context.Context, *Env) error { return nil },
			Run:         func(ctx context.Context, e *Env) error { return CreateProjectWithAgent(ctx, e, W1Project, W1Agent) },
			Check:       CheckProjectChildren,
			Collections: []string{CollProjects, CollAgents},
			// A competing creator inserts the same project id (unique violation
			// at commit); the retry then takes the add-to-existing path.
			Interfere: func(e *Env) error {
				if _, ok, err := e.Projects.ByKey(W1Project); err != nil {
					return err
				} else if ok {
					return touch(e.Projects, W1Project)
				}
				_, _, err := e.Projects.col.Insert(map[string]any{"id": W1Project, "name": "competitor",
					"status": "open", "version": 1, "live_agents": 0, "archived_agents": 0})
				return err
			},
		},
		{
			Name: "autopilot-startup",
			Seed: func(ctx context.Context, e *Env) error {
				if err := seedProject(ctx, e, W2Project, W2Agent); err != nil {
					return err
				}
				if err := CreatePlan(ctx, e, W2Plan, W2Project); err != nil {
					return err
				}
				return MarkPlanReady(ctx, e, W2Plan)
			},
			Run:         func(ctx context.Context, e *Env) error { return StartAutopilot(ctx, e, W2Run, W2Plan, W2Manager) },
			Check:       CheckAutopilotStartup,
			Collections: []string{CollRuns, CollPlans, CollEvents, CollAgents, CollProjects, CollOperations},
			Interfere:   func(e *Env) error { return touch(e.Plans, W2Plan) },
		},
		{
			Name: "pipeline-linkage",
			Seed: func(ctx context.Context, e *Env) error {
				if err := seedProject(ctx, e, W3Project, W3Agent); err != nil {
					return err
				}
				return CreatePlan(ctx, e, W3Plan, W3Project)
			},
			Run:         func(ctx context.Context, e *Env) error { return LinkPipeline(ctx, e, W3Pipeline, W3Plan, W3Jobs) },
			Check:       CheckPipelineLinkage,
			Collections: []string{CollPipelines, CollPipelineJobs, CollPlans, CollEvents, CollProjects},
			Interfere:   func(e *Env) error { return touch(e.Plans, W3Plan) },
		},
		{
			Name: "archive-restore",
			Seed: func(ctx context.Context, e *Env) error {
				return seedProject(ctx, e, W4Project, W4AgentA, W4AgentB)
			},
			Run:         func(ctx context.Context, e *Env) error { return ArchiveAgent(ctx, e, W4AgentA) },
			Check:       CheckArchive,
			Collections: []string{CollAgents, CollAgentsArchive, CollProjects},
			Interfere:   func(e *Env) error { return touch(e.Projects, W4Project) },
		},
		{
			Name: "project-closure",
			Seed: func(ctx context.Context, e *Env) error {
				if err := seedProject(ctx, e, W5Project, W5AgentA, W5AgentB); err != nil {
					return err
				}
				if err := CreatePlan(ctx, e, W5Plan, W5Project); err != nil {
					return err
				}
				return LinkPipeline(ctx, e, W5Pipeline, W5Plan, W3Jobs)
			},
			Run:         func(ctx context.Context, e *Env) error { return CloseProject(ctx, e, W5Project) },
			Check:       CheckProjectClosure,
			Collections: []string{CollProjects, CollAgents, CollPipelines},
			Interfere:   func(e *Env) error { return touch(e.Projects, W5Project) },
		},
		{
			Name:        "projection-update",
			Seed:        func(ctx context.Context, e *Env) error { return seedProject(ctx, e, W6Project, W6Agent) },
			Run:         func(ctx context.Context, e *Env) error { return RenameProject(ctx, e, W6Project, W6Name) },
			Check:       CheckProjection,
			Collections: []string{CollProjects, CollViews},
			Interfere:   func(e *Env) error { return touch(e.Projects, W6Project) },
		},
		{
			Name: "audit-event-append",
			Seed: func(ctx context.Context, e *Env) error {
				if err := seedProject(ctx, e, W7Project, "agent-w7a"); err != nil {
					return err
				}
				return CreatePlan(ctx, e, W7Plan, W7Project)
			},
			Run:         func(ctx context.Context, e *Env) error { return MarkPlanReady(ctx, e, W7Plan) },
			Check:       CheckAuditTrail,
			Collections: []string{CollPlans, CollEvents},
			Interfere:   func(e *Env) error { return touch(e.Plans, W7Plan) },
		},
	}
}

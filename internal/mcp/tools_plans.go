package mcp

import (
	"context"
	"encoding/json"
	"errors"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/planbackup"
)

// --- argument structs for plan tools ---

type planTaskArg struct {
	ID     string   `json:"id" jsonschema:"stable task id within the plan"`
	Prompt string   `json:"prompt" jsonschema:"work instruction for this task"`
	After  []string `json:"after,omitempty" jsonschema:"task ids that must complete before this task starts"`
}

type listPlansArgs struct {
	ProjectID string `json:"project_id" jsonschema:"the daemon project id (use the project's absolute path for local projects)"`
	Status    string `json:"status,omitempty" jsonschema:"optional filter: pending|in_progress|completed|archived"`
}

type getPlanArgs struct {
	PlanID string `json:"plan_id" jsonschema:"the stable plan id (plan-<8hex>)"`
}

type relatedPlansArgs struct {
	PlanID string `json:"plan_id" jsonschema:"the stable plan id (plan-<8hex>) to find related plans for"`
	Limit  int    `json:"limit,omitempty" jsonschema:"maximum hits to return (default 10)"`
}

type createPlanArgs struct {
	ProjectID   string        `json:"project_id" jsonschema:"the daemon project id"`
	Name        string        `json:"name" jsonschema:"plan name"`
	Goal        string        `json:"goal" jsonschema:"what the plan is trying to achieve"`
	Tasks       []planTaskArg `json:"tasks" jsonschema:"at least one task; each needs id and prompt"`
	Constraints []string      `json:"constraints,omitempty" jsonschema:"optional constraints the workers must follow"`
	DoneWhen    []string      `json:"done_when,omitempty" jsonschema:"optional completion criteria"`
}

type updatePlanArgs struct {
	PlanID           string        `json:"plan_id" jsonschema:"the stable plan id (plan-<8hex>)"`
	Name             string        `json:"name,omitempty" jsonschema:"new plan name (pending plans only)"`
	Goal             string        `json:"goal,omitempty" jsonschema:"new goal (pending plans only)"`
	Tasks            []planTaskArg `json:"tasks,omitempty" jsonschema:"replacement task list (pending plans only)"`
	Constraints      []string      `json:"constraints,omitempty" jsonschema:"replacement constraints (pending plans only)"`
	DoneWhen         []string      `json:"done_when,omitempty" jsonschema:"replacement completion criteria (pending plans only)"`
	ExpectedRevision int64         `json:"expected_revision,omitempty" jsonschema:"optimistic concurrency token; omit to use the revision observed at request start"`
}

type scanPlansArgs struct {
	ProjectID   string `json:"project_id" jsonschema:"the daemon project id"`
	MigrateFlat bool   `json:"migrate_flat,omitempty" jsonschema:"move flat plans/*.yaml files into plans/pending/ with git mv + commit"`
	Assess      bool   `json:"assess,omitempty" jsonschema:"run brain-assisted progress assessment for all in_progress plans"`
}

type importLegacyPlansArgs struct {
	ProjectID  string `json:"project_id" jsonschema:"the daemon project id"`
	ReportOnly bool   `json:"report_only,omitempty" jsonschema:"when true, classify without mutating ScrivaDB"`
}

type updatePlanStatusArgs struct {
	ProjectID string `json:"project_id" jsonschema:"the daemon project id"`
	PlanID    string `json:"plan_id" jsonschema:"the stable plan id (plan-<8hex>)"`
	Status    string `json:"status" jsonschema:"new status: pending|in_progress|completed|archived"`
}

type archivePlanArgs struct {
	PlanID string `json:"plan_id" jsonschema:"the stable plan id (plan-<8hex>) to archive"`
}

type assessPlanArgs struct {
	ProjectID string `json:"project_id" jsonschema:"the daemon project id"`
	PlanID    string `json:"plan_id" jsonschema:"the stable plan id (plan-<8hex>) to assess"`
}

type runPlanArgs struct {
	PlanID        string `json:"plan_id" jsonschema:"the stable plan id (plan-<8hex>) to run"`
	ExecutionMode string `json:"execution_mode" jsonschema:"execution mode: autopilot|pipeline|orchestrator_worker|manual"`
}

type controlPlanArgs struct {
	PlanID string `json:"plan_id" jsonschema:"the stable plan id (plan-<8hex>) to control"`
	Action string `json:"action" jsonschema:"pause|resume|stop"`
}

type completePlanArgs struct {
	PlanID string `json:"plan_id" jsonschema:"the stable plan id (plan-<8hex>) to complete"`
}

type syncPlanToRepoArgs struct {
	PlanID         string `json:"plan_id" jsonschema:"the stable plan id (plan-<8hex>) to export"`
	TargetRef      string `json:"target_ref" jsonschema:"PR base branch / target ref (required)"`
	RepositoryPath string `json:"repository_path,omitempty" jsonschema:"absolute local git repository path (defaults to the plan project root)"`
	OutputPath     string `json:"output_path,omitempty" jsonschema:"optional replica path override (default plans/{lifecycle}/<slug>.yaml)"`
	Repository     string `json:"repository,omitempty" jsonschema:"stable repository identity for export records (defaults to origin URL)"`
}

type exportPlanBackupArgs struct {
	PlanIDs   []string `json:"plan_ids,omitempty" jsonschema:"stable plan ids to export"`
	ProjectID string   `json:"project_id,omitempty" jsonschema:"when all=true, optionally limit to this project"`
	All       bool     `json:"all,omitempty" jsonschema:"export every plan (optionally scoped by project_id)"`
}

type restorePlanBackupArgs struct {
	Bundle     planbackup.Bundle         `json:"bundle" jsonschema:"sealed Plan backup bundle from export_plan_backup"`
	DryRun     bool                      `json:"dry_run,omitempty" jsonschema:"validate without writing"`
	OnConflict planbackup.ConflictPolicy `json:"on_conflict,omitempty" jsonschema:"skip|fail|overwrite (default skip)"`
}

// planTaskStatusArgs backs update_task_status. plan_id is the Plan CRUD form;
// run_id is the pre-existing autopilot-brain form (replaced here so both share one tool name).
type planTaskStatusArgs struct {
	PlanID   string `json:"plan_id,omitempty" jsonschema:"plan id for PlanService task-progress updates (pending|in_progress|done|skipped)"`
	RunID    string `json:"run_id,omitempty" jsonschema:"autopilot run id (brain-only form; mutually exclusive with plan_id)"`
	TaskID   string `json:"task_id" jsonschema:"the task id to update"`
	Status   string `json:"status" jsonschema:"plan form: pending|in_progress|done|skipped; autopilot form: pending|active|done|failed"`
	LandedPR int    `json:"landed_pr,omitempty" jsonschema:"autopilot form: required for done; must already be recorded by land"`
}

func toClientTasks(in []planTaskArg) []client.PlanTaskSpec {
	out := make([]client.PlanTaskSpec, 0, len(in))
	for _, t := range in {
		out = append(out, client.PlanTaskSpec{ID: t.ID, Prompt: t.Prompt, After: t.After})
	}
	return out
}

// planToolErr returns a structured error payload (human-readable message plus
// any incomplete-task / unmerged-branch lists from a 422 body).
func planToolErr(err error) (*mcpsdk.CallToolResult, any, error) {
	payload := map[string]any{"error": err.Error()}
	var se *client.StatusError
	if errors.As(err, &se) {
		payload["status"] = se.Code
		var body struct {
			Error            string   `json:"error"`
			IncompleteTasks  []string `json:"incomplete_tasks,omitempty"`
			UnmergedBranches []string `json:"unmerged_branches,omitempty"`
			PlanID           string   `json:"plan_id,omitempty"`
			Expected         int64    `json:"expected,omitempty"`
			Actual           int64    `json:"actual,omitempty"`
		}
		if json.Unmarshal(se.Body, &body) == nil && body.Error != "" {
			payload["error"] = body.Error
			if len(body.IncompleteTasks) > 0 {
				payload["incomplete_tasks"] = body.IncompleteTasks
			}
			if len(body.UnmergedBranches) > 0 {
				payload["unmerged_branches"] = body.UnmergedBranches
			}
			if body.PlanID != "" {
				payload["plan_id"] = body.PlanID
			}
			if body.Expected != 0 || body.Actual != 0 {
				payload["expected"] = body.Expected
				payload["actual"] = body.Actual
			}
		}
	}
	return jsonResultAny(payload)
}

// registerPlanTools registers the plan-management MCP tools.
func (s *Server) registerPlanTools() {
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name: "list_plans",
		Description: "List ScrivaDB-canonical plans for a daemon project (optional status filter). " +
			"Each plan includes revision, executor_id, task_summary, export_status, and timestamps. " +
			"Repository YAML replicas are never listed as additional plans — discovery is DB-only.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a listPlansArgs) (*mcpsdk.CallToolResult, any, error) {
		plans, err := s.cl.PlansList(ctx, a.ProjectID, a.Status)
		if err != nil {
			return planToolErr(err)
		}
		return jsonResultAny(plans)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name: "get_plan",
		Description: "Get one ScrivaDB-canonical plan by stable ID. Returns goal, tasks, constraints, " +
			"done_when, status, revision, content_hash, executor_id, task_summary, export_status, " +
			"repo_export metadata, linked IDs, task progress, and timestamps. Does not read repository YAML.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a getPlanArgs) (*mcpsdk.CallToolResult, any, error) {
		p, err := s.cl.PlansGet(ctx, a.PlanID)
		if err != nil {
			return planToolErr(err)
		}
		return jsonResultAny(p)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name: "find_related_plans",
		Description: "Heuristic related-plan / overlap query for a plan (same project, title/goal tokens, " +
			"linked branches/PRs). Returns heuristic=true and a disclaimer — hits are discovery aids only, " +
			"not authoritative identity or duplicate detection. Completed plans remain eligible for " +
			"historical lookup. YAML replicas never appear as extra plans.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a relatedPlansArgs) (*mcpsdk.CallToolResult, any, error) {
		res, err := s.cl.PlansRelated(ctx, a.PlanID, a.Limit)
		if err != nil {
			return planToolErr(err)
		}
		return jsonResultAny(res)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "create_plan",
		Description: "Create a new canonical Plan in ScrivaDB (no repository YAML write). Requires project_id, name, goal, and at least one task (id + prompt). Returns the created Plan including revision and content_hash.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a createPlanArgs) (*mcpsdk.CallToolResult, any, error) {
		p, err := s.cl.PlansCreate(ctx, client.PlansCreateRequest{
			ProjectID:   a.ProjectID,
			Name:        a.Name,
			Goal:        a.Goal,
			Tasks:       toClientTasks(a.Tasks),
			Constraints: a.Constraints,
			DoneWhen:    a.DoneWhen,
		})
		if err != nil {
			return planToolErr(err)
		}
		return jsonResultAny(p)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "update_plan",
		Description: "Update a pending plan's canonical definition (name, goal, tasks, constraints, done_when). Pass expected_revision for optimistic concurrency; a stale revision returns a structured 409 conflict. Rejected if the plan is not pending. Returns the updated Plan.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a updatePlanArgs) (*mcpsdk.CallToolResult, any, error) {
		req := client.PlansUpdateRequest{
			Name:             a.Name,
			Goal:             a.Goal,
			Constraints:      a.Constraints,
			DoneWhen:         a.DoneWhen,
			ExpectedRevision: a.ExpectedRevision,
		}
		if a.Tasks != nil {
			req.Tasks = toClientTasks(a.Tasks)
		}
		p, err := s.cl.PlansUpdate(ctx, a.PlanID, req)
		if err != nil {
			return planToolErr(err)
		}
		return jsonResultAny(p)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "scan_plans",
		Description: "Deprecated migration aid: walk plans/{pending,in_progress,completed,archived}/*.yaml and upsert plan records. Prefer import_legacy_plans for explicit ScrivaDB cutover. Never runs automatically at daemon startup.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a scanPlansArgs) (*mcpsdk.CallToolResult, any, error) {
		res, err := s.cl.PlanScan(ctx, a.ProjectID, client.PlanScanRequest{
			MigrateFlat: a.MigrateFlat,
			Assess:      a.Assess,
		})
		if err != nil {
			return planToolErr(err)
		}
		return jsonResultAny(res)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "import_legacy_plans",
		Description: "Operator-invoked one-time cutover: discover plans/{pending,in_progress,completed,archived}/*.yaml, parse v1 YAML into canonical ScrivaDB Plans by stable identity, leave source files untouched. Matching content hash → skipped; differing hash → conflicted. report_only=true classifies without writing.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a importLegacyPlansArgs) (*mcpsdk.CallToolResult, any, error) {
		res, err := s.cl.ImportLegacyPlans(ctx, a.ProjectID, client.ImportLegacyPlansRequest{
			ReportOnly: a.ReportOnly,
		})
		if err != nil {
			return planToolErr(err)
		}
		return jsonResultAny(res)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "update_plan_status",
		Description: "Change a plan's lifecycle status via the project-scoped API. Prefer run_plan / complete_plan / archive_plan for the PlanService state machine. Valid statuses: pending|in_progress|completed|archived.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a updatePlanStatusArgs) (*mcpsdk.CallToolResult, any, error) {
		p, err := s.cl.PlanUpdate(ctx, a.ProjectID, a.PlanID, client.PlanUpdateRequest{
			Status: a.Status,
		})
		if err != nil {
			return planToolErr(err)
		}
		return jsonResultAny(p)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "archive_plan",
		Description: "Archive a plan (any status → archived). Moves the YAML to plans/archived/ and returns the updated Plan.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a archivePlanArgs) (*mcpsdk.CallToolResult, any, error) {
		p, err := s.cl.PlansArchive(ctx, a.PlanID)
		if err != nil {
			return planToolErr(err)
		}
		return jsonResultAny(p)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "assess_plan",
		Description: "Trigger brain-assisted task progress assessment for one plan. Uses git log and open PR metadata to reconstruct task_progress.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a assessPlanArgs) (*mcpsdk.CallToolResult, any, error) {
		p, err := s.cl.PlanAssess(ctx, a.ProjectID, a.PlanID)
		if err != nil {
			return planToolErr(err)
		}
		return jsonResultAny(p)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "run_plan",
		Description: "Start execution of a plan: pending → in_progress. This is the only supported public start path (including autopilot). execution_mode is autopilot|pipeline|orchestrator_worker|manual. Returns the updated Plan (with linked executor id when started).",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a runPlanArgs) (*mcpsdk.CallToolResult, any, error) {
		p, err := s.cl.PlansRun(ctx, a.PlanID, a.ExecutionMode)
		if err != nil {
			return planToolErr(err)
		}
		return jsonResultAny(p)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "control_plan",
		Description: "Pause, resume, or stop an in-progress plan's active executor. Together with run_plan, this is the only public lifecycle surface for plan execution. action is pause|resume|stop.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a controlPlanArgs) (*mcpsdk.CallToolResult, any, error) {
		p, err := s.cl.PlansControl(ctx, a.PlanID, a.Action)
		if err != nil {
			return planToolErr(err)
		}
		return jsonResultAny(p)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "complete_plan",
		Description: "Complete a plan (in_progress → completed). Blocked with a structured error listing incomplete tasks and/or unmerged branches. On success moves the YAML to plans/completed/, cleans up worktrees, and returns the Plan.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a completePlanArgs) (*mcpsdk.CallToolResult, any, error) {
		p, err := s.cl.PlansComplete(ctx, a.PlanID)
		if err != nil {
			return planToolErr(err)
		}
		return jsonResultAny(p)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name: "sync_plan_to_repo",
		Description: "Export a canonical ScrivaDB Plan revision as an inert YAML replica onto a dedicated " +
			"warden/plan-sync/<plan-id>/<revision> branch and open or reuse a PR against target_ref. " +
			"Uses an isolated git worktree — never touches the operator checkout, force-pushes, " +
			"auto-merges, or overwrites a conflicting non-Warden file. Idempotent for the same " +
			"revision/hash/repo/ref/path (returns prior result, no new GitHub activity).",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a syncPlanToRepoArgs) (*mcpsdk.CallToolResult, any, error) {
		res, err := s.cl.PlansSyncToRepo(ctx, a.PlanID, client.PlansSyncToRepoRequest{
			TargetRef:      a.TargetRef,
			RepositoryPath: a.RepositoryPath,
			OutputPath:     a.OutputPath,
			Repository:     a.Repository,
		})
		if err != nil {
			return planToolErr(err)
		}
		return jsonResultAny(res)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name: "export_plan_backup",
		Description: "Export canonical ScrivaDB Plans into a versioned portable backup bundle " +
			"(definition, revision, execution evidence, events/notes, integrity hashes). " +
			"Excludes credentials and disposable worktrees. Never reads Git or plans/ replicas. " +
			"Provide plan_ids and/or all=true (optionally scoped by project_id).",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a exportPlanBackupArgs) (*mcpsdk.CallToolResult, any, error) {
		res, err := s.cl.PlansExportBackup(ctx, client.PlansExportBackupRequest{
			PlanIDs:   a.PlanIDs,
			ProjectID: a.ProjectID,
			All:       a.All,
		})
		if err != nil {
			return planToolErr(err)
		}
		return jsonResultAny(res)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name: "restore_plan_backup",
		Description: "Restore Plans from a portable backup bundle into ScrivaDB. Validates integrity " +
			"hashes; supports dry_run and on_conflict=skip|fail|overwrite. Idempotent when " +
			"stable id + content hash + revision match. Does not consult Git or plans/ replicas.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a restorePlanBackupArgs) (*mcpsdk.CallToolResult, any, error) {
		res, err := s.cl.PlansRestoreBackup(ctx, client.PlansRestoreBackupRequest{
			Bundle:     a.Bundle,
			DryRun:     a.DryRun,
			OnConflict: a.OnConflict,
		})
		if err != nil {
			return planToolErr(err)
		}
		return jsonResultAny(res)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "update_task_status",
		Description: "Update one task's status. Plan form: {plan_id, task_id, status} where status is pending|in_progress|done|skipped — updates TaskProgress only. Autopilot-brain form: {run_id, task_id, status, landed_pr?} where status is pending|active|done|failed.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a planTaskStatusArgs) (*mcpsdk.CallToolResult, any, error) {
		if a.PlanID != "" {
			p, err := s.cl.PlansUpdateTaskStatus(ctx, a.PlanID, a.TaskID, a.Status)
			if err != nil {
				return planToolErr(err)
			}
			return jsonResultAny(p)
		}
		if a.RunID != "" {
			task, err := s.cl.UpdateAutopilotTaskStatus(ctx, a.RunID, a.TaskID, a.Status, a.LandedPR)
			if err != nil {
				return planToolErr(err)
			}
			return jsonResultAny(task)
		}
		return jsonResultAny(map[string]any{"error": "plan_id or run_id is required"})
	})
}

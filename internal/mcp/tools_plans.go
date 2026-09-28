package mcp

import (
	"context"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/planstore"
)

// --- argument structs for plan tools ---

type listPlansArgs struct {
	ProjectID string `json:"project_id" jsonschema:"the daemon project id (use the project's absolute path for local projects)"`
	Status    string `json:"status,omitempty" jsonschema:"optional filter: pending|in_progress|completed|archived"`
}

type getPlanArgs struct {
	ProjectID string `json:"project_id" jsonschema:"the daemon project id"`
	PlanID    string `json:"plan_id" jsonschema:"the stable plan id (plan-<8hex>)"`
}

type createPlanArgs struct {
	ProjectID string `json:"project_id" jsonschema:"the daemon project id"`
	Name      string `json:"name" jsonschema:"plan name (must match the YAML name: field or filename stem)"`
	FilePath  string `json:"file_path" jsonschema:"path to the plan YAML file, relative to the project root"`
}

type scanPlansArgs struct {
	ProjectID   string `json:"project_id" jsonschema:"the daemon project id"`
	MigrateFlat bool   `json:"migrate_flat,omitempty" jsonschema:"move flat plans/*.yaml files into plans/pending/ with git mv + commit"`
	Assess      bool   `json:"assess,omitempty" jsonschema:"run brain-assisted progress assessment for all in_progress plans (Phase 4 stub)"`
}

type updatePlanStatusArgs struct {
	ProjectID string `json:"project_id" jsonschema:"the daemon project id"`
	PlanID    string `json:"plan_id" jsonschema:"the stable plan id (plan-<8hex>)"`
	Status    string `json:"status" jsonschema:"new status: pending|in_progress|completed|archived"`
}

type archivePlanArgs struct {
	ProjectID string `json:"project_id" jsonschema:"the daemon project id"`
	PlanID    string `json:"plan_id" jsonschema:"the stable plan id (plan-<8hex>) to archive"`
}

type assessPlanArgs struct {
	ProjectID string `json:"project_id" jsonschema:"the daemon project id"`
	PlanID    string `json:"plan_id" jsonschema:"the stable plan id (plan-<8hex>) to assess"`
}

type runPlanArgs struct {
	ProjectID string `json:"project_id" jsonschema:"the daemon project id"`
	PlanID    string `json:"plan_id" jsonschema:"the stable plan id (plan-<8hex>) to run"`
	Mode      string `json:"mode" jsonschema:"execution mode: autopilot|pipeline|orchestrator_worker|manual"`
}

// registerPlanTools registers the plan-management MCP tools.
func (s *Server) registerPlanTools() {
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "list_plans",
		Description: "List plans for a daemon project, optionally filtered by status (pending|in_progress|completed|archived). Returns a flat list sorted by updated_at descending.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a listPlansArgs) (*mcpsdk.CallToolResult, any, error) {
		plans, err := s.cl.PlanList(ctx, a.ProjectID, client.PlanListParams{Status: a.Status})
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		if plans == nil {
			plans = []*planstore.Plan{}
		}
		return jsonResultAny(plans)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "get_plan",
		Description: "Get one plan by its stable ID (plan-<8hex>). Returns the full record including status, file path, execution mode, linked IDs, task progress, and timestamps.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a getPlanArgs) (*mcpsdk.CallToolResult, any, error) {
		p, err := s.cl.PlanGet(ctx, a.ProjectID, a.PlanID)
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(p)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "create_plan",
		Description: "Register a new plan record in the daemon DB. The YAML file must already exist at file_path relative to the project root. Use scan_plans to bulk-import from the directory.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a createPlanArgs) (*mcpsdk.CallToolResult, any, error) {
		p, err := s.cl.PlanCreate(ctx, a.ProjectID, client.PlanCreateRequest{
			Name:     a.Name,
			FilePath: a.FilePath,
		})
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(p)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "scan_plans",
		Description: "Walk plans/{pending,in_progress,completed,archived}/*.yaml in the project root and upsert plan records. Status is inferred from directory. With migrate_flat=true, flat plans/*.yaml files are moved into plans/pending/ with git mv and committed first.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a scanPlansArgs) (*mcpsdk.CallToolResult, any, error) {
		res, err := s.cl.PlanScan(ctx, a.ProjectID, client.PlanScanRequest{
			MigrateFlat: a.MigrateFlat,
			Assess:      a.Assess,
		})
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(res)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "update_plan_status",
		Description: "Change a plan's lifecycle status. The daemon updates the DB record and performs a git mv of the YAML file to the correct plans/<status>/ subdirectory, then commits. Valid statuses: pending|in_progress|completed|archived.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a updatePlanStatusArgs) (*mcpsdk.CallToolResult, any, error) {
		p, err := s.cl.PlanUpdate(ctx, a.ProjectID, a.PlanID, client.PlanUpdateRequest{
			Status: a.Status,
		})
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(p)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "archive_plan",
		Description: "Move a plan to the archived state. Equivalent to update_plan_status with status=archived. The YAML file is git-mv'd to plans/archived/ and a commit is created.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a archivePlanArgs) (*mcpsdk.CallToolResult, any, error) {
		p, err := s.cl.PlanUpdate(ctx, a.ProjectID, a.PlanID, client.PlanUpdateRequest{
			Status: string(planstore.PlanStatusArchived),
		})
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return jsonResultAny(p)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "assess_plan",
		Description: "Trigger brain-assisted task progress assessment for one plan. Uses git log and open PR metadata to reconstruct task_progress. Phase 4 stub — daemon returns 501 until Phase 4 ships.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a assessPlanArgs) (*mcpsdk.CallToolResult, any, error) {
		if err := s.cl.PlanAssess(ctx, a.ProjectID, a.PlanID); err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return textResult("assess triggered for plan " + a.PlanID), nil, nil
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "run_plan",
		Description: "Start execution of a plan in the given mode. Modes: autopilot (autonomous), pipeline (task-per-job), orchestrator_worker (human-gated), manual (tracking only). Phase 5 stub — daemon returns 501 until Phase 5 ships.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a runPlanArgs) (*mcpsdk.CallToolResult, any, error) {
		if err := s.cl.PlanRun(ctx, a.ProjectID, a.PlanID, client.PlanRunRequest{Mode: a.Mode}); err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return textResult("plan " + a.PlanID + " run started (mode: " + a.Mode + ")"), nil, nil
	})
}

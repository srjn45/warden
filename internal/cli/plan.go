package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/planstore"
)

func newPlanCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Manage plans tracked by the daemon",
		Long: "Manage plans tracked by the daemon.\n\n" +
			"Plans are YAML files stored in plans/{pending,in_progress,completed,archived}/\n" +
			"inside a project repository. The daemon scans those files and tracks their\n" +
			"execution state (links to autopilot runs, pipelines, and task progress).\n\n" +
			"Status is encoded in the directory: moving a YAML file changes its status.\n" +
			"`wd plan status` performs the git mv, commits, and updates the DB record.",
	}
	SetCommandHelpMetadata(cmd, "run", 25, "warden plan", "", NodeNamespace)

	children := []*cobra.Command{
		newPlanListCmd(),
		newPlanShowCmd(),
		newPlanImportCmd(),
		newPlanScanCmd(),
		newPlanStatusCmd(),
		newPlanArchiveCmd(),
		newPlanAssessCmd(),
		newPlanRunCmd(),
	}
	for i, child := range children {
		SetCommandHelpMetadata(child, "run", (i+1)*10, "warden plan "+child.Name(), "", nodeKind(child))
		cmd.AddCommand(child)
	}
	return cmd
}

func newPlanListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List plans for a project",
		Long: "List plans registered in the daemon for a project.\n\n" +
			"Use --project to specify the project (defaults to the current directory).\n" +
			"Filter by lifecycle stage with --status.",
		Aliases: []string{"ls"},
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, _ := cmd.Flags().GetString("project")
			if projectID == "" {
				var err error
				projectID, err = resolveProjectID(cmd)
				if err != nil {
					return err
				}
			}
			status, _ := cmd.Flags().GetString("status")
			plans, err := clientFor(cmd).PlanList(cmd.Context(), projectID, client.PlanListParams{Status: status})
			if err != nil {
				return err
			}
			if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
				if plans == nil {
					plans = []*planstore.Plan{}
				}
				return printJSON(cmd.OutOrStdout(), plans)
			}
			if len(plans) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no plans found")
				return nil
			}
			return printPlanTable(cmd.OutOrStdout(), plans)
		},
	}
	cmd.Flags().String("project", "", "project ID (default: current directory)")
	cmd.Flags().String("status", "", "filter by status: pending|in_progress|completed|archived")
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

func newPlanShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <plan-id>",
		Short: "Show detail for one plan",
		Long:  "Show the full record for one plan: status, file path, execution mode, linked IDs, task progress, and timestamps.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, _ := cmd.Flags().GetString("project")
			if projectID == "" {
				var err error
				projectID, err = resolveProjectID(cmd)
				if err != nil {
					return err
				}
			}
			p, err := clientFor(cmd).PlanGet(cmd.Context(), projectID, args[0])
			if err != nil {
				return err
			}
			if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
				return printJSON(cmd.OutOrStdout(), p)
			}
			printPlanDetail(cmd.OutOrStdout(), p)
			return nil
		},
	}
	cmd.Flags().String("project", "", "project ID (default: current directory)")
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

func newPlanImportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "import <file>",
		Short: "Copy a plan YAML into plans/pending/ and scan",
		Long: "Copy a plan YAML file into the project's plans/pending/ directory and\n" +
			"trigger a scan so the daemon registers the imported plan.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, _ := cmd.Flags().GetString("project")
			if projectID == "" {
				var err error
				projectID, err = resolveProjectID(cmd)
				if err != nil {
					return err
				}
			}
			src := args[0]
			data, err := os.ReadFile(src)
			if err != nil {
				return fmt.Errorf("read %s: %w", src, err)
			}

			// Determine the project root to write into.
			root, err := resolveProjectRoot(cmd, projectID)
			if err != nil {
				return err
			}

			pendingDir := filepath.Join(root, "plans", "pending")
			if err := os.MkdirAll(pendingDir, 0o755); err != nil {
				return fmt.Errorf("create plans/pending: %w", err)
			}
			dst := filepath.Join(pendingDir, filepath.Base(src))
			if err := os.WriteFile(dst, data, 0o644); err != nil {
				return fmt.Errorf("write %s: %w", dst, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "imported %s → %s\n", src, dst)

			// Trigger a scan so the daemon picks it up.
			res, err := clientFor(cmd).PlanScan(cmd.Context(), projectID, client.PlanScanRequest{})
			if err != nil {
				return fmt.Errorf("scan after import: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "scanned: %d plan(s) upserted\n", res.Upserted)
			return nil
		},
	}
	cmd.Flags().String("project", "", "project ID (default: current directory)")
	return cmd
}

func newPlanScanCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "scan",
		Short: "Scan a project's plans/ directory and upsert plan records",
		Long: "Walk plans/{pending,in_progress,completed,archived}/*.yaml and upsert plan\n" +
			"records in the daemon. Status is inferred from the directory.\n\n" +
			"--migrate-flat moves any flat plans/*.yaml files into plans/pending/ with git mv\n" +
			"and creates a commit before scanning.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, _ := cmd.Flags().GetString("project")
			if projectID == "" {
				var err error
				projectID, err = resolveProjectID(cmd)
				if err != nil {
					return err
				}
			}
			migrateFlat, _ := cmd.Flags().GetBool("migrate-flat")
			assess, _ := cmd.Flags().GetBool("assess")
			res, err := clientFor(cmd).PlanScan(cmd.Context(), projectID, client.PlanScanRequest{
				MigrateFlat: migrateFlat,
				Assess:      assess,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "scanned: %d plan(s) upserted\n", res.Upserted)
			return nil
		},
	}
	cmd.Flags().String("project", "", "project ID (default: current directory)")
	cmd.Flags().Bool("migrate-flat", false, "move flat plans/*.yaml files into plans/pending/ with git mv + commit")
	cmd.Flags().Bool("assess", false, "run brain-assisted progress assessment for in_progress plans (Phase 4 stub)")
	return cmd
}

func newPlanStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status <plan-id> <new-status>",
		Short: "Transition a plan's status (git mv + commit + DB update)",
		Long: "Change a plan's lifecycle status. The daemon updates the DB record and\n" +
			"performs a git mv of the YAML file to the correct plans/<status>/ subdirectory,\n" +
			"then creates a commit.\n\n" +
			"Valid statuses: pending | in_progress | completed | archived",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			planID, newStatus := args[0], args[1]
			projectID, _ := cmd.Flags().GetString("project")
			if projectID == "" {
				var err error
				projectID, err = resolveProjectID(cmd)
				if err != nil {
					return err
				}
			}
			p, err := clientFor(cmd).PlanUpdate(cmd.Context(), projectID, planID, client.PlanUpdateRequest{
				Status: newStatus,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "plan %s status → %s (file: %s)\n", p.ID, p.Status, p.FilePath)
			return nil
		},
	}
	cmd.Flags().String("project", "", "project ID (default: current directory)")
	return cmd
}

func newPlanArchiveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "archive <plan-id>",
		Short: "Archive a plan (shorthand for `plan status <id> archived`)",
		Long:  "Move a plan to the archived state. Equivalent to `wd plan status <id> archived`.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			planID := args[0]
			projectID, _ := cmd.Flags().GetString("project")
			if projectID == "" {
				var err error
				projectID, err = resolveProjectID(cmd)
				if err != nil {
					return err
				}
			}
			p, err := clientFor(cmd).PlanUpdate(cmd.Context(), projectID, planID, client.PlanUpdateRequest{
				Status: string(planstore.PlanStatusArchived),
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "plan %s archived (file: %s)\n", p.ID, p.FilePath)
			return nil
		},
	}
	cmd.Flags().String("project", "", "project ID (default: current directory)")
	return cmd
}

func newPlanAssessCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "assess <plan-id>",
		Short: "Brain-assisted task progress assessment (Phase 4 stub)",
		Long: "Use a brain model to reconstruct task progress from git history and open PRs.\n" +
			"Updates task_progress in the DB record.\n\n" +
			"Note: this is a Phase 4 feature stub — the daemon returns 501 until Phase 4 ships.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			planID := args[0]
			projectID, _ := cmd.Flags().GetString("project")
			if projectID == "" {
				var err error
				projectID, err = resolveProjectID(cmd)
				if err != nil {
					return err
				}
			}
			if _, err := clientFor(cmd).PlanAssess(cmd.Context(), projectID, planID); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "assess triggered for plan %s\n", planID)
			return nil
		},
	}
	cmd.Flags().String("project", "", "project ID (default: current directory)")
	return cmd
}

func newPlanRunCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run <plan-id> --mode <mode>",
		Short: "Start execution of a plan in the given mode (Phase 5 stub)",
		Long: "Start execution of a plan. The mode determines how the plan is executed:\n\n" +
			"  autopilot           Fully autonomous run registered with the autopilot\n" +
			"  pipeline            Each task becomes a pipeline job\n" +
			"  orchestrator_worker Orchestrator + workers with human approval gates\n" +
			"  manual              State tracking only; human drives all prompting\n\n" +
			"Note: this is a Phase 5 feature stub — the daemon returns 501 until Phase 5 ships.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			planID := args[0]
			mode, _ := cmd.Flags().GetString("mode")
			if mode == "" {
				return fmt.Errorf("--mode is required (autopilot|pipeline|orchestrator_worker|manual)")
			}
			projectID, _ := cmd.Flags().GetString("project")
			if projectID == "" {
				var err error
				projectID, err = resolveProjectID(cmd)
				if err != nil {
					return err
				}
			}
			if err := clientFor(cmd).PlanRun(cmd.Context(), projectID, planID, client.PlanRunRequest{Mode: mode}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "plan %s run started (mode: %s)\n", planID, mode)
			return nil
		},
	}
	cmd.Flags().String("project", "", "project ID (default: current directory)")
	cmd.Flags().String("mode", "", "execution mode: autopilot|pipeline|orchestrator_worker|manual")
	return cmd
}

// resolveProjectID returns the project ID from the current directory (cwd).
// For local projects warden uses the absolute path as the project ID.
func resolveProjectID(_ *cobra.Command) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	return cwd, nil
}

// resolveProjectRoot returns the filesystem root for a project. For a local
// project whose ID is a path, that path is the root. Otherwise it asks the
// daemon for the project record's path.
func resolveProjectRoot(cmd *cobra.Command, projectID string) (string, error) {
	// If the project ID is an absolute path (the local-project convention), use it.
	if filepath.IsAbs(projectID) {
		return projectID, nil
	}
	// Ask the daemon for the project record.
	projects, err := clientFor(cmd).ListProjects(cmd.Context())
	if err != nil {
		return "", fmt.Errorf("list projects: %w", err)
	}
	for _, p := range projects {
		if p.ID == projectID && p.Path != "" {
			return p.Path, nil
		}
	}
	return "", fmt.Errorf("project %q not found or has no local path", projectID)
}

func printPlanTable(w io.Writer, plans []*planstore.Plan) error {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSTATUS\tMODE\tUPDATED")
	for _, p := range plans {
		mode := string(p.ExecutionMode)
		if mode == "" {
			mode = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			p.ID, p.Name, p.Status, mode, p.UpdatedAt.Format(time.RFC3339))
	}
	return tw.Flush()
}

func printPlanDetail(w io.Writer, p *planstore.Plan) {
	fmt.Fprintf(w, "id:             %s\n", p.ID)
	fmt.Fprintf(w, "name:           %s\n", p.Name)
	fmt.Fprintf(w, "project:        %s\n", p.ProjectID)
	fmt.Fprintf(w, "file:           %s\n", p.FilePath)
	fmt.Fprintf(w, "status:         %s\n", p.Status)
	if p.ExecutionMode != "" {
		fmt.Fprintf(w, "mode:           %s\n", p.ExecutionMode)
	}
	if p.AutopilotRunID != "" {
		fmt.Fprintf(w, "autopilot_run:  %s\n", p.AutopilotRunID)
	}
	if p.PipelineID != "" {
		fmt.Fprintf(w, "pipeline:       %s\n", p.PipelineID)
	}
	if p.OrchestratorID != "" {
		fmt.Fprintf(w, "orchestrator:   %s\n", p.OrchestratorID)
	}
	if len(p.TaskProgress) > 0 {
		fmt.Fprintln(w, "task_progress:")
		for k, v := range p.TaskProgress {
			fmt.Fprintf(w, "  %s: %s\n", k, v)
		}
	}
	fmt.Fprintf(w, "created:        %s\n", p.CreatedAt.Format(time.RFC3339))
	fmt.Fprintf(w, "updated:        %s\n", p.UpdatedAt.Format(time.RFC3339))
	if p.StartedAt != nil {
		fmt.Fprintf(w, "started:        %s\n", p.StartedAt.Format(time.RFC3339))
	}
	if p.CompletedAt != nil {
		fmt.Fprintf(w, "completed:      %s\n", p.CompletedAt.Format(time.RFC3339))
	}
}

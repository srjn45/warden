package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/planbackup"
)

func newPlanCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Manage plans tracked by the daemon",
		Long: "Manage plans tracked by the daemon.\n\n" +
			"Plans are canonical ScrivaDB records (goal, tasks, lifecycle, revision).\n" +
			"Repository YAML under plans/ is an optional inert export — not required\n" +
			"for create/run/complete/archive.\n\n" +
			"Create with `wd plan create`, start with `wd plan run`, control with\n" +
			"`wd plan pause|resume|stop`, mark tasks done with `wd plan done`, then\n" +
			"`wd plan complete` (or `wd plan archive`).",
	}
	SetCommandHelpMetadata(cmd, "run", 25, "warden plan", "", NodeNamespace)

	children := []*cobra.Command{
		newPlanListCmd(),
		newPlanCreateCmd(),
		newPlanShowCmd(),
		newPlanRelatedCmd(),
		newPlanRunCmd(),
		newPlanControlCmd("pause"),
		newPlanControlCmd("resume"),
		newPlanControlCmd("stop"),
		newPlanDoneCmd(),
		newPlanCompleteCmd(),
		newPlanArchiveCmd(),
		newPlanSyncToRepoCmd(),
		newPlanBackupCmd(),
		newPlanImportCmd(),
		newPlanImportLegacyCmd(),
		newPlanScanCmd(),
		newPlanStatusCmd(),
		newPlanAssessCmd(),
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
			projectID, err := planProjectFlag(cmd)
			if err != nil {
				return err
			}
			status, _ := cmd.Flags().GetString("status")
			plans, err := clientFor(cmd).PlansList(cmd.Context(), projectID, status)
			if err != nil {
				return err
			}
			if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
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

func newPlanCreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create --name <name> --goal <text>",
		Short: "Create a canonical plan in ScrivaDB",
		Long: "Create a new pending plan in the daemon's ScrivaDB store (no repository\n" +
			"YAML write). --name and --goal are required. Supply tasks with repeatable\n" +
			"--task id:prompt flags, or (when stdin is a TTY) enter them interactively.\n\n" +
			"Optional --constraint and --done-when may be repeated.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, err := planProjectFlag(cmd)
			if err != nil {
				return err
			}
			name, _ := cmd.Flags().GetString("name")
			goal, _ := cmd.Flags().GetString("goal")
			taskFlags, _ := cmd.Flags().GetStringArray("task")
			constraints, _ := cmd.Flags().GetStringArray("constraint")
			doneWhen, _ := cmd.Flags().GetStringArray("done-when")

			tasks, err := resolvePlanCreateTasks(cmd, taskFlags)
			if err != nil {
				return err
			}

			p, err := clientFor(cmd).PlansCreate(cmd.Context(), client.PlansCreateRequest{
				ProjectID:   projectID,
				Name:        name,
				Goal:        goal,
				Tasks:       tasks,
				Constraints: constraints,
				DoneWhen:    doneWhen,
			})
			if err != nil {
				return err
			}
			if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
				return printJSON(cmd.OutOrStdout(), p)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "created plan %s (%s) rev=%d\n", p.ID, p.Name, p.Revision)
			return nil
		},
	}
	cmd.Flags().String("project", "", "project ID (default: current directory)")
	cmd.Flags().String("name", "", "plan name")
	cmd.Flags().String("goal", "", "what the plan is trying to achieve")
	cmd.Flags().StringArray("task", nil, "task as id:prompt (repeatable; skip interactive prompt)")
	cmd.Flags().StringArray("constraint", nil, "constraint the workers must follow (repeatable)")
	cmd.Flags().StringArray("done-when", nil, "completion criterion (repeatable)")
	cmd.Flags().Bool("json", false, "output as JSON")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("goal")
	return cmd
}

func newPlanShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <plan-id>",
		Short: "Show detail for one plan",
		Long: "Show the full canonical ScrivaDB record for one plan: goal, tasks, status,\n" +
			"revision, executor, task summary, export status, linked branches, and timestamps.\n" +
			"Repository YAML is never read for this view.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := clientFor(cmd).PlansGet(cmd.Context(), args[0])
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
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

func newPlanImportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "import <file>",
		Short: "[deprecated] Copy a plan YAML into plans/pending/ and scan",
		Long: "Deprecated one-release migration aid. Copy a plan YAML file into the\n" +
			"project's plans/pending/ directory and trigger a scan.\n\n" +
			"This cannot affect canonical ScrivaDB Plan definition or execution after\n" +
			"import — prefer `wd plan import-legacy` for one-time cutover of an existing\n" +
			"plans/{pending,in_progress,completed,archived} tree, or `wd plan create` for\n" +
			"new DB-native plans.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.ErrOrStderr(), "warning: wd plan import is deprecated; prefer wd plan import-legacy or wd plan create — scan/import cannot affect canonical execution after import")
			projectID, err := planProjectFlag(cmd)
			if err != nil {
				return err
			}
			src := args[0]
			data, err := os.ReadFile(src)
			if err != nil {
				return fmt.Errorf("read %s: %w", src, err)
			}

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

			res, err := clientFor(cmd).PlanScan(cmd.Context(), projectID, client.PlanScanRequest{})
			if err != nil {
				return fmt.Errorf("scan after import: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "scanned: %d stub(s) upserted", res.Upserted)
			if res.SkippedCanonical > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), ", %d canonical skipped", res.SkippedCanonical)
			}
			fmt.Fprintln(cmd.OutOrStdout())
			if res.Notice != "" {
				fmt.Fprintln(cmd.ErrOrStderr(), res.Notice)
			}
			return nil
		},
	}
	cmd.Flags().String("project", "", "project ID (default: current directory)")
	return cmd
}

func newPlanImportLegacyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "import-legacy",
		Short: "Import legacy plans/**/*.yaml into ScrivaDB (operator cutover)",
		Long: "Discover plans/{pending,in_progress,completed,archived}/*.yaml (and flat\n" +
			"plans/*.yaml) and create or reconcile canonical ScrivaDB Plans by stable\n" +
			"identity. Source files are left untouched.\n\n" +
			"Repeated import with a matching content hash is a no-op (skipped). An\n" +
			"existing canonical definition with a different hash is reported as\n" +
			"conflicted without mutation.\n\n" +
			"--report classifies without writing. Never runs automatically at daemon\n" +
			"startup.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, err := planProjectFlag(cmd)
			if err != nil {
				return err
			}
			reportOnly, _ := cmd.Flags().GetBool("report")
			asJSON, _ := cmd.Flags().GetBool("json")
			res, err := clientFor(cmd).ImportLegacyPlans(cmd.Context(), projectID, client.ImportLegacyPlansRequest{
				ReportOnly: reportOnly,
			})
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), res)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "legacy import: imported=%d skipped=%d conflicted=%d errors=%d",
				len(res.Imported), len(res.Skipped), len(res.Conflicted), len(res.Errors))
			if res.ReportOnly {
				fmt.Fprint(cmd.OutOrStdout(), " (report only)")
			}
			fmt.Fprintln(cmd.OutOrStdout())
			for _, r := range res.Conflicted {
				fmt.Fprintf(cmd.OutOrStdout(), "  conflict: %s (%s): %s\n", r.PlanID, r.FilePath, r.Reason)
			}
			for _, r := range res.Errors {
				fmt.Fprintf(cmd.OutOrStdout(), "  error: %s: %s\n", r.FilePath, r.Reason)
			}
			return nil
		},
	}
	cmd.Flags().String("project", "", "project ID (default: current directory)")
	cmd.Flags().Bool("report", false, "classify without mutating ScrivaDB")
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

func newPlanScanCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "scan",
		Short: "[deprecated] Scan plans/ and upsert stub plan records",
		Long: "Deprecated one-release migration aid. Walk plans/{pending,in_progress,\n" +
			"completed,archived}/*.yaml and upsert stub plan records (name/status/path).\n\n" +
			"After ImportLegacy or DB-native create, scan cannot affect canonical Plan\n" +
			"definition, lifecycle, or execution — Status is not reseeded from directory\n" +
			"placement for records with a non-empty definition. Prefer\n" +
			"`wd plan import-legacy` for cutover.\n\n" +
			"--migrate-flat moves any flat plans/*.yaml files into plans/pending/ with git mv\n" +
			"and creates a commit before scanning.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonOut, _ := cmd.Flags().GetBool("json")
			if !jsonOut {
				fmt.Fprintln(cmd.ErrOrStderr(), "warning: wd plan scan is deprecated; prefer wd plan import-legacy — scan cannot affect canonical execution after import")
			}
			projectID, err := planProjectFlag(cmd)
			if err != nil {
				return err
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
			if jsonOut {
				return printJSON(cmd.OutOrStdout(), res)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "scanned: %d stub(s) upserted", res.Upserted)
			if res.SkippedCanonical > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), ", %d canonical skipped", res.SkippedCanonical)
			}
			fmt.Fprintln(cmd.OutOrStdout())
			if res.Notice != "" {
				fmt.Fprintln(cmd.ErrOrStderr(), res.Notice)
			}
			return nil
		},
	}
	cmd.Flags().String("project", "", "project ID (default: current directory)")
	cmd.Flags().Bool("migrate-flat", false, "move flat plans/*.yaml files into plans/pending/ with git mv + commit")
	cmd.Flags().Bool("assess", false, "run brain-assisted progress assessment for in_progress plans")
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

func newPlanStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status <plan-id> <new-status>",
		Short: "[deprecated] Transition a plan's status (DB field only)",
		Long: "Deprecated migration aid. Change a plan's lifecycle status via the\n" +
			"project-scoped API (ScrivaDB Status field only — no repository YAML move).\n\n" +
			"Prefer `wd plan run` / `wd plan complete` / `wd plan archive` for the\n" +
			"PlanService state machine.\n\n" +
			"Valid statuses: pending | in_progress | completed | archived",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.ErrOrStderr(), "warning: wd plan status is deprecated; prefer wd plan run|complete|archive")
			planID, newStatus := args[0], args[1]
			projectID, err := planProjectFlag(cmd)
			if err != nil {
				return err
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
		Short: "Archive a plan (any status → archived)",
		Long:  "Move a plan to the archived state. Allowed from pending, in_progress, or completed.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := clientFor(cmd).PlansArchive(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
				return printJSON(cmd.OutOrStdout(), p)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "plan %s archived (status=%s rev=%d)\n", p.ID, p.Status, p.Revision)
			return nil
		},
	}
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

func newPlanSyncToRepoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sync_to_repo <plan-id>",
		Short: "Export a plan revision to a dedicated branch and open a PR",
		Long: "Render the canonical ScrivaDB Plan as an inert replica (YAML by default;\n" +
			"`--format json` for JSON) on a dedicated `warden/plan-sync/<plan-id>/<revision>`\n" +
			"branch and open (or reuse) a PR against --base. Uses an isolated git worktree —\n" +
			"never stages the operator's checked-out branch, force-pushes, auto-merges, or\n" +
			"overwrites a conflicting non-Warden file. Repeating the same revision/hash for\n" +
			"the same repo/ref/path returns the prior result with no new GitHub activity.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			base, _ := cmd.Flags().GetString("base")
			if strings.TrimSpace(base) == "" {
				return fmt.Errorf("--base is required (PR target ref)")
			}
			repoPath, _ := cmd.Flags().GetString("repo")
			outputPath, _ := cmd.Flags().GetString("path")
			repository, _ := cmd.Flags().GetString("repository")
			format, _ := cmd.Flags().GetString("format")
			res, err := clientFor(cmd).PlansSyncToRepo(cmd.Context(), args[0], client.PlansSyncToRepoRequest{
				TargetRef:      base,
				RepositoryPath: repoPath,
				Format:         format,
				OutputPath:     outputPath,
				Repository:     repository,
			})
			if err != nil {
				return err
			}
			if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
				return printJSON(cmd.OutOrStdout(), res)
			}
			switch res.Outcome {
			case "skipped":
				fmt.Fprintf(cmd.OutOrStdout(), "plan %s sync skipped (already exported rev %d): %s\n",
					res.PlanID, res.Revision, res.PRURL)
			case "success":
				fmt.Fprintf(cmd.OutOrStdout(), "plan %s synced to %s (rev %d)\n  branch: %s\n  commit: %s\n  pr: %s\n",
					res.PlanID, res.OutputPath, res.Revision, res.Branch, res.CommitSHA, res.PRURL)
			default:
				fmt.Fprintf(cmd.OutOrStdout(), "plan %s sync %s: %s\n", res.PlanID, res.Outcome, res.ErrorMessage)
			}
			return nil
		},
	}
	cmd.Flags().String("base", "", "PR base branch / target ref (required)")
	cmd.Flags().String("repo", "", "local git repository path (default: plan project root)")
	cmd.Flags().String("format", "yaml", "replica format: yaml (default) or json")
	cmd.Flags().String("path", "", "replica output path override (default: plans/{lifecycle}/<slug>.{yaml|json})")
	cmd.Flags().String("repository", "", "stable repository identity for export records (default: origin URL)")
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

func newPlanBackupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Export or restore a portable Plan backup bundle",
		Long: "Local backup / machine-transfer for canonical ScrivaDB Plans.\n\n" +
			"Bundles include definition, revision, execution evidence, events/notes,\n" +
			"and integrity hashes. They exclude credentials and disposable worktrees.\n" +
			"Restore never consults Git or repository replicas under plans/.",
	}
	cmd.AddCommand(newPlanBackupExportCmd(), newPlanBackupRestoreCmd())
	return cmd
}

func newPlanBackupExportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "export [plan-id ...]",
		Short: "Write a Plan backup bundle to a file or stdout",
		Long: "Export one or more Plans from ScrivaDB into a versioned backup bundle.\n" +
			"Pass plan IDs as args, or --all (optionally scoped with --project).\n\n" +
			"  wd plan backup export plan-ab12cd34 -o plan.bundle.json\n" +
			"  wd plan backup export --all --project /path/to/repo -o all-plans.json",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			all, _ := cmd.Flags().GetBool("all")
			project, _ := cmd.Flags().GetString("project")
			outPath, _ := cmd.Flags().GetString("output")
			if !all && len(args) == 0 {
				return fmt.Errorf("specify plan id(s) or --all")
			}
			bundle, err := clientFor(cmd).PlansExportBackup(cmd.Context(), client.PlansExportBackupRequest{
				PlanIDs:   append([]string(nil), args...),
				ProjectID: project,
				All:       all,
			})
			if err != nil {
				return err
			}
			raw, err := json.MarshalIndent(bundle, "", "  ")
			if err != nil {
				return err
			}
			raw = append(raw, '\n')
			if outPath == "" || outPath == "-" {
				_, err = cmd.OutOrStdout().Write(raw)
				return err
			}
			if err := os.WriteFile(outPath, raw, 0o600); err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "exported %d plan(s) → %s (bundle_hash=%s)\n",
				len(bundle.Entries), outPath, bundle.BundleHash)
			return nil
		},
	}
	cmd.Flags().Bool("all", false, "export every plan (optionally scoped by --project)")
	cmd.Flags().String("project", "", "when used with --all, limit export to this project id")
	cmd.Flags().StringP("output", "o", "", "output file (default: stdout)")
	return cmd
}

func newPlanBackupRestoreCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "restore <bundle-file>",
		Short: "Restore Plans from a backup bundle into ScrivaDB",
		Long: "Validate and restore a Plan backup bundle. Does not read Git or plans/\n" +
			"replicas. Re-running the same bundle is idempotent when identity +\n" +
			"content hash + revision match.\n\n" +
			"  wd plan backup restore plan.bundle.json --dry-run\n" +
			"  wd plan backup restore plan.bundle.json --on-conflict skip",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			onConflict, _ := cmd.Flags().GetString("on-conflict")
			raw, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			var bundle planbackup.Bundle
			if err := json.Unmarshal(raw, &bundle); err != nil {
				return fmt.Errorf("parse bundle: %w", err)
			}
			res, err := clientFor(cmd).PlansRestoreBackup(cmd.Context(), client.PlansRestoreBackupRequest{
				Bundle:     bundle,
				DryRun:     dryRun,
				OnConflict: planbackup.ConflictPolicy(onConflict),
			})
			if err != nil {
				return err
			}
			if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
				return printJSON(cmd.OutOrStdout(), res)
			}
			for _, e := range res.Entries {
				line := fmt.Sprintf("%s\t%s", e.PlanID, e.Outcome)
				if e.Reason != "" {
					line += "\t" + e.Reason
				}
				fmt.Fprintln(cmd.OutOrStdout(), line)
			}
			if res.DryRun {
				fmt.Fprintln(cmd.OutOrStdout(), "(dry-run — no changes written)")
			}
			return nil
		},
	}
	cmd.Flags().Bool("dry-run", false, "validate integrity and conflicts without writing")
	cmd.Flags().String("on-conflict", "skip", "stable-id policy: skip|fail|overwrite")
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

func newPlanAssessCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "assess <plan-id>",
		Short: "Brain-assisted task progress assessment",
		Long: "Use a brain model to reconstruct task progress from git history and open PRs.\n" +
			"Updates task_progress in the DB record. Opt-in — never run automatically.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			planID := args[0]
			projectID, err := planProjectFlag(cmd)
			if err != nil {
				return err
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
		Use:   "run <plan-id>",
		Short: "Start execution of a plan in the given mode",
		Long: "Start execution of a plan (pending → in_progress). This is the only supported\n" +
			"public start path for plan execution (including autopilot). The mode determines\n" +
			"how the plan is executed:\n\n" +
			"  autopilot           Creates a live Autopilot executor + manager\n" +
			"  pipeline            Each task becomes a pipeline job\n" +
			"  orchestrator        Orchestrator + workers with human approval gates\n" +
			"  manual              Plan-bound general agent; human drives prompting\n\n" +
			"`orchestrator` is accepted as an alias for `orchestrator_worker`.\n" +
			"Control a running plan with `wd plan pause|resume|stop`.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			planID := args[0]
			mode, _ := cmd.Flags().GetString("mode")
			mode, err := normalizePlanRunMode(mode)
			if err != nil {
				return err
			}
			p, err := clientFor(cmd).PlansRun(cmd.Context(), planID, mode)
			if err != nil {
				return err
			}
			if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
				return printJSON(cmd.OutOrStdout(), p)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "plan %s run started (mode: %s)\n", p.ID, mode)
			return nil
		},
	}
	cmd.Flags().String("mode", "", "execution mode: autopilot|pipeline|orchestrator|manual")
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

func newPlanControlCmd(action string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   action + " <plan-id>",
		Short: action + " an in-progress plan's active executor",
		Long: "Control the active executor for an in-progress plan (autopilot, pipeline, or\n" +
			"plan-bound agent). Together with `wd plan run`, this is the public lifecycle\n" +
			"surface for plan execution.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := clientFor(cmd).PlansControl(cmd.Context(), args[0], action)
			if err != nil {
				return err
			}
			if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
				return printJSON(cmd.OutOrStdout(), p)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "plan %s %s\n", p.ID, action)
			return nil
		},
	}
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

func newPlanDoneCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "done <plan-id> <task-id>",
		Short: "Mark a plan task done",
		Long: "Shorthand for updating one task's status to done. Updates TaskProgress in\n" +
			"the daemon only (the YAML is unchanged).",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			planID, taskID := args[0], args[1]
			p, err := clientFor(cmd).PlansUpdateTaskStatus(cmd.Context(), planID, taskID, "done")
			if err != nil {
				return err
			}
			if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
				return printJSON(cmd.OutOrStdout(), p)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "plan %s task %s → done\n", p.ID, taskID)
			return nil
		},
	}
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

func newPlanCompleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "complete <plan-id>",
		Short: "Complete a plan (in_progress → completed)",
		Long: "Complete a plan: in_progress → completed. Blocked if any task is not\n" +
			"done/skipped or any associated branch is still unmerged. On success moves\n" +
			"the YAML to plans/completed/ and cleans up worktrees.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := clientFor(cmd).PlansComplete(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
				return printJSON(cmd.OutOrStdout(), p)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "plan %s completed (file: %s)\n", p.ID, p.FilePath)
			return nil
		},
	}
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

// planProjectFlag returns --project, defaulting to the current directory.
func planProjectFlag(cmd *cobra.Command) (string, error) {
	projectID, _ := cmd.Flags().GetString("project")
	if projectID != "" {
		return projectID, nil
	}
	return resolveProjectID(cmd)
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
	if filepath.IsAbs(projectID) {
		return projectID, nil
	}
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

func resolvePlanCreateTasks(cmd *cobra.Command, taskFlags []string) ([]client.PlanTaskSpec, error) {
	if len(taskFlags) > 0 {
		tasks := make([]client.PlanTaskSpec, 0, len(taskFlags))
		for _, raw := range taskFlags {
			t, err := parsePlanTaskFlag(raw)
			if err != nil {
				return nil, err
			}
			tasks = append(tasks, t)
		}
		return tasks, nil
	}
	if readerIsTTY(cmd.InOrStdin()) {
		return promptPlanTasks(cmd.InOrStdin(), cmd.OutOrStdout())
	}
	// Piped stdin uses the same id/prompt/blank-id format as the TTY prompt.
	tasks, err := promptPlanTasks(cmd.InOrStdin(), io.Discard)
	if err != nil {
		if errors.Is(err, errNoPlanTasks) {
			return nil, fmt.Errorf("provide --task id:prompt (repeatable) or run interactively on a TTY")
		}
		return nil, err
	}
	return tasks, nil
}

func readerIsTTY(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	return isTTY(f)
}

func parsePlanTaskFlag(s string) (client.PlanTaskSpec, error) {
	id, prompt, ok := strings.Cut(s, ":")
	id = strings.TrimSpace(id)
	prompt = strings.TrimSpace(prompt)
	if !ok || id == "" || prompt == "" {
		return client.PlanTaskSpec{}, fmt.Errorf("invalid --task %q (want id:prompt)", s)
	}
	return client.PlanTaskSpec{ID: id, Prompt: prompt}, nil
}

var errNoPlanTasks = errors.New("at least one task is required")

func promptPlanTasks(in io.Reader, out io.Writer) ([]client.PlanTaskSpec, error) {
	fmt.Fprintln(out, "Enter tasks (blank id to finish):")
	rd := bufio.NewReader(in)
	var tasks []client.PlanTaskSpec
	for {
		fmt.Fprint(out, "Task ID: ")
		id, err := rd.ReadString('\n')
		id = strings.TrimSpace(id)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if id == "" {
			break
		}
		fmt.Fprint(out, "Prompt: ")
		prompt, err := rd.ReadString('\n')
		prompt = strings.TrimSpace(prompt)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if prompt == "" {
			return nil, fmt.Errorf("task %s: prompt is required", id)
		}
		tasks = append(tasks, client.PlanTaskSpec{ID: id, Prompt: prompt})
	}
	if len(tasks) == 0 {
		return nil, errNoPlanTasks
	}
	return tasks, nil
}

func normalizePlanRunMode(mode string) (string, error) {
	switch mode {
	case "autopilot", "pipeline", "orchestrator_worker", "manual":
		return mode, nil
	case "orchestrator":
		return "orchestrator_worker", nil
	case "":
		return "", fmt.Errorf("--mode is required (autopilot|pipeline|orchestrator|manual)")
	default:
		return "", fmt.Errorf("unknown --mode %q (autopilot|pipeline|orchestrator|manual)", mode)
	}
}

func printPlanTable(w io.Writer, plans []client.PlanView) error {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSTATUS\tREV\tTASKS\tEXECUTOR\tEXPORT\tUPDATED")
	for _, p := range plans {
		tasks := "-"
		if p.TaskSummary != nil && p.TaskSummary.Total > 0 {
			tasks = fmt.Sprintf("%d/%d", p.TaskSummary.Done, p.TaskSummary.Total)
		}
		exec := p.ExecutorID
		if exec == "" {
			exec = "-"
		}
		export := p.ExportStatus
		if export == "" {
			export = "none"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
			p.ID, p.Name, p.Status, p.Revision, tasks, exec, export, p.UpdatedAt.Format(time.RFC3339))
	}
	return tw.Flush()
}

func printPlanDetail(w io.Writer, p *client.PlanView) {
	fmt.Fprintf(w, "id:             %s\n", p.ID)
	fmt.Fprintf(w, "name:           %s\n", p.Name)
	fmt.Fprintf(w, "project:        %s\n", p.ProjectID)
	fmt.Fprintf(w, "status:         %s\n", p.Status)
	fmt.Fprintf(w, "revision:       %d\n", p.Revision)
	if p.ContentHash != "" {
		fmt.Fprintf(w, "content_hash:   %s\n", p.ContentHash)
	}
	export := p.ExportStatus
	if export == "" {
		export = "none"
	}
	fmt.Fprintf(w, "export_status:  %s\n", export)
	if p.RepoExport != nil && p.RepoExport.FilePath != "" {
		fmt.Fprintf(w, "last_export:    %s\n", p.RepoExport.FilePath)
	} else if p.FilePath != "" {
		fmt.Fprintf(w, "last_export:    %s\n", p.FilePath)
	}
	if p.Goal != "" {
		fmt.Fprintf(w, "goal:           %s\n", p.Goal)
	}
	if p.ExecutionMode != "" {
		fmt.Fprintf(w, "mode:           %s\n", p.ExecutionMode)
	}
	if p.ExecutorID != "" {
		fmt.Fprintf(w, "executor:       %s\n", p.ExecutorID)
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
	if p.TaskSummary != nil && p.TaskSummary.Total > 0 {
		fmt.Fprintf(w, "task_summary:   %d/%d done (%d in_progress, %d pending, %d skipped)\n",
			p.TaskSummary.Done, p.TaskSummary.Total, p.TaskSummary.InProgress, p.TaskSummary.Pending, p.TaskSummary.Skipped)
	}
	if len(p.Constraints) > 0 {
		fmt.Fprintln(w, "constraints:")
		for _, c := range p.Constraints {
			fmt.Fprintf(w, "  - %s\n", c)
		}
	}
	if len(p.DoneWhen) > 0 {
		fmt.Fprintln(w, "done_when:")
		for _, d := range p.DoneWhen {
			fmt.Fprintf(w, "  - %s\n", d)
		}
	}
	if len(p.Tasks) > 0 {
		fmt.Fprintln(w, "tasks:")
		for _, t := range p.Tasks {
			line := fmt.Sprintf("  %s: %s", t.ID, t.Prompt)
			if len(t.After) > 0 {
				line += " (after " + strings.Join(t.After, ", ") + ")"
			}
			fmt.Fprintln(w, line)
		}
	}
	if len(p.TaskProgress) > 0 {
		fmt.Fprintln(w, "task_progress:")
		for k, v := range p.TaskProgress {
			fmt.Fprintf(w, "  %s: %s\n", k, v)
		}
	}
	if len(p.PlanBranches) > 0 {
		fmt.Fprintf(w, "branches:       %s\n", strings.Join(p.PlanBranches, ", "))
	}
	fmt.Fprintf(w, "created:        %s\n", p.CreatedAt.Format(time.RFC3339))
	fmt.Fprintf(w, "updated:        %s\n", p.UpdatedAt.Format(time.RFC3339))
	if !p.StartedAt.IsZero() {
		fmt.Fprintf(w, "started:        %s\n", p.StartedAt.Format(time.RFC3339))
	}
	if !p.CompletedAt.IsZero() {
		fmt.Fprintf(w, "completed:      %s\n", p.CompletedAt.Format(time.RFC3339))
	}
	if !p.ArchivedAt.IsZero() {
		fmt.Fprintf(w, "archived:       %s\n", p.ArchivedAt.Format(time.RFC3339))
	}
}

func newPlanRelatedCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "related <plan-id>",
		Short: "List heuristic related / overlapping plans",
		Long: "List related plans for a canonical ScrivaDB plan using heuristic overlap\n" +
			"on project, title, goal, and linked branches/PRs.\n\n" +
			"Hits are discovery aids only — not authoritative identity or duplicate detection.\n" +
			"Repository YAML replicas never appear as additional plans.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			limit, _ := cmd.Flags().GetInt("limit")
			res, err := clientFor(cmd).PlansRelated(cmd.Context(), args[0], limit)
			if err != nil {
				return err
			}
			if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
				return printJSON(cmd.OutOrStdout(), res)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "anchor: %s\n", res.AnchorID)
			fmt.Fprintf(cmd.OutOrStdout(), "note:   %s\n", res.Disclaimer)
			if len(res.Hits) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no related plans found")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "SCORE\tID\tNAME\tSTATUS\tREASONS")
			for _, h := range res.Hits {
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n",
					h.Score, h.PlanID, h.Name, h.Status, strings.Join(h.Reasons, ","))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().Int("limit", 10, "maximum hits to return")
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

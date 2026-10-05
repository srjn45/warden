package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/planbackup"
	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/plansync"
)

func newPlanCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Manage plans tracked by the daemon",
		Long: "Manage plans tracked by the daemon.\n\n" +
			"Plans are canonical ScrivaDB records (goal, tasks, lifecycle, revision).\n" +
			"Repository YAML under plans/ is an optional inert export — not required\n" +
			"for create/run/complete/archive.\n\n" +
			"Typical journey:\n\n" +
			"  1. Create    `wd plan create` (then `wd plan show` to inspect it)\n" +
			"  2. Edit      `wd plan update`, `wd plan edit` or `wd plan task add|edit|rm`\n" +
			"               (only while the plan is pending)\n" +
			"  3. Run       `wd plan run --mode <mode>` (pending → in_progress); follow\n" +
			"               progress with `wd plan show --watch`\n" +
			"  4. Control   `wd plan pause`, `resume` or `stop` the running executor\n" +
			"  5. Progress  `wd plan task status` / `wd plan done` record task progress\n" +
			"  6. Complete  `wd plan complete` (in_progress → completed)\n" +
			"  7. Finish    `wd plan archive` (reversible), or `wd plan delete` to remove a\n" +
			"               pending or archived plan permanently",
	}
	SetCommandHelpMetadata(cmd, "run", 25, "warden plan", "", NodeNamespace)

	children := []*cobra.Command{
		newPlanListCmd(),
		newPlanCreateCmd(),
		newPlanUpdateCmd(),
		newPlanEditCmd(),
		newPlanTaskCmd(),
		newPlanShowCmd(),
		newPlanRelatedCmd(),
		newPlanRunCmd(),
		newPlanControlCmd("pause"),
		newPlanControlCmd("resume"),
		newPlanControlCmd("stop"),
		newPlanRestartCmd(),
		newPlanDoneCmd(),
		newPlanCompleteCmd(),
		newPlanArchiveCmd(),
		newPlanDeleteCmd(),
		newPlanSyncToRepoCmd("sync-to-repo", false),
		newPlanSyncToRepoCmd("sync_to_repo", true), // deprecated spelling, kept as a hidden alias
		newPlanHubSyncCmd(),
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

func newPlanHubSyncCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hub-sync",
		Short: "Explicitly sync canonical plans with the configured Hub",
		Long: "Explicit, operator-driven sync of canonical plans with the configured Hub.\n" +
			"Nothing syncs automatically: the Hub is only contacted when you run one of\n" +
			"these subcommands.\n\n" +
			"  discover  List plans the Hub offers (read-only)\n" +
			"  pull      Fetch plan envelopes from the Hub (does not overwrite local plans)\n" +
			"  push      Offer one local plan revision to the Hub",
	}
	type hubVerb struct{ short, long, scope string }
	verbs := map[string]hubVerb{
		"push": {
			short: "Push one local plan revision to the Hub",
			long: "Send a local plan's current revision to the Hub and stamp the local record\n" +
				"with the remote id and sync time. The plan id is required.",
			scope: "Hub project scope to push into (default: the plan's own project)",
		},
		"pull": {
			short: "Fetch plan envelopes from the Hub",
			long: "Fetch plan envelopes from the Hub and print them. Pulling never imports or\n" +
				"overwrites local plan definitions; it only stamps sync metadata on local\n" +
				"plans the Hub already knows. With a plan id, only that plan is fetched;\n" +
				"otherwise all plans in scope are returned (narrow with --status).",
			scope: "Hub project scope to pull from (default: unscoped)",
		},
		"discover": {
			short: "List plans available on the Hub",
			long: "Ask the Hub which plans it has in scope and print their envelopes. This is\n" +
				"read-only discovery: nothing is imported. With a plan id, the result is\n" +
				"limited to that plan (narrow further with --status).",
			scope: "Hub project scope to search (default: unscoped)",
		},
	}
	for _, verb := range []string{"push", "pull", "discover"} {
		verb := verb
		hv := verbs[verb]
		use := verb + " [plan-id]"
		if verb == "push" {
			use = "push <plan-id>"
		}
		child := &cobra.Command{Use: use, Short: hv.short, Long: hv.long, Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			scope, _ := cmd.Flags().GetString("scope")
			statuses, _ := cmd.Flags().GetStringArray("status")
			req := client.PlanSyncRequest{Scope: plansync.Scope{ProjectID: scope}, Statuses: planSyncStatuses(statuses)}
			if len(args) > 0 {
				req.PlanID = args[0]
			}
			if verb == "push" {
				if req.PlanID == "" {
					return fmt.Errorf("plan-id is required")
				}
				p, err := clientFor(cmd).PlansSyncPush(cmd.Context(), req)
				if err != nil {
					return err
				}
				if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
					return printJSON(cmd.OutOrStdout(), p)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "plan %s pushed to the Hub (rev %d)\n", p.ID, p.Revision)
				return nil
			}
			var out *client.PlanSyncEnvelopes
			var err error
			if verb == "pull" {
				out, err = clientFor(cmd).PlansSyncPull(cmd.Context(), req)
			} else {
				out, err = clientFor(cmd).PlansSyncDiscover(cmd.Context(), req)
			}
			if err != nil {
				return err
			}
			if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
				return printJSON(cmd.OutOrStdout(), out)
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "%d plan envelope(s)\n", len(out.Envelopes))
			for _, env := range out.Envelopes {
				fmt.Fprintf(w, "  %s  %s  rev=%d  %s\n", env.PlanID, env.Name, env.Revision, env.Lifecycle)
			}
			return nil
		}}
		child.Flags().String("scope", "", hv.scope)
		child.Flags().Bool("json", false, "output as JSON")
		child.Flags().StringArray("status", nil, "lifecycle status filter (repeatable)")
		cmd.AddCommand(child)
	}
	return cmd
}

func planSyncStatuses(in []string) []planstore.PlanStatus {
	out := make([]planstore.PlanStatus, 0, len(in))
	for _, v := range in {
		out = append(out, planstore.PlanStatus(v))
	}
	return out
}

func newPlanListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List plans for a project",
		Long: "List plans registered in the daemon for a project.\n\n" +
			"Use --project to specify the project (defaults to the git root of the current directory).\n" +
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
	cmd.Flags().String("project", "", "project ID (default: git root of the current directory)")
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
			"--task flags, or (when stdin is a TTY) enter them interactively.\n\n" +
			"Plans are task DAGs: edges are after-deps. Prefer\n" +
			"--task id@dep1,dep2:prompt. If you pass two or more tasks with no after\n" +
			"edges, warden chains them in flag order so every multi-task plan has a DAG.\n\n" +
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
	cmd.Flags().String("project", "", "project ID (default: git root of the current directory)")
	cmd.Flags().String("name", "", "plan name")
	cmd.Flags().String("goal", "", "what the plan is trying to achieve")
	cmd.Flags().StringArray("task", nil, "task as id:prompt or id@dep1,dep2:prompt (repeatable; skip interactive prompt)")
	cmd.Flags().StringArray("constraint", nil, "constraint the workers must follow (repeatable)")
	cmd.Flags().StringArray("done-when", nil, "completion criterion (repeatable)")
	cmd.Flags().Bool("json", false, "output as JSON")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("goal")
	return cmd
}

func newPlanUpdateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update <plan-id>",
		Short: "Update a pending plan definition",
		Long: "Change a pending plan's definition. Only pending plans can be edited;\n" +
			"once a plan is running, completed or archived the update is refused.\n\n" +
			"Provide at least one of --file, --name, --goal, --constraint, or --done-when.\n" +
			"When --file is set, the plan YAML in that file is applied first and any\n" +
			"explicit flags overlay those fields. The update is checked against the\n" +
			"plan's current revision, so it fails if the plan changed since it was read.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPlanUpdate(cmd, args[0])
		},
	}
	cmd.Flags().String("file", "", "YAML definition file to apply")
	cmd.Flags().String("name", "", "new plan name")
	cmd.Flags().String("goal", "", "new plan goal")
	cmd.Flags().StringArray("constraint", nil, "replace constraints (repeatable)")
	cmd.Flags().StringArray("done-when", nil, "replace done_when criteria (repeatable)")
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

func runPlanUpdate(cmd *cobra.Command, planID string) error {
	filePath, _ := cmd.Flags().GetString("file")
	nameChanged := cmd.Flags().Changed("name")
	goalChanged := cmd.Flags().Changed("goal")
	constraintChanged := cmd.Flags().Changed("constraint")
	doneWhenChanged := cmd.Flags().Changed("done-when")
	if filePath == "" && !nameChanged && !goalChanged && !constraintChanged && !doneWhenChanged {
		return fmt.Errorf("provide at least one of --file, --name, --goal, --constraint, or --done-when")
	}

	c := clientFor(cmd)
	cur, err := c.PlansGet(cmd.Context(), planID)
	if err != nil {
		return err
	}

	var req client.PlansUpdateRequest
	if filePath != "" {
		data, err := os.ReadFile(filePath)
		if err != nil {
			return fmt.Errorf("read %s: %w", filePath, err)
		}
		req, err = client.ParsePlanYAML(data)
		if err != nil {
			return err
		}
	}
	if nameChanged {
		req.Name, _ = cmd.Flags().GetString("name")
	}
	if goalChanged {
		req.Goal, _ = cmd.Flags().GetString("goal")
	}
	if constraintChanged {
		req.Constraints, _ = cmd.Flags().GetStringArray("constraint")
	}
	if doneWhenChanged {
		req.DoneWhen, _ = cmd.Flags().GetStringArray("done-when")
	}
	req.ExpectedRevision = cur.Revision

	p, err := c.PlansUpdate(cmd.Context(), planID, req)
	if err != nil {
		return err
	}
	if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
		return printJSON(cmd.OutOrStdout(), p)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "updated plan %s (%s) rev=%d\n", p.ID, p.Name, p.Revision)
	return nil
}

func newPlanTaskCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "task",
		Short: "Add, edit, or remove tasks on a pending plan",
		Long: "Manage a plan's tasks. add, edit and rm change the task definitions and only\n" +
			"work on pending plans; status records progress and is used while a plan runs.\n\n" +
			"  add     Append a task to the plan\n" +
			"  edit    Change one task's prompt or dependencies\n" +
			"  rm      Remove a task (refused while other tasks depend on it)\n" +
			"  status  Set a task's status (pending|in_progress|done|skipped)\n\n" +
			"edit and rm take the task id as the second argument (or --id). The skipped\n" +
			"status counts as finished: `plan complete` accepts done or skipped tasks.",
	}
	cmd.AddCommand(newPlanTaskAddCmd(), newPlanTaskEditCmd(), newPlanTaskRmCmd(), newPlanTaskStatusCmd())
	return cmd
}

func newPlanTaskAddCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add <plan-id>",
		Short: "Add a task to a pending plan",
		Long: "Append a task to a pending plan's DAG. --id and --prompt are required.\n" +
			"Repeat --after for dependencies. Optional --expected-revision fails the change\n" +
			"if the plan was modified since that revision (omit to skip the check).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, _ := cmd.Flags().GetString("id")
			prompt, _ := cmd.Flags().GetString("prompt")
			after, _ := cmd.Flags().GetStringArray("after")
			expected, _ := cmd.Flags().GetInt64("expected-revision")
			p, err := clientFor(cmd).PlansTaskAdd(cmd.Context(), args[0], client.PlansTaskAddRequest{
				ID:               id,
				Prompt:           prompt,
				After:            after,
				ExpectedRevision: expected,
			})
			if err != nil {
				return err
			}
			if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
				return printJSON(cmd.OutOrStdout(), p)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "plan %s task %s added (rev=%d)\n", p.ID, id, p.Revision)
			return nil
		},
	}
	cmd.Flags().String("id", "", "task id")
	cmd.Flags().String("prompt", "", "task prompt")
	cmd.Flags().StringArray("after", nil, "dependency task id (repeatable)")
	cmd.Flags().Int64("expected-revision", 0, "fail if the plan is no longer at this revision (omit to skip the check)")
	cmd.Flags().Bool("json", false, "output as JSON")
	_ = cmd.MarkFlagRequired("id")
	_ = cmd.MarkFlagRequired("prompt")
	return cmd
}

func newPlanTaskEditCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "edit <plan-id> [task-id]",
		Short: "Edit a task definition on a pending plan",
		Long: "Patch one task's prompt and/or after-deps on a pending plan. Pass the task\n" +
			"id as a second argument or via --id. Provide --prompt and/or --after; omitted fields are left unchanged.\n" +
			"Optional --expected-revision fails the change if the plan was modified\n" +
			"since that revision (omit to skip the check).",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			taskID, err := planTaskIDArg(cmd, args, 1)
			if err != nil {
				return err
			}
			req := client.PlansTaskUpdateRequest{}
			if cmd.Flags().Changed("prompt") {
				prompt, _ := cmd.Flags().GetString("prompt")
				req.Prompt = &prompt
			}
			if cmd.Flags().Changed("after") {
				after, _ := cmd.Flags().GetStringArray("after")
				req.After = &after
			}
			if req.Prompt == nil && req.After == nil {
				return fmt.Errorf("provide --prompt and/or --after")
			}
			if cmd.Flags().Changed("expected-revision") {
				expected, _ := cmd.Flags().GetInt64("expected-revision")
				req.ExpectedRevision = expected
			}
			p, err := clientFor(cmd).PlansTaskUpdate(cmd.Context(), args[0], taskID, req)
			if err != nil {
				return err
			}
			if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
				return printJSON(cmd.OutOrStdout(), p)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "plan %s task %s updated (rev=%d)\n", p.ID, taskID, p.Revision)
			return nil
		},
	}
	cmd.Flags().String("id", "", "task id (alternative to positional)")
	cmd.Flags().String("prompt", "", "new task prompt")
	cmd.Flags().StringArray("after", nil, "replace after-deps (repeatable; pass once with empty to clear)")
	cmd.Flags().Int64("expected-revision", 0, "fail if the plan is no longer at this revision (omit to skip the check)")
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

func newPlanTaskRmCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rm <plan-id> [task-id]",
		Short: "Remove a task from a pending plan",
		Long: "Remove a task from a pending plan. Pass the task id as a second argument\n" +
			"or via --id. Removal is rejected if other tasks still depend on it.\n" +
			"Optional --expected-revision fails the change if the plan was modified\n" +
			"since that revision (omit to skip the check).",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			planID := args[0]
			taskID, err := planTaskIDArg(cmd, args, 1)
			if err != nil {
				return err
			}
			var expected int64
			if cmd.Flags().Changed("expected-revision") {
				expected, _ = cmd.Flags().GetInt64("expected-revision")
			}
			p, err := clientFor(cmd).PlansTaskDelete(cmd.Context(), planID, taskID, expected)
			if err != nil {
				return err
			}
			if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
				return printJSON(cmd.OutOrStdout(), p)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "plan %s task %s removed (rev=%d)\n", p.ID, taskID, p.Revision)
			return nil
		},
	}
	cmd.Flags().String("id", "", "task id (alternative to positional)")
	cmd.Flags().Int64("expected-revision", 0, "fail if the plan is no longer at this revision (omit to skip the check)")
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

func newPlanShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <plan-id>",
		Short: "Show detail for one plan",
		Long: "Show the full canonical ScrivaDB record for one plan: goal, tasks, status,\n" +
			"revision, executor, task summary, export status, linked branches, and timestamps.\n" +
			"Repository YAML is never read for this view.\n\n" +
			"For an in_progress plan it is the single status view of the run: executor\n" +
			"kind and state (active, healing, degraded, paused, stopped), backoff detail\n" +
			"when present, the integration branch, and per task the state, worker agent\n" +
			"and PR. Use --watch to keep refreshing it.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonOut, _ := cmd.Flags().GetBool("json")
			show := func() error {
				p, err := clientFor(cmd).PlansGet(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				if jsonOut {
					return printJSON(cmd.OutOrStdout(), p)
				}
				printPlanDetail(cmd.OutOrStdout(), p)
				return nil
			}
			if watch, _ := cmd.Flags().GetBool("watch"); watch {
				return watchPlanShow(cmd, show, jsonOut)
			}
			return show()
		},
	}
	cmd.Flags().Bool("json", false, "output as JSON")
	cmd.Flags().Bool("watch", false, "refresh the view every few seconds until interrupted")
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
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.ErrOrStderr(), "warning: wd plan import is deprecated; use wd plan create or wd plan import-legacy instead")
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
	cmd.Flags().String("project", "", "project ID (default: git root of the current directory)")
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
	cmd.Flags().String("project", "", "project ID (default: git root of the current directory)")
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
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonOut, _ := cmd.Flags().GetBool("json")
			fmt.Fprintln(cmd.ErrOrStderr(), "warning: wd plan scan is deprecated; use wd plan create or wd plan import-legacy instead")
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
	cmd.Flags().String("project", "", "project ID (default: git root of the current directory)")
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
		Hidden: true,
		Args:   cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.ErrOrStderr(), "warning: wd plan status is deprecated; use wd plan run, complete, archive or task status instead")
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
	cmd.Flags().String("project", "", "project ID (default: git root of the current directory)")
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

func newPlanDeleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <plan-id>",
		Short: "Permanently delete a plan",
		Long: "Permanently delete a pending or archived plan. This cannot be undone; use\n" +
			"`plan archive` for the reversible alternative. Consider `plan backup export`\n" +
			"first. In-progress plans (stop and archive first) and completed plans (archive\n" +
			"first) are refused. Any YAML replica in the repository is left untouched.\n" +
			"Asks for confirmation unless --yes is given; --json requires --yes.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			planID := args[0]
			yes, _ := cmd.Flags().GetBool("yes")
			jsonOut, _ := cmd.Flags().GetBool("json")
			if jsonOut && !yes {
				return errors.New("confirmation required (pass --yes with --json)")
			}
			c := clientFor(cmd)
			if !yes {
				p, err := c.PlansGet(cmd.Context(), planID)
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				fmt.Fprintf(out, "Plan:   %s (%s)\nStatus: %s\n", p.ID, p.Name, p.Status)
				if ts := p.TaskSummary; ts != nil {
					fmt.Fprintf(out, "Tasks:  %d total: %d done, %d in progress, %d pending, %d skipped\n",
						ts.Total, ts.Done, ts.InProgress, ts.Pending, ts.Skipped)
				}
				fmt.Fprintf(out, "Permanently delete plan %s (%q)? [y/N]: ", p.ID, p.Name)
				line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				if ans := strings.ToLower(strings.TrimSpace(line)); ans != "y" && ans != "yes" {
					fmt.Fprintln(out, "cancelled")
					return nil
				}
			}
			if err := c.PlansDelete(cmd.Context(), planID); err != nil {
				return err
			}
			if jsonOut {
				return printJSON(cmd.OutOrStdout(), map[string]string{"status": "deleted", "id": planID})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "plan %s deleted\n", planID)
			return nil
		},
	}
	cmd.Flags().BoolP("yes", "y", false, "skip the confirmation prompt")
	cmd.Flags().Bool("json", false, "output as JSON (requires --yes)")
	return cmd
}

func newPlanSyncToRepoCmd(use string, hidden bool) *cobra.Command {
	cmd := &cobra.Command{
		Use:    use + " <plan-id>",
		Hidden: hidden,
		Short:  "Export a plan revision to a dedicated branch and open a PR",
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
			if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
				return printJSON(cmd.OutOrStdout(), map[string]any{
					"output":      outPath,
					"plans":       len(bundle.Entries),
					"bundle_hash": bundle.BundleHash,
				})
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "exported %d plan(s) → %s (bundle_hash=%s)\n",
				len(bundle.Entries), outPath, bundle.BundleHash)
			return nil
		},
	}
	cmd.Flags().Bool("all", false, "export every plan (optionally scoped by --project)")
	cmd.Flags().String("project", "", "when used with --all, limit export to this project id")
	cmd.Flags().StringP("output", "o", "", "output file (default: stdout)")
	cmd.Flags().Bool("json", false, "with -o, print the export summary as JSON instead of text (the bundle itself is always JSON)")
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
			"Updates the plan's recorded task progress. Opt-in — never run automatically.\n" +
			"Plan ids resolve globally; --project is optional and rarely needed.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			planID := args[0]
			projectID, err := planProjectFlag(cmd)
			if err != nil {
				return err
			}
			p, err := clientFor(cmd).PlanAssess(cmd.Context(), projectID, planID)
			if err != nil {
				return err
			}
			if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
				return printJSON(cmd.OutOrStdout(), p)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "assess triggered for plan %s\n", planID)
			return nil
		},
	}
	cmd.Flags().String("project", "", "project ID (optional; plan ids resolve globally)")
	_ = cmd.Flags().MarkHidden("project")
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

func newPlanRunCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run <plan-id>",
		Short: "Start execution of a plan in the given mode",
		Long: "Start execution of a plan (pending → in_progress). This is the only supported\n" +
			"public start path for plan execution (including autopilot). --mode is\n" +
			"required; it decides how the plan is executed:\n\n" +
			"  autopilot            Creates a live Autopilot executor + manager\n" +
			"                       (pause/resume supported)\n" +
			"  pipeline             Each task becomes a pipeline job\n" +
			"                       (pause/resume supported)\n" +
			"  orchestrator_worker  Orchestrator + workers with human approval gates\n" +
			"                       (pause/resume refused; use stop)\n" +
			"  manual               Plan-bound general agent; human drives prompting\n" +
			"                       (pause/resume refused; use stop)\n\n" +
			"`orchestrator` is accepted as a shorthand for `orchestrator_worker`.\n" +
			"Follow progress with `wd plan show --watch`. `wd plan stop` works for every\n" +
			"mode; `wd plan pause|resume` only for autopilot and pipeline.",
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
	cmd.Flags().String("mode", "", "execution mode (required): autopilot|pipeline|orchestrator_worker|manual")
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

func newPlanControlCmd(action string) *cobra.Command {
	short, long := planControlHelp(action)
	cmd := &cobra.Command{
		Use:   action + " <plan-id>",
		Short: short,
		Long:  long,
		Args:  cobra.ExactArgs(1),
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

// newPlanRestartCmd builds `wd plan restart`.
func newPlanRestartCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "restart <plan-id>",
		Short: "Restart an in-progress plan's executor with fresh agents",
		Long: "Restart the executor of an in-progress plan with a brand-new set of agents,\n" +
			"keeping the work already done. Use it when a plan is stopped, degraded,\n" +
			"parked or stuck. This is destructive: the old agents are terminated and\n" +
			"their worktrees removed.\n\n" +
			"  autopilot     The manager and workers are terminated and their worktrees\n" +
			"                removed. The landed-task ledger and the plan's tasks are kept;\n" +
			"                branches with unmerged commits are kept, empty ones deleted.\n" +
			"                A new manager starts with a restart context (what is merged,\n" +
			"                what is unfinished); in-flight tasks are re-issued.\n" +
			"  pipeline      Running job agents are terminated and their worktrees removed.\n" +
			"                Done jobs and handoffs are kept; failed, skipped and unfinished\n" +
			"                jobs are reset and re-run with fresh agents. Branches with\n" +
			"                commits are kept as the base for the re-run.\n" +
			"  orchestrator_worker, manual\n" +
			"                Not supported; use `wd plan stop` and run a new path.\n\n" +
			"An executor that is still active, starting or paused is refused unless\n" +
			"--force is given. --backend picks the backend of the new autopilot manager.\n" +
			"Without --yes the command asks for confirmation and refuses when stdin is\n" +
			"not a terminal.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			force, _ := cmd.Flags().GetBool("force")
			backend, _ := cmd.Flags().GetString("backend")
			yes, _ := cmd.Flags().GetBool("yes")
			jsonOut, _ := cmd.Flags().GetBool("json")
			c := clientFor(cmd)
			if !yes {
				in := cmd.InOrStdin()
				if f, ok := in.(*os.File); ok && !isTTY(f) {
					return fmt.Errorf("refusing to restart plan %s without --yes in a non-interactive session", args[0])
				}
				p, err := c.PlansGet(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				out := cmd.ErrOrStderr()
				fmt.Fprintf(out, "Plan:   %s (%s)\nStatus: %s\nMode:   %s\n", p.ID, p.Name, p.Status, p.ExecutionMode)
				if e := p.Executor; e != nil {
					fmt.Fprintf(out, "Executor: %s %s (%s)\n", e.Kind, e.ID, e.State)
				}
				fmt.Fprintln(out, "Will terminate all agents of this plan's executor and remove their worktrees.")
				fmt.Fprintln(out, "Branches with unmerged commits are kept; empty task branches are deleted.")
				fmt.Fprintf(out, "Restart plan %s? [y/N]: ", p.ID)
				line, _ := bufio.NewReader(in).ReadString('\n')
				if ans := strings.ToLower(strings.TrimSpace(line)); ans != "y" && ans != "yes" {
					fmt.Fprintln(out, "cancelled")
					return nil
				}
			}
			p, err := c.PlansRestart(cmd.Context(), args[0], client.RestartPlanRequest{Force: force, Backend: backend})
			if err != nil {
				return err
			}
			if jsonOut {
				return printJSON(cmd.OutOrStdout(), p)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "plan %s restarted\n", p.ID)
			return nil
		},
	}
	cmd.Flags().Bool("force", false, "restart even when the executor is active, starting or paused")
	cmd.Flags().String("backend", "", "backend id for the new autopilot manager (autopilot only)")
	cmd.Flags().BoolP("yes", "y", false, "skip the confirmation prompt")
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

// planControlHelp returns the Short and Long help for pause, resume or stop.
// The per-executor behaviour mirrors internal/daemon/plan_control.go.
func planControlHelp(action string) (string, string) {
	const inProgressOnly = "Only plans that are in_progress and have an active executor can be controlled.\n"
	switch action {
	case "pause":
		return "Pause an in-progress plan's executor",
			"Pause the executor of an in-progress plan so no new work starts. The plan\n" +
				"stays in_progress; undo with `wd plan resume`.\n\n" +
				"  autopilot     The run is paused. Workers already running keep going.\n" +
				"  pipeline      No new jobs are started. Jobs already running keep going\n" +
				"                and may still finish or fail.\n" +
				"  plan-bound    Not supported (orchestrator and manual plans); use\n" +
				"  agent         `wd plan stop` instead.\n\n" + inProgressOnly
	case "resume":
		return "Resume a paused plan's executor",
			"Resume an executor paused with `wd plan pause`. The plan stays in_progress.\n\n" +
				"  autopilot     The paused run becomes active again (its brain is respawned\n" +
				"                if it was torn down).\n" +
				"  pipeline      Jobs that became ready while paused are started.\n" +
				"  plan-bound    Not supported (orchestrator and manual plans).\n" +
				"  agent\n\n" +
				"Resuming an executor that is not paused is refused. A stopped executor\n" +
				"cannot be resumed — use `wd plan restart` to bring it back.\n\n" + inProgressOnly
	default:
		return "Stop an in-progress plan's executor",
			"Stop the executor of an in-progress plan. The plan itself stays in_progress\n" +
				"(stop does not complete, archive or reset it).\n\n" +
				"  autopilot     The run is stopped and its brain is shut down. A stopped\n" +
				"                run cannot be resumed; use `wd plan restart`.\n" +
				"  pipeline      The pipeline is canceled: running jobs are terminated and\n" +
				"                unfinished jobs are skipped. It cannot be resumed; use\n" +
				"                `wd plan restart`.\n" +
				"  plan-bound    The agent is terminated.\n" +
				"  agent\n\n" +
				"A stopped plan is not re-run with `wd plan run` (that needs a pending plan).\n" +
				"Record the outcome with `wd plan task status`, then `wd plan complete`, or\n" +
				"`wd plan archive` to give up.\n\n" + inProgressOnly
	}
}

// planTaskStatuses are the task statuses the daemon accepts.
var planTaskStatuses = []string{"pending", "in_progress", "done", "skipped"}

func validPlanTaskStatus(status string) bool {
	for _, s := range planTaskStatuses {
		if s == status {
			return true
		}
	}
	return false
}

// runPlanTaskStatus is the shared code path for `plan task status` and
// `plan done`: validate client-side, fetch the old status, then update.
func runPlanTaskStatus(cmd *cobra.Command, planID, taskID, status string) error {
	if !validPlanTaskStatus(status) {
		return fmt.Errorf("invalid status %q (valid: %s)", status, strings.Join(planTaskStatuses, ", "))
	}
	c := clientFor(cmd)
	old := "unknown"
	if cur, err := c.PlansGet(cmd.Context(), planID); err == nil {
		old = "pending"
		if st := cur.TaskProgress[taskID]; st != "" {
			old = st
		}
	}
	p, err := c.PlansUpdateTaskStatus(cmd.Context(), planID, taskID, status)
	if err != nil {
		return err
	}
	if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
		return printJSON(cmd.OutOrStdout(), map[string]any{
			"plan_id":    p.ID,
			"task_id":    taskID,
			"old_status": old,
			"new_status": status,
			"plan":       p,
		})
	}
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "plan %s task %s: %s → %s\n", p.ID, taskID, old, status)
	if ts := p.TaskSummary; ts != nil {
		fmt.Fprintf(w, "tasks: %d/%d done (%d in_progress, %d pending, %d skipped)\n",
			ts.Done, ts.Total, ts.InProgress, ts.Pending, ts.Skipped)
	}
	return nil
}

func newPlanTaskStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status <plan-id> <task-id> <pending|in_progress|done|skipped>",
		Short: "Set a plan task's status",
		Long: "Set one task's progress status to pending, in_progress, done, or skipped.\n" +
			"Prints the task's old and new status plus the plan's task summary. Works on\n" +
			"plans in any lifecycle state. skipped counts as finished for `plan complete`.\n" +
			"`wd plan done <plan-id> <task-id>` is the shorthand for setting a task to done.",
		Args: cobra.ExactArgs(3),
		ValidArgsFunction: func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
			if len(args) == 2 {
				return planTaskStatuses, cobra.ShellCompDirectiveNoFileComp
			}
			return nil, cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPlanTaskStatus(cmd, args[0], args[1], args[2])
		},
	}
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

func newPlanDoneCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "done <plan-id> <task-id>",
		Short: "Mark a plan task done",
		Long: "Shorthand for `plan task status <plan-id> <task-id> done`. Updates the\n" +
			"task's progress on the plan.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPlanTaskStatus(cmd, args[0], args[1], "done")
		},
	}
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

// planTaskIDArg resolves the task id from a positional arg and/or --id.
func planTaskIDArg(cmd *cobra.Command, args []string, pos int) (string, error) {
	taskID, _ := cmd.Flags().GetString("id")
	if len(args) > pos {
		if taskID != "" && taskID != args[pos] {
			return "", fmt.Errorf("conflicting task id: flag %q vs arg %q", taskID, args[pos])
		}
		taskID = args[pos]
	}
	if taskID == "" {
		return "", fmt.Errorf("task id required (positional or --id)")
	}
	return taskID, nil
}

func newPlanCompleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "complete <plan-id>",
		Short: "Complete a plan (in_progress → completed)",
		Long: "Complete a plan: in_progress → completed. Blocked if any task is not\n" +
			"done or skipped (skipped counts as finished), or if any branch the plan's\n" +
			"work opened a PR for is still unmerged.\n\n" +
			"On success the daemon records an execution summary on the plan, tears down\n" +
			"its executor (autopilot run, pipeline and plan-bound agents), and removes\n" +
			"their worktrees and branches. The plan record itself is kept; PR references\n" +
			"and execution history are preserved. If cleanup only partly succeeds the\n" +
			"plan stays in_progress and the command can be run again.",
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

// planProjectFlag returns --project, defaulting to the launch dir's project id
// (git root; linked worktree -> parent repo root), same as `warden start`.
func planProjectFlag(cmd *cobra.Command) (string, error) {
	projectID, _ := cmd.Flags().GetString("project")
	if projectID != "" {
		return projectID, nil
	}
	return projectIDForDir("")
}

// projectIDForDir returns the project id to send for a launch from dir: the
// git repository root (a linked worktree maps to its parent repository root),
// or dir itself when it is not inside a git repository. Shared by agent spawn
// and pipeline create so both default --project identically. dir "" means cwd.
func projectIDForDir(dir string) (string, error) {
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("get working directory: %w", err)
		}
		dir = wd
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if st, err := os.Stat(abs); err == nil && !st.IsDir() {
		abs = filepath.Dir(abs) // a file path (e.g. a spec) resolves from its directory
	}
	// --git-common-dir is the main repo's .git for both the main checkout and any
	// linked worktree, so its parent is the repo root either way.
	out, err := exec.Command("git", "-C", abs, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err == nil {
		common := strings.TrimSpace(string(out))
		if filepath.Base(common) == ".git" {
			return filepath.Dir(common), nil
		}
	}
	if out, err := exec.Command("git", "-C", abs, "rev-parse", "--show-toplevel").Output(); err == nil {
		if top := strings.TrimSpace(string(out)); top != "" {
			return top, nil
		}
	}
	return abs, nil
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
	idPart, prompt, ok := strings.Cut(s, ":")
	idPart = strings.TrimSpace(idPart)
	prompt = strings.TrimSpace(prompt)
	if !ok || idPart == "" || prompt == "" {
		return client.PlanTaskSpec{}, fmt.Errorf("invalid --task %q (want id:prompt or id@dep1,dep2:prompt)", s)
	}
	id, afterRaw, hasAfter := strings.Cut(idPart, "@")
	id = strings.TrimSpace(id)
	if id == "" {
		return client.PlanTaskSpec{}, fmt.Errorf("invalid --task %q (empty id)", s)
	}
	var after []string
	if hasAfter {
		for _, dep := range strings.Split(afterRaw, ",") {
			dep = strings.TrimSpace(dep)
			if dep == "" {
				continue
			}
			after = append(after, dep)
		}
		if len(after) == 0 {
			return client.PlanTaskSpec{}, fmt.Errorf("invalid --task %q (empty after list after @)", s)
		}
	}
	return client.PlanTaskSpec{ID: id, Prompt: prompt, After: after}, nil
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
		fmt.Fprint(out, "After (comma-separated task ids, blank for none): ")
		afterLine, err := rd.ReadString('\n')
		afterLine = strings.TrimSpace(afterLine)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		var after []string
		if afterLine != "" {
			for _, dep := range strings.Split(afterLine, ",") {
				dep = strings.TrimSpace(dep)
				if dep != "" {
					after = append(after, dep)
				}
			}
		}
		tasks = append(tasks, client.PlanTaskSpec{ID: id, Prompt: prompt, After: after})
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
		return "", fmt.Errorf("--mode is required (autopilot|pipeline|orchestrator_worker|manual)")
	default:
		return "", fmt.Errorf("unknown --mode %q (autopilot|pipeline|orchestrator_worker|manual)", mode)
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

// printPlanExecutor renders the live executor block; nothing for plans without one.
func printPlanExecutor(w io.Writer, e *client.PlanExecutor) {
	if e == nil {
		return
	}
	fmt.Fprintf(w, "executor_state: %s (%s %s)\n", e.State, e.Kind, e.ID)
	if b := e.Backoff; b != nil {
		fmt.Fprintf(w, "backoff:        stage %d, next retry %s\n", b.Stage, b.NextRetryAt)
		if b.LastError != "" {
			fmt.Fprintf(w, "last_error:     %s\n", b.LastError)
		}
	}
	if e.IntegrationBranch != "" {
		fmt.Fprintf(w, "integration:    %s\n", e.IntegrationBranch)
	}
	if e.ManagerAgentID != "" {
		fmt.Fprintf(w, "manager:        %s\n", e.ManagerAgentID)
	}
	if e.RestartCount > 0 {
		fmt.Fprintf(w, "restarts:       %d (last: %s at %s)\n", e.RestartCount, e.LastRestartReason, e.LastRestartAt)
	}
	if len(e.Tasks) > 0 {
		fmt.Fprintln(w, "executor_tasks:")
		for _, t := range e.Tasks {
			line := fmt.Sprintf("  %s: %s", t.ID, t.State)
			if t.WorkerAgentID != "" {
				line += " worker=" + t.WorkerAgentID
			}
			if t.Branch != "" {
				line += " branch=" + t.Branch
			}
			if t.PR > 0 {
				line += fmt.Sprintf(" pr=#%d", t.PR)
			}
			fmt.Fprintln(w, line)
		}
	}
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
	printPlanExecutor(w, p.Executor)
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

// watchPlanShow redraws `plan show` on an interval until interrupted, with the
// same clear-and-home redraw as `wd stats --watch`. Redrawing only happens on a
// real terminal; piped output just appends each snapshot.
func watchPlanShow(cmd *cobra.Command, show func() error, jsonOut bool) error {
	out := cmd.OutOrStdout()
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	clearable := !jsonOut && isTTY(out)
	t := time.NewTicker(planWatchInterval)
	defer t.Stop()
	for {
		if clearable {
			fmt.Fprint(out, "\033[2J\033[H")
		}
		if err := show(); err != nil {
			// Ctrl-C (or the caller's deadline) landing mid-refresh is a normal
			// exit, not a failed fetch.
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

var planWatchInterval = 3 * time.Second

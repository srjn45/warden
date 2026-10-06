package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/pipeline"
)

// renderPipelineDetail formats a pipeline for `pipeline show`: the header plus,
// per job, its status + deps and (when present) the branch it worked on and the
// handoff output it emitted — so a finished pipeline's results are visible from
// the CLI even after its agents are gone.
func renderPipelineDetail(p *pipeline.Pipeline, allJobs bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s [%s] repo=%s\n", p.ID, p.Status, p.Repo)
	if p.ProjectID != "" {
		fmt.Fprintf(&b, "project: %s\n", p.ProjectID)
	}
	if p.PlanID != "" {
		fmt.Fprintf(&b, "plan: %s\n", p.PlanID)
	}
	for _, j := range p.Jobs {
		if j.IsSynthetic() && !allJobs {
			continue
		}
		depIDs := j.DependsOn
		if !allJobs {
			depIDs = realDependencies(p, j.DependsOn)
		}
		deps := ""
		if len(depIDs) > 0 {
			deps = fmt.Sprintf(" (depends: %v)", depIDs)
		}
		name := j.ID
		if j.IsSynthetic() {
			name += " [warden]"
		}
		fmt.Fprintf(&b, "  %-12s %-9s%s\n", name, j.Status, deps)
		if j.Branch != "" {
			fmt.Fprintf(&b, "      branch: %s\n", j.Branch)
		}
		if j.Output != "" {
			fmt.Fprintf(&b, "      output: %s\n", strings.ReplaceAll(j.Output, "\n", "\n              "))
		}
	}
	return b.String()
}

func newPipelineCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pipeline",
		Short: "Define and run DAG pipelines of agent jobs",
		Long: "Define and run DAG pipelines of agent jobs.\n\n" +
			"Use validate locally before create; template list shows built-in starters;\n" +
			"start/pause/resume/cancel control lifecycle; show lists per-job status and handoffs.",
	}
	SetCommandHelpMetadata(cmd, "run", 20, "warden pipeline", "", NodeNamespace)

	children := []*cobra.Command{
		newPipelineValidateCmd(), newPipelineCreateCmd(), newPipelineTemplateCmd(),
		newPipelineListCmd(), newPipelineShowCmd(),
		newPipelineStartCmd(), newPipelinePauseCmd(), newPipelineResumeCmd(),
		newPipelineCancelCmd(), newPipelineDeleteCmd(), newPipelineEmitCmd(),
		newPipelineEditJobCmd(), newPipelineRetryCmd(),
	}
	for i, child := range children {
		SetCommandHelpMetadata(child, "run", (i+1)*10, "warden pipeline "+child.Name(), "", nodeKind(child))
		cmd.AddCommand(child)
	}
	listTemplatesAlias := newPipelineListTemplatesCmd()
	markCompatibilityChild(listTemplatesAlias, "warden pipeline template list")
	cmd.AddCommand(listTemplatesAlias)
	return cmd
}

func newPipelineTemplateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "template",
		Short: "Built-in pipeline templates",
		Long:  "Built-in pipeline templates render via create --template and support placeholder substitution.",
	}
	SetCommandHelpMetadata(cmd, "run", 30, "warden pipeline template", "", NodeNamespace)
	list := canonicalPipelineCommand(newPipelineListTemplatesCmd(), "list")
	SetCommandHelpMetadata(list, "run", 10, "warden pipeline template list", "", NodeLeaf)
	cmd.AddCommand(list)
	return cmd
}

func canonicalPipelineCommand(cmd *cobra.Command, name string) *cobra.Command {
	parts := strings.SplitN(cmd.Use, " ", 2)
	legacyName := parts[0]
	rewritePipelineHelpPaths(cmd, legacyName, name)
	cmd.Use = name
	if len(parts) == 2 {
		cmd.Use += " " + parts[1]
	}
	cmd.Aliases = nil
	return cmd
}

func rewritePipelineHelpPaths(cmd *cobra.Command, legacyName, canonicalName string) {
	replacer := strings.NewReplacer(
		"warden pipeline "+legacyName, "warden pipeline template "+canonicalName,
		"wd pipeline "+legacyName, "wd pipeline template "+canonicalName,
		"pipeline "+legacyName, "pipeline template "+canonicalName,
	)
	cmd.Long = replacer.Replace(cmd.Long)
	cmd.Example = replacer.Replace(cmd.Example)
}

func newPipelineValidateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "validate -f <spec.yaml>",
		Short: "Validate a pipeline YAML spec without creating it",
		Long: "Parse and validate a pipeline spec locally — checks required fields, " +
			"job ids, dependency references, worktree/run_if values, and DAG cycles. " +
			"Exits 0 if valid, 1 if not (suitable for CI). Does not contact the daemon.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			file, _ := cmd.Flags().GetString("file")
			if file == "" {
				return fmt.Errorf("provide a spec with -f <spec.yaml>")
			}
			data, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			p, err := pipeline.ParseSpec(data)
			if err != nil {
				return fmt.Errorf("invalid pipeline %s: %w", file, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s is valid — pipeline %q, %d jobs\n", file, p.ID, userJobs(p))
			return nil
		},
	}
	cmd.Flags().StringP("file", "f", "", "path to the pipeline YAML spec")
	return cmd
}

func newPipelineCreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create (-f <spec.yaml> | --template <name>)",
		Short: "Create a pipeline from a YAML spec or a built-in template",
		Long: "Create a pipeline either from a YAML spec file (-f) or from a built-in\n" +
			"template (--template). Templates render with placeholder substitution: --name\n" +
			"fills {{NAME}} (default the template name), --repo fills {{REPO}} (default the\n" +
			"current directory), and each remaining {{KEY}} is filled with --set KEY=VALUE.\n" +
			"Run `warden pipeline template list` to see templates and their placeholders.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			file, _ := cmd.Flags().GetString("file")
			tmpl, _ := cmd.Flags().GetString("template")
			if (file == "") == (tmpl == "") {
				return fmt.Errorf("provide exactly one of -f <spec.yaml> or --template <name>")
			}

			var spec string
			if file != "" {
				data, err := os.ReadFile(file)
				if err != nil {
					return err
				}
				spec = string(data)
			} else {
				rendered, err := renderTemplateFromFlags(cmd, tmpl)
				if err != nil {
					return err
				}
				spec = rendered
			}

			projectID, _ := cmd.Flags().GetString("project")
			if projectID == "" {
				// A spec-authored project_id wins; only default when it has none.
				if parsed, perr := pipeline.ParseSpec([]byte(spec)); perr != nil || parsed.ProjectID == "" {
					base, _ := cmd.Flags().GetString("repo")
					if base == "" && file != "" {
						base = file
					}
					var derr error
					if projectID, derr = projectIDForDir(base); derr != nil {
						return derr
					}
				}
			}
			planID, _ := cmd.Flags().GetString("plan")
			p, err := clientFor(cmd).PipelineCreateWith(cmd.Context(), client.PipelineCreateParams{
				Spec: spec, ProjectID: projectID, PlanID: planID,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "created pipeline %s (%d jobs) — start it with `warden pipeline start %s`\n", p.ID, userJobs(p), p.ID)
			return nil
		},
	}
	cmd.Flags().StringP("file", "f", "", "path to the pipeline YAML spec")
	cmd.Flags().String("template", "", "built-in template `<NAME>` to render (see 'warden pipeline template list')")
	cmd.Flags().String("name", "", "pipeline name — fills {{NAME}} (default: the template name)")
	cmd.Flags().String("repo", "", "repo path — fills {{REPO}} (default: the current directory)")
	cmd.Flags().StringArray("set", nil, "fill a template placeholder, KEY=VALUE (repeatable)")
	cmd.Flags().String("project", "", "optional project id this pipeline joins; overrides YAML project_id (default: git root of --repo / the spec file / cwd, unless the YAML sets project_id)")
	cmd.Flags().String("plan", "", "optional planstore plan id in the same project; empty = planless pipeline")
	return cmd
}

// renderTemplateFromFlags builds the substitution map for a --template create
// from the --name/--repo/--set flags and renders the named template. NAME
// defaults to the template name and REPO to the current directory; any other
// placeholder must be supplied via --set.
func renderTemplateFromFlags(cmd *cobra.Command, tmpl string) (string, error) {
	name, _ := cmd.Flags().GetString("name")
	repo, _ := cmd.Flags().GetString("repo")
	sets, _ := cmd.Flags().GetStringArray("set")

	if name == "" {
		name = tmpl
	}
	if repo == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		repo = wd
	}
	vars := map[string]string{"NAME": name, "REPO": repo}
	for _, kv := range sets {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return "", fmt.Errorf("invalid --set %q (want KEY=VALUE)", kv)
		}
		vars[k] = v
	}
	return pipeline.RenderTemplate(tmpl, vars)
}

func newPipelineListTemplatesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list-templates",
		Short: "List the built-in pipeline templates and their placeholders",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, t := range pipeline.ListTemplates() {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\n    %s\n    placeholders: %s\n",
					t.Name, t.Description, strings.Join(t.Placeholders, ", "))
			}
			return nil
		},
	}
}

// pipelineStatuses is the set of pipeline statuses `--status` accepts.
var pipelineStatuses = []pipeline.Status{
	pipeline.StatusPending, pipeline.StatusRunning, pipeline.StatusPaused,
	pipeline.StatusDone, pipeline.StatusStalled, pipeline.StatusCanceled,
}

// parsePipelineStatuses validates --status values (repeatable or
// comma-separated) and returns them comma-joined for the daemon.
func parsePipelineStatuses(in []string) (string, error) {
	var out []string
	for _, raw := range in {
		for _, v := range strings.Split(raw, ",") {
			if v = strings.TrimSpace(v); v == "" {
				continue
			}
			ok := false
			for _, st := range pipelineStatuses {
				if string(st) == v {
					ok = true
					break
				}
			}
			if !ok {
				valid := make([]string, len(pipelineStatuses))
				for i, st := range pipelineStatuses {
					valid[i] = string(st)
				}
				return "", fmt.Errorf("invalid status %q (valid: %s)", v, strings.Join(valid, ", "))
			}
			out = append(out, v)
		}
	}
	return strings.Join(out, ","), nil
}

// pipelineListScope resolves which project `pipeline list` covers. all is true
// when every project is listed (--all, or the directory is in no project).
func pipelineListScope(cmd *cobra.Command) (projectID string, all bool, err error) {
	if a, _ := cmd.Flags().GetBool("all"); a {
		return "", true, nil
	}
	if p, _ := cmd.Flags().GetString("project"); p != "" {
		if st, serr := os.Stat(p); serr == nil && st.IsDir() {
			id, err := projectIDForDir(p)
			return id, false, err
		}
		return p, false, nil
	}
	id, err := projectIDForDir("")
	if err != nil {
		return "", true, nil
	}
	if _, serr := os.Stat(filepath.Join(id, ".git")); serr != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "not in a project; listing pipelines for all projects")
		return "", true, nil
	}
	return id, false, nil
}

func newPipelineListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List pipelines",
		Long: "List pipelines as a table. By default only the project of the current\n" +
			"directory is listed; use --all for every project or --project to pick one.\n" +
			"Outside any project all pipelines are listed.\n\n" +
			"Columns: NAME, STATUS, JOBS (finished/total of the jobs in the spec),\n" +
			"PROJECT (only with --all), PLAN (the plan it executes, or -) and\n" +
			"SCHEDULE (only when a pipeline was started by a schedule).",
		Example: "  warden pipeline list\n" +
			"  warden pipeline list --all --status running,paused\n" +
			"  warden pipeline list --project ~/dev/app --json",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			statusFlags, _ := cmd.Flags().GetStringSlice("status")
			status, err := parsePipelineStatuses(statusFlags)
			if err != nil {
				return err
			}
			projectID, all, err := pipelineListScope(cmd)
			if err != nil {
				return err
			}
			ps, err := clientFor(cmd).PipelineListFiltered(cmd.Context(), projectID, status)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
				if ps == nil {
					ps = []*pipeline.Pipeline{}
				}
				return printJSON(out, ps)
			}
			if len(ps) == 0 {
				printNoPipelines(out, projectID, status)
				return nil
			}
			return printPipelineTable(out, ps, all)
		},
	}
	cmd.Flags().Bool("all", false, "list pipelines from every project")
	cmd.Flags().String("project", "", "project `<id-or-path>` (default: the project of the current directory)")
	cmd.Flags().StringSlice("status", nil, "only pipelines in this status (repeatable or comma-separated: pending, running, paused, done, stalled, canceled)")
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

func printNoPipelines(w io.Writer, projectID, status string) {
	var filters []string
	if projectID != "" {
		filters = append(filters, "project "+projectID)
	}
	if status != "" {
		filters = append(filters, "status "+status)
	}
	if len(filters) > 0 {
		fmt.Fprintf(w, "no pipelines match %s\n", strings.Join(filters, " and "))
	} else {
		fmt.Fprintln(w, "no pipelines")
	}
	fmt.Fprintln(w, "hint: wd pipeline create <spec.yaml>  or  wd pipeline template list")
}

func printPipelineTable(w io.Writer, ps []*pipeline.Pipeline, all bool) error {
	hasSchedule := false
	for _, p := range ps {
		if p.ScheduleName != "" || p.ScheduleID != "" {
			hasSchedule = true
			break
		}
	}
	dash := func(v string) string {
		if v == "" {
			return "-"
		}
		return v
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	header := []string{"NAME", "STATUS", "JOBS"}
	if all {
		header = append(header, "PROJECT")
	}
	header = append(header, "PLAN")
	if hasSchedule {
		header = append(header, "SCHEDULE")
	}
	fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, p := range ps {
		total, done := p.UserJobCount()
		row := []string{p.Name, string(p.Status), fmt.Sprintf("%d/%d", done, total)}
		if row[0] == "" {
			row[0] = p.ID
		}
		if all {
			proj := ""
			if p.ProjectID != "" {
				proj = filepath.Base(p.ProjectID)
			}
			row = append(row, dash(proj))
		}
		row = append(row, dash(p.PlanID))
		if hasSchedule {
			sched := p.ScheduleName
			if sched == "" {
				sched = p.ScheduleID
			}
			row = append(row, dash(sched))
		}
		fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	return tw.Flush()
}

// realDependencies maps dependency ids onto user-authored jobs: a synthetic
// dependency is replaced by the real jobs behind it, or dropped when nothing
// real stands behind it (the root fan-out).
func realDependencies(p *pipeline.Pipeline, ids []string) []string {
	var out []string
	seen := map[string]bool{}
	var walk func(id string)
	walk = func(id string) {
		if seen[id] {
			return
		}
		seen[id] = true
		j := p.Job(id)
		if j == nil || !j.IsSynthetic() {
			out = append(out, id)
			return
		}
		for _, d := range j.DependsOn {
			walk(d)
		}
	}
	for _, id := range ids {
		walk(id)
	}
	return out
}

func newPipelineShowCmd() *cobra.Command {
	var allJobs bool
	cmd := &cobra.Command{
		Use:   "show <pipeline>",
		Short: "Show a pipeline's jobs and their status",
		Long: `Show a pipeline's jobs and their status.

Jobs warden adds on its own to fan work out and join it back are hidden by
default; pass --all-jobs to list them too, marked [warden].`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := clientFor(cmd).PipelineGet(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), renderPipelineDetail(p, allJobs))
			return nil
		},
	}
	cmd.Flags().BoolVar(&allJobs, "all-jobs", false, "also list jobs warden added itself (marked [warden])")
	return cmd
}

func newPipelineStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start <pipeline>",
		Short: "Start a pipeline (spawns jobs with no dependencies)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := clientFor(cmd).PipelineStart(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "started %s\n", args[0])
			return nil
		},
	}
}

func newPipelinePauseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pause <pipeline>",
		Short: "Pause a running pipeline (in-flight jobs finish; no new jobs spawn)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := clientFor(cmd).PipelinePause(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "paused %s\n", args[0])
			return nil
		},
	}
}

func newPipelineResumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume <pipeline>",
		Short: "Resume a paused pipeline (spawns jobs that became ready while paused)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := clientFor(cmd).PipelineResume(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "resumed %s\n", args[0])
			return nil
		},
	}
}

func newPipelineCancelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <pipeline>",
		Short: "Cancel a pipeline (terminates running jobs)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := clientFor(cmd).PipelineCancel(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "canceled %s\n", args[0])
			return nil
		},
	}
}

func newPipelineDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <pipeline>",
		Short: "Delete a pipeline's record (must not have live jobs — cancel first)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := clientFor(cmd).PipelineDelete(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted %s\n", args[0])
			return nil
		},
	}
}

func newPipelineEmitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "emit <text>",
		Short: "Publish this job's handoff (run from inside a pipeline job)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pid, _ := cmd.Flags().GetString("pipeline")
			job, _ := cmd.Flags().GetString("job")
			if pid == "" {
				pid = envID("PIPELINE_ID")
			}
			if job == "" {
				job = envID("JOB_ID")
			}
			if pid == "" || job == "" {
				return fmt.Errorf("no pipeline/job: run inside a pipeline job, or pass --pipeline and --job")
			}
			text := strings.Join(args, " ")
			if err := clientFor(cmd).PipelineEmit(cmd.Context(), pid, job, text); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "emitted handoff for %s/%s\n", pid, job)
			return nil
		},
	}
	cmd.Flags().String("pipeline", "", "pipeline id (defaults to $WARDEN_PIPELINE_ID)")
	cmd.Flags().String("job", "", "job id (defaults to $WARDEN_JOB_ID)")
	return cmd
}

func newPipelineEditJobCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "edit-job <pipeline> <job>",
		Short: "Edit a pending job's prompt and/or handoff",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var prompt, handoff *string
			if cmd.Flags().Changed("prompt") {
				v, _ := cmd.Flags().GetString("prompt")
				prompt = &v
			}
			if cmd.Flags().Changed("handoff") {
				v, _ := cmd.Flags().GetString("handoff")
				handoff = &v
			}
			if prompt == nil && handoff == nil {
				return fmt.Errorf("provide --prompt and/or --handoff")
			}
			if err := clientFor(cmd).PipelineEditJob(cmd.Context(), args[0], args[1], prompt, handoff); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "edited %s/%s\n", args[0], args[1])
			return nil
		},
	}
	cmd.Flags().String("prompt", "", "new prompt for the job")
	cmd.Flags().String("handoff", "", "new handoff hint for the job")
	return cmd
}

func newPipelineRetryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "retry <pipeline> <job>",
		Short: "Re-run a failed or needs-attention job (reopens skipped descendants)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := clientFor(cmd).PipelineRetry(cmd.Context(), args[0], args[1]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "retrying %s/%s\n", args[0], args[1])
			return nil
		},
	}
}

// userJobs counts the jobs a spec author wrote, excluding those warden injects.
func userJobs(p *pipeline.Pipeline) int {
	n, _ := p.UserJobCount()
	return n
}

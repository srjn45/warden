package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/srjn45/warden/internal/pipeline"
)

func newPipelineJobCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "job",
		Short: "Inspect, edit and retry one job of a pipeline",
		Long:  "Commands that act on a single job of a pipeline: show everything about it, edit a job that has not started, or retry one that failed.",
	}
	show, edit, retry := newPipelineJobShowCmd(), newPipelineJobEditCmd(), newPipelineJobRetryCmd()
	SetCommandHelpMetadata(show, "run", 10, "warden pipeline job show", "", NodeLeaf)
	SetCommandHelpMetadata(edit, "run", 20, "warden pipeline job edit", "", NodeLeaf)
	SetCommandHelpMetadata(retry, "run", 30, "warden pipeline job retry", "", NodeLeaf)
	cmd.AddCommand(show, edit, retry)
	return cmd
}

func newPipelineJobShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <pipeline> <job>",
		Short: "Show everything about one job",
		Long: `Show one job of a pipeline in full: its status, type, role, tier, backend and
model, worktree mode, whether it is supervised, when it runs, what it depends
on, the agent and session that ran it, its branch, how many times it was
retried automatically, and its complete prompt, handoff hint and output.

Jobs warden adds on its own to fan work out and join it back are shown too,
marked as created by warden. --json prints the job as JSON.

Examples:
  wd pipeline job show my-run build
  wd pipeline job show my-run build --json`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := clientFor(cmd).PipelineGet(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			var job *pipeline.Job
			for i := range p.Jobs {
				if p.Jobs[i].ID == args[1] {
					job = &p.Jobs[i]
					break
				}
			}
			if job == nil {
				return fmt.Errorf("pipeline %q has no job %q", args[0], args[1])
			}
			if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
				return printJSON(cmd.OutOrStdout(), job)
			}
			renderJobDetail(cmd.OutOrStdout(), p, job)
			return nil
		},
	}
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

func renderJobDetail(w io.Writer, p *pipeline.Pipeline, j *pipeline.Job) {
	id := j.ID
	if j.IsSynthetic() {
		id += " (created by warden)"
	}
	rows := [][2]string{
		{"pipeline", firstNonEmpty(p.Name, p.ID)},
		{"job", id},
		{"status", string(j.Status)},
		{"type", j.Type},
		{"role", j.Role},
		{"tier", j.Tier},
		{"backend", j.Backend},
		{"model", j.Model},
		{"worktree", j.Worktree},
		{"supervised", fmt.Sprint(j.Supervised)},
		{"run if", j.RunIf},
		{"after", strings.Join(j.DependsOn, ", ")},
		{"agent", j.AgentID},
		{"session", j.SessionID},
		{"branch", j.Branch},
		{"auto-retries", fmt.Sprint(j.AutoRetryCount)},
	}
	for _, r := range rows {
		fmt.Fprintf(w, "%-13s %s\n", r[0]+":", dashIfEmpty(r[1]))
	}
	for _, sec := range [][2]string{{"prompt", j.Prompt}, {"handoff", j.Handoff}, {"output", j.Output}} {
		fmt.Fprintf(w, "\n%s:\n", sec[0])
		if sec[1] == "" {
			fmt.Fprintln(w, "  -")
			continue
		}
		fmt.Fprintln(w, indentLines(sec[1], "  "))
	}
}

func newPipelineJobEditCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "edit <pipeline> <job>",
		Short: "Edit the prompt and/or handoff of a job that has not started",
		Long: `Change the prompt and/or handoff hint of a job before it starts. A job that
has already started cannot be edited.

Examples:
  wd pipeline job edit my-run build --prompt "build with -race"
  wd pipeline job edit my-run build --handoff "report the test count"`,
		Args: cobra.ExactArgs(2),
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
				return fmt.Errorf("nothing to edit: pass --prompt and/or --handoff")
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

func newPipelineJobRetryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "retry <pipeline> <job>",
		Short: "Re-run a failed or needs-attention job (reopens skipped descendants)",
		Long: `Re-run a job that failed or needs attention. Jobs downstream of it that were
skipped because it failed are reopened so the pipeline can carry on.

Example:
  wd pipeline job retry my-run build`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := clientFor(cmd).PipelineRetry(cmd.Context(), args[0], args[1]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "retrying %s/%s\n", args[0], args[1])
			return nil
		},
	}
}

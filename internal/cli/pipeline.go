package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func newPipelineCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pipeline",
		Short: "Define and run DAG pipelines of agent jobs",
	}
	cmd.AddCommand(newPipelineCreateCmd(), newPipelineListCmd(), newPipelineShowCmd(),
		newPipelineStartCmd(), newPipelineCancelCmd(), newPipelineEmitCmd())
	return cmd
}

func newPipelineCreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create -f <spec.yaml>",
		Short: "Create a pipeline from a YAML spec",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			file, _ := cmd.Flags().GetString("file")
			if file == "" {
				return fmt.Errorf("provide a spec with -f <spec.yaml>")
			}
			data, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			p, err := clientFor(cmd).PipelineCreate(cmd.Context(), string(data))
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "created pipeline %s (%d jobs) — start it with `agentctl pipeline start %s`\n", p.ID, len(p.Jobs), p.ID)
			return nil
		},
	}
	cmd.Flags().StringP("file", "f", "", "path to the pipeline YAML spec")
	return cmd
}

func newPipelineListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List pipelines",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ps, err := clientFor(cmd).PipelineList(cmd.Context())
			if err != nil {
				return err
			}
			for _, p := range ps {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%d jobs\n", p.ID, p.Status, len(p.Jobs))
			}
			return nil
		},
	}
}

func newPipelineShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <pipeline>",
		Short: "Show a pipeline's jobs and their status",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := clientFor(cmd).PipelineGet(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s [%s] repo=%s\n", p.ID, p.Status, p.Repo)
			for _, j := range p.Jobs {
				deps := ""
				if len(j.DependsOn) > 0 {
					deps = fmt.Sprintf(" (depends: %v)", j.DependsOn)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "  %-12s %-9s%s\n", j.ID, j.Status, deps)
			}
			return nil
		},
	}
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

func newPipelineEmitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "emit <text>",
		Short: "Publish this job's handoff (run from inside a pipeline job)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pid, _ := cmd.Flags().GetString("pipeline")
			job, _ := cmd.Flags().GetString("job")
			if pid == "" {
				pid = os.Getenv("AGENTCTL_PIPELINE_ID")
			}
			if job == "" {
				job = os.Getenv("AGENTCTL_JOB_ID")
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
	cmd.Flags().String("pipeline", "", "pipeline id (defaults to $AGENTCTL_PIPELINE_ID)")
	cmd.Flags().String("job", "", "job id (defaults to $AGENTCTL_JOB_ID)")
	return cmd
}

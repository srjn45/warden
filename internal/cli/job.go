package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/lifecycle"
)

// newJobCmd groups the commands a worker runs from *inside* its own pipeline job
// — today just `job done`, the A1 completion signal.
func newJobCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "job",
		Short: "Worker-side pipeline job commands (run from inside a job)",
		Long: "Commands a pipeline worker runs from inside its own job.\n\n" +
			"`warden job done` is the primary completion signal: a worker declares its\n" +
			"job complete with a status + one-line summary, so warden closes the job in\n" +
			"one shot without spending a follow-up interrogation turn.",
	}
	cmd.AddCommand(newJobDoneCmd())
	return cmd
}

// newJobDoneCmd is the primary, deterministic done-signal. It reads the ambient
// WARDEN_PIPELINE_ID / WARDEN_JOB_ID (injected into every pipeline-job session),
// or explicit --pipeline/--job flags, and records the worker's completion. Run
// outside a pipeline it degrades gracefully: it prints the equivalent
// `<<WARDEN_DONE>>{json}` sentinel line — the backstop warden watches — so a
// standalone worker can still declare completion, then exits 0.
func newJobDoneCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "done",
		Short: "Signal this pipeline job complete with a status + summary",
		Long: "Declare the current pipeline job complete.\n\n" +
			"Warden captures the status and summary in one shot — no extra turn. Run\n" +
			"from inside a pipeline job (WARDEN_PIPELINE_ID / WARDEN_JOB_ID are set\n" +
			"automatically), or pass --pipeline and --job explicitly. Outside a pipeline\n" +
			"it prints a <<WARDEN_DONE>> sentinel line and exits.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			summary, _ := cmd.Flags().GetString("summary")
			status, _ := cmd.Flags().GetString("status")
			status = lifecycle.NormalizeDoneStatus(status)

			pid, _ := cmd.Flags().GetString("pipeline")
			job, _ := cmd.Flags().GetString("job")
			if pid == "" {
				pid = envID("PIPELINE_ID")
			}
			if job == "" {
				job = envID("JOB_ID")
			}

			// Not inside a pipeline job: degrade gracefully by emitting the sentinel
			// line to stdout. Any watching lifecycle captures it (backstop path); a
			// truly standalone run simply records the declaration in its transcript.
			if pid == "" || job == "" {
				line, _ := json.Marshal(lifecycle.DoneSignal{Status: status, Summary: summary})
				fmt.Fprintf(cmd.OutOrStdout(), "%s%s\n", lifecycle.DoneSentinel, line)
				return nil
			}

			if err := clientFor(cmd).PipelineJobDone(cmd.Context(), pid, job, status, summary); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "done %s/%s (%s)\n", pid, job, status)
			return nil
		},
	}
	cmd.Flags().String("summary", "", "one-line summary of what the job accomplished")
	cmd.Flags().String("status", "success", "job outcome: success | failure | blocked")
	cmd.Flags().String("pipeline", "", "pipeline id (defaults to $WARDEN_PIPELINE_ID)")
	cmd.Flags().String("job", "", "job id (defaults to $WARDEN_JOB_ID)")
	return cmd
}

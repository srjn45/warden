package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/tui"
)

func newTUICmd() *cobra.Command {
	var pane, detailPane, pipelineID, jobID string
	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Live terminal cockpit for agents",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a := clientFor(cmd)
			switch pane {
			case "list":
				return tui.RunListPane(a, detailPane)
			case "jobdetail":
				return tui.RunJobDetailPane(a, pipelineID, jobID)
			case "":
				return runCockpit(a)
			default:
				return fmt.Errorf("unknown --pane %q (want list or jobdetail)", pane)
			}
		},
	}
	cmd.Flags().StringVar(&pane, "pane", "", "internal: render a single cockpit pane (list, jobdetail)")
	cmd.Flags().StringVar(&detailPane, "detail-pane", "", "internal: tmux id of the detail pane the list drives")
	cmd.Flags().StringVar(&pipelineID, "pipeline", "", "internal: pipeline id for --pane=jobdetail")
	cmd.Flags().StringVar(&jobID, "job", "", "internal: job id for --pane=jobdetail")
	for _, f := range []string{"pane", "detail-pane", "pipeline", "job"} {
		_ = cmd.Flags().MarkHidden(f)
	}
	return cmd
}

// runCockpit builds the tmux cockpit for this process and attaches to it. The
// list pane runs in the launching shell's directory so agents spawned from it
// (`n`) launch in that dir.
func runCockpit(a *client.Client) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate warden binary: %w", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working dir: %w", err)
	}
	return tui.RunCockpit(a, self, cwd)
}

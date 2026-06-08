package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/srajanpathak/agentctl/internal/client"
	"github.com/srajanpathak/agentctl/internal/tui"
)

func newTUICmd() *cobra.Command {
	var classic bool
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
				return runCockpitOrClassic(a, classic)
			default:
				return fmt.Errorf("unknown --pane %q (want list or jobdetail)", pane)
			}
		},
	}
	cmd.Flags().BoolVar(&classic, "classic", false, "use the legacy single-pane TUI (no tmux)")
	cmd.Flags().StringVar(&pane, "pane", "", "internal: render a single cockpit pane (list, jobdetail)")
	cmd.Flags().StringVar(&detailPane, "detail-pane", "", "internal: tmux id of the detail pane the list drives")
	cmd.Flags().StringVar(&pipelineID, "pipeline", "", "internal: pipeline id for --pane=jobdetail")
	cmd.Flags().StringVar(&jobID, "job", "", "internal: job id for --pane=jobdetail")
	for _, f := range []string{"pane", "detail-pane", "pipeline", "job"} {
		_ = cmd.Flags().MarkHidden(f)
	}
	return cmd
}

// runCockpitOrClassic launches the tmux cockpit, or the legacy single-pane TUI
// when --classic is set, tmux is unavailable, or we are already inside tmux.
func runCockpitOrClassic(a *client.Client, classic bool) error {
	if tui.ChooseClassic(classic, tui.TmuxAvailable(), os.Getenv("TMUX") != "") {
		return tui.Run(a)
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate agentctl binary: %w", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working dir: %w", err)
	}
	return tui.RunCockpit(a, self, cwd)
}

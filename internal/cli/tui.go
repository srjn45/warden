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
	var pane, detailPane string
	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Live terminal cockpit for agents",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a := clientFor(cmd)
			switch pane {
			case "list":
				return tui.RunListPane(a, detailPane)
			case "":
				return runCockpitOrClassic(a, classic)
			default:
				return fmt.Errorf("unknown --pane %q (want list)", pane)
			}
		},
	}
	cmd.Flags().BoolVar(&classic, "classic", false, "use the legacy single-pane TUI (no tmux)")
	cmd.Flags().StringVar(&pane, "pane", "", "internal: render a single cockpit pane (list)")
	cmd.Flags().StringVar(&detailPane, "detail-pane", "", "internal: tmux id of the detail pane the list drives")
	_ = cmd.Flags().MarkHidden("pane")
	_ = cmd.Flags().MarkHidden("detail-pane")
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

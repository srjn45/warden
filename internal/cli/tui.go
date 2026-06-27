package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/config"
	"github.com/srjn45/warden/internal/tui"
)

func newTUICmd() *cobra.Command {
	var pane, detailPane, pipelineID, jobID, agentID string
	var repl bool
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
			case "agentdetail":
				return tui.RunAgentDetailPane(a, agentID)
			case "":
				return runCockpit(a, cockpitUsesRepl(cmd, repl))
			default:
				return fmt.Errorf("unknown --pane %q (want list, jobdetail, or agentdetail)", pane)
			}
		},
	}
	cmd.Flags().BoolVar(&repl, "repl", false, "run the REPL (wd repl) in the master pane instead of a shell (default: repl config setting)")
	cmd.Flags().StringVar(&pane, "pane", "", "internal: render a single cockpit pane (list, jobdetail, agentdetail)")
	cmd.Flags().StringVar(&detailPane, "detail-pane", "", "internal: tmux id of the detail pane the list drives")
	cmd.Flags().StringVar(&pipelineID, "pipeline", "", "internal: pipeline id for --pane=jobdetail")
	cmd.Flags().StringVar(&jobID, "job", "", "internal: job id for --pane=jobdetail")
	cmd.Flags().StringVar(&agentID, "agent", "", "internal: agent id for --pane=agentdetail")
	for _, f := range []string{"pane", "detail-pane", "pipeline", "job", "agent"} {
		_ = cmd.Flags().MarkHidden(f)
	}
	return cmd
}

// cockpitUsesRepl decides which cockpit flavor to launch: the master pane runs
// the REPL when --repl is passed explicitly, otherwise it follows the `repl`
// config setting. An explicit flag always wins so both flavors stay launchable
// regardless of the default.
func cockpitUsesRepl(cmd *cobra.Command, flagVal bool) bool {
	if f := cmd.Flags().Lookup("repl"); f != nil && f.Changed {
		return flagVal
	}
	return config.Load(configPathFor(cmd)).GetRepl()
}

// runCockpit builds the tmux cockpit for this process and attaches to it. The
// list pane runs in the launching shell's directory so agents spawned from it
// (`n`) launch in that dir. useRepl selects the flavor whose master pane runs
// `wd repl` rather than a plain shell.
func runCockpit(a *client.Client, useRepl bool) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate warden binary: %w", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working dir: %w", err)
	}
	return tui.RunCockpit(a, self, cwd, useRepl)
}

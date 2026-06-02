package cli

import (
	"github.com/spf13/cobra"
	"github.com/srajanpathak/agentctl/internal/tui"
)

func newTUICmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tui",
		Short: "Live terminal cockpit for agents",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return tui.Run(clientFor(cmd))
		},
	}
}

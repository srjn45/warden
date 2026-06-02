package cli

import (
	"context"

	"github.com/spf13/cobra"
	"github.com/srajanpathak/agentctl/internal/tui"
)

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "agentctl",
		Short:         "Spawn, monitor, and tear down per-ticket Claude Code agent sessions",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().String("addr", "", "daemon address (overrides AGENTCTL_ADDR)")
	root.AddCommand(newDaemonCmd())
	root.AddCommand(newLsCmd(), newStatusCmd())
	root.AddCommand(newStartCmd(), newDoneCmd(), newAttachCmd())
	root.AddCommand(newSendCmd(), newTailCmd())
	root.AddCommand(newMCPCmd())
	root.AddCommand(newTUICmd())
	root.Args = cobra.NoArgs
	root.RunE = func(cmd *cobra.Command, args []string) error {
		return tui.Run(clientFor(cmd))
	}
	return root
}

// Execute is the single entrypoint for the binary.
func Execute() error {
	return newRootCmd().ExecuteContext(context.Background())
}

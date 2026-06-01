package cli

import (
	"github.com/spf13/cobra"
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
	return root
}

// Execute is the single entrypoint for the binary.
func Execute() error {
	return newRootCmd().Execute()
}

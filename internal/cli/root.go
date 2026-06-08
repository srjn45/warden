package cli

import (
	"context"

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
	root.AddCommand(newLsCmd(), newStatusCmd(), newDigestCmd())
	root.AddCommand(newStartCmd(), newTerminateCmd(), newDeleteCmd(), newRemoveWorktreeCmd(), newDoneCmd(), newRestoreCmd(), newAttachCmd(), newAdoptCmd())
	root.AddCommand(newSendCmd(), newTailCmd())
	root.AddCommand(newApprovalsCmd(), newApproveCmd(), newRotateCmd())
	root.AddCommand(newCtxCmd())
	root.AddCommand(newMsgCmd())
	root.AddCommand(newPipelineCmd())
	root.AddCommand(newMCPCmd())
	root.AddCommand(newTUICmd())
	root.AddCommand(newDoctorCmd())
	root.Args = cobra.NoArgs
	root.RunE = func(cmd *cobra.Command, args []string) error {
		return runCockpitOrClassic(clientFor(cmd), false)
	}
	return root
}

// Execute is the single entrypoint for the binary.
func Execute() error {
	return newRootCmd().ExecuteContext(context.Background())
}

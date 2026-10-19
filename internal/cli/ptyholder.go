package cli

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/srjn45/warden/internal/ptyholder"
)

// newPTYHolderCmd is the PTY holder process: the daemon re-execs the warden
// binary with this hidden subcommand to own one session's PTY master, so the
// shell survives the daemon. It is not a user command.
func newPTYHolderCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "ptyholder <spec-file>",
		Short:  "internal: own one PTY session's master (started by the daemon)",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var ready *os.File
			if os.Getenv("WARDEN_PTYHOLDER_READY_FD") == "3" {
				ready = os.NewFile(3, "ready")
			}
			cmd.SilenceUsage = true
			return ptyholder.Run(args[0], ready)
		},
	}
}

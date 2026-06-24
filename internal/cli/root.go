package cli

import (
	"context"

	"github.com/spf13/cobra"
)

// banner is the ASCII wordmark shown at the top of `warden --help`.
const banner = `                       _
__      ____ _ _ __ __| | ___ _ __
\ \ /\ / / _` + "`" + ` | '__/ _` + "`" + ` |/ _ \ '_ \
 \ V  V / (_| | | | (_| |  __/ | | |
  \_/\_/ \__,_|_|  \__,_|\___|_| |_|`

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "warden",
		Short:         "warden — spawn, monitor, and tear down per-ticket Claude Code agent sessions (alias: wd)",
		Long:          banner + "\n\nspawn, monitor, and tear down Claude Code agent sessions.\nRun `warden` with no arguments to open the cockpit TUI. Alias: wd.",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	// `warden --version` prints the full build info (commit/date/go/platform),
	// the same block as `warden version`.
	root.SetVersionTemplate(currentBuildInfo().String() + "\n")
	root.PersistentFlags().String("addr", "", "daemon address (overrides the addr config setting)")
	root.PersistentFlags().String("config", "", "config file path (default ~/.warden/config.yaml)")
	root.AddCommand(newDaemonCmd())
	root.AddCommand(newConfigCmd())
	root.AddCommand(newLsCmd(), newStatusCmd(), newDigestCmd(), newStatsCmd())
	root.AddCommand(newStartCmd(), newTerminateCmd(), newDeleteCmd(), newRemoveWorktreeCmd(), newDoneCmd(), newRestoreCmd(), newAttachCmd(), newAdoptCmd())
	root.AddCommand(newWorktreeCmd(), newPruneCmd())
	root.AddCommand(newSendCmd(), newTailCmd())
	root.AddCommand(newApprovalsCmd(), newApproveCmd(), newAutoApproveCmd(), newSetPermissionModeCmd(), newRotateCmd())
	root.AddCommand(newTokenCmd())
	root.AddCommand(newHookCmd())
	root.AddCommand(newCtxCmd())
	root.AddCommand(newMsgCmd())
	root.AddCommand(newCollabCmd())
	root.AddCommand(newPipelineCmd())
	root.AddCommand(newMCPCmd())
	root.AddCommand(newTUICmd())
	root.AddCommand(newDoctorCmd())
	root.AddCommand(newCompletionCmd())
	root.AddCommand(newVersionCmd())
	root.Args = cobra.NoArgs
	root.RunE = func(cmd *cobra.Command, args []string) error {
		return runCockpit(clientFor(cmd))
	}
	return root
}

// Execute is the single entrypoint for the binary.
func Execute() error {
	return newRootCmd().ExecuteContext(context.Background())
}

package cli

import (
	"context"

	"github.com/spf13/cobra"
)

// version is the warden version string. It defaults to "dev" for source builds
// and is overridden at release time via the linker:
//
//	-ldflags "-X github.com/srjn45/warden/internal/cli.version=<tag>"
//
// (goreleaser sets this automatically from the git tag.)
var version = "dev"

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
	root.PersistentFlags().String("addr", "", "daemon address (overrides WARDEN_ADDR)")
	root.AddCommand(newDaemonCmd())
	root.AddCommand(newLsCmd(), newStatusCmd(), newDigestCmd(), newStatsCmd())
	root.AddCommand(newStartCmd(), newTerminateCmd(), newDeleteCmd(), newRemoveWorktreeCmd(), newDoneCmd(), newRestoreCmd(), newAttachCmd(), newAdoptCmd())
	root.AddCommand(newSendCmd(), newTailCmd())
	root.AddCommand(newApprovalsCmd(), newApproveCmd(), newAutoApproveCmd(), newRotateCmd())
	root.AddCommand(newCtxCmd())
	root.AddCommand(newMsgCmd())
	root.AddCommand(newPipelineCmd())
	root.AddCommand(newMCPCmd())
	root.AddCommand(newTUICmd())
	root.AddCommand(newDoctorCmd())
	root.AddCommand(newCompletionCmd())
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

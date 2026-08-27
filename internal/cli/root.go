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
		// First-run nudge: a single, non-blocking hint toward `wd tutorial`,
		// emitted to stderr before any command runs. maybeHintTutorial gates
		// itself on a missing marker + interactive TTY + the `tutorial` config
		// setting, and stays silent for machine/full-screen commands — so
		// automation, pipes, and the daemon/MCP surfaces are never touched.
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			maybeHintTutorial(cmd)
		},
	}
	// `warden --version` prints the full build info (commit/date/go/platform),
	// the same block as `warden version`.
	root.SetVersionTemplate(currentBuildInfo().String() + "\n")
	root.PersistentFlags().String("addr", "", "daemon address (overrides the addr config setting)")
	root.PersistentFlags().String("config", "", "config file path (default ~/.warden/config.yaml)")
	root.AddCommand(newDaemonCmd())
	root.AddCommand(newConfigCmd())
	root.AddCommand(newPresetCmd())
	root.AddCommand(newPromptTemplateCmd())
	root.AddCommand(newLibraryCmd())
	root.AddCommand(newCostCmd())
	root.AddCommand(newLsCmd(), newStatusCmd(), newDigestCmd(), newStatsCmd(), newInsightsCmd(), newSavingsCmd(), newSpendCmd())
	root.AddCommand(newSearchCmd(), newHistoryCmd())
	root.AddCommand(newAuditCmd())
	root.AddCommand(newExportCmd(), newImportCmd())
	root.AddCommand(newStartCmd(), newForkCmd(), newStopCmd(), newTerminateCmd(), newDeleteCmd(), newRemoveWorktreeCmd(), newDoneCmd(), newRestoreCmd(), newAttachCmd(), newAdoptCmd(), newRecoverCmd())
	root.AddCommand(newWorktreeCmd(), newPruneCmd())
	root.AddCommand(newSendCmd(), newTailCmd())
	root.AddCommand(newCommitCmd(), newPushCmd(), newSyncCmd(), newCheckCmd(), newReviewCmd(), newModelsCmd())
	root.AddCommand(newBackendsCmd())
	root.AddCommand(newMemoryCmd())
	root.AddCommand(newSnapshotCmd())
	root.AddCommand(newPluginCmd())
	root.AddCommand(newApprovalsCmd(), newApproveCmd(), newAutoApproveCmd(), newForceCompactCmd(), newSetPermissionModeCmd(), newSetRoleCmd(), newRoleCmd(), newRotateCmd(), newHandoffCmd(), newSwitchCmd())
	root.AddCommand(newTokenCmd())
	root.AddCommand(newHookCmd())
	root.AddCommand(newCtxCmd())
	root.AddCommand(newMsgCmd())
	root.AddCommand(newCollabCmd())
	root.AddCommand(newCollaborateCmd())
	root.AddCommand(newBranchesCmd())
	root.AddCommand(newPipelineCmd())
	root.AddCommand(newJobCmd())
	root.AddCommand(newScheduleCmd())
	root.AddCommand(newAutopilotCmd())
	root.AddCommand(newLandCmd())
	root.AddCommand(newMCPCmd())
	root.AddCommand(newTUICmd())
	root.AddCommand(newReplCmd())
	root.AddCommand(newLLMCmd())
	root.AddCommand(newDoctorCmd())
	root.AddCommand(newSetupCmd())
	root.AddCommand(newTutorialCmd())
	root.AddCommand(newCompletionCmd())
	root.AddCommand(newVersionCmd())
	root.Args = cobra.NoArgs
	var rootTmuxNative bool
	root.Flags().BoolVar(&rootTmuxNative, "tmux-native", false, "lay the cockpit out as a native tmux window in the current session instead of a nested tmux (auto-enabled when launched inside tmux; requires $TMUX)")
	root.RunE = func(cmd *cobra.Command, args []string) error {
		return runCockpit(clientFor(cmd), cockpitTmuxNative(cmd, rootTmuxNative))
	}
	return root
}

// Execute is the single entrypoint for the binary.
func Execute() error {
	return newRootCmd().ExecuteContext(context.Background())
}

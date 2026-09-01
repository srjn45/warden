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
	root.AddCommand(newAgentCmd())
	root.AddCommand(newScheduleCmd())
	root.AddCommand(newConfigCmd())
	root.AddCommand(newDaemonCmd())
	root.AddCommand(newPresetCmd())
	root.AddCommand(newPromptTemplateCmd())
	root.AddCommand(newLibraryCmd())
	root.AddCommand(newCostCmd())
	lsCmd, statusCmd := newLsCmd(), newStatusCmd()
	markPermanentAgentShortcut(lsCmd, "warden agent list")
	markPermanentAgentShortcut(statusCmd, "warden agent status")
	digestCmd := newDigestCmd()
	markCompatibilityCommand(digestCmd, "warden agent digest")
	root.AddCommand(lsCmd, statusCmd, digestCmd, newStatsCmd(), newInsightsCmd(), newSavingsCmd(), newSpendCmd())
	root.AddCommand(newSearchCmd(), newHistoryCmd())
	root.AddCommand(newAuditCmd())
	root.AddCommand(newExportCmd(), newImportCmd())
	startCmd := newStartCmd()
	markPermanentAgentShortcut(startCmd, "warden agent start")
	root.AddCommand(startCmd)
	for _, legacy := range []struct {
		cmd       *cobra.Command
		canonical string
	}{
		{newForkCmd(), "warden agent fork"}, {newStopCmd(), "warden agent stop"},
		{newTerminateCmd(), "warden agent terminate"}, {newDeleteCmd(), "warden agent delete"},
		{newRemoveWorktreeCmd(), "warden agent remove-worktree"}, {newDoneCmd(), "warden agent done"},
		{newRestoreCmd(), "warden agent restore"}, {newAttachCmd(), "warden agent attach"},
		{newAdoptCmd(), "warden agent adopt"}, {newRecoverCmd(), "warden agent recover"},
	} {
		markCompatibilityCommand(legacy.cmd, legacy.canonical)
		root.AddCommand(legacy.cmd)
	}
	root.AddCommand(newWorktreeCmd(), newPruneCmd())
	sendCmd := newSendCmd()
	markPermanentAgentShortcut(sendCmd, "warden agent send")
	tailCmd := newTailCmd()
	markCompatibilityCommand(tailCmd, "warden agent tail")
	root.AddCommand(sendCmd, tailCmd)
	root.AddCommand(newGitCmd(), newCheckNamespaceCmd())
	commitCmd, pushCmd, syncCmd := newCommitCmd(), newPushCmd(), newSyncCmd()
	markPermanentGitShortcut(commitCmd, "warden git commit")
	markPermanentGitShortcut(pushCmd, "warden git push")
	markPermanentGitShortcut(syncCmd, "warden git sync")
	root.AddCommand(commitCmd, pushCmd, syncCmd)
	reviewCmd := newReviewCmd()
	markCompatibilityCommand(reviewCmd, "warden git review")
	root.AddCommand(reviewCmd, newModelsCmd())
	root.AddCommand(newBackendsCmd())
	root.AddCommand(newUsageCmd())
	root.AddCommand(newMemoryCmd())
	root.AddCommand(newSnapshotCmd())
	root.AddCommand(newPluginCmd())
	root.AddCommand(newApprovalsCmd(), newApproveCmd(), newAutoApproveCmd())
	for _, legacy := range []struct {
		cmd       *cobra.Command
		canonical string
	}{
		{newForceCompactCmd(), "warden agent compact set"},
		{newSetPermissionModeCmd(), "warden agent permission-mode set"},
		{newSetRoleCmd(), "warden agent role set"}, {newRoleCmd(), "warden agent role"},
		{newRotateCmd(), "warden agent rotate"}, {newHandoffCmd(), "warden agent handoff"},
		{newSwitchCmd(), "warden agent switch"},
	} {
		markCompatibilityCommand(legacy.cmd, legacy.canonical)
		root.AddCommand(legacy.cmd)
	}
	tokenAlias := newTokenCmd()
	markCompatibilityCommand(tokenAlias, "warden daemon token")
	root.AddCommand(tokenAlias)
	root.AddCommand(newHookCmd())
	root.AddCommand(newCtxCmd())
	root.AddCommand(newMsgCmd())
	root.AddCommand(newCollabCmd())
	root.AddCommand(newBranchesCmd())
	root.AddCommand(newPipelineCmd())
	root.AddCommand(newAutopilotCmd())
	root.AddCommand(newLandCmd())
	mcpAlias := newMCPCmd()
	markCompatibilityCommand(mcpAlias, "warden daemon mcp")
	root.AddCommand(mcpAlias)
	tuiCmd := newTUICmd()
	markEntryPoint(tuiCmd, 40)
	root.AddCommand(tuiCmd)
	root.AddCommand(newReplCmd())
	root.AddCommand(newLLMCmd())
	doctorCmd := newDoctorCmd()
	markEntryPoint(doctorCmd, 30)
	root.AddCommand(doctorCmd)
	root.AddCommand(newRepairCmd())
	setupCmd := newSetupCmd()
	markEntryPoint(setupCmd, 10)
	root.AddCommand(setupCmd)
	tutorialCmd := newTutorialCmd()
	markEntryPoint(tutorialCmd, 20)
	root.AddCommand(tutorialCmd)
	root.AddCommand(newCompletionCmd())
	versionCmd := newVersionCmd()
	markEntryPoint(versionCmd, 60)
	root.AddCommand(versionCmd)
	if err := installCommandHelp(root); err != nil {
		panic(err)
	}
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

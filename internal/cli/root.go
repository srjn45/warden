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
	root.AddCommand(newProjectCmd(), newWorkspaceCmd())
	root.AddCommand(newBackendCmd())
	root.AddCommand(newUsageNamespaceCmd())
	root.AddCommand(newInspectCmd())
	root.AddCommand(newScheduleCmd())
	root.AddCommand(newConfigCmd())
	root.AddCommand(newDaemonCmd())
	costCmd := newCostCmd()
	markCompatibilityCommand(costCmd, "warden cost")
	for _, child := range costCmd.Commands() {
		SetCommandHelpMetadata(child, "observe", 900, "warden usage "+child.Name(), AliasCompatibility, nodeKind(child))
	}
	root.AddCommand(costCmd)
	lsCmd, statusCmd := newLsCmd(), newStatusCmd()
	markPermanentAgentShortcut(lsCmd, "warden agent list")
	markPermanentAgentShortcut(statusCmd, "warden agent status")
	digestCmd := newDigestCmd()
	markCompatibilityCommand(digestCmd, "warden agent digest")
	root.AddCommand(lsCmd, statusCmd, digestCmd)
	for _, legacy := range []struct {
		cmd       *cobra.Command
		canonical string
	}{
		{newStatsCmd(), "warden inspect resources"},
		{newInsightsCmd(), "warden usage insights"},
		{newSavingsCmd(), "warden usage savings"},
		{newSpendCmd(), "warden usage spend"},
	} {
		markCompatibilityCommand(legacy.cmd, legacy.canonical)
		root.AddCommand(legacy.cmd)
	}
	for _, legacy := range []struct {
		cmd       *cobra.Command
		canonical string
	}{
		{newSearchCmd(), "warden inspect search"},
		{newHistoryCmd(), "warden inspect history"},
		{newExportCmd(), "warden inspect export"},
		{newImportCmd(), "warden inspect import"},
	} {
		markCompatibilityCommand(legacy.cmd, legacy.canonical)
		root.AddCommand(legacy.cmd)
	}
	auditCmd := newAuditCmd()
	markCompatibilityCommand(auditCmd, "warden inspect audit")
	for _, child := range auditCmd.Commands() {
		if child.Annotations == nil {
			child.Annotations = map[string]string{}
		}
		child.Annotations[AnnotationCanonicalPath] = "warden inspect audit"
	}
	root.AddCommand(auditCmd)
	repairCmd := newRepairCmd()
	markCompatibilityCommand(repairCmd, "warden inspect repair")
	root.AddCommand(repairCmd)
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
	for _, legacy := range []struct {
		cmd       *cobra.Command
		canonical string
		mark      func(*cobra.Command, string)
	}{
		{newMemoryCmd(), "warden project memory", markCompatibilityCommand},
		{newPresetCmd(), "warden project preset", markCompatibilityCommand},
		{newPromptTemplateCmd(), "warden project prompt-template", markCompatibilityCommand},
		{newLibraryCmd(), "warden project library", func(cmd *cobra.Command, _ string) { markProjectLibraryCompatibility(cmd) }},
		{newPluginCmd(), "warden project plugin", markCompatibilityCommand},
		{newWorktreeCmd(), "warden workspace", markCompatibilityCommand},
		{newPruneCmd(), "warden workspace prune", markCompatibilityCommand},
		{newSnapshotCmd(), "warden workspace snapshot", markCompatibilityCommand},
		{newBranchesCmd(), "warden workspace branches", markCompatibilityCommand},
		{newCollabCmd(), "warden workspace", func(cmd *cobra.Command, _ string) { markWorkspaceCollabCompatibility(cmd) }},
	} {
		legacy.mark(legacy.cmd, legacy.canonical)
		root.AddCommand(legacy.cmd)
	}
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
	root.AddCommand(reviewCmd)
	modelsCmd := newModelsCmd()
	markCompatibilityCommand(modelsCmd, "warden backend model")
	backendsCmd := newBackendsCmd()
	markCompatibilityCommand(backendsCmd, "warden backend")
	root.AddCommand(modelsCmd, backendsCmd)
	root.AddCommand(newContextCmd(), newMessageCmd(), newApprovalCmd())
	ctxCmd := newCtxCmd()
	markCtxCompatibility(ctxCmd)
	root.AddCommand(ctxCmd)
	msgCmd := newMsgCmd()
	markMsgCompatibility(msgCmd)
	root.AddCommand(msgCmd)
	approvalsCmd := newApprovalsCmd()
	markApprovalsCompatibility(approvalsCmd)
	approveCmd := newApproveCmd()
	markApproveCompatibility(approveCmd)
	autoApproveCmd := newAutoApproveCmd()
	markAutoApproveCompatibility(autoApproveCmd)
	root.AddCommand(approvalsCmd, approveCmd, autoApproveCmd)
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
	root.AddCommand(newPipelineCmd())
	root.AddCommand(newAutopilotCmd())
	landCmd := newLandCmd()
	markCompatibilityCommand(landCmd, "warden autopilot land")
	root.AddCommand(landCmd)
	mcpAlias := newMCPCmd()
	markCompatibilityCommand(mcpAlias, "warden daemon mcp")
	root.AddCommand(mcpAlias)
	tuiCmd := newTUICmd()
	markEntryPoint(tuiCmd, 40)
	root.AddCommand(tuiCmd)
	replCmd := newReplCmd()
	markCompatibilityCommand(replCmd, "warden backend repl")
	llmCmd := newLLMCmd()
	llmCmd.Hidden = true
	SetCommandHelpMetadata(llmCmd, "observe", 150, "warden backend suggest", AliasCompatibility, NodeNamespace)
	for _, child := range llmCmd.Commands() {
		child.Hidden = true
		SetCommandHelpMetadata(child, "observe", 150, "warden backend suggest", AliasCompatibility, nodeKind(child))
	}
	root.AddCommand(replCmd, llmCmd)
	doctorCmd := newDoctorCmd()
	markEntryPoint(doctorCmd, 30)
	root.AddCommand(doctorCmd)
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

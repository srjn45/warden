package cli

import (
	"strings"

	"github.com/spf13/cobra"
)

// newAgentCmd builds the canonical agent namespace. Every child is allocated by
// the same fresh factory used by its legacy root wrapper, so flags and run logic
// stay identical without re-parenting stateful Cobra nodes.
func newAgentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Create, inspect, communicate with, and manage agents",
		Long: `Create, inspect, communicate with, and manage agents.

Lifecycle commands deliberately remain distinct: terminate keeps the record and
worktree; done clears the record but keeps the worktree; delete changes only the
record; remove-worktree changes only the worktree; and stop composes teardown
steps according to its keep flags and preserves its confirmation safeguards.`,
	}
	SetCommandHelpMetadata(cmd, "run", 10, "warden agent", "", NodeNamespace)

	children := []*cobra.Command{
		canonicalAgentCommand(newLsCmd(), "list"),
		canonicalAgentCommand(newStartCmd(), "start"), canonicalAgentCommand(newStatusCmd(), "status"),
		canonicalAgentCommand(newDigestCmd(), "digest"), canonicalAgentCommand(newForkCmd(), "fork"),
		canonicalAgentCommand(newRestoreCmd(), "restore"), canonicalAgentCommand(newRecoverCmd(), "recover"),
		canonicalAgentCommand(newAdoptCmd(), "adopt"), canonicalAgentCommand(newAttachCmd(), "attach"),
		canonicalAgentCommand(newStopCmd(), "stop"), canonicalAgentCommand(newTerminateCmd(), "terminate"),
		canonicalAgentCommand(newDoneCmd(), "done"), canonicalAgentCommand(newDeleteCmd(), "delete"),
		canonicalAgentCommand(newRemoveWorktreeCmd(), "remove-worktree"),
		canonicalAgentCommand(newSendCmd(), "send"), canonicalAgentCommand(newTailCmd(), "tail"),
		canonicalAgentCommand(newHandoffCmd(), "handoff"), canonicalAgentCommand(newRotateCmd(), "rotate"),
		canonicalAgentCommand(newSwitchCmd(), "switch"),
		newAgentPermissionModeCmd(), newAgentRoleCmd(), newAgentCompactCmd(),
	}
	for i, child := range children {
		SetCommandHelpMetadata(child, "run", (i+1)*10, "warden agent "+child.Name(), "", nodeKind(child))
		cmd.AddCommand(child)
	}
	return cmd
}

func canonicalAgentCommand(cmd *cobra.Command, name string) *cobra.Command {
	parts := strings.SplitN(cmd.Use, " ", 2)
	legacyName := parts[0]
	rewriteAgentHelpPaths(cmd, legacyName, name)
	cmd.Use = name
	if len(parts) == 2 {
		cmd.Use += " " + parts[1]
	}
	cmd.Aliases = nil
	return cmd
}

func rewriteAgentHelpPaths(cmd *cobra.Command, legacyName, canonicalName string) {
	replacer := strings.NewReplacer(
		"warden "+legacyName, "warden agent "+canonicalName,
		"wd "+legacyName, "wd agent "+canonicalName,
	)
	cmd.Long = replacer.Replace(cmd.Long)
	cmd.Example = replacer.Replace(cmd.Example)
}

func nodeKind(cmd *cobra.Command) string {
	if cmd.HasAvailableSubCommands() {
		return NodeNamespace
	}
	return NodeLeaf
}

func newAgentPermissionModeCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "permission-mode", Short: "Manage an agent's permission mode"}
	cmd.AddCommand(canonicalAgentCommand(newSetPermissionModeCmd(), "set"))
	return cmd
}

func newAgentRoleCmd() *cobra.Command {
	cmd := newRoleCmd()
	for _, child := range cmd.Commands() {
		rewriteAgentHelpPaths(child, "role", "role")
		for _, grandchild := range child.Commands() {
			rewriteAgentHelpPaths(grandchild, "role", "role")
		}
	}
	cmd.AddCommand(canonicalAgentCommand(newSetRoleCmd(), "set"))
	return cmd
}

func newAgentCompactCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "compact",
		Short: "Manage an agent's force-compact override",
		Long:  "Manage the per-agent force-compact override. Setting it may interrupt an in-flight turn when the configured context threshold is crossed.",
	}
	cmd.AddCommand(canonicalAgentCommand(newForceCompactCmd(), "set"))
	return cmd
}

func markCompatibilityCommand(cmd *cobra.Command, canonicalPath string) {
	cmd.Hidden = true
	SetCommandHelpMetadata(cmd, "run", 900, canonicalPath, AliasCompatibility, nodeKind(cmd))
	for _, child := range cmd.Commands() {
		markCompatibilityChild(child, canonicalPath+" "+child.Name())
	}
}

func markCompatibilityChild(cmd *cobra.Command, canonicalPath string) {
	cmd.Hidden = true
	SetCommandHelpMetadata(cmd, "run", 900, canonicalPath, AliasCompatibility, nodeKind(cmd))
	for _, child := range cmd.Commands() {
		markCompatibilityChild(child, canonicalPath+" "+child.Name())
	}
}

func markPermanentAgentShortcut(cmd *cobra.Command, canonicalPath string) {
	SetCommandHelpMetadata(cmd, "shortcut", rootHelpPlacement[cmd.Name()].order, canonicalPath, AliasPermanentShortcut, NodeLeaf)
}

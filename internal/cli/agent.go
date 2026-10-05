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

Wherever a command takes <AGENT>, it is the agent's name, id or ticket as shown
by 'warden ls'.

Teardown: 'stop' is the full teardown (terminate the session, clear the record,
remove the worktree and branch) and takes --keep-worktree, --keep-record, --hard
and --pr to keep parts. 'terminate' only kills the session, keeping the record
and worktree. The narrower 'done', 'delete' and 'remove-worktree' verbs still work
but are hidden; see 'warden help agent stop'.`,
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
		newAgentSetCmd(), newAgentGetCmd(),
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

// hiddenSettingCmd renames a legacy per-agent setting command to `set`, rewrites
// its examples to the real `warden agent set <AGENT> <key> <value>` form, and
// hides it (kept for compatibility; `agent set` is the canonical entry point).
func hiddenSettingCmd(cmd *cobra.Command, legacyUse, key string) *cobra.Command {
	for _, p := range []string{"warden", "wd"} {
		cmd.Long = strings.ReplaceAll(cmd.Long, p+" "+legacyUse+" ", p+" agent set ")
	}
	// Insert the key after the agent argument in each example line.
	lines := strings.Split(cmd.Long, "\n")
	for i, l := range lines {
		for _, p := range []string{"warden", "wd"} {
			prefix := "  " + p + " agent set "
			if strings.HasPrefix(l, prefix) {
				rest := strings.TrimPrefix(l, prefix)
				if sp := strings.Index(rest, " "); sp > 0 {
					lines[i] = prefix + rest[:sp] + " " + key + rest[sp:]
				}
			}
		}
	}
	cmd.Long = strings.Join(lines, "\n")
	cmd.Use = "set" + strings.TrimPrefix(cmd.Use, legacyUse)
	cmd.Aliases = nil
	return cmd
}

// hideAgentSettingGroup hides a legacy setting group and every child, pointing
// them all at the canonical `agent set`.
func hideAgentSettingGroup(cmd *cobra.Command) *cobra.Command {
	markSettingCompat(cmd)
	return cmd
}

func markSettingCompat(cmd *cobra.Command) {
	cmd.Hidden = true
	SetCommandHelpMetadata(cmd, "run", 900, "warden agent set", AliasCompatibility, nodeKind(cmd))
	for _, child := range cmd.Commands() {
		markSettingCompat(child)
	}
}

func newAgentPermissionModeCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "permission-mode", Short: "Manage an agent's permission mode"}
	cmd.AddCommand(hiddenSettingCmd(newSetPermissionModeCmd(), "set-permission-mode", "permission-mode"))
	return hideAgentSettingGroup(cmd)
}

func newAgentRoleCmd() *cobra.Command {
	cmd := newRoleCmd()
	for _, child := range cmd.Commands() {
		rewriteAgentHelpPaths(child, "role", "role")
		for _, grandchild := range child.Commands() {
			rewriteAgentHelpPaths(grandchild, "role", "role")
		}
	}
	set := hiddenSettingCmd(newSetRoleCmd(), "set-role", "role")
	cmd.AddCommand(set)
	markSettingCompat(set)
	return cmd
}

func newAgentCompactCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "compact",
		Short: "Manage an agent's force-compact override",
		Long:  "Manage the per-agent force-compact override. Setting it may interrupt an in-flight turn when the configured context threshold is crossed.",
	}
	cmd.AddCommand(hiddenSettingCmd(newForceCompactCmd(), "force-compact", "compact"))
	return hideAgentSettingGroup(cmd)
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

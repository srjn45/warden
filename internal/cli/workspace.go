package cli

import (
	"strings"

	"github.com/spf13/cobra"
)

// newWorkspaceCmd builds the canonical workspace namespace for git worktrees,
// snapshots, branch tracking, and file-collision inspection. Each child is
// allocated by the same fresh factory as its legacy root wrapper.
func newWorkspaceCmd() *cobra.Command {
	legacy := newWorktreeCmd()
	cmd := &cobra.Command{
		Use:   "workspace",
		Short: "Inspect and manage warden git worktrees, snapshots, branches, and file collisions",
		Long: rewriteWorkspaceHelpPaths(
			"One umbrella over warden's workspace operations:\n"+
				"  • LIST worktrees — the warden-owned worktrees under .worktrees (`wd workspace list`)\n"+
				"  • PRUNE orphaned worktrees (`wd workspace prune`)\n"+
				"  • SNAPSHOT an agent's worktree + transcript (`wd workspace snapshot`)\n"+
				"  • BRANCHES — per-agent CI and branch-vs-main status (`wd workspace branches`)\n"+
				"  • CONFLICTS — files edited by more than one agent (`wd workspace conflicts`)\n"+
				"  • WHO-IS-EDITING — which agents are editing a file (`wd workspace who-is-editing`)\n\n"+
				"`wd workspace` with no subcommand prints the worktree list (the same view as `wd workspace list`).",
			"worktree", "workspace"),
		Args: cobra.NoArgs,
		RunE: runWorktreeList,
	}
	SetCommandHelpMetadata(cmd, "project", 8, "warden workspace", "", NodeNamespace)
	addWorktreeListFlags(cmd)

	children := []*cobra.Command{
		canonicalWorkspaceCommand(newWorktreeListCmd(), "list"),
		canonicalWorkspaceCommand(newPruneCmd(), "prune"),
		newWorkspaceSnapshotCmd(),
		canonicalWorkspaceCommand(newBranchesCmd(), "branches"),
		canonicalWorkspaceCommand(newCollabConflictsCmd(), "conflicts"),
		canonicalWorkspaceCommand(newCollabWhoIsEditingCmd(), "who-is-editing"),
	}
	for i, child := range children {
		SetCommandHelpMetadata(child, "project", (i+1)*10, "warden workspace "+child.Name(), "", nodeKind(child))
		cmd.AddCommand(child)
	}
	_ = legacy // bare-parent behavior copied explicitly above
	return cmd
}

func newWorkspaceSnapshotCmd() *cobra.Command {
	legacy := newSnapshotCmd()
	cmd := &cobra.Command{
		Use:   "snapshot",
		Short: legacy.Short,
		Long:  rewriteWorkspaceHelpPaths(legacy.Long, "snapshot", "workspace snapshot"),
	}
	children := []*cobra.Command{
		canonicalWorkspaceSnapshotCommand(newSnapshotCreateCmd(), "create"),
		canonicalWorkspaceSnapshotCommand(newSnapshotListCmd(), "list"),
		canonicalWorkspaceSnapshotCommand(newSnapshotRestoreCmd(), "restore"),
	}
	for i, child := range children {
		SetCommandHelpMetadata(child, "project", (i+1)*10, "warden workspace snapshot "+child.Name(), "", NodeLeaf)
		cmd.AddCommand(child)
	}
	return cmd
}

func canonicalWorkspaceCommand(cmd *cobra.Command, name string) *cobra.Command {
	parts := strings.SplitN(cmd.Use, " ", 2)
	legacyName := parts[0]
	rewriteWorkspaceHelpPathsOn(cmd, legacyName, name)
	cmd.Use = name
	if len(parts) == 2 {
		cmd.Use += " " + parts[1]
	}
	cmd.Aliases = nil
	return cmd
}

func canonicalWorkspaceSnapshotCommand(cmd *cobra.Command, name string) *cobra.Command {
	parts := strings.SplitN(cmd.Use, " ", 2)
	legacyLeaf := parts[0]
	rewriteWorkspaceHelpPathsOn(cmd, "snapshot "+legacyLeaf, "workspace snapshot "+name)
	cmd.Use = name
	if len(parts) == 2 {
		cmd.Use += " " + parts[1]
	}
	cmd.Aliases = nil
	return cmd
}

func rewriteWorkspaceHelpPathsOn(cmd *cobra.Command, oldPrefix, newPrefix string) {
	replacer := strings.NewReplacer(
		"warden "+oldPrefix, "warden "+newPrefix,
		"wd "+oldPrefix, "wd "+newPrefix,
	)
	cmd.Long = replacer.Replace(cmd.Long)
	cmd.Example = replacer.Replace(cmd.Example)
}

func rewriteWorkspaceHelpPaths(text, oldPrefix, newPrefix string) string {
	replacer := strings.NewReplacer(
		"warden "+oldPrefix, "warden "+newPrefix,
		"wd "+oldPrefix, "wd "+newPrefix,
	)
	return replacer.Replace(text)
}

func markWorkspaceCollabCompatibility(cmd *cobra.Command) {
	cmd.Hidden = true
	SetCommandHelpMetadata(cmd, "project", 900, "warden workspace", AliasCompatibility, nodeKind(cmd))
	for _, child := range cmd.Commands() {
		markCompatibilityChild(child, "warden workspace "+child.Name())
	}
}

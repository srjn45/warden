package cli

import (
	"strings"

	"github.com/spf13/cobra"
)

// newGitCmd builds the canonical git namespace. Every child is allocated by the
// same fresh factory used by its legacy root wrapper, so flags and run logic stay
// identical without re-parenting stateful Cobra nodes.
func newGitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "git",
		Short: "Commit, push, sync, and review an agent worktree on warden rails",
		Long: `Commit, push, sync, and review an agent worktree on warden rails.

Git verbs run locally in the agent's worktree (or the current directory when no
agent session is bound). They enforce branch rails, hook bookkeeping, and the
daemon-side session link — the high-frequency flat shortcuts ` + "`wd commit`" + `,
` + "`wd push`" + `, and ` + "`wd sync`" + ` remain permanently supported wrappers.`,
	}
	SetCommandHelpMetadata(cmd, "project", 12, "warden git", "", NodeNamespace)

	children := []*cobra.Command{
		canonicalGitCommand(newCommitCmd(), "commit"),
		canonicalGitCommand(newPushCmd(), "push"),
		canonicalGitCommand(newSyncCmd(), "sync"),
		canonicalGitCommand(newReviewCmd(), "review"),
		canonicalGitHookCommand(newHookGitGuardCmd(), "guard", "git-guard"),
	}
	for i, child := range children {
		kind := nodeKind(child)
		if kind == NodeInternal {
			child.Hidden = true
		}
		SetCommandHelpMetadata(child, "project", (i+1)*10, "warden git "+child.Name(), "", kind)
		cmd.AddCommand(child)
	}
	return cmd
}

func canonicalGitCommand(cmd *cobra.Command, name string) *cobra.Command {
	parts := strings.SplitN(cmd.Use, " ", 2)
	legacyName := parts[0]
	rewriteGitHelpPaths(cmd, legacyName, name)
	cmd.Use = name
	if len(parts) == 2 {
		cmd.Use += " " + parts[1]
	}
	cmd.Aliases = nil
	return cmd
}

func canonicalGitHookCommand(cmd *cobra.Command, name, legacyName string) *cobra.Command {
	rewriteGitHelpPaths(cmd, legacyName, name)
	cmd.Use = name
	cmd.Aliases = nil
	SetCommandHelpMetadata(cmd, "project", 0, "warden git "+name, "", NodeInternal)
	return cmd
}

func rewriteGitHelpPaths(cmd *cobra.Command, legacyName, canonicalName string) {
	replacer := strings.NewReplacer(
		"warden "+legacyName, "warden git "+canonicalName,
		"wd "+legacyName, "wd git "+canonicalName,
	)
	cmd.Long = replacer.Replace(cmd.Long)
	cmd.Example = replacer.Replace(cmd.Example)
}

func markPermanentGitShortcut(cmd *cobra.Command, canonicalPath string) {
	SetCommandHelpMetadata(cmd, "shortcut", rootHelpPlacement[cmd.Name()].order, canonicalPath, AliasPermanentShortcut, NodeLeaf)
}

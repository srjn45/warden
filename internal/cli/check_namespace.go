package cli

import (
	"strings"

	"github.com/spf13/cobra"
)

// newCheckNamespaceCmd builds the canonical check namespace. The namespace itself
// remains directly runnable as the permanent `wd check` shortcut; `check run` is the
// explicit canonical spelling with identical behavior.
func newCheckNamespaceCmd() *cobra.Command {
	runCmd := newCheckRunCmd()
	cmd := &cobra.Command{
		Use:   "check [name]",
		Short: "Run project checks and install hook guards",
		Long: `Run project checks and install hook guards.

` + "`wd check`" + ` (or ` + "`wd check run`" + `) executes the commands declared in .warden/check.yml
and returns only failures. Guard subcommands are hook-facing entry points installed
by warden; they preserve the stdin/stdout JSON protocol and fail-open semantics of
the legacy ` + "`hook`" + ` paths.`,
		Args: runCmd.Args,
		RunE: runCmd.RunE,
	}
	cmd.Flags().AddFlagSet(runCmd.Flags())
	SetCommandHelpMetadata(cmd, "shortcut", rootHelpPlacement["check"].order, "warden check run", AliasPermanentShortcut, NodeNamespace)

	children := []*cobra.Command{
		canonicalCheckCommand(newCheckRunCmd(), "run"),
		canonicalCheckHookCommand(newHookCheckGuardCmd(), "guard", "check-guard"),
		canonicalCheckHookCommand(newHookGuardCmd(), "boundary", "guard"),
		canonicalCheckHookCommand(newHookRootGuardCmd(), "root-guard", "root-guard"),
	}
	for i, child := range children {
		kind := nodeKind(child)
		if kind == NodeInternal {
			child.Hidden = true
		}
		SetCommandHelpMetadata(child, "project", (i+1)*10, "warden check "+child.Name(), "", kind)
		cmd.AddCommand(child)
	}
	return cmd
}

func canonicalCheckCommand(cmd *cobra.Command, name string) *cobra.Command {
	parts := strings.SplitN(cmd.Use, " ", 2)
	legacyName := parts[0]
	rewriteCheckHelpPaths(cmd, legacyName, name)
	cmd.Use = name
	if len(parts) == 2 {
		cmd.Use += " " + parts[1]
	}
	cmd.Aliases = nil
	return cmd
}

func canonicalCheckHookCommand(cmd *cobra.Command, name, legacyName string) *cobra.Command {
	rewriteCheckHelpPaths(cmd, legacyName, name)
	cmd.Use = name
	cmd.Aliases = nil
	SetCommandHelpMetadata(cmd, "project", 0, "warden check "+name, "", NodeInternal)
	return cmd
}

func rewriteCheckHelpPaths(cmd *cobra.Command, legacyName, canonicalName string) {
	replacer := strings.NewReplacer(
		"warden "+legacyName, "warden check "+canonicalName,
		"wd "+legacyName, "wd check "+canonicalName,
		"hook "+legacyName, "check "+canonicalName,
	)
	cmd.Long = replacer.Replace(cmd.Long)
	cmd.Example = replacer.Replace(cmd.Example)
}

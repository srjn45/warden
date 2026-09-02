package cli

import (
	"strings"

	"github.com/spf13/cobra"
)

// markEntryPoint tags a top-level host/entry command that is not a domain namespace.
func markEntryPoint(cmd *cobra.Command, order int) {
	SetCommandHelpMetadata(cmd, "entry", order, "warden "+cmd.Name(), "", NodeEntryPoint)
}

// newDaemonCmd builds the canonical daemon namespace: run the hub process, host MCP,
// and manage remote-access bearer tokens.
func newDaemonCmd() *cobra.Command {
	cmd := newDaemonRunCmd()
	SetCommandHelpMetadata(cmd, "operate", 10, "warden daemon", "", NodeNamespace)

	mcp := newMCPCmd()
	SetCommandHelpMetadata(mcp, "operate", 20, "warden daemon mcp", "", NodeLeaf)

	token := newDaemonTokenNamespaceCmd()
	cmd.AddCommand(mcp, token)
	return cmd
}

func newDaemonTokenNamespaceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Manage the daemon's remote-access bearer token",
	}
	SetCommandHelpMetadata(cmd, "operate", 30, "warden daemon token", "", NodeNamespace)
	children := []*cobra.Command{
		newTokenGenerateCmd(),
		newTokenShowCmd(),
		newTokenRotateCmd(),
	}
	for i, child := range children {
		rewriteDaemonTokenHelpPaths(child)
		SetCommandHelpMetadata(child, "operate", (i+1)*10, "warden daemon token "+child.Name(), "", NodeLeaf)
		cmd.AddCommand(child)
	}
	return cmd
}

func rewriteDaemonTokenHelpPaths(cmd *cobra.Command) {
	replacer := strings.NewReplacer(
		"warden token", "warden daemon token",
		"wd token", "wd daemon token",
		"`warden token", "`warden daemon token",
		"`wd token", "`wd daemon token",
	)
	cmd.Long = replacer.Replace(cmd.Long)
	cmd.Example = replacer.Replace(cmd.Example)
}

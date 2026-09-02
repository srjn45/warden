package cli

import (
	"strings"

	"github.com/spf13/cobra"
)

// newInspectCmd builds the canonical inspect namespace for fleet search, audit,
// portability, repair, and resource measurements.
func newInspectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "inspect",
		Short: "Search, audit, export, repair, and measure warden's fleet and resources",
		Long: `Search, audit, export, repair, and measure warden's fleet and resources.

Resource samples (CPU, memory, pressure, daemon stats) live under ` + "`resources`" + `.
Financial usage and provider quota snapshots live under ` + "`usage`" + `, not here.`,
	}
	SetCommandHelpMetadata(cmd, "observe", 35, "warden inspect", "", NodeNamespace)

	children := []*cobra.Command{
		canonicalInspectCommand(newStatsCmd(), "stats", "resources"),
		canonicalInspectCommand(newSearchCmd(), "search", "search"),
		canonicalInspectCommand(newHistoryCmd(), "history", "history"),
		canonicalInspectCommand(newAuditLogCmd(), "audit log", "audit"),
		canonicalInspectCommand(newExportCmd(), "export", "export"),
		canonicalInspectCommand(newImportCmd(), "import", "import"),
		newInspectRepairCmd(),
	}
	for i, child := range children {
		SetCommandHelpMetadata(child, "observe", (i+1)*10, "warden inspect "+child.Name(), "", nodeKind(child))
		cmd.AddCommand(child)
	}
	return cmd
}

func newInspectRepairCmd() *cobra.Command {
	cmd := newRepairCmd()
	rewriteInspectHelpPaths(cmd, "repair", "repair")
	cmd.Use = "repair"
	for _, child := range cmd.Commands() {
		rewriteInspectHelpPaths(child, "repair sessions", "repair sessions")
		SetCommandHelpMetadata(child, "observe", 80, "warden inspect repair "+child.Name(), "", nodeKind(child))
	}
	return cmd
}

func canonicalInspectCommand(cmd *cobra.Command, legacyPath, canonicalName string) *cobra.Command {
	parts := strings.SplitN(cmd.Use, " ", 2)
	rewriteInspectHelpPaths(cmd, legacyPath, canonicalName)
	cmd.Use = canonicalName
	if len(parts) == 2 && !strings.Contains(legacyPath, " ") {
		cmd.Use += " " + parts[1]
	}
	cmd.Aliases = nil
	return cmd
}

func rewriteInspectHelpPaths(cmd *cobra.Command, legacyPath, canonicalName string) {
	replacer := strings.NewReplacer(
		"warden "+legacyPath, "warden inspect "+canonicalName,
		"wd "+legacyPath, "wd inspect "+canonicalName,
		"warden stats", "warden inspect resources",
		"wd stats", "wd inspect resources",
		"warden audit log", "warden inspect audit",
		"wd audit log", "wd inspect audit",
	)
	cmd.Long = replacer.Replace(cmd.Long)
	cmd.Example = replacer.Replace(cmd.Example)
}

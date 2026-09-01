package cli

import (
	"strings"

	"github.com/spf13/cobra"
)

// newBackendCmd builds the canonical backend namespace. Every child is allocated
// by the same fresh factory used by its legacy root wrapper, so flags and run
// logic stay identical without re-parenting stateful Cobra nodes.
func newBackendCmd() *cobra.Command {
	cmd := canonicalBackendCommand(newBackendsCmd(), "backend")
	SetCommandHelpMetadata(cmd, "observe", 45, "warden backend", "", NodeNamespace)

	for i, child := range cmd.Commands() {
		rewriteBackendHelpPaths(child, "backends", "backend")
		SetCommandHelpMetadata(child, "observe", (i+1)*10, "warden backend "+child.Name(), "", nodeKind(child))
	}

	modelCmd := newBackendModelCmd()
	suggestCmd := canonicalBackendCommand(newLLMSuggestCmd(), "suggest")
	replCmd := canonicalBackendCommand(newReplCmd(), "repl")
	for i, child := range []*cobra.Command{modelCmd, suggestCmd, replCmd} {
		SetCommandHelpMetadata(child, "observe", 80+(i+1)*10, "warden backend "+child.Name(), "", nodeKind(child))
		cmd.AddCommand(child)
	}
	return cmd
}

func newBackendModelCmd() *cobra.Command {
	cmd := newModelsCmd()
	for _, child := range cmd.Commands() {
		rewriteBackendHelpPaths(child, "models", "model")
	}
	cmd = canonicalBackendCommand(cmd, "model")
	for i, child := range cmd.Commands() {
		SetCommandHelpMetadata(child, "observe", (i+1)*10, "warden backend model "+child.Name(), "", nodeKind(child))
	}
	return cmd
}

func canonicalBackendCommand(cmd *cobra.Command, name string) *cobra.Command {
	parts := strings.SplitN(cmd.Use, " ", 2)
	legacyName := parts[0]
	rewriteBackendHelpPaths(cmd, legacyName, name)
	cmd.Use = name
	if len(parts) == 2 {
		cmd.Use += " " + parts[1]
	}
	cmd.Aliases = nil
	return cmd
}

func rewriteBackendHelpPaths(cmd *cobra.Command, legacyName, canonicalName string) {
	replacer := strings.NewReplacer(
		"warden "+legacyName, "warden backend "+canonicalName,
		"wd "+legacyName, "wd backend "+canonicalName,
		"warden backends", "warden backend",
		"wd backends", "wd backend",
		"warden models", "warden backend model",
		"wd models", "wd backend model",
		"warden llm suggest", "warden backend suggest",
		"wd llm suggest", "wd backend suggest",
	)
	cmd.Long = replacer.Replace(cmd.Long)
	cmd.Example = replacer.Replace(cmd.Example)
}

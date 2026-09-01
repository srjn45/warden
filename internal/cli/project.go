package cli

import (
	"strings"

	"github.com/spf13/cobra"
)

// newProjectCmd builds the canonical project namespace for repo-local
// configuration: memory, presets, prompt templates, the library umbrella, and
// plugins. Each child is allocated by the same fresh factory as its legacy root
// wrapper so flags and run logic stay identical.
func newProjectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "project",
		Short: "Manage repo-local warden configuration: memory, presets, templates, and plugins",
		Long: "Manage repo-local warden configuration: durable project memory, reusable\n" +
			"spawn presets and prompt templates, the library umbrella over saved launch\n" +
			"configs, and the plugin registry for custom task types and lifecycle hooks.",
	}
	SetCommandHelpMetadata(cmd, "project", 5, "warden project", "", NodeNamespace)

	children := []*cobra.Command{
		canonicalProjectCommand(newMemoryCmd(), "memory"),
		newProjectPresetCmd(),
		newProjectPromptTemplateCmd(),
		newProjectLibraryCmd(),
		newProjectPluginCmd(),
	}
	for i, child := range children {
		SetCommandHelpMetadata(child, "project", (i+1)*10, "warden project "+child.Name(), "", nodeKind(child))
		cmd.AddCommand(child)
	}
	return cmd
}

func newProjectPresetCmd() *cobra.Command {
	legacy := newPresetCmd()
	cmd := &cobra.Command{
		Use:   "preset",
		Short: legacy.Short,
		Long:  rewriteProjectHelpPaths(legacy.Long, "preset", "project preset"),
	}
	cmd.AddCommand(
		canonicalProjectNestedCommand(newPresetSaveCmd(), "preset", "save"),
		canonicalProjectNestedCommand(newPresetListCmd(), "preset", "list"),
	)
	return cmd
}

func newProjectPromptTemplateCmd() *cobra.Command {
	legacy := newPromptTemplateCmd()
	cmd := &cobra.Command{
		Use:   "prompt-template",
		Short: legacy.Short,
		Long:  rewriteProjectHelpPaths(legacy.Long, "prompt-template", "project prompt-template"),
	}
	cmd.AddCommand(
		canonicalProjectNestedCommand(newPromptTemplateSaveCmd(), "prompt-template", "save"),
		canonicalProjectNestedCommand(newPromptTemplateListCmd(), "prompt-template", "list"),
	)
	return cmd
}

func newProjectLibraryCmd() *cobra.Command {
	legacy := newLibraryCmd()
	cmd := &cobra.Command{
		Use:   "library",
		Short: legacy.Short,
		Long:  rewriteProjectHelpPaths(legacy.Long, "library", "project library"),
	}
	list := canonicalProjectNestedCommand(newLibraryListCmd(), "library", "list")
	savePreset := newLibrarySavePresetCmd()
	markProjectLibrarySaveAlias(savePreset, "warden project preset save")
	savePrompt := newLibrarySavePromptCmd()
	markProjectLibrarySaveAlias(savePrompt, "warden project prompt-template save")
	cmd.AddCommand(list, savePreset, savePrompt)
	return cmd
}

func newProjectPluginCmd() *cobra.Command {
	legacy := newPluginCmd()
	cmd := &cobra.Command{
		Use:   "plugin",
		Short: legacy.Short,
		Long:  rewriteProjectHelpPaths(legacy.Long, "plugin", "project plugin"),
	}
	cmd.AddCommand(canonicalProjectNestedCommand(newPluginListCmd(), "plugin", "list"))
	return cmd
}

func canonicalProjectCommand(cmd *cobra.Command, name string) *cobra.Command {
	parts := strings.SplitN(cmd.Use, " ", 2)
	legacyName := parts[0]
	rewriteProjectHelpPathsOn(cmd, legacyName, name)
	cmd.Use = name
	if len(parts) == 2 {
		cmd.Use += " " + parts[1]
	}
	cmd.Aliases = nil
	return cmd
}

func canonicalProjectNestedCommand(cmd *cobra.Command, parent, name string) *cobra.Command {
	parts := strings.SplitN(cmd.Use, " ", 2)
	legacyLeaf := parts[0]
	rewriteProjectHelpPathsOn(cmd, parent+" "+legacyLeaf, "project "+parent+" "+name)
	cmd.Use = name
	if len(parts) == 2 {
		cmd.Use += " " + parts[1]
	}
	cmd.Aliases = nil
	return cmd
}

func rewriteProjectHelpPaths(text, oldPrefix, newPrefix string) string {
	replacer := strings.NewReplacer(
		"warden "+oldPrefix, "warden "+newPrefix,
		"wd "+oldPrefix, "wd "+newPrefix,
	)
	return replacer.Replace(text)
}

func rewriteProjectHelpPathsOn(cmd *cobra.Command, oldPrefix, newPrefix string) {
	replacer := strings.NewReplacer(
		"warden "+oldPrefix, "warden "+newPrefix,
		"wd "+oldPrefix, "wd "+newPrefix,
	)
	cmd.Long = replacer.Replace(cmd.Long)
	cmd.Example = replacer.Replace(cmd.Example)
}

func markProjectLibraryCompatibility(cmd *cobra.Command) {
	cmd.Hidden = true
	SetCommandHelpMetadata(cmd, "project", 900, "warden project library", AliasCompatibility, nodeKind(cmd))
	for _, child := range cmd.Commands() {
		switch child.Name() {
		case "save-preset":
			markCompatibilityChild(child, "warden project preset save")
		case "save-prompt":
			markCompatibilityChild(child, "warden project prompt-template save")
		default:
			markCompatibilityChild(child, "warden project library "+child.Name())
		}
	}
}

func markProjectLibrarySaveAlias(cmd *cobra.Command, canonicalPath string) {
	cmd.Hidden = true
	SetCommandHelpMetadata(cmd, "project", 900, canonicalPath, AliasCompatibility, NodeLeaf)
}

package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/preset"
	"github.com/srjn45/warden/internal/prompttemplate"
)

// newLibraryCmd is the unified "library" umbrella over warden's saved, reusable
// launch configs: spawn PRESETS (named `warden start` defaults), reusable PROMPT
// TEMPLATES (variabled prompt bodies), and the built-in pipeline TEMPLATES. It
// adds no storage or format of its own — it is a single discoverable entry point
// that reuses the existing preset store, the prompt-template store, and the
// embedded pipeline-template catalog. The standalone `preset`, `prompt-template`,
// and `pipeline list-templates` commands keep working exactly as before.
func newLibraryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "library",
		Aliases: []string{"lib"},
		Short:   "Browse saved spawn presets, prompt templates, and pipeline templates in one place",
		Long: "One umbrella over warden's reusable launch configs:\n" +
			"  • spawn PRESETS      — named `warden start` defaults (saved in ~/.warden/presets.yaml)\n" +
			"  • prompt TEMPLATES   — variabled prompt bodies (saved in ~/.warden/prompt-templates.yaml)\n" +
			"  • pipeline TEMPLATES — built-in DAG starters bundled with warden (read-only)\n\n" +
			"`library list` shows all three. `library save-preset` saves a spawn preset (the\n" +
			"same as `warden preset save`) and `library save-prompt` saves a prompt template\n" +
			"(the same as `warden prompt-template save`). Pipeline templates are embedded and\n" +
			"read-only, so there is no `save-template`; author a pipeline from a YAML spec with\n" +
			"`warden pipeline create -f <spec.yaml>` instead. The `preset`, `prompt-template`,\n" +
			"and `pipeline list-templates` commands remain available and unchanged.",
	}
	cmd.AddCommand(newLibraryListCmd(), newLibrarySavePresetCmd(), newLibrarySavePromptCmd())
	return cmd
}

func newLibraryListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List saved spawn presets and built-in pipeline templates",
		Long: "Show both libraries in two labeled sections: saved spawn presets (name +\n" +
			"their stored defaults) and the built-in pipeline templates (name + a short\n" +
			"description). Reuses the same sources as `warden preset list` and `warden\n" +
			"pipeline list-templates`.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()

			// Section 1 — spawn presets (reuse the preset store loader).
			store, err := preset.Load(presetPathFor(cmd))
			if err != nil {
				return err
			}
			fmt.Fprintln(out, "SPAWN PRESETS")
			names := store.Names()
			if len(names) == 0 {
				fmt.Fprintln(out, "  none saved — create one with `warden library save-preset <name> [spawn flags]`")
			} else {
				tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
				for _, n := range names {
					p, _ := store.Get(n)
					fmt.Fprintf(tw, "  %s\t%s\n", n, presetSummary(p))
				}
				if err := tw.Flush(); err != nil {
					return err
				}
			}

			// Section 2 — prompt templates (reuse the prompt-template store).
			fmt.Fprintln(out)
			fmt.Fprintln(out, "PROMPT TEMPLATES")
			ptStore, err := prompttemplate.Load(promptTemplatePathFor(cmd))
			if err != nil {
				return err
			}
			ptNames := ptStore.Names()
			if len(ptNames) == 0 {
				fmt.Fprintln(out, "  none saved — create one with `warden library save-prompt <name> --prompt \"…{{VAR}}…\"`")
			} else {
				tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
				for _, n := range ptNames {
					t, _ := ptStore.Get(n)
					fmt.Fprintf(tw, "  %s\t%s\n", n, promptTemplateVarsSummary(t))
				}
				if err := tw.Flush(); err != nil {
					return err
				}
			}

			// Section 3 — pipeline templates (reuse the embedded catalog).
			fmt.Fprintln(out)
			fmt.Fprintln(out, "PIPELINE TEMPLATES")
			templates := pipeline.ListTemplates()
			if len(templates) == 0 {
				fmt.Fprintln(out, "  none")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
			for _, t := range templates {
				fmt.Fprintf(tw, "  %s\t%s\n", t.Name, t.Description)
			}
			return tw.Flush()
		},
	}
}

// newLibrarySavePresetCmd is a thin delegate to the existing preset-save path:
// it reuses newPresetSaveCmd's flags and RunE verbatim, only relabeling it as
// `library save-preset <name>`.
func newLibrarySavePresetCmd() *cobra.Command {
	cmd := newPresetSaveCmd()
	cmd.Use = "save-preset <name> [spawn flags]"
	cmd.Short = "Save the given spawn flags as a named preset (same as `warden preset save`)"
	return cmd
}

// newLibrarySavePromptCmd is a thin delegate to the existing prompt-template
// save path: it reuses newPromptTemplateSaveCmd's flags and RunE verbatim, only
// relabeling it as `library save-prompt <name>`.
func newLibrarySavePromptCmd() *cobra.Command {
	cmd := newPromptTemplateSaveCmd()
	cmd.Use = "save-prompt <name> --prompt \"<body with {{VAR}} placeholders>\""
	cmd.Short = "Save a variabled prompt template (same as `warden prompt-template save`)"
	return cmd
}

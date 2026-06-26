package cli

import (
	"fmt"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/prompttemplate"
)

// promptTemplatePathFor resolves the prompt-templates file path for a command.
// Templates live beside the config file (prompt-templates.yaml in the same
// directory), so a custom --config dir also redirects templates — mirroring
// presetPathFor so `prompt-template save`, `library list`, and `start
// --prompt-template` read the same store under tests.
func promptTemplatePathFor(cmd *cobra.Command) string {
	if p, _ := cmd.Flags().GetString("config"); p != "" {
		return filepath.Join(filepath.Dir(p), "prompt-templates.yaml")
	}
	return prompttemplate.DefaultPath()
}

func newPromptTemplateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "prompt-template",
		Aliases: []string{"prompt-templates", "pt"},
		Short:   "Save and list reusable, variabled prompt templates (fill with `warden start --prompt-template <name> --set VAR=value`)",
	}
	cmd.AddCommand(newPromptTemplateSaveCmd(), newPromptTemplateListCmd())
	return cmd
}

func newPromptTemplateSaveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "save <name> --prompt \"<body with {{VAR}} placeholders>\"",
		Short: "Save a named prompt template with {{VAR}} placeholders",
		Long: "Persist a reusable prompt body under a name in\n" +
			"~/.warden/prompt-templates.yaml. The body may contain `{{VAR}}` placeholders;\n" +
			"the declared variables are derived from the body automatically. Saving an\n" +
			"existing name overwrites it. Fill in and spawn with\n" +
			"`warden start --prompt-template <name> --set VAR=value …`.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			body, _ := cmd.Flags().GetString("prompt")
			if strings.TrimSpace(body) == "" {
				return fmt.Errorf("--prompt is required (the template body, e.g. --prompt \"Fix {{FILE}}\")")
			}
			tpl := prompttemplate.Template{
				Prompt: body,
				Vars:   prompttemplate.Placeholders(body),
			}
			path := promptTemplatePathFor(cmd)
			store, err := prompttemplate.Load(path)
			if err != nil {
				return err
			}
			_, existed := store.Get(name)
			store.Set(name, tpl)
			if err := store.Save(path); err != nil {
				return err
			}
			verb := "saved"
			if existed {
				verb = "updated"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s prompt template %q (%s) — fill with `warden start --prompt-template %s%s`\n",
				verb, name, promptTemplateVarsSummary(tpl), name, setHint(tpl))
			return nil
		},
	}
	cmd.Flags().String("prompt", "", "the template body, with {{VAR}} placeholders")
	return cmd
}

func newPromptTemplateListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List saved prompt templates and their variables",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := promptTemplatePathFor(cmd)
			store, err := prompttemplate.Load(path)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			names := store.Names()
			if len(names) == 0 {
				fmt.Fprintln(out, "no prompt templates saved — create one with `warden prompt-template save <name> --prompt \"…{{VAR}}…\"`")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tVARS")
			for _, n := range names {
				tpl, _ := store.Get(n)
				fmt.Fprintf(tw, "%s\t%s\n", n, promptTemplateVarsSummary(tpl))
			}
			return tw.Flush()
		},
	}
}

// promptTemplateVarsSummary renders a template's declared variables as a compact
// comma list (or "(no vars)") for `prompt-template list` and `library list`.
func promptTemplateVarsSummary(t prompttemplate.Template) string {
	if len(t.Vars) == 0 {
		return "(no vars)"
	}
	return strings.Join(t.Vars, ", ")
}

// setHint builds an illustrative ` --set VAR=… --set …` suffix for a template's
// declared variables, so save/help output shows exactly what must be supplied.
func setHint(t prompttemplate.Template) string {
	var b strings.Builder
	for _, v := range t.Vars {
		b.WriteString(" --set " + v + "=…")
	}
	return b.String()
}

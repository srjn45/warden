package cli

import (
	"fmt"
	"path/filepath"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/preset"
)

// presetPathFor resolves the presets file path for a command. Presets live
// beside the config file (presets.yaml in the same directory), so a custom
// --config dir also redirects presets — which keeps `preset save`,
// `preset list`, and `start --preset` reading the same store under tests.
func presetPathFor(cmd *cobra.Command) string {
	if p, _ := cmd.Flags().GetString("config"); p != "" {
		return filepath.Join(filepath.Dir(p), "presets.yaml")
	}
	return preset.DefaultPath()
}

func newPresetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "preset",
		Short: "Save and list named spawn configs (replay with `warden start --preset <name>`)",
	}
	cmd.AddCommand(newPresetSaveCmd(), newPresetListCmd())
	return cmd
}

func newPresetSaveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "save <name> [spawn flags]",
		Short: "Save the given spawn flags as a named preset",
		Long: "Persist a reusable set of `warden start` defaults under a name in\n" +
			"~/.warden/presets.yaml. Saving an existing name overwrites it. Replay with\n" +
			"`warden start --preset <name>` (explicit CLI flags still win).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			supervised, _ := cmd.Flags().GetBool("supervised")
			permissionMode, _ := cmd.Flags().GetString("permission-mode")
			// --supervised is an alias for --permission-mode acceptEdits, resolved
			// at save time so the stored preset records the canonical mode.
			if supervised && permissionMode == "" {
				permissionMode = "acceptEdits"
			}
			typ, _ := cmd.Flags().GetString("type")
			model, _ := cmd.Flags().GetString("model")
			autoRestart, _ := cmd.Flags().GetBool("auto-restart")
			worktree, _ := cmd.Flags().GetBool("worktree")
			inRepo, _ := cmd.Flags().GetBool("in-repo")

			p := preset.Preset{
				Type:           typ,
				Model:          model,
				PermissionMode: permissionMode,
				AutoRestart:    autoRestart,
				Worktree:       worktree,
				InRepo:         inRepo,
			}
			path := presetPathFor(cmd)
			store, err := preset.Load(path)
			if err != nil {
				return err
			}
			_, existed := store.Get(name)
			store.Set(name, p)
			if err := store.Save(path); err != nil {
				return err
			}
			verb := "saved"
			if existed {
				verb = "updated"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s preset %q — replay with `warden start --preset %s`\n", verb, name, name)
			return nil
		},
	}
	cmd.Flags().String("type", "", "task type: development|analysis|spike|pr-review|code|docs|website|debug-ci|tests|other")
	cmd.Flags().String("model", "", "claude model: opus, sonnet, haiku, fable, or full model ID")
	cmd.Flags().Bool("supervised", false, "alias for --permission-mode acceptEdits")
	cmd.Flags().String("permission-mode", "", "permission mode: acceptEdits|auto|bypassPermissions|default|dontAsk|plan")
	cmd.Flags().Bool("auto-restart", false, "auto-resume this agent if it crashes (errored)")
	cmd.Flags().Bool("worktree", false, "create a scratch worktree for analysis/spike")
	cmd.Flags().Bool("in-repo", false, "run in the shared repo instead of an isolated worktree")
	return cmd
}

func newPresetListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List saved presets and their defaults",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := presetPathFor(cmd)
			store, err := preset.Load(path)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			names := store.Names()
			if len(names) == 0 {
				fmt.Fprintln(out, "no presets saved — create one with `warden preset save <name> [spawn flags]`")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tDEFAULTS")
			for _, n := range names {
				p, _ := store.Get(n)
				fmt.Fprintf(tw, "%s\t%s\n", n, presetSummary(p))
			}
			return tw.Flush()
		},
	}
}

// stringFlagOr returns the flag's value when the user set it explicitly,
// otherwise the preset fallback. This is the override rule for `start
// --preset`: an explicit CLI flag always wins; an unset flag inherits the
// preset's default.
func stringFlagOr(cmd *cobra.Command, name, fallback string) string {
	if cmd.Flags().Changed(name) {
		v, _ := cmd.Flags().GetString(name)
		return v
	}
	return fallback
}

// boolFlagOr is the bool counterpart of stringFlagOr.
func boolFlagOr(cmd *cobra.Command, name string, fallback bool) bool {
	if cmd.Flags().Changed(name) {
		v, _ := cmd.Flags().GetBool(name)
		return v
	}
	return fallback
}

// presetSummary renders a preset's set fields as a compact, single-line
// flag=value list for `preset list` (omitting unset/false fields).
func presetSummary(p preset.Preset) string {
	parts := []string{}
	if p.Type != "" {
		parts = append(parts, "type="+p.Type)
	}
	if p.Model != "" {
		parts = append(parts, "model="+p.Model)
	}
	if p.PermissionMode != "" {
		parts = append(parts, "permission-mode="+p.PermissionMode)
	}
	if p.AutoRestart {
		parts = append(parts, "auto-restart")
	}
	if p.Worktree {
		parts = append(parts, "worktree")
	}
	if p.InRepo {
		parts = append(parts, "in-repo")
	}
	if len(parts) == 0 {
		return "(empty)"
	}
	out := parts[0]
	for _, s := range parts[1:] {
		out += " " + s
	}
	return out
}

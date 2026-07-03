package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/srjn45/warden/internal/memory"
)

// memoryStore resolves the memory.Store `wd memory` reads. It is a package var so
// tests can inject a repo-root resolver and avoid a real git repo — mirroring how
// review.go makes reviewBackend a stub-able var.
var memoryStore = func() *memory.Store { return &memory.Store{} }

// openEditor launches $VISUAL/$EDITOR (falling back to vi) on path, wired to the
// command's stdio. A package var so the --edit test can resolve the editor without
// spawning a real one.
var openEditor = func(cmd *cobra.Command, path string) error {
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	ec := exec.CommandContext(cmd.Context(), editor, path)
	ec.Stdin = cmd.InOrStdin()
	ec.Stdout = cmd.OutOrStdout()
	ec.Stderr = cmd.ErrOrStderr()
	if err := ec.Run(); err != nil {
		if isCommandNotFound(err) {
			return fmt.Errorf("editor %q not found — set $EDITOR to your editor", editor)
		}
		return err
	}
	return nil
}

func newMemoryCmd() *cobra.Command {
	var edit, raw, pathOnly bool
	cmd := &cobra.Command{
		Use:   "memory",
		Short: "Show or edit this repo's warden project memory (.warden/memory.md)",
		Long: "Show or edit warden's project memory for the current repo — the committed,\n" +
			"backend-neutral .warden/memory.md (beside .warden/check.yml) holding durable,\n" +
			"cross-agent facts: where things live, how to run X, project invariants. Keeping\n" +
			"them here means the NEXT agent (any backend) doesn't re-pay the rediscovery tax.\n\n" +
			"The file is keyed implicitly by the repo root (git rev-parse --show-toplevel) and\n" +
			"auto-created on first use — no `wd init`, no registration. warden READS but never\n" +
			"rewrites your CLAUDE.md / AGENTS.md / CONVENTIONS.md; this file is warden's own.\n\n" +
			"With no flags it prints the resolved path and the budgeted, navigational view of\n" +
			"the memory (the same projection a future release will inject at agent launch).\n" +
			"Use --raw to print the file verbatim, --path for just the resolved path (handy in\n" +
			"scripts), and --edit to open it in $EDITOR (auto-creating it first if missing).\n\n" +
			"This verb is CLI-local (like `wd check` / `wd review`): it reads/writes the file\n" +
			"directly with no daemon round-trip. Note: launch-time projection into agents and\n" +
			"auto-curation from fleet digests are deliberately deferred to later changes — this\n" +
			"is the reader only.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("resolve working directory: %w", err)
			}
			store := memoryStore()

			// --path resolves without auto-creating, so a script can learn the path
			// without side effects.
			if pathOnly {
				path, err := store.Locate(context.Background(), dir)
				if err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), path)
				return nil
			}

			// All other modes resolve-and-auto-create so the file always exists after.
			path, created, err := store.Resolve(context.Background(), dir)
			if err != nil {
				return err
			}

			if edit {
				return openEditor(cmd, path)
			}

			data, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("read %q: %w", path, err)
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "# %s", path)
			if created {
				fmt.Fprint(out, " (auto-created)")
			}
			fmt.Fprintln(out)

			if raw {
				fmt.Fprint(out, string(data))
				if len(data) > 0 && data[len(data)-1] != '\n' {
					fmt.Fprintln(out)
				}
				return nil
			}

			projection := memory.Parse(string(data)).RenderDefault()
			if projection == "" {
				fmt.Fprintln(out, "(no memory entries yet — add facts with --edit, or write them as `- ` bullets)")
				return nil
			}
			fmt.Fprintln(out, projection)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&edit, "edit", "e", false, "open the memory file in $EDITOR (auto-creates it first)")
	cmd.Flags().BoolVar(&raw, "raw", false, "print the file verbatim instead of the rendered view")
	cmd.Flags().BoolVar(&pathOnly, "path", false, "print just the resolved file path (scriptable; no auto-create)")
	return cmd
}

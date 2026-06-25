package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/store"
)

func newExportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Serialize agent session metadata to JSON on stdout",
		Long: "Dump active agent records as a JSON envelope on stdout, for backup, " +
			"sharing, or migration. Metadata only — worktrees, branches, and tmux " +
			"sessions are NOT serialized and `warden import` does not recreate them.\n\n" +
			"With --all the archived (closed) records are included too.\n\n" +
			"  warden export --all > backup.json",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			all, _ := cmd.Flags().GetBool("all")
			cl := clientFor(cmd)
			ctx := cmd.Context()

			sessions, err := cl.List(ctx)
			if err != nil {
				return err
			}
			if all {
				closed, err := cl.History(ctx, client.HistoryParams{})
				if err != nil {
					return err
				}
				sessions = append(sessions, closed...)
			}
			if sessions == nil {
				sessions = []*store.Session{}
			}
			env := store.Export{
				Version:    store.ExportVersion,
				ExportedAt: time.Now().UTC(),
				Sessions:   sessions,
			}
			return printJSON(cmd.OutOrStdout(), env)
		},
	}
	cmd.Flags().Bool("all", false, "also include archived (closed) agents")
	return cmd
}

func newImportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Insert agent session metadata from a JSON dump on stdin",
		Long: "Read a `warden export` envelope from stdin and insert its records into " +
			"the store. Metadata only: worktrees and tmux sessions are NOT recreated " +
			"— an imported record just remembers where its (now absent) worktree used " +
			"to live.\n\n" +
			"Idempotent by id: a record whose id already exists is skipped, so " +
			"re-importing the same dump is a no-op. Pass --merge to overwrite " +
			"colliding records with the imported data instead.\n\n" +
			"  warden import < backup.json",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			merge, _ := cmd.Flags().GetBool("merge")
			out := cmd.OutOrStdout()

			data, err := io.ReadAll(cmd.InOrStdin())
			if err != nil {
				return fmt.Errorf("read stdin: %w", err)
			}
			var env store.Export
			if err := json.Unmarshal(data, &env); err != nil {
				return fmt.Errorf("parse import JSON: %w", err)
			}

			res, err := clientFor(cmd).Import(cmd.Context(), &env, merge)
			if err != nil {
				return err
			}
			if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
				return printJSON(out, res)
			}
			fmt.Fprintf(out, "imported %d, merged %d, skipped %d",
				len(res.Imported), len(res.Merged), len(res.Skipped))
			if len(res.Renamed) > 0 {
				fmt.Fprintf(out, ", renamed %d (name collision)", len(res.Renamed))
			}
			fmt.Fprintln(out)
			return nil
		},
	}
	cmd.Flags().Bool("merge", false, "overwrite existing records on id collision (default: skip)")
	cmd.Flags().Bool("json", false, "output the import result as JSON")
	return cmd
}

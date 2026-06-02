package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/srajanpathak/agentctl/internal/store"
)

func newLsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List all active agent sessions",
		RunE: func(cmd *cobra.Command, args []string) error {
			sessions, err := clientFor(cmd).List(cmd.Context())
			if err != nil {
				return err
			}
			if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
				if sessions == nil {
					sessions = []*store.Session{}
				}
				return printJSON(cmd.OutOrStdout(), sessions)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tTYPE\tSTATUS\tAGE\tDIR\tSUBJECT")
			for _, s := range sessions {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
					s.ID, typeOrPending(s.Type), s.Status, age(s.UpdatedAt), dirName(s.Workdir), s.Subject)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

func newStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status <TICKET>",
		Short: "Show full status for one session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := clientFor(cmd).Get(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
				return printJSON(cmd.OutOrStdout(), s)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "id:       %s\ntype:     %s\nticket:   %s\nstatus:   %s\nrepo:     %s\nworkdir:  %s\nworktree: %s\nbranch:   %s\npr:       %s\nsubject:  %s\nclaude:   %s\nupdated:  %s\n",
				s.ID, typeOrPending(s.Type), s.Ticket, s.Status, s.Repo, s.Workdir, s.Worktree, s.Branch, s.PR, s.Subject, s.ClaudeSessionID, s.UpdatedAt.Format(time.RFC3339))
			fmt.Fprintln(out, "events:")
			for _, e := range s.Events {
				fmt.Fprintf(out, "  %s  %-14s %s\n", e.TS.Format("15:04:05"), e.Type, e.Detail)
			}
			return nil
		},
	}
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

func age(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t).Round(time.Minute)
	if d < time.Minute {
		return "<1m"
	}
	return d.String()
}

func dirName(workdir string) string {
	if workdir == "" {
		return "—"
	}
	return filepath.Base(workdir)
}

func typeOrPending(t store.Type) string {
	if t == "" {
		return "…"
	}
	return string(t)
}

// printJSON writes v as indented JSON followed by a newline. Used by the
// --json flag so agents/scripts can parse agentctl output reliably.
func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

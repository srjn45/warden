package cli

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/srajanpathak/agentctl/internal/store"
)

func newLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List all active agent sessions",
		RunE: func(cmd *cobra.Command, args []string) error {
			sessions, err := clientFor(cmd).List(cmd.Context())
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tTYPE\tSTATUS\tAGE\tDETAIL")
			for _, s := range sessions {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", s.ID, s.Type, s.Status, age(s.UpdatedAt), lastDetail(s))
			}
			return tw.Flush()
		},
	}
}

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status <TICKET>",
		Short: "Show full status for one session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := clientFor(cmd).Get(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "id:       %s\ntype:     %s\nticket:   %s\nstatus:   %s\nrepo:     %s\nworktree: %s\nbranch:   %s\npr:       %s\nupdated:  %s\n",
				s.ID, s.Type, s.Ticket, s.Status, s.Repo, s.Worktree, s.Branch, s.PR, s.UpdatedAt.Format(time.RFC3339))
			fmt.Fprintln(out, "events:")
			for _, e := range s.Events {
				fmt.Fprintf(out, "  %s  %-14s %s\n", e.TS.Format("15:04:05"), e.Type, e.Detail)
			}
			return nil
		},
	}
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

func lastDetail(s *store.Session) string {
	if len(s.Events) == 0 {
		return ""
	}
	return s.Events[len(s.Events)-1].Detail
}

package cli

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/store"
)

func newSearchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "search <QUERY...>",
		Short: "Full-text search agents by subject, prompt, type, name, branch, or pane text",
		Long: "Search across active agents' searchable text (name, id, ticket, type, " +
			"subject, prompt, branch, last pane excerpt). Multiple terms are AND-ed. " +
			"Pass --closed to also search archived agents.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			closed, _ := cmd.Flags().GetBool("closed")
			jsonOut, _ := cmd.Flags().GetBool("json")
			out := cmd.OutOrStdout()
			sessions, err := clientFor(cmd).Search(cmd.Context(), client.SearchParams{
				Query:  strings.Join(args, " "),
				Closed: closed,
			})
			if err != nil {
				return err
			}
			if jsonOut {
				if sessions == nil {
					sessions = []*store.Session{}
				}
				return printJSON(out, sessions)
			}
			return renderSessions(out, sessions, isTTY(out))
		},
	}
	cmd.Flags().Bool("closed", false, "also search archived (closed) agents")
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

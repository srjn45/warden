package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/client"
)

func newCollabCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "collab",
		Short: "Inter-agent collaboration: see which agents are editing the same files",
	}
	cmd.AddCommand(newCollabConflictsCmd(), newCollabWhoIsEditingCmd())
	return cmd
}

func newCollabConflictsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "conflicts",
		Short: "List files currently being edited by more than one agent",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			conflicts, err := clientFor(cmd).CollabConflicts(cmd.Context())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(conflicts) == 0 {
				fmt.Fprintln(out, "No file conflicts.")
				return nil
			}
			fmt.Fprintf(out, "File conflicts (%d):\n\n", len(conflicts))
			for _, c := range conflicts {
				fmt.Fprintln(out, c.File)
				for _, a := range c.Agents {
					fmt.Fprintf(out, "  - %s\n", agentLabel(a))
				}
				fmt.Fprintln(out)
			}
			return nil
		},
	}
}

func newCollabWhoIsEditingCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "who-is-editing <file>",
		Short: "Show which agents are editing a specific file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			file := args[0]
			conflicts, err := clientFor(cmd).CollabConflicts(cmd.Context())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, c := range conflicts {
				if c.File == file {
					fmt.Fprintf(out, "Agents editing %s:\n", file)
					for _, a := range c.Agents {
						fmt.Fprintf(out, "  - %s\n", agentLabel(a))
					}
					return nil
				}
			}
			fmt.Fprintf(out, "No other agent is editing %s.\n", file)
			return nil
		},
	}
}

func agentLabel(a client.ConflictAgent) string {
	if a.Name != "" {
		return fmt.Sprintf("%s (%s)", a.ID, a.Name)
	}
	return a.ID
}

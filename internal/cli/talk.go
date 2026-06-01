package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

func newSendCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "send <TICKET> <message...>",
		Short: "Type a message into an agent's claude session and press Enter",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, msg := args[0], strings.Join(args[1:], " ")
			if err := clientFor(cmd).Input(cmd.Context(), id, msg); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "sent to %s\n", id)
			return nil
		},
	}
}

func newTailCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tail <TICKET>",
		Short: "Print the recent output of an agent's claude session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lines, _ := cmd.Flags().GetInt("lines")
			out, err := clientFor(cmd).Output(cmd.Context(), args[0], lines)
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), out)
			return nil
		},
	}
	cmd.Flags().Int("lines", 200, "number of pane lines to capture")
	return cmd
}

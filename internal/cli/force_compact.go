package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newForceCompactCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "force-compact <agent-id> <on|off|inherit>",
		Short: "Override force-compact for one agent (interrupt → /compact → resume)",
		Long: `Set the per-agent force-compact override.

When force-compact is on and an agent's context crosses the critical threshold
while it is still working, warden interrupts the agent (Escape), runs /compact
once it goes idle, then sends the configured resume prompt so it picks its work
back up. This is destructive: the interrupt discards the agent's in-flight turn.

States:
  on       force-compact this agent (overrides the global setting)
  off      never force-compact this agent (overrides the global setting)
  inherit  clear the override; follow the global token_force_compact setting

Examples:
  warden force-compact abc123 on       # always force-compact agent abc123
  warden force-compact abc123 off      # never force-compact agent abc123
  warden force-compact abc123 inherit  # follow the global default

The global default is the token_force_compact config setting (off by default).`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			var state string
			switch args[1] {
			case "on", "1", "true":
				state = "on"
			case "off", "0", "false":
				state = "off"
			case "inherit", "default", "clear":
				state = "inherit"
			default:
				return fmt.Errorf("state must be 'on', 'off', or 'inherit', got %q", args[1])
			}

			c := clientFor(cmd)
			if err := c.SetForceCompact(cmd.Context(), id, state); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "force-compact %s for %s\n", state, id)
			return nil
		},
	}
}

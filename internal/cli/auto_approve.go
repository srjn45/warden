package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newAutoApproveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "auto-approve <agent-id> <on|off>",
		Short: "Enable or disable auto-approval for an agent's prompts",
		Long: `Enable or disable automatic approval of yes/no prompts for a specific agent.

When auto-approve is enabled, the daemon automatically selects option 1 for
recognized yes/no tool-permission prompts. Unrecognized prompts, multi-select,
and text-entry fields are skipped and require manual approval.

Examples:
  warden auto-approve abc123 on   # Enable auto-approve for agent abc123
  warden auto-approve abc123 off  # Disable auto-approve for agent abc123

Global default is controlled by the auto_approve config setting.
Per-agent setting overrides the global default.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			mode := args[1]

			var enabled bool
			switch mode {
			case "on", "1", "true":
				enabled = true
			case "off", "0", "false":
				enabled = false
			default:
				return fmt.Errorf("mode must be 'on' or 'off', got %q", mode)
			}

			c := clientFor(cmd)
			if err := c.SetAutoApprove(cmd.Context(), id, enabled); err != nil {
				return err
			}

			status := "disabled"
			if enabled {
				status = "enabled"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "auto-approve %s for %s\n", status, id)
			return nil
		},
	}
}

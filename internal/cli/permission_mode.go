package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newSetPermissionModeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set-permission-mode <agent-id> <mode>",
		Short: "Set the permission mode for an agent",
		Long: `Set the permission mode for a specific agent.

Valid permission modes:
  acceptEdits        - Prompt for tool permissions (supervised mode)
  auto               - Default behavior
  bypassPermissions  - Skip all permission prompts
  default            - Use global default from config
  dontAsk            - Don't ask for permissions
  plan               - Plan mode

The permission mode controls how Claude handles tool permission prompts.
Setting to "default" (or empty string) clears the agent-specific override
and uses the global WARDEN_DEFAULT_PERMISSION_MODE config.

Examples:
  warden set-permission-mode abc123 acceptEdits  # Enable supervised mode
  warden set-permission-mode abc123 auto         # Use auto mode
  warden set-permission-mode abc123 default      # Use global default`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			mode := args[1]

			// Validate mode
			validModes := []string{"acceptEdits", "auto", "bypassPermissions", "default", "dontAsk", "plan"}
			valid := false
			for _, m := range validModes {
				if mode == m {
					valid = true
					break
				}
			}
			if !valid {
				return fmt.Errorf("invalid permission mode %q, must be one of: acceptEdits, auto, bypassPermissions, default, dontAsk, plan", mode)
			}

			c := clientFor(cmd)
			if err := c.SetPermissionMode(cmd.Context(), id, mode); err != nil {
				return err
			}

			fmt.Fprintf(cmd.OutOrStdout(), "permission mode set to %q for %s\n", mode, id)
			return nil
		},
	}
}

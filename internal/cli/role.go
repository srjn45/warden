package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/role"
)

// newSetRoleCmd switches a running agent's built-in role. It validates the name
// against the registry, persists it (empty/general clears the persona), and the
// daemon relaunches the agent so the new persona re-injects — mirroring
// set-permission-mode, but with a relaunch since a persona only takes effect at
// (re)launch.
func newSetRoleCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set-role <agent-id> <role>",
		Short: "Switch an agent's built-in role (relaunches to re-inject the persona)",
		Long: `Switch a running agent's built-in role.

The role's persona is injected as a system-prompt addendum; changing it relaunches
the agent (its current turn is discarded) so the new persona takes effect. Set the
role to "general" (or "") to clear the persona and behave like a plain agent.

Valid roles (see ` + "`warden role list`" + ` for descriptions):
  general | orchestrator | implementer | auto-merger | reviewer

Examples:
  warden set-role abc123 reviewer      # give the agent the reviewer persona
  warden set-role abc123 general       # clear the persona`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			name := args[1]
			r, ok := role.Get(name)
			if !ok {
				return fmt.Errorf("unknown role %q (valid: %s)", name, strings.Join(role.Names(), ", "))
			}
			if err := clientFor(cmd).SetRole(cmd.Context(), id, r.Name); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "role set to %q for %s\n", r.Name, id)
			return nil
		},
	}
}

// newRoleCmd groups the read-side role verbs (currently just `list`). The role set
// is a fixed built-in catalog, so `list` is driven straight off the registry.
func newRoleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "role",
		Short: "Inspect warden's built-in agent roles",
	}
	cmd.AddCommand(newRoleListCmd())
	return cmd
}

func newRoleListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the built-in agent roles and their descriptions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ROLE\tDESCRIPTION")
			for _, r := range role.All() {
				desc := r.Description
				if desc == "" {
					desc = "(no persona; behaves like a plain agent)"
				}
				fmt.Fprintf(w, "%s\t%s\n", r.Name, desc)
			}
			return w.Flush()
		},
	}
}

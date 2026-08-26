package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/backendstore"
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

// newRoleCmd groups the role inspection and tier management verbs.
func newRoleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "role",
		Short: "Inspect warden's built-in agent roles and tier mappings",
	}
	cmd.AddCommand(
		newRoleListCmd(),
		newRoleTierCmd(),
		newRoleSetTierCmd(),
	)
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

func newRoleTierCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "tier",
		Short: "Inspect and manage role-to-tier mappings",
		Long: `Inspect and manage default model tier mappings for agent roles.

Subcommands:
  list    List all role-to-tier mappings
  set     Set the default model tier for a role

When run without subcommands, ` + "`warden role tier`" + ` lists all mappings.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := openBackendStore(cmd)
			if err != nil {
				return err
			}
			defer st.Close()

			mappings, err := st.ListRoleTiers()
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if asJSON {
				if mappings == nil {
					mappings = []backendstore.RoleTierMapping{}
				}
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(mappings)
			}

			w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "ROLE\tDEFAULT TIER")
			for _, m := range mappings {
				fmt.Fprintf(w, "%s\t%s\n", m.RoleName, m.DefaultTier)
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit role tier mappings as a JSON array")

	cmd.AddCommand(newRoleTierListCmd())
	return cmd
}

func newRoleTierListCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "list",
		Short:   "List agent roles and their default model tiers",
		Aliases: []string{"ls"},
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := openBackendStore(cmd)
			if err != nil {
				return err
			}
			defer st.Close()

			mappings, err := st.ListRoleTiers()
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if asJSON {
				if mappings == nil {
					mappings = []backendstore.RoleTierMapping{}
				}
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(mappings)
			}

			w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "ROLE\tDEFAULT TIER")
			for _, m := range mappings {
				fmt.Fprintf(w, "%s\t%s\n", m.RoleName, m.DefaultTier)
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit role tier mappings as a JSON array")
	return cmd
}

func newRoleSetTierCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set-tier <role> <tier>",
		Short: "Set the default model tier for an agent role (tier-1|tier-2|tier-3)",
		Long: `Set the default model tier assigned when creating agents with this role.

Tiers:
  tier-1   Highest-capability models (e.g. Claude Opus, o1) for architecture, design, and complex planning
  tier-2   Standard implementation models (e.g. Claude Sonnet, Gemini Pro, GPT-4.1) for everyday coding
  tier-3   Fast, low-cost models (e.g. Claude Haiku, Gemini Flash, GPT-4.1-mini) for quick tasks and CI triage

Example:
  warden role set-tier implementation tier-2
  warden role set-tier architecture tier-1`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			roleName := args[0]
			tierStr := args[1]

			tier := backendstore.ModelTier(tierStr)
			if !tier.Valid() {
				return fmt.Errorf("invalid tier %q (valid: tier-1, tier-2, tier-3)", tierStr)
			}

			st, err := openBackendStore(cmd)
			if err != nil {
				return err
			}
			defer st.Close()

			if err := st.SetRoleTier(roleName, tier); err != nil {
				return err
			}

			fmt.Fprintf(cmd.OutOrStdout(), "role %q default tier set to %s\n", roleName, tier)
			return nil
		},
	}
}

package cli

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/client"
)

// newBackendsCmd is the umbrella over the agent-backend registry (docs/specs/
// 2026-08-06-backend-registry.md §9). The store — persisted by the daemon — is
// warden's source of truth for which CLI backends exist, their billing tier, the
// single default, and whether each is enabled.
// Every subcommand is a thin caller of the Stage-2 daemon endpoints (§6).
func newBackendsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backends",
		Short: "Inspect and manage the agent-backend registry",
		Long: `Inspect and manage warden's agent-backend registry.

warden detects the coding-agent CLIs installed on this machine (claude, codex,
aider, …), and persists each with a billing tier, an enabled flag, and at most
one default. The daemon's store is the source of truth — autopilot's cost-tier
ladder reads from it.

Tiers:   free | subscription | pay_per_use | unclassified

Examples:
  warden backends list                 # full table of detected backends
  warden backends rescan               # re-detect installed CLIs, print the table
  warden backends tier codex free      # tier codex as a $0 backend
  warden backends default claude       # make claude the default backend
  warden backends disable aider        # stop using a backend`,
	}
	cmd.AddCommand(
		newBackendsListCmd(),
		newBackendsRescanCmd(),
		newBackendsTierCmd(),
		newBackendsDefaultCmd(),
		newBackendsEnableCmd(),
		newBackendsDisableCmd(),
	)
	return cmd
}

func newBackendsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List detected backends (installed, tier, default, enabled, limited)",
		Long: `List every backend in the registry, with its
installed state, billing tier, whether it is the default, whether it is enabled,
and whether it is currently rate-limited. The current internal-thinking mode is
printed below the table.`,
		Aliases: []string{"ls"},
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			state, err := clientFor(cmd).ListBackends(cmd.Context())
			if err != nil {
				return err
			}
			return printBackends(cmd, state)
		},
	}
}

func newBackendsRescanCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rescan",
		Short: "Re-detect installed backend CLIs and print the updated table",
		Long: `Re-run backend detection and reconcile the registry: newly installed CLIs are
added and no-longer-present ones are marked uninstalled, while each backend's
tier, default, and enabled preferences are preserved. Prints the refreshed table.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			state, err := clientFor(cmd).RescanBackends(cmd.Context())
			if err != nil {
				return err
			}
			return printBackends(cmd, state)
		},
	}
}

func newBackendsTierCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tier <id> <tier>",
		Short: "Set a backend's billing tier (free|subscription|pay_per_use|unclassified)",
		Long: `Set a backend's billing tier. The tier drives autopilot's cost-tier ladder and
the internal free/local thinking router (only free backends are called for
internal thinking).

Valid tiers:
  free           # a $0 backend (free plan)
  subscription   # covered by a flat subscription
  pay_per_use    # metered / pay-as-you-go
  unclassified   # not yet tiered (treated as not free)

The reserved ` + "`local`" + ` tier is system-set and cannot be assigned.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, tier := args[0], args[1]
			if !validTier(tier) {
				return fmt.Errorf("invalid tier %q (valid: %s)", tier, strings.Join(assignableTiers, ", "))
			}
			b, err := clientFor(cmd).SetBackendTier(cmd.Context(), id, tier)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "backend %s tiered as %s\n", b.ID, b.Tier)
			return nil
		},
	}
}

func newBackendsDefaultCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "default <id>",
		Short: "Set the default backend (rejects the reserved local row)",
		Long: `Make <id> the single default backend used when spawning agents without an
explicit backend. The daemon rejects an unknown, uninstalled, disabled, or
reserved (local) target.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			state, err := clientFor(cmd).SetDefaultBackend(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "default backend set to %s\n\n", args[0])
			return printBackends(cmd, state)
		},
	}
}

func newBackendsEnableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "enable <id>",
		Short: "Enable a backend so it may be used",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return setBackendEnabled(cmd, args[0], true)
		},
	}
}

func newBackendsDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disable <id>",
		Short: "Disable a backend so it is no longer used",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return setBackendEnabled(cmd, args[0], false)
		},
	}
}

func setBackendEnabled(cmd *cobra.Command, id string, enabled bool) error {
	b, err := clientFor(cmd).SetBackendEnabled(cmd.Context(), id, enabled)
	if err != nil {
		return err
	}
	state := "disabled"
	if b.Enabled {
		state = "enabled"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "backend %s %s\n", b.ID, state)
	return nil
}

// assignableTiers are the user-assignable billing tiers (the reserved `local`
// tier is excluded — it is system-set on the local row).
var assignableTiers = []string{
	backendstore.TierFree,
	backendstore.TierSubscription,
	backendstore.TierPayPerUse,
	backendstore.TierUnclassified,
}

func validTier(t string) bool { return slices.Contains(assignableTiers, t) }

// printBackends renders the registry as a table (id, installed, tier, default,
// enabled, limited). Rows are sorted id-ascending.
func printBackends(cmd *cobra.Command, state client.BackendsState) error {
	out := cmd.OutOrStdout()
	rows := append([]client.Backend(nil), state.Backends...)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })

	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tINSTALLED\tTIER\tDEFAULT\tENABLED\tLIMITED")
	for _, b := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			b.ID,
			checkMark(b.Installed),
			b.Tier,
			checkMark(b.Default),
			checkMark(b.Enabled),
			limitedCell(b.LimitedUntil),
		)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	return nil
}

func checkMark(v bool) string {
	if v {
		return "✓"
	}
	return "-"
}

// limitedCell shows how long a backend stays rate-limited, or "-" when it is not
// currently limited. The local row is never limited.
func limitedCell(until time.Time) string {
	if until.IsZero() {
		return "-"
	}
	rem := time.Until(until)
	if rem <= 0 {
		return "-"
	}
	return fmt.Sprintf("%s (%s)", rem.Round(time.Second), until.Format(time.Kitchen))
}

package cli

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/srjn45/warden/internal/client"
)

// newCostCmd is the unified "cost" umbrella over warden's two financial views:
// SPEND (the real dollars agents billed Claude, measured from transcripts) and
// SAVINGS (the tokens — and the dollars they represent — warden kept OUT of
// context). It adds no storage, pricing, or format of its own — it is a single
// discoverable entry point that reuses the existing spend rollup and savings
// ledger and their render helpers. `wd cost` with no subcommand prints a combined
// at-a-glance summary of both axes; `wd cost spend` and `wd cost savings` are the
// very same commands as the top-level `wd spend` and `wd savings`, which remain
// available and unchanged. (Resource footprint — memory/CPU/pressure — is a
// different axis and stays under `wd stats`.)
func newCostCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cost",
		Short: "Show warden's money picture: dollars spent and tokens/$ saved, in one place",
		Long: "One umbrella over warden's two financial views:\n" +
			"  • SPEND   — the REAL dollars agents billed Claude, measured from their transcripts (`wd cost spend`)\n" +
			"  • SAVINGS — the tokens, and the dollars they represent, warden kept OUT of context (`wd cost savings`)\n\n" +
			"`wd cost` with no subcommand prints a combined at-a-glance summary of both axes. " +
			"`wd cost spend` and `wd cost savings` are the same commands as the top-level `wd spend` and " +
			"`wd savings`, which remain available and unchanged. Both views are gated by the `savings` " +
			"config setting. Resource footprint — memory/CPU/pressure — is a different axis; see `wd stats`.",
		Args: cobra.NoArgs,
		RunE: runCostSummary,
	}
	cmd.AddCommand(newSpendCmd(), newSavingsCmd())
	return cmd
}

// runCostSummary renders the no-subcommand `wd cost` view: a combined snapshot of
// both financial axes under two labeled sections. It reuses the spend rollup +
// formatSpend and the savings ledger + formatSavings verbatim, so the figures and
// wording never disagree with the standalone `wd spend` / `wd savings`. Each side
// is fetched independently and a disabled feature (403) degrades to its own hint
// rather than failing the whole summary.
func runCostSummary(cmd *cobra.Command, _ []string) error {
	out := cmd.OutOrStdout()
	c := clientFor(cmd)

	fmt.Fprintln(out, "SPEND — dollars agents billed Claude")
	rep, err := c.Spend(cmd.Context())
	switch {
	case featureDisabled(err):
		fmt.Fprintln(out, "  spend tracking is disabled — enable it with `savings: true` in the config file")
	case err != nil:
		return err
	default:
		fmt.Fprint(out, formatSpend(rep, ""))
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "SAVINGS — tokens warden kept out of context")
	sum, err := c.Savings(cmd.Context(), time.Time{}, "", false)
	switch {
	case featureDisabled(err):
		fmt.Fprintln(out, "  savings ledger is disabled — enable it with `savings: true` in the config file")
	case err != nil:
		return err
	default:
		fmt.Fprint(out, formatSavings(sum, ""))
	}
	return nil
}

// featureDisabled reports whether err is the daemon's 403 for a gated feature
// (spend / savings turned off in config), so the combined cost summary can show a
// per-section hint instead of aborting the whole view.
func featureDisabled(err error) bool {
	var se *client.StatusError
	return errors.As(err, &se) && se.Code == http.StatusForbidden
}

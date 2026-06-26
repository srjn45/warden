package cli

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/savings"
)

// formatSavings renders the savings summary as a human report: a headline line
// (cumulative tokens + dollars + reduction %), then a per-feature table sorted
// biggest-win-first. The window line names the period so a screenshot is
// self-describing. Empty ledger reads as an explicit "nothing recorded yet"
// rather than a blank table.
func formatSavings(sum *savings.Summary, sinceStr string) string {
	var b strings.Builder
	window := "all time"
	if sinceStr != "" {
		window = "since " + sinceStr
	}
	if sum.Events == 0 {
		fmt.Fprintf(&b, "no savings recorded yet (%s)\n", window)
		fmt.Fprintf(&b, "warden records a saving each time a lifecycle feature keeps tokens out of an agent's context — run `wd check` in a project with a .warden/check.yml to start the ledger.\n")
		return b.String()
	}
	fmt.Fprintf(&b, "token savings (%s) — priced at $%.0f/M input tokens\n", window, savings.PricePerMTok)
	fmt.Fprintf(&b, "  %s tokens saved · $%.2f · %.1f%% of would-be context spend eliminated · %d events\n",
		humanCount(sum.SavedTokens), sum.SavedDollars, sum.ReductionPct, sum.Events)
	fmt.Fprintf(&b, "%-14s %10s %10s %8s\n", "FEATURE", "SAVED", "RAW", "EVENTS")
	for _, f := range sum.Features {
		fmt.Fprintf(&b, "%-14s %10s %10s %8d\n",
			f.Feature, humanCount(f.SavedTokens), humanCount(f.RawTokens), f.Events)
	}
	return b.String()
}

// humanCount renders a token count compactly: 12 / 3.4k / 1.2M. Savings figures
// span single tokens (a tiny check) to millions (a fleet over weeks), so a plain
// integer reads poorly at the top end.
func humanCount(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func newSavingsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "savings",
		Short: "Show the token reductions warden's lifecycle features have earned",
		Long: "Report the measured token savings warden has recorded — the raw output its lifecycle " +
			"features (starting with `wd check`) kept out of agents' context windows — as a per-feature " +
			"breakdown with cumulative tokens, an estimated dollar figure, and the percentage of would-be " +
			"context spend eliminated. The data is a real, append-only ledger, not an estimate: a proof " +
			"point you can screenshot. Gated by the `savings` config setting.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			sinceStr, _ := cmd.Flags().GetString("since")
			jsonOut, _ := cmd.Flags().GetBool("json")
			out := cmd.OutOrStdout()

			since, err := parseSince(sinceStr, time.Now())
			if err != nil {
				return err
			}
			sum, err := clientFor(cmd).Savings(cmd.Context(), since)
			if err != nil {
				// A disabled ledger (403) is operator config, not a failure — point
				// at the switch rather than dumping a raw HTTP error.
				var se *client.StatusError
				if errors.As(err, &se) && se.Code == http.StatusForbidden {
					return fmt.Errorf("savings ledger is disabled — enable it with `savings: true` in the config file")
				}
				return err
			}
			if jsonOut {
				return printJSON(out, sum)
			}
			fmt.Fprint(out, formatSavings(sum, strings.TrimSpace(sinceStr)))
			return nil
		},
	}
	cmd.Flags().String("since", "", "only count savings since this window (24h, 7d, 2w) or date (2006-01-02 / RFC3339)")
	cmd.Flags().Bool("json", false, "output the structured summary as JSON")
	return cmd
}

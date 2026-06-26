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

// formatSavings renders the savings summary as a human report: two honest
// headline claims — the context axis (how much leaner agent context stayed, in %
// and $) and, separately, the offload axis (Claude work moved off entirely, in
// $) — then a per-feature table sorted biggest-win-first. The two axes are never
// blended into a single percentage. The window line names the period so a
// screenshot is self-describing. Empty ledger reads as an explicit "nothing
// recorded yet" rather than a blank table.
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
	contextEvents := sum.Events - sum.OffloadedEvents
	fmt.Fprintf(&b, "token savings (%s) — input $%.0f/M, output $%.0f/M (Opus)\n", window, savings.PricePerMTok, savings.OutputPricePerMTok)
	if contextEvents > 0 {
		fmt.Fprintf(&b, "  agent context kept %.1f%% leaner · $%.2f saved · %d events\n",
			sum.ContextReductionPct, sum.ContextSavedDollars, contextEvents)
	}
	if sum.OffloadedEvents > 0 {
		fmt.Fprintf(&b, "  + $%.2f of Claude work offloaded entirely (%d calls; output volume assumed, not measured)\n",
			sum.OffloadedDollars, sum.OffloadedEvents)
	}
	fmt.Fprintf(&b, "%-14s %10s %10s %8s\n", "FEATURE", "SAVED", "RAW", "EVENTS")
	for _, f := range sum.Features {
		fmt.Fprintf(&b, "%-14s %10s %10s %8d\n",
			f.Feature, humanCount(f.SavedTokens), humanCount(f.RawTokens), f.Events)
	}
	return b.String()
}

// formatBenchmark renders the ledger as the headline A/B proof point. The
// without/with block is restricted to the CONTEXT axis — the counterfactual
// ("without warden", the raw tokens that would have entered Claude) versus the
// measured reality ("with warden", what actually did) — because the offload axis
// keeps nothing (kept==0) and would distort a without/with framing into a
// meaningless ∞×. Offload is reported honestly on its own line below the verdict.
// It reframes the same totals formatSavings tables, so the two views never
// disagree; this one is built to screenshot and sell. Empty ledger reuses the
// same "nothing yet" guidance.
func formatBenchmark(sum *savings.Summary, sinceStr string) string {
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
	fmt.Fprintf(&b, "warden A/B — %s · %d events · input $%.0f/M, output $%.0f/M (Opus)\n\n", window, sum.Events, savings.PricePerMTok, savings.OutputPricePerMTok)
	if contextEvents := sum.Events - sum.OffloadedEvents; contextEvents > 0 {
		rawDollars := dollarsFor(sum.ContextRawTokens)
		keptDollars := dollarsFor(sum.ContextKeptTokens)
		fmt.Fprintf(&b, "  without warden   %8s tokens   $%8.2f   would have entered Claude\n", humanCount(sum.ContextRawTokens), rawDollars)
		fmt.Fprintf(&b, "  with warden      %8s tokens   $%8.2f   actually did\n", humanCount(sum.ContextKeptTokens), keptDollars)
		fmt.Fprintf(&b, "  %s\n", strings.Repeat("─", 56))
		fmt.Fprintf(&b, "  %.1f%% less context · %s leaner · $%.2f saved\n",
			sum.ContextReductionPct, leanFactor(sum.ContextRawTokens, sum.ContextKeptTokens), sum.ContextSavedDollars)
	}
	if sum.OffloadedEvents > 0 {
		fmt.Fprintf(&b, "  + $%.2f of Claude work offloaded entirely (%d calls, %s input tokens; output volume assumed, not measured)\n",
			sum.OffloadedDollars, sum.OffloadedEvents, humanCount(sum.OffloadedTokens))
	}
	// When real spend has been measured, lead with the most persuasive framing: the
	// saving as a share of what agents ACTUALLY billed Claude (saved ÷ (saved+spend)),
	// grounded in observed transcript usage rather than only the counterfactual. No
	// spend data ⇒ this line is omitted and the context-reduction verdict stands.
	if pct, ok := spendCutPct(sum.SavedTokens, sum.MeasuredSpend); ok {
		fmt.Fprintf(&b, "  cut measured Claude spend ~%.1f%% (saved %s of %s billed+saved tokens)\n",
			pct, humanCount(sum.SavedTokens), humanCount(sum.SavedTokens+sum.MeasuredSpend))
	}
	if len(sum.Features) > 0 {
		fmt.Fprintf(&b, "\ndriven by:\n")
		for _, f := range sum.Features {
			share := 0.0
			if sum.SavedTokens > 0 {
				share = float64(f.SavedTokens) / float64(sum.SavedTokens) * 100
			}
			fmt.Fprintf(&b, "  %-12s %8s saved (%.0f%%)\n", f.Feature, humanCount(f.SavedTokens), share)
		}
	}
	return b.String()
}

// spendCutPct expresses the saving as a share of real measured spend:
// saved ÷ (saved + measuredSpend) × 100 — the portion of what agents would
// otherwise have billed Claude (the saved tokens plus the spend that still
// landed) that warden eliminated. ok=false when no spend was measured
// (measuredSpend <= 0) or nothing was saved, so the caller falls back to the
// context-reduction wording rather than printing a 0% or divide-by-zero figure.
func spendCutPct(saved, measuredSpend int) (float64, bool) {
	if measuredSpend <= 0 || saved <= 0 {
		return 0, false
	}
	denom := saved + measuredSpend
	return float64(saved) / float64(denom) * 100, true
}

// dollarsFor prices a token count at the same input rate the ledger uses, so the
// A/B "without"/"with" dollar columns are consistent with SavedDollars.
func dollarsFor(tokens int) float64 { return float64(tokens) * savings.PricePerMTok / 1_000_000 }

// leanFactor expresses how many times more tokens agents would have burned
// without warden (raw ÷ kept). When nothing was kept (every counted token left
// Claude entirely — e.g. a pure-offload window) the ratio is unbounded, rendered
// as "∞×" rather than a divide-by-zero.
func leanFactor(raw, kept int) string {
	if kept <= 0 {
		return "∞×"
	}
	return fmt.Sprintf("%.1f×", float64(raw)/float64(kept))
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
			if bench, _ := cmd.Flags().GetBool("benchmark"); bench {
				fmt.Fprint(out, formatBenchmark(sum, strings.TrimSpace(sinceStr)))
				return nil
			}
			fmt.Fprint(out, formatSavings(sum, strings.TrimSpace(sinceStr)))
			return nil
		},
	}
	cmd.Flags().String("since", "", "only count savings since this window (24h, 7d, 2w) or date (2006-01-02 / RFC3339)")
	cmd.Flags().Bool("json", false, "output the structured summary as JSON")
	cmd.Flags().Bool("benchmark", false, "show the headline A/B proof (without-vs-with-warden tokens, reduction %, $ saved) instead of the per-feature table")
	return cmd
}

package cli

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/config"
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
	fmt.Fprintf(&b, "%s\n", basisLine(sum))
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
	fmt.Fprintf(&b, "warden A/B — %s · %d events · input $%.0f/M, output $%.0f/M (Opus)\n", window, sum.Events, savings.PricePerMTok, savings.OutputPricePerMTok)
	fmt.Fprintf(&b, "%s\n\n", basisLine(sum))
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
	// Trend: a per-day sparkline of saved tokens so the headline shows movement,
	// not just a cumulative total. Only when the daemon returned day buckets.
	if len(sum.Buckets) > 0 {
		vals := make([]int, len(sum.Buckets))
		for i, d := range sum.Buckets {
			vals[i] = d.SavedTokens
		}
		fmt.Fprintf(&b, "\ntrend  %s  (%d day%s, saved/day)\n", sparkline(vals), len(vals), plural(len(vals)))
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

// sparkRamp is the eight-level block ramp the sparkline draws from, lowest first.
var sparkRamp = []rune("▁▂▃▄▅▆▇█")

// sparkline renders values as a one-rune-per-value unicode bar chart scaled to
// the series max (▁▂▃▄▅▆▇█). It is pure and deterministic: empty input → "",
// a flat series (incl. a single value) renders as the full block at every
// position (the max is each value), and a 1..8 ramp renders ▁..█. Non-positive
// values render as the lowest bar. Used for the savings trend in --benchmark.
func sparkline(values []int) string {
	if len(values) == 0 {
		return ""
	}
	max := 0
	for _, v := range values {
		if v > max {
			max = v
		}
	}
	var b strings.Builder
	last := len(sparkRamp) - 1
	for _, v := range values {
		idx := 0
		if max > 0 && v > 0 {
			idx = v * last / max // integer floor; v==max → last (full block)
		}
		b.WriteRune(sparkRamp[idx])
	}
	return b.String()
}

// plural returns "s" unless n is 1, for simple count phrasing.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// formatAudit renders the retained provenance samples (wd savings --audit): a few
// real raw-vs-kept pairs so a skeptic can eyeball actual bytes behind the token
// counts. Each side is clipped for the terminal (the stored sample is already
// truncated). When nothing is retained it prints why and how to turn capture on.
func formatAudit(sum *savings.Summary, sinceStr string) string {
	var b strings.Builder
	window := "all time"
	if sinceStr != "" {
		window = "since " + sinceStr
	}
	if len(sum.Samples) == 0 {
		fmt.Fprintf(&b, "no provenance samples retained (%s)\n", window)
		fmt.Fprintf(&b, "warden retains raw-vs-kept samples only when `savings_samples: true` is set in the config file (off by default — samples hold substrings of real build/test/git output). Enable it, then re-run a `wd check`/`wd commit` to capture a few.\n")
		return b.String()
	}
	fmt.Fprintf(&b, "retained provenance samples (%s) — real bytes behind the token counts\n", window)
	for i, s := range sum.Samples {
		fmt.Fprintf(&b, "\n[%d] %s\n", i+1, s.Feature)
		fmt.Fprintf(&b, "  raw  (%d bytes): %s\n", len(s.RawSample), clip(s.RawSample, 240))
		fmt.Fprintf(&b, "  kept (%d bytes): %s\n", len(s.KeptSample), clip(s.KeptSample, 240))
	}
	return b.String()
}

// clip flattens s to a single line and caps it at n runes with an ellipsis, so a
// multi-line sample stays readable in the audit table. Empty → "(empty)".
func clip(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\n", "⏎"), "\t", " "))
	if s == "" {
		return "(empty)"
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// basisLine states whether the figures rest on a calibrated, workload-measured
// bytes-per-token factor or the generic 4-bytes/token heuristic, so the basis is
// never ambiguous. Calibration is forward-only — it prices events recorded after
// `wd savings --calibrate`, while older rows keep the heuristic counts they were
// recorded with — which the wording flags so the claim stays honest.
func basisLine(sum *savings.Summary) string {
	if sum.Calibrated {
		return fmt.Sprintf("basis: CALIBRATED — %.2f bytes/token measured for this workload (Claude %s, %d samples); prices events recorded after calibration, earlier rows keep heuristic counts",
			sum.CalibratedBytesPerToken, savings.CalibrationModel, sum.CalibrationSamples)
	}
	return "basis: HEURISTIC — 4 bytes/token estimate; run `wd savings --calibrate` to measure this workload's true ratio"
}

// runCalibrate derives an empirical bytes-per-token factor for this install's
// workload and persists it. It reads the retained provenance samples straight
// from the local ledger (no daemon round-trip), counts a bounded sample against
// Claude's count_tokens endpoint, and writes the factor next to the ledger. The
// API key comes from ANTHROPIC_API_KEY via the SDK and is never printed; if it is
// unset the run exits non-zero WITHOUT touching the persisted factor, and every
// other command keeps working offline on the heuristic.
func runCalibrate(cmd *cobra.Command, sinceStr string, since time.Time, maxCalls int) error {
	out := cmd.OutOrStdout()
	cfg := config.Load(configPathFor(cmd))
	dir := filepath.Join(cfg.DataDir, "savings")

	store, err := savings.NewStore(dir)
	if err != nil {
		return err
	}
	samples, err := store.CalibrationSamples(since)
	if err != nil {
		return fmt.Errorf("read ledger samples: %w", err)
	}
	if len(samples) == 0 {
		window := "all time"
		if sinceStr != "" {
			window = "since " + sinceStr
		}
		return fmt.Errorf("no retained provenance samples to calibrate from (%s) — set `savings_samples: true` in the config file, run a few `wd check`/`wd commit` actions to populate the ledger, then re-run `wd savings --calibrate`", window)
	}
	// Construct the counter BEFORE any network use; a missing API key fails here,
	// leaving the persisted factor untouched.
	counter, err := savings.NewAnthropicCounter()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "calibrating against Claude %s count_tokens (up to %d samples)…\n", savings.CalibrationModel, maxCalls)
	cal, err := savings.DeriveCalibration(cmd.Context(), counter, samples, maxCalls)
	if err != nil {
		return err
	}
	if err := savings.SaveCalibration(dir, cal); err != nil {
		return fmt.Errorf("persist calibration: %w", err)
	}
	fmt.Fprintf(out, "calibrated: %.3f bytes/token (%d samples, %s bytes → %s tokens)\n",
		cal.BytesPerToken, cal.Samples, humanCount(cal.SampleBytes), humanCount(cal.SampleTokens))
	fmt.Fprintf(out, "heuristic was 4.000 bytes/token; saved to %s\n", filepath.Join(dir, "calibration.json"))
	fmt.Fprintf(out, "applies to events recorded from now on; earlier events keep their heuristic counts.\n")
	return nil
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
			bench, _ := cmd.Flags().GetBool("benchmark")
			audit, _ := cmd.Flags().GetBool("audit")
			calibrate, _ := cmd.Flags().GetBool("calibrate")
			maxCalls, _ := cmd.Flags().GetInt("calibrate-max")
			out := cmd.OutOrStdout()

			since, err := parseSince(sinceStr, time.Now())
			if err != nil {
				return err
			}
			// --calibrate is a local, write-side operation: it reads the ledger and
			// the count_tokens API directly, persisting a factor — it does not query
			// the daemon report, so handle it before the client call.
			if calibrate {
				return runCalibrate(cmd, strings.TrimSpace(sinceStr), since, maxCalls)
			}
			// Ask the daemon for the day buckets (sparkline) only when benchmarking,
			// and the provenance samples only when auditing — the common report and
			// plain --json stay byte-for-byte unchanged.
			bucket := ""
			if bench {
				bucket = savings.GranularityDay
			}
			sum, err := clientFor(cmd).Savings(cmd.Context(), since, bucket, audit)
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
			if audit {
				fmt.Fprint(out, formatAudit(sum, strings.TrimSpace(sinceStr)))
				return nil
			}
			if bench {
				fmt.Fprint(out, formatBenchmark(sum, strings.TrimSpace(sinceStr)))
				return nil
			}
			fmt.Fprint(out, formatSavings(sum, strings.TrimSpace(sinceStr)))
			return nil
		},
	}
	cmd.Flags().String("since", "", "only count savings since this window (24h, 7d, 2w) or date (2006-01-02 / RFC3339)")
	cmd.Flags().Bool("json", false, "output the structured summary as JSON")
	cmd.Flags().Bool("benchmark", false, "show the headline A/B proof (without-vs-with-warden tokens, reduction %, $ saved) with a per-day trend sparkline, instead of the per-feature table")
	cmd.Flags().Bool("audit", false, "print a few retained raw-vs-kept provenance samples (requires savings_samples) so real bytes behind the counts can be eyeballed")
	cmd.Flags().Bool("calibrate", false, "measure this workload's true bytes-per-token ratio against Claude's count_tokens endpoint (needs ANTHROPIC_API_KEY and retained samples) and persist it, so figures stop relying on the generic 4-bytes/token guess")
	cmd.Flags().Int("calibrate-max", savings.DefaultCalibrationCalls, "cap the number of paid count_tokens calls a --calibrate run makes")
	return cmd
}

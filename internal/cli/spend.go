package cli

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/spend"
)

// formatSpend renders the cost rollup as a human report: a headline total with
// the daily/weekly windows the budget gate watches, then per-agent, per-repo, and
// per-day $ tables (each optionally restricted by --by). Empty spend reads as an
// explicit "nothing measured yet" rather than blank tables, pointing at how the
// figure is sourced (exact billed tokens from transcripts, dollar figures estimated).
func formatSpend(rep *spend.Report, by string) string {
	var b strings.Builder
	if rep.InputTokens == 0 && rep.OutputTokens == 0 {
		fmt.Fprintf(&b, "no spend measured yet\n")
		fmt.Fprintf(&b, "warden reads each agent's exact input/output tokens from its Claude transcript — spawn an agent (and let it work a little) to start the meter. Gated by the `savings` config setting.\n")
		return b.String()
	}
	fmt.Fprintf(&b, "measured Claude spend — $%.2f total · $%.2f today · $%.2f this week (estimated)\n",
		rep.TotalUSD, rep.DailyUSD, rep.WeeklyUSD)
	fmt.Fprintf(&b, "%s tokens (%s in · %s out), priced per model (Opus $%.0f/$%.0f, Sonnet $3/$15, Haiku $0.8/$4 per Mtok in/out)\n",
		humanCount(rep.InputTokens+rep.OutputTokens), humanCount(rep.InputTokens), humanCount(rep.OutputTokens),
		spend.PriceFor("opus").InputPerMTok, spend.PriceFor("opus").OutputPerMTok)

	show := func(col string, rows []spend.Bucket) {
		if len(rows) == 0 {
			return
		}
		fmt.Fprintf(&b, "\nby %s\n", col)
		tw := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", strings.ToUpper(col), "COST", "IN", "OUT")
		for _, r := range rows {
			fmt.Fprintf(tw, "%s\t$%.2f\t%s\t%s\n", r.Key, r.USD, humanCount(r.Input), humanCount(r.Output))
		}
		tw.Flush()
	}

	switch by {
	case "agent":
		show("agent", rep.ByAgent)
	case "repo":
		show("repo", rep.ByRepo)
	case "day":
		show("day", rep.ByDay)
	default:
		show("agent", rep.ByAgent)
		show("repo", rep.ByRepo)
		show("day", rep.ByDay)
	}
	fmt.Fprintf(&b, "\nDollar figures are estimates based on published list prices (as of 2026-06); they exclude prompt-cache tokens and any volume/batch/enterprise discounts, so they may differ from your actual bill. Token counts are exact.\n")
	return b.String()
}

func newSpendCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "spend",
		Short: "Show measured Claude spend in dollars, per agent / repo / day",
		Long: "Report the measured Claude spend warden read from agents' transcripts — the exact " +
			"input/output tokens each agent sent and received — priced per model into estimated dollar figures " +
			"and rolled up per-agent, per-repo, and per-day. Token counts are exact (read directly from " +
			"the transcript); dollar figures are estimates based on published list prices (as of 2026-06) " +
			"and exclude prompt-cache tokens and any volume/batch/enterprise discounts, so they may differ " +
			"from your actual bill. The headline names the daily and weekly totals the budget gate " +
			"(budget_gate / budget_daily_usd / budget_weekly_usd) enforces. This is the cost side of warden's " +
			"savings ledger: where `wd savings` reports what warden kept OUT of context, `wd spend` reports " +
			"what agents actually billed. Gated by the `savings` config setting.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonOut, _ := cmd.Flags().GetBool("json")
			by, _ := cmd.Flags().GetString("by")
			out := cmd.OutOrStdout()
			if by != "" && by != "agent" && by != "repo" && by != "day" {
				return fmt.Errorf("invalid --by %q: use agent, repo, or day", by)
			}
			rep, err := clientFor(cmd).Spend(cmd.Context())
			if err != nil {
				var se *client.StatusError
				if errors.As(err, &se) && se.Code == http.StatusForbidden {
					return fmt.Errorf("spend tracking is disabled — enable it with `savings: true` in the config file")
				}
				return err
			}
			if jsonOut {
				return printJSON(out, rep)
			}
			fmt.Fprint(out, formatSpend(rep, by))
			return nil
		},
	}
	cmd.Flags().Bool("json", false, "output the structured rollup as JSON")
	cmd.Flags().String("by", "", "show only one rollup: agent, repo, or day (default: all three)")
	return cmd
}

// spendAgentCosts fetches the per-agent cost map for the `wd ls` cost column,
// best-effort: any error (daemon down, feature off) yields a nil map so the column
// degrades to "—" rather than failing the listing. w is unused but keeps the
// signature symmetrical with other helpers.
func spendAgentCosts(cmd *cobra.Command) map[string]float64 {
	rep, err := clientFor(cmd).Spend(cmd.Context())
	if err != nil || rep == nil {
		return nil
	}
	return rep.AgentUSD()
}

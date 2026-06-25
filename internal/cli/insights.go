package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/config"
	"github.com/srjn45/warden/internal/insights"
	"github.com/srjn45/warden/internal/llm"
)

// insightsClient is the daemon surface `wd insights` needs: the single shared
// aggregator that gathers history + metrics and runs the engine. Keeping it an
// interface lets the command be tested with a fake (mirrors rotate's rotator).
type insightsClient interface {
	Insights(ctx context.Context, p client.InsightsParams) (*insights.Report, error)
}

// Client must satisfy insightsClient.
var _ insightsClient = (*client.Client)(nil)

// filterParallelBySession narrows a report's parallelization suggestions to those
// involving the named session (matched by id or label). It is a no-op when name
// is empty. Returns the (possibly trimmed) suggestion slice; the rest of the
// report's aggregate stats stay global, since a single session can't be a
// meaningful baseline on its own.
func filterParallelBySession(r *insights.Report, name string) []insights.ParallelSuggestion {
	if strings.TrimSpace(name) == "" {
		return r.Parallelizable
	}
	out := make([]insights.ParallelSuggestion, 0, len(r.Parallelizable))
	for _, s := range r.Parallelizable {
		if s.A == name || s.B == name || s.ALabel == name || s.BLabel == name {
			out = append(out, s)
		}
	}
	return out
}

// runInsights fetches the report, applies the optional --session scoping, and
// narrates it (deterministic when completer is nil). Pure over its inputs so the
// command body stays a thin shell.
func runInsights(ctx context.Context, ic insightsClient, comp llm.Completer, p client.InsightsParams, session string) (*insights.Report, string, error) {
	r, err := ic.Insights(ctx, p)
	if err != nil {
		return nil, "", err
	}
	r.Parallelizable = filterParallelBySession(r, session)
	return r, insights.Narrate(ctx, comp, *r), nil
}

// formatInsights renders the human layout: the narrative paragraph, then the
// deterministic detail sections (only the non-empty ones).
func formatInsights(r *insights.Report, narration, session string) string {
	var b strings.Builder
	if narration != "" {
		b.WriteString(narration)
		b.WriteString("\n\n")
	}
	if session != "" {
		fmt.Fprintf(&b, "(scoped to session %s)\n\n", session)
	}

	if len(r.Durations) > 0 {
		b.WriteString("session duration by type:\n")
		for _, d := range r.Durations {
			fmt.Fprintf(&b, "  %-14s %3d runs · median %s · p90 %s · max %s",
				d.Type, d.Count, humanDuration(d.MedianSec), humanDuration(d.P90Sec), humanDuration(d.MaxSec))
			if len(d.Outliers) > 0 {
				fmt.Fprintf(&b, " · outliers: %s", strings.Join(d.Outliers, ", "))
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	if len(r.Parallelizable) > 0 {
		b.WriteString("parallelization opportunities:\n")
		for _, s := range r.Parallelizable {
			fmt.Fprintf(&b, "  %s + %s · ~%s saveable · %s\n",
				s.ALabel, s.BLabel, humanDuration(s.SavedSec), s.Reason)
		}
		b.WriteString("\n")
	}

	if len(r.CoEdits) > 0 {
		b.WriteString("frequently co-edited files:\n")
		for _, c := range r.CoEdits {
			fmt.Fprintf(&b, "  %s + %s · %d sessions\n", c.A, c.B, c.Count)
		}
		b.WriteString("\n")
	}

	if len(r.ErrorRates) > 0 {
		b.WriteString("error rate by type:\n")
		for _, e := range r.ErrorRates {
			fmt.Fprintf(&b, "  %-14s %d/%d (%.0f%%)\n", e.Type, e.Errored, e.Total, e.Rate*100)
		}
		b.WriteString("\n")
	}

	if len(r.BusiestPeriods) > 0 {
		b.WriteString("busiest hours (UTC):\n")
		for _, h := range r.BusiestPeriods {
			fmt.Fprintf(&b, "  %02d:00 · %d sessions\n", h.Hour, h.Count)
		}
		b.WriteString("\n")
	}

	if len(r.Anomalies) > 0 {
		b.WriteString("live agent anomalies:\n")
		for _, a := range r.Anomalies {
			fmt.Fprintf(&b, "  %s [%s]: %s\n", a.Agent, a.Status, strings.Join(a.Notes, "; "))
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "%d sessions analyzed (%d active)\n", r.Sessions, r.ActiveSessions)
	return b.String()
}

func newInsightsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "insights",
		Short: "Mine agent history for patterns and parallelization suggestions",
		Long: "Analyze warden's own history — completed and active agent sessions plus recorded " +
			"resource metrics — into actionable suggestions: typical/outlier durations by type, " +
			"frequently co-edited files, error rates, busy periods, and sequential-but-disjoint " +
			"sessions that could have run in parallel. Deterministic by default; when local_llm is " +
			"enabled the summary is narrated by the local model (and degrades to the deterministic " +
			"text on any model error).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := config.Load(configPathFor(cmd))
			if !cfg.GetInsights() {
				return fmt.Errorf("insights disabled (enable with insights: true in the config file)")
			}
			sinceStr, _ := cmd.Flags().GetString("since")
			limit, _ := cmd.Flags().GetInt("limit")
			session, _ := cmd.Flags().GetString("session")
			jsonOut, _ := cmd.Flags().GetBool("json")
			out := cmd.OutOrStdout()

			since, err := parseSince(sinceStr, time.Now())
			if err != nil {
				return err
			}

			// Narration is opt-in via local_llm, mirroring the digest narrator: a nil
			// completer means deterministic-only, which Narrate handles.
			var comp llm.Completer
			if cfg.GetLocalLLM() {
				comp = llm.NewOllama(cfg.LocalLLMURL, cfg.LocalLLMModel, cfg.LocalLLMTimeoutDuration())
			}

			r, narration, err := runInsights(cmd.Context(), clientFor(cmd), comp,
				client.InsightsParams{Since: since, HistoryLimit: limit}, session)
			if err != nil {
				return err
			}
			if jsonOut {
				return printJSON(out, r)
			}
			fmt.Fprint(out, formatInsights(r, narration, session))
			return nil
		},
	}
	cmd.Flags().String("since", "", "only mine sessions since this window (24h, 7d, 2w) or date (2006-01-02 / RFC3339)")
	cmd.Flags().Int("limit", 0, "cap the number of archived sessions mined (0 = daemon default)")
	cmd.Flags().String("session", "", "scope parallelization suggestions to one session (by id or name)")
	cmd.Flags().Bool("json", false, "output the structured report as JSON")
	return cmd
}

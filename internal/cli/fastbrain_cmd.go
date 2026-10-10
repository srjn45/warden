package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/fastbrain"
)

// newInspectFastBrainCmd builds `inspect fastbrain`: redacted telemetry, recent
// decision traces and narrowly scoped per-kind pause/resume for the daemon's
// internal decisions (Fast-Brain and Thinking-Brain).
func newInspectFastBrainCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fastbrain",
		Short: "Fast-Brain/Thinking-Brain telemetry, traces and per-kind pause/resume",
		Long: `Inspect warden's own internal decisions (approval arbitration, stall triage,
commit/PR drafting, activity summaries, ...) without exposing their content.

status     per-kind attempts, outcomes, cache/coalesce/shed counters, queue
           wait and run-time percentiles, runner circuit state and pauses
decisions  the recent redacted decision trace (newest first)
pause      pause one decision kind; its calls fail open without a model call
resume     clear a pause

Labels are bounded to decision kind, tier, outcome, provider (AI CLI) and
model. Prompts, outputs, agent/session ids and paths are never reported. A
pause defaults to 1h and is capped at 24h; a daemon restart clears operator
pauses (config fast_brain.disabled_kinds pauses persist). Pausing never
changes the destructive-action guard: paused arbitration escalates to a human.`,
	}
	children := []*cobra.Command{newFastBrainStatusCmd(), newFastBrainDecisionsCmd(), newFastBrainPauseCmd(), newFastBrainResumeCmd()}
	for _, c := range children {
		cmd.AddCommand(c)
	}
	return cmd
}

func newFastBrainStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "status",
		Short:   "Show Fast-Brain/Thinking-Brain telemetry",
		Example: "  warden inspect fastbrain status\n  warden inspect fastbrain status --json",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := clientFor(cmd).FastBrainMetrics(cmd.Context())
			if err != nil {
				return err
			}
			if jsonRequested(cmd) {
				return emitJSON(cmd, s)
			}
			fmt.Fprint(cmd.OutOrStdout(), FormatFastBrainStatus(s))
			return nil
		},
	}
	addJSONFlag(cmd, "emit the snapshot as JSON")
	return cmd
}

func newFastBrainDecisionsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "decisions",
		Short: "Show recent redacted decision traces",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			n, _ := cmd.Flags().GetInt("limit")
			ds, err := clientFor(cmd).FastBrainDecisions(cmd.Context(), n)
			if err != nil {
				return err
			}
			if jsonRequested(cmd) {
				return emitJSON(cmd, ds)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "TIME\tKIND\tTIER\tOUTCOME\tFINAL\tPROVIDER\tMODEL\tQUEUE\tRUN\tFLAGS")
			for _, d := range ds {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%dms\t%dms\t%s\n",
					d.Time.Format("15:04:05"), d.Kind, d.Tier, d.Outcome, d.FinalAction,
					fbDash(d.Provider), fbDash(d.Model), d.QueueWaitMs, d.RunMs, decisionFlags(d))
			}
			return w.Flush()
		},
	}
	cmd.Flags().Int("limit", 50, "maximum decisions to show")
	addJSONFlag(cmd, "emit the decisions as JSON")
	return cmd
}

func newFastBrainPauseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pause KIND",
		Short: "Pause one decision kind (fails open, no model call)",
		Long: `Pause one decision kind for a bounded time (default 1h, max 24h). Calls of
that kind return a deferred/fail-open result immediately without invoking a
runner. Resume with "warden inspect fastbrain resume KIND"; a daemon restart
also clears the pause. Audit-logged.`,
		Example: "  warden inspect fastbrain pause summarize_activity --for 2h",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			d, _ := cmd.Flags().GetDuration("for")
			if _, err := clientFor(cmd).SetFastBrainControl(cmd.Context(), args[0], true, int(d.Seconds())); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "paused %s\n", args[0])
			return nil
		},
	}
	cmd.Flags().Duration("for", 0, "pause duration (0 = default 1h, capped at 24h)")
	return cmd
}

func newFastBrainResumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume KIND",
		Short: "Resume a paused decision kind",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := clientFor(cmd).SetFastBrainControl(cmd.Context(), args[0], false, 0); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "resumed %s\n", args[0])
			return nil
		},
	}
}

func fbDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func decisionFlags(d fastbrain.Decision) string {
	var f []string
	if d.Cached {
		f = append(f, "cached")
	}
	if d.Coalesced {
		f = append(f, "coalesced")
	}
	if d.Fallback {
		f = append(f, "fallback")
	}
	if d.Cancel != "" {
		f = append(f, "cancel:"+d.Cancel)
	}
	if d.Reason != "" {
		f = append(f, "reason:"+d.Reason)
	}
	return strings.Join(f, ",")
}

// FormatFastBrainStatus renders a telemetry snapshot as plain text; shared by
// the CLI and the TUI so both surfaces show identical, redacted numbers.
func FormatFastBrainStatus(s fastbrain.TelemetrySnapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "slots %d/%d  admitted %d  coalesced %d  cache-hits %d  preempted %d  abandoned %d (live %d)\n",
		s.Active, s.MaxConcurrent, s.Admitted, s.Coalesced, s.CacheHits, s.Preempted, s.Abandoned, s.AbandonedLive)
	if len(s.Queues) > 0 {
		b.WriteString("queues  ")
		for _, q := range s.Queues {
			fmt.Fprintf(&b, "P%d active=%d queued=%d  ", q.Class, q.Active, q.Queued)
		}
		b.WriteString("\n")
	}
	writeKindTable(&b, s)
	if len(s.Runners) > 0 {
		b.WriteString("\nRUNNERS\n")
		w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "PROVIDER\tMODEL\tTIER\tCALLS\tOK\tFAILED\tAVG RUN")
		for _, r := range s.Runners {
			avg := int64(0)
			if r.Calls > 0 {
				avg = r.RunSumMs / int64(r.Calls)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%d\t%dms\n", r.Provider, fbDash(r.Model), r.Tier, r.Calls, r.OK, r.Failed, avg)
		}
		_ = w.Flush()
	}
	if len(s.Circuits) > 0 {
		b.WriteString("\nCIRCUITS\n")
		for _, c := range s.Circuits {
			line := fmt.Sprintf("  %s: %s (consecutive failures %d, opens %d)", c.Provider, c.State, c.ConsecutiveFailures, c.Opens)
			if c.RetryAt != nil {
				line += " retry " + c.RetryAt.Format(time.TimeOnly)
			}
			b.WriteString(line + "\n")
		}
	}
	var paused []string
	for _, c := range s.Controls {
		if c.Paused {
			p := string(c.Kind)
			if c.Until != nil {
				p += " (until " + c.Until.Format(time.TimeOnly) + ")"
			}
			paused = append(paused, p)
		}
	}
	if len(paused) > 0 {
		fmt.Fprintf(&b, "\nPAUSED  %s\n", strings.Join(paused, ", "))
	}
	return b.String()
}

func writeKindTable(out io.Writer, s fastbrain.TelemetrySnapshot) {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "KIND\tTIER\tCALLS\tOK\tFAIL-OPEN\tCACHED\tCOALESCED\tQ p95\tRUN p50\tRUN p95")
	for _, k := range s.Kinds {
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%d\t%d\t%dms\t%dms\t%dms\n", k.Kind, k.Tier, k.Attempts,
			k.Outcomes["ok"], k.FailOpen, k.Cached, k.Coalesced, k.QueueWait.P95Ms, k.RunTime.P50Ms, k.RunTime.P95Ms)
	}
	_ = w.Flush()
}

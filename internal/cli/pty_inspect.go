package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/ptyobs"
)

// newInspectPTYCmd builds `inspect pty`: the runtime-supervised operator view -
// live state/owner/posture, the redacted per-session decision trace, and the
// stable warden_pty_* metrics.
func newInspectPTYCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pty",
		Short: "PTY runtime status, decision trace and metrics",
		Long: `Inspect the PTY runtime (runtime.pty: shadow|on) and the permission
decision engine built on it.

runtime <id>    live state with typed waiting reason and evidence, who owns the
                input line, the effective permission posture (a conspicuous
                banner for dangerously-execute-all), the latest decision route
                and a plain-language summary of the prompt on screen
decisions <id>  the append-only, redacted decision trace for the session: one
                record per auto-approve attempt/outcome (newest first)
metrics         the stable warden_pty_* metric families (JSON with --json)

Nothing here ever shows raw keystrokes or secrets: traces and metric labels are
redacted and bounded. Retention: runtime.trace_max_records / trace_max_age_days.`,
	}
	cmd.AddCommand(newPTYRuntimeCmd(), newPTYDecisionsCmd(), newPTYMetricsCmd())
	return cmd
}

func newPTYRuntimeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "runtime <id>",
		Short:   "Show a session's live PTY-runtime status",
		Example: "  warden inspect pty runtime A-1\n  warden inspect pty runtime A-1 --json",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := clientFor(cmd).SessionRuntime(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if jsonRequested(cmd) {
				return emitJSON(cmd, v)
			}
			fmt.Fprint(cmd.OutOrStdout(), FormatRuntime(v))
			return nil
		},
	}
	addJSONFlag(cmd, "emit the runtime view as JSON")
	return cmd
}

func newPTYDecisionsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "decisions <id>",
		Short:   "Show a session's redacted auto-approve decision trace",
		Example: "  warden inspect pty decisions A-1 --limit 20\n  warden inspect pty decisions A-1 --outcome escalated_human",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			n, _ := cmd.Flags().GetInt("limit")
			o, _ := cmd.Flags().GetString("outcome")
			recs, err := clientFor(cmd).SessionDecisions(cmd.Context(), args[0], n, o)
			if err != nil {
				return err
			}
			if jsonRequested(cmd) {
				return emitJSON(cmd, recs)
			}
			fmt.Fprint(cmd.OutOrStdout(), FormatDecisions(recs))
			return nil
		},
	}
	cmd.Flags().Int("limit", 50, "maximum records (newest first)")
	cmd.Flags().String("outcome", "", "only this outcome (approved, denied, dry_run, deferred, escalated_human, needs_attention, routed_brain, error)")
	addJSONFlag(cmd, "emit the records as JSON")
	return cmd
}

func newPTYMetricsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "metrics",
		Short: "Show the warden_pty_* metrics",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			snap, err := clientFor(cmd).PTYMetrics(cmd.Context())
			if err != nil {
				return err
			}
			if jsonRequested(cmd) {
				return emitJSON(cmd, snap)
			}
			return snap.WritePrometheus(cmd.OutOrStdout())
		},
	}
	addJSONFlag(cmd, "emit the snapshot as JSON instead of Prometheus text")
	return cmd
}

// FormatRuntime renders a RuntimeView for a terminal.
func FormatRuntime(v ptyobs.RuntimeView) string {
	var b strings.Builder
	if v.Danger {
		fmt.Fprintf(&b, "!!! %s !!!\n", v.DangerBanner)
	}
	fmt.Fprintf(&b, "session   %s\n", v.SessionID)
	fmt.Fprintf(&b, "runtime   %s (supervised=%t authoritative=%t)\n", v.Mode, v.Supervised, v.Authoritative)
	if v.RequestedMode != "" || v.ActualKind != "" || v.ProcessOwner != "" {
		fmt.Fprintf(&b, "host      requested=%s actual=%s owner=%s\n", v.RequestedMode, v.ActualKind, v.ProcessOwner)
	}
	if v.EvidenceSource != "" || v.EvidenceFreshness != "" {
		fmt.Fprintf(&b, "evidence  source=%s freshness=%s\n", v.EvidenceSource, v.EvidenceFreshness)
	}
	if v.UnsupportedReason != "" {
		fmt.Fprintf(&b, "readiness %s\n", v.UnsupportedReason)
	}
	fmt.Fprintf(&b, "posture   %s\n", v.Posture)
	if !v.Supervised {
		b.WriteString("(no PTY event stream backs this session; state comes from the legacy poller)\n")
	} else {
		state := v.State
		if v.Reason != "" {
			state += " (" + v.Reason + ")"
		}
		fmt.Fprintf(&b, "state     %s\n", state)
		if len(v.Evidence) > 0 {
			fmt.Fprintf(&b, "evidence  %s\n", strings.Join(v.Evidence, ", "))
		}
		fmt.Fprintf(&b, "input     %s", v.InputOwner)
		if v.HumanLineOpen {
			b.WriteString(" (human mid-line)")
		}
		b.WriteString("\n")
		if v.QuietSeconds > 0 {
			fmt.Fprintf(&b, "quiet     %.0fs (advisory)\n", v.QuietSeconds)
		}
	}
	if v.Route != "" {
		fmt.Fprintf(&b, "decision  route=%s outcome=%s\n", v.Route, v.LastOutcome)
	}
	if v.Prompt != nil {
		fmt.Fprintf(&b, "prompt    %s\n", v.Prompt.Text)
		for _, o := range v.Prompt.Options {
			fmt.Fprintf(&b, "  %d. %s [%s]\n", o.Index, o.Label, o.Semantic)
		}
	}
	return b.String()
}

// FormatDecisions renders decision-trace records as a table.
func FormatDecisions(recs []ptyobs.TraceRecord) string {
	if len(recs) == 0 {
		return "(no decisions recorded)\n"
	}
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "TIME\tOUTCOME\tROUTE\tPOSTURE\tCLASS\tACTION\tLATENCY\tREASON\tSUMMARY")
	for _, r := range recs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%dms\t%s\t%s\n",
			r.Time.Local().Format(time.TimeOnly), r.Outcome, r.Route, r.Posture, r.Class,
			r.Action, r.LatencyMS, r.Reason, r.Permission.Text)
	}
	_ = w.Flush()
	return b.String()
}

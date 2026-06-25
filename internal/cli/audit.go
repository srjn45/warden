package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/config"
)

func newAuditCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Inspect the append-only action audit trail",
		Long: "Read the daemon's audit trail (~/.warden/audit.jsonl) — who did what, " +
			"when, to which object. The file is written by the daemon; this command " +
			"reads it directly, so it works even while the daemon is down.",
	}
	cmd.AddCommand(newAuditLogCmd())
	return cmd
}

func newAuditLogCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "log",
		Short: "Show recent audited actions, newest last",
		Long: "Print audit records in chronological order. Tail the most recent with " +
			"--tail (0 = all), and narrow with --action, --target, and --since/--until.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			action, _ := cmd.Flags().GetString("action")
			target, _ := cmd.Flags().GetString("target")
			sinceStr, _ := cmd.Flags().GetString("since")
			untilStr, _ := cmd.Flags().GetString("until")
			tail, _ := cmd.Flags().GetInt("tail")
			jsonOut, _ := cmd.Flags().GetBool("json")
			out := cmd.OutOrStdout()

			now := time.Now()
			since, err := parseSince(sinceStr, now)
			if err != nil {
				return err
			}
			until, err := parseSince(untilStr, now)
			if err != nil {
				return fmt.Errorf("--until: %w", err)
			}

			cfg := config.Load(configPathFor(cmd))
			path := filepath.Join(cfg.DataDir, "audit.jsonl")
			events, err := audit.Read(path, audit.Filter{
				Action: action, Target: target, Since: since, Until: until, Limit: tail,
			})
			if err != nil {
				return err
			}
			if jsonOut {
				return printJSON(out, events)
			}
			if len(events) == 0 {
				fmt.Fprintln(out, "no audit records match")
				return nil
			}
			return renderAudit(out, events)
		},
	}
	cmd.Flags().String("action", "", "filter by action (spawn, terminate, delete, approve, pipeline_start, pipeline_cancel)")
	cmd.Flags().String("target", "", "filter by target substring (agent or pipeline ID)")
	cmd.Flags().String("since", "", "only records since this window (24h, 7d, 2w) or date (2006-01-02 / RFC3339)")
	cmd.Flags().String("until", "", "only records up to this window or date")
	cmd.Flags().Int("tail", 50, "show only the most recent N records (0 = all)")
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

// renderAudit prints events as an aligned table, one row per record.
func renderAudit(w io.Writer, events []audit.Event) error {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME\tACTION\tTARGET\tACTOR\tDETAIL")
	for _, ev := range events {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			ev.Time.Local().Format("2006-01-02 15:04:05"),
			dash(ev.Action), dash(ev.Target), dash(ev.Actor), detailStr(ev.Detail))
	}
	return tw.Flush()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// detailStr renders the detail map as stable "k=v" pairs (sorted for
// determinism), or "-" when empty.
func detailStr(d map[string]string) string {
	if len(d) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(d))
	for k := range d {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+d[k])
	}
	return strings.Join(parts, " ")
}

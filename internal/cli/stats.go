package cli

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/metrics"
)

// humanBytes renders a byte count as a compact IEC string (B/KiB/MiB/GiB).
func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// humanDuration renders seconds as a compact "1h2m" / "3m" / "45s".
func humanDuration(sec int64) string {
	d := time.Duration(sec) * time.Second
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%ds", sec)
	}
}

// formatStats renders a Sample as a compact human report: a system summary line
// then per-agent rows sorted by RSS descending (the hog on top).
func formatStats(s *metrics.Sample) string {
	var b strings.Builder
	sys := s.System
	fmt.Fprintf(&b, "system: %s used / %s total · %s swap · pressure %s · %d agents · %s attributed\n",
		humanBytes(sys.UsedBytes), humanBytes(sys.TotalBytes), humanBytes(sys.SwapUsedBytes),
		sys.PressureLevel, sys.AgentCount, humanBytes(sys.AttributedRSSBytes))
	fmt.Fprintf(&b, "daemon: %s rss · %d goroutines · %d fds\n",
		humanBytes(s.Daemon.RSSBytes), s.Daemon.Goroutines, s.Daemon.OpenFDs)

	agents := append([]metrics.AgentStat(nil), s.Agents...)
	sort.Slice(agents, func(i, j int) bool { return agents[i].RSSBytes > agents[j].RSSBytes })
	if len(agents) == 0 {
		b.WriteString("(no live agents)\n")
		return b.String()
	}
	fmt.Fprintf(&b, "%-24s %10s %6s %5s %8s\n", "AGENT", "RSS", "CPU%", "PROCS", "UPTIME")
	for _, a := range agents {
		rss := humanBytes(a.RSSBytes)
		if !a.Paneable {
			rss = "—"
		}
		fmt.Fprintf(&b, "%-24s %10s %6.1f %5d %8s\n", a.ID, rss, a.CPUPercent, a.ProcCount, humanDuration(a.UptimeSec))
	}
	return b.String()
}

// formatHistory renders per-agent performance summaries: one block per agent
// with runtime, memory/CPU/context trends, changed files, and any anomaly
// warnings. Sorted by agent ID (as the daemon returns them).
func formatHistory(sums []metrics.AgentSummary) string {
	if len(sums) == 0 {
		return "(no recorded history)\n"
	}
	var b strings.Builder
	for _, s := range sums {
		fmt.Fprintf(&b, "%s [%s] · %d samples · runtime %s\n", s.ID, s.Status, s.Samples, humanDuration(s.RuntimeSec))
		fmt.Fprintf(&b, "  mem:     %s now · %s peak · %s\n",
			humanBytes(s.LatestRSSBytes), humanBytes(s.PeakRSSBytes), signedBytes(s.RSSTrendBytes))
		fmt.Fprintf(&b, "  cpu:     %.1f%% avg · %.1f%% peak\n", s.AvgCPUPercent, s.PeakCPUPercent)
		fmt.Fprintf(&b, "  context: %dk now · %dk peak · %s · %d files changed\n",
			s.LatestContextTokens/1000, s.PeakContextTokens/1000, signedTokens(s.ContextTrendTokens), s.PeakFilesModified)
		for _, a := range s.Anomalies {
			fmt.Fprintf(&b, "  ⚠ %s\n", a)
		}
	}
	return b.String()
}

// signedBytes renders a signed byte delta with an explicit +/- and "flat" at 0.
func signedBytes(d int64) string {
	switch {
	case d > 0:
		return "↑ " + humanBytes(uint64(d))
	case d < 0:
		return "↓ " + humanBytes(uint64(-d))
	default:
		return "flat"
	}
}

// signedTokens renders a signed token delta in thousands.
func signedTokens(d int) string {
	switch {
	case d > 0:
		return fmt.Sprintf("↑ %dk", d/1000)
	case d < 0:
		return fmt.Sprintf("↓ %dk", -d/1000)
	default:
		return "flat"
	}
}

func newStatsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Show warden's resource footprint (per-agent memory/CPU, system pressure, daemon stats)",
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonOut, _ := cmd.Flags().GetBool("json")
			watch, _ := cmd.Flags().GetBool("watch")
			history, _ := cmd.Flags().GetBool("history")
			agent, _ := cmd.Flags().GetString("agent")
			if history {
				sums, err := clientFor(cmd).GetAgentHistory(cmd.Context(), "", agent)
				if err != nil {
					return err
				}
				if jsonOut {
					return printJSON(cmd.OutOrStdout(), sums)
				}
				fmt.Fprint(cmd.OutOrStdout(), formatHistory(sums))
				return nil
			}
			render := func() error {
				s, err := clientFor(cmd).GetMetrics(cmd.Context())
				if err != nil {
					return err
				}
				if jsonOut {
					return printJSON(cmd.OutOrStdout(), s)
				}
				fmt.Fprint(cmd.OutOrStdout(), formatStats(s))
				return nil
			}
			if !watch {
				return render()
			}
			// --watch: clear + redraw on an interval until interrupted.
			t := time.NewTicker(3 * time.Second)
			defer t.Stop()
			for {
				fmt.Fprint(cmd.OutOrStdout(), "\033[2J\033[H") // clear screen, home cursor
				if err := render(); err != nil {
					return err
				}
				select {
				case <-cmd.Context().Done():
					return nil
				case <-t.C:
				}
			}
		},
	}
	cmd.Flags().Bool("json", false, "output as JSON")
	cmd.Flags().Bool("watch", false, "redraw every 3s until interrupted")
	cmd.Flags().Bool("history", false, "show persisted per-agent performance history + anomaly warnings")
	cmd.Flags().String("agent", "", "with --history, limit to one agent ID")
	return cmd
}

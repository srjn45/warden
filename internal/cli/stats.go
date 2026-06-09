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

func newStatsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Show warden's resource footprint (per-agent memory/CPU, system pressure, daemon stats)",
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonOut, _ := cmd.Flags().GetBool("json")
			watch, _ := cmd.Flags().GetBool("watch")
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
	return cmd
}

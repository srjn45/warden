package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/store"
)

func newLsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List all active agent sessions",
		RunE: func(cmd *cobra.Command, args []string) error {
			sessions, err := clientFor(cmd).List(cmd.Context())
			if err != nil {
				return err
			}
			if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
				if sessions == nil {
					sessions = []*store.Session{}
				}
				return printJSON(cmd.OutOrStdout(), sessions)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			color := isTTY(cmd.OutOrStdout())
			fmt.Fprintln(tw, "NAME\tID\tTYPE\tSTATUS\tCONTEXT\tAGE\tDIR\tSUBJECT")
			for _, s := range sessions {
				name := s.Name
				if name == "" {
					name = "—"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					name, s.ID, typeOrPending(s.Type), statusCell(s.Status, color), contextCell(s.ContextTokens, s.ContextState, color),
					age(s.UpdatedAt), dirName(s.Workdir), s.Subject)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

func newStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status <TICKET>",
		Short: "Show full status for one session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := clientFor(cmd).Get(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
				return printJSON(cmd.OutOrStdout(), s)
			}
			out := cmd.OutOrStdout()
			name := s.Name
			if name == "" {
				name = "—"
			}
			color := isTTY(out)
			fmt.Fprintf(out, "id:         %s\nname:       %s\ntype:       %s\nticket:     %s\nstatus:     %s\nrepo:       %s\nworkdir:    %s\nworktree:   %s\nbranch:     %s\npr:         %s\nsupervised: %v\nsubject:    %s\nclaude:     %s\nupdated:    %s\n",
				s.ID, name, typeOrPending(s.Type), s.Ticket, statusCell(s.Status, color), s.Repo, s.Workdir, s.Worktree, s.Branch, s.PR, s.Supervised, s.Subject, s.ClaudeSessionID, s.UpdatedAt.Format(time.RFC3339))

			// Show rate limit info if present
			if rateLimitInfo := formatRateLimitInfo(s); rateLimitInfo != "" {
				fmt.Fprintln(out, rateLimitInfo)
			}

			fmt.Fprintln(out, "events:")
			for _, e := range s.Events {
				fmt.Fprintf(out, "  %s  %-14s %s\n", e.TS.Format("15:04:05"), e.Type, e.Detail)
			}
			return nil
		},
	}
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

func age(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t).Round(time.Minute)
	if d < time.Minute {
		return "<1m"
	}
	return d.String()
}

func dirName(workdir string) string {
	if workdir == "" {
		return "—"
	}
	return filepath.Base(workdir)
}

// statusCell formats an agent's status with color. When color is true (stdout
// is a TTY) the status is tinted by semantic meaning.
func statusCell(status store.Status, color bool) string {
	s := string(status)
	if !color {
		return s
	}
	switch status {
	case store.StatusDone:
		return "\033[32m" + s + "\033[0m" // green
	case store.StatusWorking:
		return "\033[34m" + s + "\033[0m" // blue
	case store.StatusErrored:
		return "\033[31m" + s + "\033[0m" // red
	case store.StatusRateLimited:
		return "\033[33m" + s + "\033[0m" // yellow/amber (warning)
	default:
		return s
	}
}

// contextCell formats an agent's context-window gauge for the ls table. An
// unknown gauge (no model turn yet) renders "—". When color is true (stdout is
// a TTY) the figure is tinted green/orange/red by state.
func contextCell(tokens int, state string, color bool) string {
	if tokens == 0 && state == "" {
		return "—"
	}
	s := fmt.Sprintf("%dk", tokens/1000)
	if !color {
		return s
	}
	switch state {
	case store.ContextWarning:
		return "\033[33m" + s + "\033[0m" // orange/yellow
	case store.ContextCritical:
		return "\033[31m" + s + "\033[0m" // red
	default:
		return "\033[32m" + s + "\033[0m" // green
	}
}

// isTTY reports whether w is a terminal (for opt-in ANSI coloring).
func isTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func typeOrPending(t store.Type) string {
	if t == "" {
		return "…"
	}
	return string(t)
}

// formatRateLimitInfo formats rate limit metadata for the status detail view.
// Returns empty string if the session is not rate limited.
func formatRateLimitInfo(sess *store.Session) string {
	if sess.Status != store.StatusRateLimited {
		return ""
	}

	var lines []string
	lines = append(lines, "rate limit:")

	if sess.RateLimitedAt != nil {
		lines = append(lines, fmt.Sprintf("  limited at: %s",
			sess.RateLimitedAt.Format("2006-01-02 15:04:05")))
	}

	if sess.RateLimitRestoreAt != nil {
		until := time.Until(*sess.RateLimitRestoreAt)
		if until > 0 {
			lines = append(lines, fmt.Sprintf("  resume at:  %s (in %s)",
				sess.RateLimitRestoreAt.Format("2006-01-02 15:04:05"),
				formatDuration(until)))
		} else {
			lines = append(lines, fmt.Sprintf("  resume at:  %s (resuming...)",
				sess.RateLimitRestoreAt.Format("2006-01-02 15:04:05")))
		}
	}

	lines = append(lines, fmt.Sprintf("  retries:    %d", sess.RateLimitRetryCount))

	result := ""
	for _, line := range lines {
		result += line + "\n"
	}
	return result
}

// formatDuration formats a duration in a human-readable way (e.g., "1h 23m 45s").
func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second

	if h > 0 {
		return fmt.Sprintf("%dh %dm %ds", h, m, s)
	}
	if m > 0 {
		return fmt.Sprintf("%dm %ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

// printJSON writes v as indented JSON followed by a newline. Used by the
// --json flag so agents/scripts can parse warden output reliably.
func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/store"
)

func newLsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List all active agent sessions",
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonOut, _ := cmd.Flags().GetBool("json")
			watch, _ := cmd.Flags().GetBool("watch")
			tags, _ := cmd.Flags().GetStringSlice("tag")
			out := cmd.OutOrStdout()
			if watch {
				return watchSessions(cmd, out, jsonOut)
			}
			sessions, err := clientFor(cmd).List(cmd.Context())
			if err != nil {
				return err
			}
			sessions = filterByTags(sessions, tags)
			if jsonOut {
				if sessions == nil {
					sessions = []*store.Session{}
				}
				return printJSON(out, sessions)
			}
			// Best-effort per-agent cost for the COST column; a nil map (feature off
			// or daemon hiccup) degrades the column to "—" rather than failing the list.
			return renderSessions(out, sessions, spendAgentCosts(cmd), isTTY(out))
		},
	}
	cmd.Flags().Bool("json", false, "output as JSON")
	cmd.Flags().BoolP("watch", "w", false, "live-update the list on every agent state change (Ctrl+C to exit)")
	cmd.Flags().StringSlice("tag", nil, "only show agents carrying every given tag (repeatable or comma-separated, e.g. --tag backend --tag urgent)")
	return cmd
}

// filterByTags returns the sessions that carry every tag in want (AND
// semantics, mirroring search's multi-term matching). An empty want is a no-op
// that returns the input unchanged, so untagged fleets are unaffected.
func filterByTags(sessions []*store.Session, want []string) []*store.Session {
	want = store.NormalizeTags(want)
	if len(want) == 0 {
		return sessions
	}
	out := make([]*store.Session, 0, len(sessions))
	for _, s := range sessions {
		match := true
		for _, t := range want {
			if !s.HasTag(t) {
				match = false
				break
			}
		}
		if match {
			out = append(out, s)
		}
	}
	return out
}

// renderSessions writes the agent table to w. cost maps agent id → measured $
// spend for the COST column (nil/absent ⇒ "—"). color enables ANSI tinting.
func renderSessions(w io.Writer, sessions []*store.Session, cost map[string]float64, color bool) error {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tID\tTYPE\tMODEL\tPERMISSION_MODE\tSTATUS\tCONTEXT\tCOST\tAGE\tDIR\tSUBJECT")
	for _, s := range sessions {
		name := s.Name
		if name == "" {
			name = "—"
		}
		permMode := s.PermissionMode
		if permMode == "" {
			permMode = "default"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			name, s.ID, typeOrPending(s.Type), modelCell(s.Model), permMode, statusCell(s.Status, color), contextCell(s.ContextTokens, s.ContextState, color),
			costCell(cost, s.ID), age(s.UpdatedAt), dirName(s.Workdir), s.Subject)
	}
	return tw.Flush()
}

// costCell renders an agent's measured spend for the COST column: "$1.23", or
// "—" when no spend has been attributed (or the cost map is unavailable).
func costCell(cost map[string]float64, id string) string {
	if usd, ok := cost[id]; ok && usd > 0 {
		return fmt.Sprintf("$%.2f", usd)
	}
	return "—"
}

// watchSessions streams live snapshots from the daemon's SSE endpoint, redrawing
// the table (or emitting JSON) on every state change until the user hits Ctrl+C.
func watchSessions(cmd *cobra.Command, out io.Writer, jsonOut bool) error {
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	color := !jsonOut && isTTY(out)
	clearable := color // only clear/redraw when stdout is a real terminal

	render := func(sessions []*store.Session) error {
		if jsonOut {
			if sessions == nil {
				sessions = []*store.Session{}
			}
			return printJSON(out, sessions)
		}
		var buf bytes.Buffer
		if clearable {
			// Home cursor + clear screen so each snapshot replaces the last.
			buf.WriteString("\033[H\033[2J")
			fmt.Fprintf(&buf, "watching %d agent(s) — updated %s — Ctrl+C to exit\n\n",
				len(sessions), time.Now().Format("15:04:05"))
		}
		// Watch mode streams session snapshots over SSE; the cost column degrades to
		// "—" here rather than re-fetching the spend rollup on every redraw.
		if err := renderSessions(&buf, sessions, nil, color); err != nil {
			return err
		}
		_, err := out.Write(buf.Bytes())
		return err
	}

	err := clientFor(cmd).Watch(ctx, render)
	// A user-initiated Ctrl+C is a clean exit, not an error.
	if errors.Is(err, context.Canceled) || ctx.Err() != nil {
		return nil
	}
	return err
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
			permMode := s.PermissionMode
			if permMode == "" {
				permMode = "default"
			}
			fmt.Fprintf(out, "id:              %s\nname:            %s\ntype:            %s\nmodel:           %s\nticket:          %s\nstatus:          %s\nrepo:            %s\nworkdir:         %s\nworktree:        %s\nbranch:          %s\npr:              %s\npermission_mode: %s\nsubject:         %s\nclaude:          %s\nupdated:         %s\n",
				s.ID, name, typeOrPending(s.Type), modelOrDefault(s.Model), s.Ticket, statusCell(s.Status, color), s.Repo, s.Workdir, s.Worktree, s.Branch, s.PR, permMode, s.Subject, s.ClaudeSessionID, s.UpdatedAt.Format(time.RFC3339))

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

// modelCell formats the model for the ls table. Shows short alias if the
// model matches a known full ID, otherwise shows the full ID. Empty model
// defaults to "sonnet" display.
func modelCell(model string) string {
	if model == "" {
		return "sonnet" // default
	}

	// Map of full IDs to short aliases (reverse of lifecycle.modelAliases)
	aliases := map[string]string{
		"claude-opus-4-8":   "opus",
		"claude-sonnet-4-6": "sonnet",
		"claude-haiku-4-5":  "haiku",
		"claude-fable-5":    "fable",
	}

	if alias, ok := aliases[model]; ok {
		return alias
	}
	return model // show full ID if custom
}

// modelOrDefault returns the model display value for status output.
// Shows the full model ID, or lifecycle.DefaultModel for empty.
func modelOrDefault(model string) string {
	if model == "" {
		return lifecycle.DefaultModel // default
	}
	return model
}

package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/store"
)

// parseSince resolves a --since value to an absolute lower-bound time. It accepts
// a relative window ("24h", "90m", "7d", "2w" — days/weeks beyond what
// time.ParseDuration handles) or an absolute date ("2006-01-02") / RFC3339
// timestamp. now is passed in so the relative path is testable.
func parseSince(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	// Relative day/week windows: time.ParseDuration has no 'd'/'w' units.
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(n)
		if err == nil {
			return now.Add(-time.Duration(days) * 24 * time.Hour), nil
		}
	}
	if n, ok := strings.CutSuffix(s, "w"); ok {
		weeks, err := strconv.Atoi(n)
		if err == nil {
			return now.Add(-time.Duration(weeks) * 7 * 24 * time.Hour), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(-d), nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("invalid --since %q: want a window (24h, 7d, 2w) or a date (2006-01-02 / RFC3339)", s)
}

func newHistoryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "history",
		Short: "Browse archived (closed) agents, newest first",
		Long: "List the archived agent records the soft-delete path persists. " +
			"Filter with --since (24h, 7d, 2w, or a date) and --type.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			sinceStr, _ := cmd.Flags().GetString("since")
			typ, _ := cmd.Flags().GetString("type")
			limit, _ := cmd.Flags().GetInt("limit")
			jsonOut, _ := cmd.Flags().GetBool("json")
			out := cmd.OutOrStdout()
			since, err := parseSince(sinceStr, time.Now())
			if err != nil {
				return err
			}
			sessions, err := clientFor(cmd).History(cmd.Context(), client.HistoryParams{
				Since: since, Type: typ, Limit: limit,
			})
			if err != nil {
				return err
			}
			if jsonOut {
				if sessions == nil {
					sessions = []*store.Session{}
				}
				return printJSON(out, sessions)
			}
			if len(sessions) == 0 {
				fmt.Fprintln(out, "no archived agents match")
				return nil
			}
			return renderSessions(out, sessions, isTTY(out))
		},
	}
	cmd.Flags().String("since", "", "only agents updated since this window (24h, 7d, 2w) or date (2006-01-02 / RFC3339)")
	cmd.Flags().String("type", "", "filter by task type (development, pr-review, analysis, …)")
	cmd.Flags().Int("limit", 0, "cap the number of results (0 = no cap)")
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

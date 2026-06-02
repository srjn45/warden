package tui

import (
	"fmt"
	"time"

	"github.com/srajanpathak/agentctl/internal/store"
)

func age(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

// renderList returns the left-pane lines for the given width.
func (m Model) renderList(width int) string {
	if len(m.sessions) == 0 {
		return stMuted.Render("No agents — press n to create one")
	}
	out := ""
	for i, s := range m.sessions {
		label, st := badge(s.Status)
		cursor := "  "
		line := fmt.Sprintf("%-12s %-9s %-11s %s", trunc(s.ID, 12), trunc(string(typeOr(s)), 9), st.Render(label), trunc(s.Subject, max(0, width-40)))
		if i == m.cursor {
			cursor = stCursor.Render("› ")
			line = stCursor.Render(line)
		}
		out += cursor + line + "\n"
	}
	return out
}

func typeOr(s *store.Session) string {
	if s.Type == "" {
		return "classifying"
	}
	return string(s.Type)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

package tui

import (
	"fmt"
	"strings"
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

// renderList renders the agent list windowed to exactly `height` lines and
// `width` columns of inner content, always keeping the selected row visible.
func renderList(sessions []*store.Session, cursor, width, height int) string {
	if height < 1 {
		height = 1
	}
	if len(sessions) == 0 {
		return padTo(stMuted.Render("No agents — press n to create one"), height)
	}
	n := len(sessions)
	visible := height
	hidden := n > height
	if hidden {
		if visible = height - 1; visible < 1 {
			visible = 1
		}
	}
	top := listWindow(n, cursor, visible)

	var b strings.Builder
	used := 0
	for i := top; i < top+visible && i < n; i++ {
		s := sessions[i]
		label, st := badge(s.Status)
		line := fmt.Sprintf("%-12s %-9s %-11s %-5s %s",
			trunc(s.ID, 12), trunc(typeOr(s), 9), st.Render(label), age(s.UpdatedAt),
			trunc(s.Subject, max(0, width-44)))
		cur := "  "
		if i == cursor {
			cur = stCursor.Render("› ")
			line = stCursor.Render(line)
		}
		b.WriteString(cur + line + "\n")
		used++
	}
	if hidden && height > 1 {
		var parts []string
		if top > 0 {
			parts = append(parts, fmt.Sprintf("▲ %d more", top))
		}
		if below := n - (top + visible); below > 0 {
			parts = append(parts, fmt.Sprintf("▼ %d more", below))
		}
		b.WriteString(stMuted.Render(strings.Join(parts, "   ")) + "\n")
		used++
	}
	for ; used < height; used++ {
		b.WriteString("\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// padTo pads s with blank lines to exactly height lines.
func padTo(s string, height int) string {
	for cur := strings.Count(s, "\n") + 1; cur < height; cur++ {
		s += "\n"
	}
	return s
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

// listWindow returns the index of the first row to render so a window of
// `visible` rows always contains the cursor. Stateless — derived from cursor +
// height each render, so no scroll state is kept on the Model.
func listWindow(n, cursor, visible int) int {
	if visible < 1 || n <= visible {
		return 0
	}
	top := 0
	if cursor >= visible {
		top = cursor - visible + 1
	}
	if maxTop := n - visible; top > maxTop {
		top = maxTop
	}
	if top < 0 {
		top = 0
	}
	return top
}

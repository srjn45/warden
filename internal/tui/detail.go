package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	"github.com/srajanpathak/agentctl/internal/store"
)

func renderDetail(s *store.Session, vp viewport.Model, outputFocused bool, width int) string {
	if s == nil {
		return stMuted.Render("Select an agent")
	}
	label, st := badge(s.Status)
	head := stPaneTitle.Render(s.ID) + " " + st.Render(label) + "  " + stMuted.Render(typeOr(s))
	meta := stMuted.Render("dir: " + dashIfEmpty(s.Workdir))
	subj := stMuted.Render("subject: " + dashIfEmpty(s.Subject))

	outTitle := stPaneTitle.Render("─ output ") + stMuted.Render(focusHint(outputFocused))
	out := vp.View()

	hist := stPaneTitle.Render("─ history ─") + "\n" + renderHistory(s, 6)

	return strings.Join([]string{head, meta, subj, "", outTitle, out, "", hist}, "\n")
}

func focusHint(focused bool) string {
	if focused {
		return "(scrolling — tab/esc to leave)"
	}
	return "(tab to scroll)"
}

func renderHistory(s *store.Session, n int) string {
	ev := s.Events
	if len(ev) == 0 {
		return stMuted.Render("no events yet")
	}
	start := 0
	if len(ev) > n {
		start = len(ev) - n
	}
	var b strings.Builder
	for _, e := range ev[start:] {
		fmt.Fprintf(&b, "%s  %-14s %s\n", e.TS.Format("15:04:05"), e.Type, trunc(e.Detail, 40))
	}
	return b.String()
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

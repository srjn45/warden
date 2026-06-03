package tui

import (
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

	return strings.Join([]string{head, meta, subj, "", outTitle, out}, "\n")
}

func focusHint(focused bool) string {
	if focused {
		return "(scrolling — tab/esc to leave)"
	}
	return "(tab to scroll)"
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

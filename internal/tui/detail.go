package tui

import "fmt"

func (m Model) renderDetail(width int) string {
	s := m.selected()
	if s == nil {
		return stMuted.Render("Select an agent")
	}
	label, st := badge(s.Status)
	head := stPaneTitle.Render(s.ID) + " " + st.Render(label)
	meta := stMuted.Render(fmt.Sprintf("dir: %s", dashIfEmpty(s.Workdir)))
	subj := stMuted.Render(fmt.Sprintf("subject: %s", dashIfEmpty(s.Subject)))
	return fmt.Sprintf("%s\n%s\n%s", head, meta, subj)
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

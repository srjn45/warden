package orchestrator

import (
	"io"

	"github.com/charmbracelet/lipgloss"
)

// styler colours the REPL. It builds its styles from a writer-bound lipgloss
// renderer, so colour is enabled only when the destination is a real terminal
// (and honours NO_COLOR) — writing to a test buffer or a pipe yields plain text
// automatically, which keeps tests assertion-friendly.
type styler struct {
	prompt  lipgloss.Style
	banner  lipgloss.Style
	hint    lipgloss.Style
	tool    lipgloss.Style
	ok      lipgloss.Style
	errorS  lipgloss.Style
	heading lipgloss.Style
}

func newStyler(w io.Writer) *styler {
	r := lipgloss.NewRenderer(w)
	return &styler{
		prompt:  r.NewStyle().Bold(true).Foreground(lipgloss.Color("44")),  // cyan
		banner:  r.NewStyle().Bold(true).Foreground(lipgloss.Color("63")),  // indigo
		hint:    r.NewStyle().Faint(true),                                  // dim
		tool:    r.NewStyle().Bold(true).Foreground(lipgloss.Color("178")), // amber
		ok:      r.NewStyle().Foreground(lipgloss.Color("42")),             // green
		errorS:  r.NewStyle().Foreground(lipgloss.Color("203")),            // red
		heading: r.NewStyle().Bold(true),
	}
}

// Promptf renders the editor prompt ("warden› ").
func (s *styler) Promptf() string { return s.prompt.Render("warden› ") }

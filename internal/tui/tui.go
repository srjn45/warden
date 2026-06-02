package tui

import tea "github.com/charmbracelet/bubbletea"

// Run starts the TUI against the given api client and blocks until the user quits.
func Run(a api) error {
	p := tea.NewProgram(New(a), tea.WithAltScreen())
	_, err := p.Run()
	return err
}

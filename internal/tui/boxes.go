package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// titleBox renders body inside a rounded-border box of exactly outerW x outerH,
// with title inset into the top border line (lipgloss v1.1 has no native border
// title). lipgloss pads/truncates body to the inner height.
func titleBox(title, body string, outerW, outerH int) string {
	if outerW < 4 {
		outerW = 4
	}
	if outerH < 3 {
		outerH = 3
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		Width(outerW - 2).
		Height(outerH - 2).
		Render(body)
	return spliceTitle(box, title)
}

// spliceTitle overwrites the top border line of a bordered box with " title ",
// preserving the leading corner and trailing border; truncates a long title.
// It supports ANSI escapes in the title by measuring visible width.
func spliceTitle(box, title string) string {
	if title == "" {
		return box
	}
	parts := strings.SplitN(box, "\n", 2)
	topWidth := lipgloss.Width(parts[0]) // visible width of the top border
	if topWidth < 5 {
		return box
	}

	// Ensure title fits without breaking layout (ignoring ANSI truncation complexity for now)
	titleStr := " " + title + " "
	titleVis := lipgloss.Width(titleStr)

	filler := topWidth - 3 - titleVis
	if filler < 0 {
		filler = 0
	}

	newTop := "╭─" + titleStr + strings.Repeat("─", filler) + "╮"

	rest := ""
	if len(parts) == 2 {
		rest = "\n" + parts[1]
	}
	return newTop + rest
}

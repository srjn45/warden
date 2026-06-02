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
func spliceTitle(box, title string) string {
	if title == "" {
		return box
	}
	parts := strings.SplitN(box, "\n", 2)
	top := []rune(parts[0])
	if len(top) < 5 {
		return box
	}
	inner := len(top) - 3 // writable columns [2 .. len-2)
	label := []rune(" " + trunc(title, max(0, inner-1)) + " ")
	if len(label) > inner {
		label = label[:inner]
	}
	for i, r := range label {
		top[2+i] = r
	}
	rest := ""
	if len(parts) == 2 {
		rest = "\n" + parts[1]
	}
	return string(top) + rest
}

package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/require"
)

func TestTitleBoxDimsAndTitle(t *testing.T) {
	out := titleBox("Agents (3)", "row1\nrow2", 24, 6)
	lines := strings.Split(out, "\n")
	require.Len(t, lines, 6, "exactly outerH lines")
	require.Contains(t, lines[0], "Agents (3)", "title inset into the top border")
	require.True(t, strings.HasPrefix(lines[0], "╭"), "rounded top-left corner preserved")
	require.Equal(t, 24, lipgloss.Width(out), "box is outerW wide")
}

func TestTitleBoxTruncatesLongTitle(t *testing.T) {
	out := titleBox("a very long title that does not fit", "x", 16, 4)
	require.Equal(t, 16, lipgloss.Width(out), "over-long title still fits the box width")
}

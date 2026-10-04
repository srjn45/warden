package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/stretchr/testify/require"
)

func logsModel(t *testing.T, dest string) controlPaneModel {
	t.Helper()
	t.Setenv("WARDEN_TUI_LOG", dest)
	m := newListPane(&fakeAPI{}, "%9", "")
	return lstep(m, tea.WindowSizeMsg{Width: 80, Height: 24})
}

func TestLogViewerToggle(t *testing.T) {
	p := filepath.Join(t.TempDir(), "tui.log")
	require.NoError(t, os.WriteFile(p, []byte("level=INFO msg=hello\n"), 0o600))
	m := logsModel(t, p)
	m = lstep(m, key("l"))
	require.Equal(t, modeLogs, m.mode)
	m = lstep(m, key("l"))
	require.Equal(t, modeNormal, m.mode)
	m = lstep(m, key("l"))
	m = lstep(m, key("esc"))
	require.Equal(t, modeNormal, m.mode)
}

func TestLogViewerShowsTailAtBottomWithTitle(t *testing.T) {
	p := filepath.Join(t.TempDir(), "tui.log")
	var sb strings.Builder
	for i := 0; i < 200; i++ {
		sb.WriteString("level=INFO msg=line\n")
	}
	sb.WriteString("level=ERROR msg=the-last-line\n")
	require.NoError(t, os.WriteFile(p, []byte(sb.String()), 0o600))
	m := logsModel(t, p)
	m = lstep(m, key("l"))
	v := m.View()
	require.Contains(t, v, "Logs  (esc / l to close · G bottom · g top)")
	require.Contains(t, v, "the-last-line")
	m = lstep(m, key("g"))
	require.NotContains(t, m.View(), "the-last-line")
	m = lstep(m, key("G"))
	require.Contains(t, m.View(), "the-last-line")
}

func TestLogViewerRefreshesOnTick(t *testing.T) {
	p := filepath.Join(t.TempDir(), "tui.log")
	require.NoError(t, os.WriteFile(p, []byte("level=INFO msg=first\n"), 0o600))
	m := logsModel(t, p)
	m = lstep(m, key("l"))
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, _ = f.WriteString("level=WARN msg=second\n")
	f.Close()
	m = lstep(m, tickMsg{})
	require.Contains(t, m.View(), "second")
}

func TestLogViewerEmptyStates(t *testing.T) {
	for _, dest := range []string{"off", "discard", "/dev/null", filepath.Join(t.TempDir(), "missing.log")} {
		m := logsModel(t, dest)
		m = lstep(m, key("l"))
		require.Equal(t, modeLogs, m.mode)
		require.NotPanics(t, func() { _ = m.View() })
		require.NotEmpty(t, m.logTail, dest)
	}
	p := filepath.Join(t.TempDir(), "empty.log")
	require.NoError(t, os.WriteFile(p, nil, 0o600))
	require.Contains(t, logsModel(t, p).logTailAfterOpen(), "empty")
}

func (m controlPaneModel) logTailAfterOpen() string {
	nm, _ := m.Update(key("l"))
	return nm.(controlPaneModel).logTail
}

func TestColorizeLogsLevels(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI)
	defer lipgloss.SetColorProfile(prev)
	out := colorizeLogs("level=ERROR a\nlevel=WARN b\nlevel=INFO c\nplain")
	lines := strings.Split(out, "\n")
	require.Contains(t, lines[0], "\x1b[31m")
	require.Contains(t, lines[1], "\x1b[33m")
	require.Contains(t, lines[2], "\x1b[36m")
	require.Equal(t, "plain", lines[3])
}

func TestRightStillExpandsAndCDigestUnaffected(t *testing.T) {
	m := logsModel(t, "off")
	m = lstep(m, key("c"))
	require.Equal(t, modeInspector, m.mode)
}

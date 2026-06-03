package approval

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return string(b)
}

func TestParseRecognizesBashPrompt(t *testing.T) {
	a, ok := Parse(readFixture(t, "bash_prompt.txt"))
	require.True(t, ok)
	require.GreaterOrEqual(t, len(a.Options), 2)
	require.NotEmpty(t, a.Question)
	require.Equal(t, 1, a.SelectedIdx)
	require.Equal(t, "Bash(rm -rf node_modules)", a.Action)
}

func TestParseRecognizesEditPrompt(t *testing.T) {
	a, ok := Parse(readFixture(t, "edit_prompt.txt"))
	require.True(t, ok)
	require.Equal(t, 1, a.SelectedIdx)
	require.Equal(t, "Edit(src/auth/middleware.ts)", a.Action)
	require.GreaterOrEqual(t, len(a.Options), 2)
}

func TestParseRejectsFreeform(t *testing.T) {
	_, ok := Parse(readFixture(t, "freeform.txt"))
	require.False(t, ok)
}

func TestParseRejectsNoBox(t *testing.T) {
	_, ok := Parse(readFixture(t, "no_box.txt"))
	require.False(t, ok)
}

func TestParseRejectsSingleOption(t *testing.T) {
	_, ok := Parse("Something\n  1. Only choice\n")
	require.False(t, ok)
}

func TestParseRejectsNonSequential(t *testing.T) {
	_, ok := Parse("Do you want to proceed?\n  1. Yes\n  3. No\n")
	require.False(t, ok)
}

func TestParseRejectsBareNumberedList(t *testing.T) {
	pane := "Here is my plan:\n\n  1. Extract validation\n  2. Add a test\n  3. Update call sites\n"
	_, ok := Parse(pane)
	require.False(t, ok)
}

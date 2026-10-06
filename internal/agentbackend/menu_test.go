package agentbackend

import (
	"testing"

	"github.com/stretchr/testify/require"
)

const agyRunCommandPane = `● Bash(git rev-parse HEAD origin/main) (ctrl+o to expand)

Command
────────────────────────────────────────────

Requesting permission for:
   git rev-parse HEAD origin/main

Run this command?
> 1. Yes, run command
  2. Yes, and always allow in this conversation for commands that start with 'git rev-parse'
  3. No, cancel

  ↑/↓ Navigate · tab Amend
esc to cancel                                  Gemini 3.8 Flash · medium
`

func TestFindMenu(t *testing.T) {
	key, ok := FindMenu(agyRunCommandPane)
	require.True(t, ok)
	require.Contains(t, key, "Run this command?")
	require.Contains(t, key, "No, cancel")

	// The key ignores redraws outside the menu (status bar, spinner).
	other, ok := FindMenu(agyRunCommandPane + "⣟ Running...\n")
	require.True(t, ok)
	require.Equal(t, key, other)

	for name, pane := range map[string]string{
		"idle composer": "some output\n────────\n>\n────────\n? for shortcuts\n",
		"lone cursor":   "done.\n\n> type here\n",
		"plain prose":   "Here are the steps:\n  1. do this\n  2. do that\n",
		"empty":         "",
	} {
		_, ok := FindMenu(pane)
		require.False(t, ok, name)
	}
}

func TestLocateOptions(t *testing.T) {
	opts := []string{
		"Yes, run command",
		"Yes, and always allow in this conversation for commands that start with 'git rev-parse'",
		"No, cancel",
	}
	loc, ok := LocateOptions(agyRunCommandPane, opts)
	require.True(t, ok)
	require.Equal(t, 1, loc.Selected)
	require.True(t, loc.Numbered)

	// Unnumbered cursor menu, cursor on the second option, description lines between.
	pane := "Do you trust this folder?\n\n   No, exit\n     leaves the CLI\n ❯ Yes, I trust this folder\n\n Enter to confirm\n"
	loc, ok = LocateOptions(pane, []string{"No, exit", "Yes, I trust this folder"})
	require.True(t, ok)
	require.Equal(t, 2, loc.Selected)
	require.False(t, loc.Numbered)

	// A paraphrased, reordered or invented label does not locate.
	_, ok = LocateOptions(agyRunCommandPane, []string{"Yes", "No"})
	require.False(t, ok, "paraphrased labels")
	_, ok = LocateOptions(agyRunCommandPane, []string{"No, cancel", "Yes, run command"})
	require.False(t, ok, "reordered labels")
	_, ok = LocateOptions(agyRunCommandPane, []string{"Yes, run command"})
	require.False(t, ok, "a single option is not a menu")

	// No cursor glyph: located, but the selection is unknown.
	loc, ok = LocateOptions("Pick:\n  1. Alpha\n  2. Beta\n", []string{"Alpha", "Beta"})
	require.True(t, ok)
	require.Zero(t, loc.Selected)
}

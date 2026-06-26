package orchestrator

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A bare "/" surfaces the whole menu (capped), so typing just the slash reveals
// every command — Claude-Code style.
func TestSuggestMatches_BareSlashShowsAll(t *testing.T) {
	m := suggestMatches("/")
	require.NotEmpty(t, m)
	require.GreaterOrEqual(t, len(m), len(commandList), "every command plus /help is offered")
}

// A prefix narrows to the matching verbs, by canonical name or alias.
func TestSuggestMatches_NarrowsByPrefix(t *testing.T) {
	usages := func(s string) []string {
		var u []string
		for _, m := range suggestMatches(s) {
			u = append(u, m.usage)
		}
		return u
	}
	require.Contains(t, usages("/sp"), "/spawn <prompt...>")
	require.NotContains(t, usages("/sp"), "/agents", "an unrelated verb is filtered out")

	// An alias prefix resolves to its canonical command's usage.
	require.Contains(t, usages("/ls"), "/agents", "the /ls alias surfaces /agents")
}

// Once the operator types a space (moving on to arguments) or a non-slash line,
// the panel bows out — the verb is already chosen.
func TestSuggestMatches_QuietForArgsAndPlainText(t *testing.T) {
	require.Nil(t, suggestMatches("/spawn fix the bug"), "args typed → no panel")
	require.Nil(t, suggestMatches("review the docs"), "plain NL → no panel")
	require.Nil(t, suggestMatches(""), "empty line → no panel")
}

// formatSuggestions renders one row per match with the usage and summary, and
// is plain text when the writer is not a terminal.
func TestFormatSuggestions_RendersRows(t *testing.T) {
	var buf bytes.Buffer
	st := newStyler(&buf) // non-TTY ⇒ no ANSI
	out := formatSuggestions(suggestMatches("/sp"), st)
	require.Contains(t, out, "/spawn <prompt...>")
	require.Contains(t, out, "spawn an agent to do a task")
	require.Empty(t, formatSuggestions(nil, st), "no matches ⇒ empty panel")
}

// More than suggestMax matches are capped with a "…and N more" footer so the
// panel never floods the screen.
func TestFormatSuggestions_CapsWithMoreFooter(t *testing.T) {
	var buf bytes.Buffer
	st := newStyler(&buf)
	out := formatSuggestions(suggestMatches("/"), st)
	require.Equal(t, suggestMax, strings.Count(out, "\n"), "capped to suggestMax rows + footer line")
	require.Contains(t, out, "more — keep typing to narrow")
}

// The Listener paints a panel for a slash prefix and restores the cursor (DECSC
// /DECRC), and clears it on submit.
func TestSuggester_PaintsAndClears(t *testing.T) {
	var buf bytes.Buffer
	sg := &suggester{out: &buf, style: newStyler(&buf)}

	sg.OnChange([]rune("/sp"), 3, 'p')
	painted := buf.String()
	require.Contains(t, painted, "/spawn <prompt...>", "panel painted under the line")
	require.Contains(t, painted, "\0337", "saves the cursor before painting")
	require.Contains(t, painted, "\0338", "restores the cursor after painting")
	require.True(t, sg.shown)

	buf.Reset()
	sg.OnChange(nil, 0, '\r') // submit
	require.Equal(t, "\033[0J", buf.String(), "panel cleared on submit")
	require.False(t, sg.shown)
}

// Tab (and reverse-search) hand the area below the line to readline, so the
// panel clears itself instead of overprinting readline's own UI.
func TestSuggester_YieldsToReadlineUI(t *testing.T) {
	var buf bytes.Buffer
	sg := &suggester{out: &buf, style: newStyler(&buf)}
	sg.OnChange([]rune("/s"), 2, 's') // paint first
	require.True(t, sg.shown)

	buf.Reset()
	sg.OnChange([]rune("/s"), 2, '\t') // Tab → clear, let readline complete
	require.Contains(t, buf.String(), "\033[0J", "panel cleared for Tab completion")
	require.False(t, sg.shown)
}

// A non-slash line never paints, and clears any panel left from a prior key.
func TestSuggester_ClearsWhenLineNoLongerSlash(t *testing.T) {
	var buf bytes.Buffer
	sg := &suggester{out: &buf, style: newStyler(&buf)}
	sg.OnChange([]rune("/sp"), 3, 'p')
	require.True(t, sg.shown)

	buf.Reset()
	sg.OnChange([]rune("review"), 6, 'w') // typed past the slash word
	require.Contains(t, buf.String(), "\033[0J", "stale panel cleared")
	require.False(t, sg.shown)
}

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

func TestParseRecognizesPlainBoxBashPrompt(t *testing.T) {
	// Current Claude Code renders the options as plain indented lines under a
	// ──── divider — no │ on the option lines (older boxes drew │ per line).
	// Captured live from a supervised agent's rm prompt; the regression guard
	// for the fixtures-vs-real-box gap.
	a, ok := Parse(readFixture(t, "bash_prompt_plain.txt"))
	require.True(t, ok, "a real plain-option permission box must be recognized")
	require.Equal(t, []string{
		"Yes",
		"Yes, and always allow access to tmp/ from this project",
		"No",
	}, a.Options)
	require.Equal(t, 1, a.SelectedIdx)
	require.Equal(t, "Do you want to proceed?", a.Question)
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

func TestClassifyOptions(t *testing.T) {
	cases := []struct {
		name       string
		opts       []string
		wantIdx    int
		wantSticky bool
	}{
		{"plain yes/no", []string{"Yes", "No"}, 1, false},
		{
			"plain fixture order (non-sticky Yes is option 1)",
			[]string{"Yes", "Yes, and always allow access to tmp/ from this project", "No"},
			1, false,
		},
		{
			"sticky first (least-privilege picks plain Yes at index 2)",
			[]string{"Yes, and don't ask again for Bash commands", "Yes", "No"},
			2, false,
		},
		{"sticky only", []string{"Yes, allow always", "No, keep asking"}, 1, true},
		{"no affirmative", []string{"No", "Cancel"}, 0, false},
		{
			"negation trap (No, and don't ask again is not affirmative)",
			[]string{"No, and don't ask again", "Yes"},
			2, false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idx, sticky := classifyOptions(tc.opts)
			require.Equal(t, tc.wantIdx, idx)
			require.Equal(t, tc.wantSticky, sticky)
		})
	}
}

func TestParseStickyFirstFixture(t *testing.T) {
	a, ok := Parse(readFixture(t, "bash_prompt_sticky_first.txt"))
	require.True(t, ok)
	require.Equal(t, 2, a.AffirmativeIdx, "least-privilege picks the plain Yes at index 2")
	require.False(t, a.AffirmativeSticky)
}

func TestParsePlainFixtureClassifies(t *testing.T) {
	a, ok := Parse(readFixture(t, "bash_prompt_plain.txt"))
	require.True(t, ok)
	require.Equal(t, 1, a.AffirmativeIdx)
	require.False(t, a.AffirmativeSticky)
}

func TestIsDestructive(t *testing.T) {
	cases := []struct {
		name string
		a    Approval
		want bool
	}{
		{"rm -rf", Approval{Action: "Bash(rm -rf build)"}, true},
		{"git push --force", Approval{Action: "Bash(git push --force)"}, true},
		{"git reset --hard", Approval{Action: "Bash(git reset --hard)"}, true},
		{"overwrite question", Approval{Question: "Overwrite existing file?"}, true},
		{
			"destructive affirmative label",
			Approval{Options: []string{"Yes, delete it", "No"}, AffirmativeIdx: 1},
			true,
		},
		{"read is safe", Approval{Action: "Read(src/x.go)"}, false},
		{"edit is safe", Approval{Action: "Edit(src/x.go)"}, false},
		{
			"benign yes/no",
			Approval{Question: "Do you want to proceed?", Options: []string{"Yes", "No"}, AffirmativeIdx: 1},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad, marker := IsDestructive(tc.a)
			require.Equal(t, tc.want, bad)
			if tc.want {
				require.NotEmpty(t, marker)
			} else {
				require.Empty(t, marker)
			}
		})
	}
}

func TestFingerprintStableAndDistinct(t *testing.T) {
	a := Fingerprint([]string{"Yes", "No"})
	require.Equal(t, a, Fingerprint([]string{"Yes", "No"})) // stable
	require.NotEqual(t, a, Fingerprint([]string{"Yes", "Maybe"}))
	require.NotEmpty(t, a)
}

func TestBuildViewRecognized(t *testing.T) {
	v := BuildView("agent-1", readFixture(t, "bash_prompt.txt"))
	require.Equal(t, "agent-1", v.ID)
	require.True(t, v.Recognized)
	require.NotEmpty(t, v.Fingerprint)
	require.GreaterOrEqual(t, len(v.Options), 2)
}

func TestBuildViewUnrecognized(t *testing.T) {
	v := BuildView("agent-2", readFixture(t, "freeform.txt"))
	require.Equal(t, "agent-2", v.ID)
	require.False(t, v.Recognized)
	require.Empty(t, v.Options)
}

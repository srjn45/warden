package lifecycle

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestParseCommitMessage(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "feat(api): add retry", "feat(api): add retry"},
		{"trims space", "  fix: handle nil  \n", "fix: handle nil"},
		{"strips backticks", "`chore: bump deps`", "chore: bump deps"},
		{"strips quotes", "\"docs: fix typo\"", "docs: fix typo"},
		{"first real line wins", "```\nfeat: thing\n```", "feat: thing"},
		{"skips blank lines", "\n\n  refactor: split file\nbody text", "refactor: split file"},
		{"empty reply", "   \n  ", ""},
		{"caps runaway length", "feat: " + strings.Repeat("x", 200), "feat: " + strings.Repeat("x", 94)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, parseCommitMessage(c.in))
		})
	}
}

func TestDeterministicCommitMessage(t *testing.T) {
	cases := []struct {
		name  string
		files []string
		want  string
	}{
		{"single root file", []string{"main.go"}, "chore: update main.go"},
		{"single scoped file", []string{"internal/foo.go"}, "chore(internal): update foo.go"},
		{"docs only", []string{"docs/a.md", "docs/b.md"}, "docs(docs): update 2 files"},
		{"markdown at root", []string{"README.md"}, "docs: update README.md"},
		{"tests only", []string{"internal/foo_test.go"}, "test(internal): update foo_test.go"},
		{"mixed dirs drop scope", []string{"internal/a.go", "cmd/b.go"}, "chore: update 2 files"},
		{"mixed kinds are chore", []string{"docs/a.md", "main.go"}, "chore: update 2 files"},
		{"no files", nil, "chore: update working tree"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, deterministicCommitMessage(c.files))
		})
	}
}

func TestCapDiffIsValidUTF8(t *testing.T) {
	// A diff longer than the cap, ending mid multi-byte rune, must stay valid UTF-8.
	big := strings.Repeat("a", commitMsgMaxDiffBytes-1) + "€" // '€' is 3 bytes, straddles the cap
	got := capDiff(big)
	require.LessOrEqual(t, len(got), commitMsgMaxDiffBytes)
	require.True(t, utf8.ValidString(got))
}

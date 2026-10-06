package knownprompts

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Placeholder stands for a variable span (a command, a path) in a template.
const Placeholder = "{*}"

// minActionLen is the shortest Action text replaced wherever it occurs; shorter
// strings ("ls") are too likely to be coincidence. Quoted spans are always
// replaced regardless.
const minActionLen = 3

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' }

// Template turns a concrete prompt text (a question or an option label) into a
// reusable shape: every occurrence of action, and every quoted ('…'), backticked
// (`…`) or double-quoted ("…") span, becomes Placeholder. A quote only opens
// after a non-word rune and only closes before one, so the apostrophe in "don't"
// never starts a span. Template is idempotent.
func Template(s, action string) string {
	s = replaceWord(s, strings.TrimSpace(action))
	return replaceQuoted(s)
}

// replaceWord replaces occurrences of action that are not embedded in a larger
// word.
func replaceWord(s, action string) string {
	if len(action) < minActionLen {
		return s
	}
	var b strings.Builder
	for {
		i := strings.Index(s, action)
		if i < 0 {
			break
		}
		end := i + len(action)
		before, _ := utf8.DecodeLastRuneInString(s[:i])
		after, _ := utf8.DecodeRuneInString(s[end:])
		bounded := (i == 0 || !isWordRune(before)) && (end == len(s) || !isWordRune(after))
		if bounded {
			b.WriteString(s[:i])
			b.WriteString(Placeholder)
		} else {
			b.WriteString(s[:end])
		}
		s = s[end:]
	}
	b.WriteString(s)
	return b.String()
}

func replaceQuoted(s string) string {
	r := []rune(s)
	var b strings.Builder
	for i := 0; i < len(r); i++ {
		q := r[i]
		if (q == '\'' || q == '`' || q == '"') && (i == 0 || !isWordRune(r[i-1])) {
			if j := closeQuote(r, i); j > 0 {
				b.WriteRune(q)
				b.WriteString(Placeholder)
				b.WriteRune(q)
				i = j
				continue
			}
		}
		b.WriteRune(r[i])
	}
	return b.String()
}

// closeQuote returns the index of the quote closing the one at open, or -1.
func closeQuote(r []rune, open int) int {
	for j := open + 2; j < len(r); j++ {
		if r[j] == r[open] && (j+1 == len(r) || !isWordRune(r[j+1])) {
			return j
		}
	}
	return -1
}

// normalize is the key form of a template: case-folded, whitespace collapsed.
func normalize(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// literalLen counts the template's non-placeholder letters and digits.
func literalLen(tmpl string) int {
	n := 0
	for _, r := range strings.ReplaceAll(tmpl, Placeholder, "") {
		if isWordRune(r) {
			n++
		}
	}
	return n
}

var wsRe = regexp.MustCompile(`\s+`)

// compile builds the matcher for one label template: literal text must match
// (whitespace-flexible), each placeholder matches any non-empty run.
func compile(tmpl string) (*regexp.Regexp, error) {
	parts := strings.Split(strings.TrimSpace(tmpl), Placeholder)
	for i, p := range parts {
		parts[i] = wsRe.ReplaceAllString(regexp.QuoteMeta(p), `\s+`)
	}
	return regexp.Compile(`^` + strings.Join(parts, `.+?`) + `$`)
}

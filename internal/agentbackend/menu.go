package agentbackend

import (
	"regexp"
	"strings"
)

// This file is the backend-neutral half of prompt recognition. A backend's own
// ParseApproval keys on that CLI's exact wording, which the vendor is free to
// change in any release. When no parser matches, warden asks a model what the
// pane is showing (see fastbrain.RecognizePrompt) — but a model's answer is only
// a claim. The helpers here are what make it safe to act on: FindMenu is the
// cheap structural gate that decides a pane is worth asking about at all, and
// LocateOptions re-finds the claimed options in the live pane, so every option
// warden may select is text that is really on screen, in order, with a real
// cursor on it.

// menuCursors are the glyphs CLIs draw in front of the highlighted option.
const menuCursors = ">❯›▶▸→➜"

// menuWindow is how many trailing pane lines are searched for a menu: a blocking
// prompt sits at the bottom of the pane.
const menuWindow = 30

var menuNumberRe = regexp.MustCompile(`^(\d{1,2})[.)]\s+(.*)$`)

// menuLine splits one pane line into its parts: whether it carries a cursor
// glyph, its option number (0 when unnumbered), the remaining label, and the
// column (in runes) the label starts at.
func menuLine(line string) (cursor bool, number int, label string, col int) {
	r := []rune(line)
	i := 0
	skip := func() {
		for i < len(r) && (r[i] == ' ' || r[i] == '\t') {
			i++
		}
	}
	skip()
	if i >= len(r) {
		return false, 0, "", 0
	}
	if strings.ContainsRune(menuCursors, r[i]) {
		j := i
		i++
		skip()
		if i >= len(r) {
			return false, 0, strings.TrimSpace(string(r[j:])), j // a bare glyph is just text
		}
		cursor = true
	}
	if m := menuNumberRe.FindStringSubmatch(string(r[i:])); m != nil {
		for _, c := range m[1] {
			number = number*10 + int(c-'0')
		}
		i += len([]rune(string(r[i:]))) - len([]rune(m[2]))
	}
	return cursor, number, strings.TrimSpace(string(r[i:])), i
}

// hasText reports whether s holds a letter or digit (a rule or box-drawing line
// does not).
func hasText(s string) bool {
	for _, r := range s {
		if r > 127 {
			if !strings.ContainsRune("─━═│┃╭╮╰╯┌┐└┘├┤┬┴┼▄▀█░▒▓⣀⣿", r) {
				return true
			}
			continue
		}
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return true
		}
	}
	return false
}

// FindMenu reports whether the bottom of pane holds something shaped like a
// choice menu: a line carrying a cursor glyph with at least one adjacent line
// whose label starts in the same column (options of one menu are aligned; a
// user message echoed as "> text" above the reply is not). It deliberately knows
// nothing about wording. The returned key
// identifies the menu (its lines plus the few lines above it) so a caller can
// tell "the same menu is still showing" from "a new one appeared" even while a
// spinner or clock redraws elsewhere in the pane.
func FindMenu(pane string) (key string, ok bool) {
	lines := strings.Split(strings.TrimRight(pane, "\n"), "\n")
	lo := len(lines) - menuWindow
	if lo < 0 {
		lo = 0
	}
	for i := len(lines) - 1; i >= lo; i-- {
		cursor, _, label, col := menuLine(lines[i])
		if !cursor || !hasText(label) {
			continue
		}
		// Grow the run of adjacent text lines around the cursor line.
		start, end := i, i
		for start-1 >= lo && isMenuNeighbor(lines[start-1], col) {
			start--
		}
		for end+1 < len(lines) && isMenuNeighbor(lines[end+1], col) {
			end++
		}
		if end == start {
			continue // a lone "> " composer line is not a menu
		}
		ctx := start - 6
		if ctx < 0 {
			ctx = 0
		}
		var b strings.Builder
		for _, l := range lines[ctx : end+1] {
			b.WriteString(strings.TrimRight(l, " \t"))
			b.WriteByte('\n')
		}
		return b.String(), true
	}
	return "", false
}

// isMenuNeighbor reports whether line could be another option of the menu a
// cursor line belongs to: text without its own cursor glyph whose label starts in
// the same column.
func isMenuNeighbor(line string, col int) bool {
	cursor, _, label, c := menuLine(line)
	return !cursor && c == col && hasText(label)
}

// MenuLocation is where a set of option labels sits in a pane.
type MenuLocation struct {
	// Selected is the 1-based option the cursor glyph is on; 0 when no option (or
	// more than one) carries a cursor.
	Selected int
	// Numbered is true when every option line is numbered 1..N in order.
	Numbered bool
	// First is the pane line index of option 1.
	First int
}

// LocateOptions finds options in pane as they would appear in a live menu: each
// on its own line, in order, at most two lines apart (a menu may put a short
// description under an option), within the bottom window. The bottom-most match
// wins. It is the verification step for a model-recognized prompt: a label the
// model invented, reordered or paraphrased does not locate.
func LocateOptions(pane string, options []string) (MenuLocation, bool) {
	if len(options) < 2 {
		return MenuLocation{}, false
	}
	loc, _, ok := LocateMatching(pane, len(options), func(i int, label string) bool {
		return options[i] != "" && label == strings.TrimSpace(options[i])
	})
	return loc, ok
}

// LocateMatching is LocateOptions generalized to a predicate: it finds n option
// lines in order (same geometry and bottom-most-wins rule) where match(i, label)
// accepts the i-th label, and returns the labels exactly as they appear on
// screen (number and cursor glyph stripped). It lets a caller match option
// shapes — templates — rather than fixed text, while still reading the concrete
// labels from the pane.
func LocateMatching(pane string, n int, match func(i int, label string) bool) (MenuLocation, []string, bool) {
	if n < 2 {
		return MenuLocation{}, nil, false
	}
	lines := strings.Split(strings.TrimRight(pane, "\n"), "\n")
	lo := len(lines) - menuWindow
	if lo < 0 {
		lo = 0
	}
	for first := len(lines) - 1; first >= lo; first-- {
		if loc, labels, ok := locateFrom(lines, first, n, match); ok {
			return loc, labels, true
		}
	}
	return MenuLocation{}, nil, false
}

func locateFrom(lines []string, first, n int, match func(i int, label string) bool) (MenuLocation, []string, bool) {
	loc := MenuLocation{First: first, Numbered: true}
	cursors := 0
	at := first
	labels := make([]string, 0, n)
	for i := 0; i < n; i++ {
		found := -1
		limit := at
		if i > 0 {
			limit = at + 2
		}
		for j := at; j <= limit && j < len(lines); j++ {
			cursor, number, label, _ := menuLine(lines[j])
			if label == "" || !match(i, label) {
				// A skipped line may be a description under the option, but not
				// another option: that menu has an extra choice.
				if cursor || number > 0 {
					break
				}
				continue
			}
			found = j
			labels = append(labels, label)
			if cursor {
				cursors++
				loc.Selected = i + 1
			}
			if number != i+1 {
				loc.Numbered = false
			}
			break
		}
		if found < 0 {
			return MenuLocation{}, nil, false
		}
		at = found + 1
	}
	// Another numbered option right below the last one: the menu has more choices
	// than the shape being located.
	if at < len(lines) {
		if _, number, label, _ := menuLine(lines[at]); label != "" && number == n+1 {
			return MenuLocation{}, nil, false
		}
	}
	if cursors != 1 {
		loc.Selected = 0
	}
	return loc, labels, true
}

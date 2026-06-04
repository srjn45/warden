// Package approval parses Claude Code's numbered tool-permission prompt out of a
// tmux pane capture and fingerprints its options, so the daemon can offer
// one-click answers for recognized prompts and route everything else to attach.
package approval

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
)

// Approval is a recognized numbered tool-permission prompt.
type Approval struct {
	Action      string   // e.g. "Bash(rm -rf node_modules)"; "" if not found
	Question    string   // e.g. "Do you want to proceed?"; "" if not found
	Options     []string // option labels, top-down, 1-indexed by position
	SelectedIdx int      // 1-based index of the ❯-highlighted option; 0 if none
}

// optionRe matches a numbered option line, tolerating leading box-drawing
// characters and an optional ❯ selection cursor.
var optionRe = regexp.MustCompile(`^[\s│┃|]*(❯?)\s*(\d+)\.\s+(.+?)\s*$`)

// boxTrim strips leading/trailing box-drawing chrome from a non-option line.
var boxTrim = strings.NewReplacer("│", " ", "┃", " ", "|", " ", "╭", " ",
	"╮", " ", "╰", " ", "╯", " ", "─", " ")

// Parse recognizes the prompt only on a confident match: a contiguous run of
// options numbered 1..N (N>=2) at the bottom of the pane. Freeform prompts,
// multi-selects, text-entry fields, and partial redraws return ok=false.
func Parse(pane string) (Approval, bool) {
	lines := strings.Split(pane, "\n")

	// The live prompt sits at the bottom: find the last option line.
	end := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if optionRe.MatchString(lines[i]) {
			end = i
			break
		}
	}
	if end < 0 {
		return Approval{}, false
	}

	// Walk up while lines stay options; collect labels + the selected index.
	start := end
	for start-1 >= 0 && optionRe.MatchString(lines[start-1]) {
		start--
	}

	var opts []string
	sel := 0
	for i := start; i <= end; i++ {
		m := optionRe.FindStringSubmatch(lines[i])
		// Numbering must be sequential 1..N.
		if m[2] != strconv.Itoa(i-start+1) {
			return Approval{}, false
		}
		if m[1] == "❯" {
			sel = i - start + 1
		}
		opts = append(opts, strings.TrimSpace(m[3]))
	}
	if len(opts) < 2 {
		return Approval{}, false
	}

	// Distinguish a real permission menu from a bare numbered list in agent prose.
	// A real menu has a ❯ selection cursor on its default option, OR box/divider
	// chrome framing the prompt. We require one of those: older Claude boxes drew
	// │ on each option line, but current ones render plain indented options under
	// a ──── divider — so the chrome lives ABOVE the run, not on the option lines.
	if sel == 0 && !chromeNear(lines, start, end) {
		return Approval{}, false
	}

	a := Approval{Options: opts, SelectedIdx: sel}

	// Question = nearest non-empty line above the run; Action = the next
	// non-empty line above that, if it looks like a Tool(...) call.
	i := start - 1
	for ; i >= 0; i-- {
		if t := strings.TrimSpace(boxTrim.Replace(lines[i])); t != "" {
			a.Question = t
			i--
			break
		}
	}
	for ; i >= 0; i-- {
		t := strings.TrimSpace(boxTrim.Replace(lines[i]))
		if t == "" {
			continue
		}
		if looksLikeAction(t) {
			a.Action = t
		}
		break
	}
	return a, true
}

// chromeNear reports whether box-drawing chrome (panel borders or a ──── divider)
// appears on the option lines or in the few lines just above them — the frame a
// real permission box draws around its prompt, which a bare numbered list lacks.
func chromeNear(lines []string, start, end int) bool {
	from := start - 8
	if from < 0 {
		from = 0
	}
	for i := from; i <= end; i++ {
		if strings.ContainsAny(lines[i], "│┃|─╭╮╰╯") {
			return true
		}
	}
	return false
}

// looksLikeAction reports whether a line resembles a tool invocation header
// such as "Bash(...)" or "Edit(path)".
func looksLikeAction(s string) bool {
	open := strings.IndexByte(s, '(')
	return open > 0 && strings.HasSuffix(s, ")")
}

// View is the wire shape returned by GET /approvals and consumed by both UIs.
type View struct {
	ID          string   `json:"id"`
	Action      string   `json:"action"`
	Question    string   `json:"question"`
	Options     []string `json:"options"`
	Fingerprint string   `json:"fingerprint"`
	Recognized  bool     `json:"recognized"`
}

// Fingerprint is a stable short hash of the option labels. The UI echoes it back
// on answer so the daemon can prove the prompt has not changed underneath it.
func Fingerprint(opts []string) string {
	h := sha256.Sum256([]byte(strings.Join(opts, "\x00")))
	return hex.EncodeToString(h[:8]) // 16 hex chars
}

// BuildView parses pane for session id, returning a recognized View with options
// + fingerprint, or an unrecognized View (Recognized=false) to route to attach.
func BuildView(id, pane string) View {
	a, ok := Parse(pane)
	if !ok {
		return View{ID: id, Recognized: false}
	}
	return View{
		ID: id, Action: a.Action, Question: a.Question,
		Options: a.Options, Fingerprint: Fingerprint(a.Options), Recognized: true,
	}
}

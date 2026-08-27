package lifecycle

import (
	"encoding/json"
	"slices"
	"strings"
)

// DoneSentinel is the transcript marker a worker prints to declare completion
// when it cannot (or is not asked to) run `wd job done` mid-task. The bytes
// after it on the same line are a JSON object carrying the status + summary, so
// warden captures the completion in one shot — no extra interrogation turn.
//
//	<<WARDEN_DONE>>{"status":"success","summary":"added the done-signal"}
const DoneSentinel = "<<WARDEN_DONE>>"

// DoneSignal is a worker's parsed self-report: the outcome plus a one-line
// summary of what it did. Status is normalized to one of "success" / "failure"
// / "blocked" (defaulting to "success" when the worker omits it — reaching the
// signal at all means the worker believes it finished).
type DoneSignal struct {
	Status  string `json:"status"`
	Summary string `json:"summary"`
	Details string `json:"details,omitempty"`
}

// ParseDoneSignal scans a captured pane (or transcript tail) for the LAST
// `<<WARDEN_DONE>>{json}` sentinel line and returns the decoded signal. The last
// occurrence wins so a re-emitted signal supersedes an earlier one still on
// screen. ok is false when no well-formed sentinel is present: a marker with no
// JSON, or JSON that does not decode, is ignored rather than guessed at.
//
// The parser is deliberately lenient about surrounding text — the marker may be
// preceded by a prompt glyph, timestamp, or box-drawing border — it keys only on
// the marker substring and decodes the remainder of that line.
func ParseDoneSignal(pane string) (DoneSignal, bool) {
	lines := strings.Split(pane, "\n")
	for _, line := range slices.Backward(lines) {
		_, after, found := strings.Cut(line, DoneSentinel)
		if !found {
			continue
		}
		rest := strings.TrimSpace(after)
		if rest == "" {
			continue
		}
		// Trim any trailing box-border/pane decoration a TUI may have appended
		// after the closing brace so the raw JSON object decodes cleanly.
		if end := strings.LastIndex(rest, "}"); end >= 0 {
			rest = rest[:end+1]
		}
		var sig DoneSignal
		if err := json.Unmarshal([]byte(rest), &sig); err != nil {
			continue
		}
		sig.Status = NormalizeDoneStatus(sig.Status)
		return sig, true
	}
	return DoneSignal{}, false
}

// NormalizeDoneStatus maps a free-form self-reported status onto the three
// outcomes warden acts on. An empty or unrecognized value is treated as
// "success": a worker only signals done when it believes its work is complete,
// and any genuine failure is independently caught by exit-code / stuck
// detection rather than trusted from the self-report alone.
func NormalizeDoneStatus(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "failure", "failed", "fail", "error":
		return "failure"
	case "blocked", "block", "stuck", "needs-input", "needs_input":
		return "blocked"
	default:
		return "success"
	}
}

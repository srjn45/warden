package poller

import "strings"

const (
	// loopWindow is how many recent pane-change samples are retained per agent.
	// Only distinct-from-previous excerpts are recorded (the tick loop appends a
	// sample exactly when the pane changes), so a full window means the pane has
	// changed loopWindow times.
	loopWindow = 8
	// loopMinRepeats is how many times one excerpt must recur within the window
	// to count as an infinite loop. 3 is conservative: a single stray repeat
	// (e.g. a transient redraw) never trips it, but a 2- or 3-state churn cycle
	// (A,B,A,B,A,B) does.
	loopMinRepeats = 3
)

// looksLikeLoop reports whether an agent's recent pane history shows it churning
// the same few states — busy output that never progresses. This complements the
// quiet-stuck timer (classify's stuckAfter→idle), which fires only when the pane
// goes STALE; a looping agent's pane keeps changing, so the stuck timer never
// catches it. Detection: within a full window, some non-empty excerpt recurs at
// least loopMinRepeats times. Samples are distinct-from-adjacent by construction
// (the caller appends only on a real pane change), so a recurrence is a genuine
// cycle, not a frozen screen.
func looksLikeLoop(samples []string) bool {
	if len(samples) < loopWindow {
		return false
	}
	counts := make(map[string]int, len(samples))
	for _, s := range samples {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		counts[s]++
		if counts[s] >= loopMinRepeats {
			return true
		}
	}
	return false
}

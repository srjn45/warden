// Package handoff turns an agent's recorded transcript into a compact, structured
// handoff document warden can hand to a successor backend during a mid-session
// hot-swap. The context extractor (this file) distills warden's neutral
// []agentbackend.Turn history into a Handoff — the Goal that started the session, a
// Decisions Log of the choices made along the way, the set of Modified Files, and
// the Immediate Next Step — and the serializer (serialize.go) renders that into the
// `.warden/handoff-<session-id>.md` file the successor reads on startup.
//
// Extraction is deliberately PURE and deterministic: it takes only the turn slice
// and never touches the clock, filesystem, or network, so the same transcript
// always yields the same Handoff. The caller (lifecycle's hot-swap engine) stamps
// the wall-clock time and enriches the git-diff / system-context fields, which
// depend on live state the turns alone do not carry.
package handoff

import (
	"sort"
	"strings"

	"github.com/srjn45/warden/internal/agentbackend"
)

// Handoff is the structured, backend-neutral snapshot of an agent's session that a
// successor backend needs to continue the work. Extract populates the transcript-
// derived fields (Goal / Decisions / ModifiedFiles / NextStep); the caller fills the
// live-state fields (SessionID, Backend/Model, SuccessorBackend/SuccessorModel,
// GitDiff, GeneratedAt, Reason) it alone knows.
type Handoff struct {
	SessionID string // the retiring agent's id (names the handoff file)

	// Provenance: which backend/model produced the work, and which is taking over.
	Backend          string
	Model            string
	SuccessorBackend string
	SuccessorModel   string
	Reason           string // why the swap fired (context_fill, quota, manual, …)

	// Transcript-derived (populated by Extract).
	Goal          string   // the original task/goal (first user turn)
	Decisions     []string // key decisions/choices made during the session
	ModifiedFiles []string // files touched across the session (deduped, sorted)
	NextStep      string   // the immediate next step to continue with

	// Live-state (populated by the caller).
	GitDiff       string // `git diff --numstat` (or similar) snapshot of the worktree
	SystemContext string // freeform environment context (branch, worktree, tooling)
	GeneratedAt   string // RFC3339 timestamp; caller-stamped so Extract stays pure
}

// maxDecisions caps the Decisions Log so a long session does not produce an
// unbounded handoff. The most recent decisions are the most relevant to a
// successor, so extraction keeps the tail when it overflows.
const maxDecisions = 25

// maxGoalLen bounds the extracted Goal so a pasted-in wall of text (a whole spec
// dropped as the first message) does not swamp the handoff. The successor is
// pointed at the full transcript for detail; the Goal is the orienting summary.
const maxGoalLen = 2000

// decisionMarkers are lowercase substrings whose presence in an assistant line
// flags it as a decision/choice worth logging. They are matched case-insensitively
// against each line of every assistant turn.
var decisionMarkers = []string{
	"decid", "chose", "choosing", "opted", "opting", "i will ", "we will ",
	"going to ", "i'll ", "we'll ", "instead of", "rather than", "approach is",
	"approach:", "plan:", "the plan", "because ", "so that ", "let me ",
	"i'm going to", "next i", "first,", "settled on", "will use", "using the",
}

// nextStepMarkers flag a line in the final assistant turn as describing the
// immediate next step. Checked before falling back to the turn's last line.
var nextStepMarkers = []string{
	"next step", "next,", "next i", "todo", "to do", "remaining", "still need",
	"still to", "then i", "then we", "i will next", "after this", "left to do",
	"outstanding", "follow up", "follow-up", "not yet",
}

// Extract distills a neutral transcript into the transcript-derived fields of a
// Handoff: the Goal (the first user turn), the Decisions Log (assistant lines that
// read as choices), the set of Modified Files (every Turn.Files entry, deduped and
// sorted), and the Immediate Next Step (the successor's starting point, taken from
// the final assistant turn). It is pure — no clock, filesystem, or network — so a
// given transcript always yields the same result. An empty turn slice yields a
// zero-value Handoff (all fields empty), which the serializer renders as an explicit
// "unknown" rather than crashing the swap.
func Extract(turns []agentbackend.Turn) Handoff {
	return Handoff{
		Goal:          firstUserGoal(turns),
		Decisions:     extractDecisions(turns),
		ModifiedFiles: modifiedFiles(turns),
		NextStep:      immediateNextStep(turns),
	}
}

// firstUserGoal returns the text of the first user turn — the task that seeded the
// session — trimmed and length-capped. When no user turn carries text (a session
// resumed with no fresh prompt, a malformed transcript) it returns "".
func firstUserGoal(turns []agentbackend.Turn) string {
	for _, t := range turns {
		if t.Role != "user" {
			continue
		}
		text := strings.TrimSpace(t.Text)
		if text == "" {
			continue
		}
		return truncate(text, maxGoalLen)
	}
	return ""
}

// extractDecisions scans every assistant turn line-by-line for lines that read as a
// decision or choice (per decisionMarkers) and returns them in order, deduped. A
// session with more than maxDecisions logged keeps the most RECENT ones (the tail),
// which best orient a successor picking up the work. Lines are normalized to single-
// spaced, list-marker-stripped text so the log stays tidy.
func extractDecisions(turns []agentbackend.Turn) []string {
	var out []string
	seen := make(map[string]struct{})
	for _, t := range turns {
		if t.Role != "assistant" {
			continue
		}
		for _, raw := range strings.Split(t.Text, "\n") {
			line := normalizeLine(raw)
			if line == "" || !looksLikeDecision(line) {
				continue
			}
			key := strings.ToLower(line)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, line)
		}
	}
	if len(out) > maxDecisions {
		out = out[len(out)-maxDecisions:]
	}
	return out
}

// looksLikeDecision reports whether a normalized line contains any decision marker.
func looksLikeDecision(line string) bool {
	l := strings.ToLower(line)
	for _, m := range decisionMarkers {
		if strings.Contains(l, m) {
			return true
		}
	}
	return false
}

// modifiedFiles returns the sorted, deduplicated union of every Turn.Files entry —
// the files the agent touched across the whole session. Empty entries are dropped.
func modifiedFiles(turns []agentbackend.Turn) []string {
	seen := make(map[string]struct{})
	for _, t := range turns {
		for _, f := range t.Files {
			f = strings.TrimSpace(f)
			if f == "" {
				continue
			}
			seen[f] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for f := range seen {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// immediateNextStep derives the successor's starting point from the LAST assistant
// turn: it prefers a line that explicitly signals a next step (per nextStepMarkers)
// and otherwise falls back to the turn's final non-empty line (the most recent thing
// the agent said, which is usually where it left off). Returns "" when there is no
// assistant turn with text.
func immediateNextStep(turns []agentbackend.Turn) string {
	last := lastAssistantText(turns)
	if last == "" {
		return ""
	}
	lines := splitNonEmptyLines(last)
	if len(lines) == 0 {
		return ""
	}
	// Prefer an explicit next-step line, scanning from the end so the most recent
	// forward-looking statement wins.
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.ToLower(lines[i])
		for _, m := range nextStepMarkers {
			if strings.Contains(l, m) {
				return normalizeLine(lines[i])
			}
		}
	}
	// Fallback: the final line the agent produced.
	return normalizeLine(lines[len(lines)-1])
}

// lastAssistantText returns the text of the last assistant turn that carries any,
// or "" when none does.
func lastAssistantText(turns []agentbackend.Turn) string {
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Role != "assistant" {
			continue
		}
		if text := strings.TrimSpace(turns[i].Text); text != "" {
			return text
		}
	}
	return ""
}

// splitNonEmptyLines splits s on newlines and returns the trimmed, non-empty lines.
func splitNonEmptyLines(s string) []string {
	var out []string
	for _, raw := range strings.Split(s, "\n") {
		if line := strings.TrimSpace(raw); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// normalizeLine trims a line, strips a leading markdown list/heading marker, and
// collapses internal whitespace so logged decisions/steps read cleanly.
func normalizeLine(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimLeft(s, "#-*> \t")
	// Strip a leading ordered-list marker like "1." / "2)".
	s = strings.TrimSpace(s)
	return strings.Join(strings.Fields(s), " ")
}

// truncate caps s at n runes, appending an ellipsis when it had to cut. It counts
// runes (not bytes) so a multibyte transcript is never sliced mid-character.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + " …"
}

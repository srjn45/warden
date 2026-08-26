package handoff

import (
	"strings"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
)

func turn(role, text string, files ...string) agentbackend.Turn {
	return agentbackend.Turn{Role: role, Text: text, Files: files, Timestamp: time.Time{}}
}

// TestExtractGoalIsFirstUserTurn: the goal is the first user turn's text, trimmed,
// and a leading system/assistant turn does not become the goal.
func TestExtractGoalIsFirstUserTurn(t *testing.T) {
	turns := []agentbackend.Turn{
		turn("assistant", "I am ready."),
		turn("user", "  Implement the CSV export feature.  "),
		turn("user", "and also add tests"),
	}
	h := Extract(turns)
	if h.Goal != "Implement the CSV export feature." {
		t.Fatalf("Goal = %q, want the first user turn trimmed", h.Goal)
	}
}

func TestExtractGoalEmptyWhenNoUserTurn(t *testing.T) {
	h := Extract([]agentbackend.Turn{turn("assistant", "hello")})
	if h.Goal != "" {
		t.Fatalf("Goal = %q, want empty when no user turn", h.Goal)
	}
}

func TestExtractGoalTruncatesLongText(t *testing.T) {
	long := strings.Repeat("x", maxGoalLen+500)
	h := Extract([]agentbackend.Turn{turn("user", long)})
	if !strings.HasSuffix(h.Goal, "…") {
		t.Fatalf("expected truncated goal to end with ellipsis, got suffix %q", tail(h.Goal, 5))
	}
	if len([]rune(h.Goal)) > maxGoalLen+3 {
		t.Fatalf("truncated goal too long: %d runes", len([]rune(h.Goal)))
	}
}

// TestExtractDecisions: assistant lines that read as choices are logged, in order,
// deduped; non-decision prose and user turns are ignored.
func TestExtractDecisions(t *testing.T) {
	turns := []agentbackend.Turn{
		turn("user", "I decided to use SQLite"), // user turn: ignored even if it matches
		turn("assistant", "Let me look at the code.\nI will use a streaming parser for memory.\nHere is some neutral prose."),
		turn("assistant", "I chose the buffered approach instead of mmap.\nI will use a streaming parser for memory."), // dup line dropped
	}
	h := Extract(turns)
	want := []string{
		"Let me look at the code.",
		"I will use a streaming parser for memory.",
		"I chose the buffered approach instead of mmap.",
	}
	if len(h.Decisions) != len(want) {
		t.Fatalf("Decisions = %#v, want %#v", h.Decisions, want)
	}
	for i := range want {
		if h.Decisions[i] != want[i] {
			t.Fatalf("Decisions[%d] = %q, want %q", i, h.Decisions[i], want[i])
		}
	}
}

func TestExtractDecisionsCapKeepsTail(t *testing.T) {
	var turns []agentbackend.Turn
	for i := 0; i < maxDecisions+10; i++ {
		turns = append(turns, turn("assistant", "I will do step "+itoa(i)))
	}
	h := Extract(turns)
	if len(h.Decisions) != maxDecisions {
		t.Fatalf("len(Decisions) = %d, want cap %d", len(h.Decisions), maxDecisions)
	}
	// The tail (most recent) is kept: last decision is the final step.
	last := "I will do step " + itoa(maxDecisions+10-1)
	if h.Decisions[len(h.Decisions)-1] != last {
		t.Fatalf("last decision = %q, want %q (tail kept)", h.Decisions[len(h.Decisions)-1], last)
	}
}

// TestExtractModifiedFiles: the union of every Turn.Files, deduped and sorted;
// empties dropped.
func TestExtractModifiedFiles(t *testing.T) {
	turns := []agentbackend.Turn{
		turn("assistant", "edit", "b.go", "a.go"),
		turn("assistant", "edit", "a.go", "", "  "),
		turn("assistant", "edit", "c.go"),
	}
	h := Extract(turns)
	want := []string{"a.go", "b.go", "c.go"}
	if strings.Join(h.ModifiedFiles, ",") != strings.Join(want, ",") {
		t.Fatalf("ModifiedFiles = %#v, want %#v (sorted, deduped)", h.ModifiedFiles, want)
	}
}

func TestExtractModifiedFilesNilWhenNone(t *testing.T) {
	h := Extract([]agentbackend.Turn{turn("assistant", "no files")})
	if h.ModifiedFiles != nil {
		t.Fatalf("ModifiedFiles = %#v, want nil", h.ModifiedFiles)
	}
}

// TestExtractNextStepPrefersMarkerLine: the immediate next step is taken from a
// next-step-marked line in the LAST assistant turn.
func TestExtractNextStepPrefersMarkerLine(t *testing.T) {
	turns := []agentbackend.Turn{
		turn("assistant", "Done with parsing.\nNext step: wire up the CLI flag.\nThat is all for now."),
	}
	h := Extract(turns)
	if h.NextStep != "Next step: wire up the CLI flag." {
		t.Fatalf("NextStep = %q, want the marked next-step line", h.NextStep)
	}
}

// TestExtractNextStepFallsBackToLastLine: with no marker, the final line of the last
// assistant turn is used.
func TestExtractNextStepFallsBackToLastLine(t *testing.T) {
	turns := []agentbackend.Turn{
		turn("assistant", "an earlier assistant turn"),
		turn("assistant", "First I read the file.\nThen I edited main.go."),
	}
	h := Extract(turns)
	if h.NextStep != "Then I edited main.go." {
		t.Fatalf("NextStep = %q, want the last line of the last assistant turn", h.NextStep)
	}
}

func TestExtractNextStepEmptyWhenNoAssistant(t *testing.T) {
	h := Extract([]agentbackend.Turn{turn("user", "do a thing")})
	if h.NextStep != "" {
		t.Fatalf("NextStep = %q, want empty", h.NextStep)
	}
}

// TestExtractIsDeterministic: the same transcript yields byte-identical Handoffs
// (no clock/map-order nondeterminism leaks into the transcript-derived fields).
func TestExtractIsDeterministic(t *testing.T) {
	turns := []agentbackend.Turn{
		turn("user", "build it"),
		turn("assistant", "I will use approach A.", "z.go", "a.go", "m.go"),
		turn("assistant", "Next, run the tests."),
	}
	a := Extract(turns)
	b := Extract(turns)
	if a.Markdown() != b.Markdown() {
		t.Fatalf("Extract not deterministic:\nA=%q\nB=%q", a.Markdown(), b.Markdown())
	}
}

func TestExtractEmptyTranscript(t *testing.T) {
	h := Extract(nil)
	if h.Goal != "" || h.NextStep != "" || len(h.Decisions) != 0 || len(h.ModifiedFiles) != 0 {
		t.Fatalf("Extract(nil) = %#v, want a zero-value handoff", h)
	}
}

// --- tiny local helpers (avoid strconv churn in the test) ---

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	s := string(buf[i:])
	if neg {
		return "-" + s
	}
	return s
}

func tail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

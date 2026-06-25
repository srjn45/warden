package insights

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeCompleter is an llm.Completer stand-in: it returns canned text/err.
type fakeCompleter struct {
	reply     string
	err       error
	gotPrompt string
}

func (f *fakeCompleter) Complete(_ context.Context, prompt string) (string, error) {
	f.gotPrompt = prompt
	return f.reply, f.err
}

func sampleReport() Report {
	return Report{
		Sessions:       4,
		ActiveSessions: 1,
		Durations:      []TypeDuration{{Type: "development", Count: 3, MedianSec: 600, P90Sec: 1200, MaxSec: 3600, Outliers: []string{"d4"}}},
		ErrorRates:     []TypeErrorRate{{Type: "tests", Total: 3, Errored: 1, Rate: 1.0 / 3.0}},
		BusiestPeriods: []HourBucket{{Hour: 14, Count: 3}},
		Parallelizable: []ParallelSuggestion{{A: "s1", B: "s2", Repo: "/r", SavedSec: 600, Reason: "s1 and s2 ran sequentially"}},
		CoEdits:        []CoEditPair{{A: "a.go", B: "b.go", Count: 2}},
		Anomalies:      []AgentAnomaly{{Agent: "z", Notes: []string{"CPU pinned"}}},
	}
}

func TestDeterministicSummaryEmpty(t *testing.T) {
	if got := DeterministicSummary(Report{}); got != "No agent session history to analyze yet." {
		t.Fatalf("empty report summary = %q", got)
	}
}

func TestDeterministicSummaryContent(t *testing.T) {
	got := DeterministicSummary(sampleReport())
	for _, want := range []string{
		"Analyzed 4 sessions (1 active)",
		"Slowest type: development (median 10m), 1 outlier",
		"Highest error rate: tests 33%",
		"Busiest hour: 14:00 UTC",
		"1 parallelization opportunity (≈10m wall-clock saveable)",
		"Most co-edited: a.go + b.go (2 sessions)",
		"1 agent anomaly flagged",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q in:\n%s", want, got)
		}
	}
}

func TestNarratePrefersModel(t *testing.T) {
	c := &fakeCompleter{reply: "  Run s1 and s2 in parallel to save ten minutes.\n"}
	got := Narrate(context.Background(), c, sampleReport())
	if got != "Run s1 and s2 in parallel to save ten minutes." {
		t.Fatalf("want cleaned model text, got %q", got)
	}
	if !strings.Contains(c.gotPrompt, "Parallelize:") {
		t.Errorf("prompt should carry structured facts, got:\n%s", c.gotPrompt)
	}
}

func TestNarrateNilCompleterIsDeterministic(t *testing.T) {
	got := Narrate(context.Background(), nil, sampleReport())
	if got != DeterministicSummary(sampleReport()) {
		t.Fatalf("nil completer must yield the deterministic floor, got %q", got)
	}
}

func TestNarrateErrorFallsBack(t *testing.T) {
	c := &fakeCompleter{reply: "ignored", err: errors.New("boom")}
	got := Narrate(context.Background(), c, sampleReport())
	if got != DeterministicSummary(sampleReport()) {
		t.Fatalf("erroring completer must fall back, got %q", got)
	}
}

func TestNarrateEmptyReplyNotTrusted(t *testing.T) {
	c := &fakeCompleter{reply: "   \n  "}
	got := Narrate(context.Background(), c, sampleReport())
	if got != DeterministicSummary(sampleReport()) {
		t.Fatalf("empty model reply must fall back, got %q", got)
	}
}

func TestNarratorPromptNoPreambleInstruction(t *testing.T) {
	p := NarratorPrompt(sampleReport())
	if !strings.Contains(p, "Output ONLY the summary") {
		t.Errorf("prompt missing the no-preamble instruction:\n%s", p)
	}
}

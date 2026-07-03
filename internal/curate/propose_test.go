package curate

import (
	"context"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/memory"
)

// stubLLM is a canned local model for the offload-preference test.
type stubLLM struct {
	out    string
	called bool
}

func (s *stubLLM) Complete(_ context.Context, _ string) (string, error) {
	s.called = true
	return s.out, nil
}

// TestProposePrefersLocalLLM: with a local model configured, the pass runs at $0 —
// the local LLM is used and headless claude -p (Run) is NEVER called. Candidates come
// back as unverified with batch provenance.
func TestProposePrefersLocalLLM(t *testing.T) {
	llm := &stubLLM{out: "- the daemon API is spec-first\n- tests run behind `wd check`\n"}
	ranCloud := false
	p := LLMProposer{
		LLM: llm,
		Run: func(_ context.Context, _ string) (string, error) { ranCloud = true; return "", nil },
	}
	got, err := p.Propose(context.Background(), ProposeInput{Signals: []Signal{{Agent: "a1", Commit: "deadbeefcafe"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !llm.called {
		t.Error("local LLM not used")
	}
	if ranCloud {
		t.Error("cloud claude -p called despite local LLM ($0 path violated)")
	}
	if len(got) != 2 {
		t.Fatalf("got %d candidates, want 2", len(got))
	}
	for _, e := range got {
		if e.Trust != memory.TrustUnverified {
			t.Errorf("candidate %q not unverified", e.Text)
		}
		if !strings.Contains(e.Provenance, "agent a1") || !strings.Contains(e.Provenance, "sha deadbee") {
			t.Errorf("candidate provenance = %q", e.Provenance)
		}
	}
}

// TestProposeNoModelSkips: with no local model AND no configured cloud fallback (Run
// nil), the pass proposes nothing rather than spending — curation stays $0/off the
// paid path by default.
func TestProposeNoModelSkips(t *testing.T) {
	got, err := (LLMProposer{}).Propose(context.Background(), ProposeInput{})
	if err != nil || got != nil {
		t.Fatalf("expected nil/nil skip, got %v / %v", got, err)
	}
}

// TestProposeCloudFallback: with no local model but a configured Run, the pass
// degrades to the headless fallback.
func TestProposeCloudFallback(t *testing.T) {
	p := LLMProposer{Run: func(_ context.Context, _ string) (string, error) { return "- a durable fact\n", nil }}
	got, err := p.Propose(context.Background(), ProposeInput{Signals: []Signal{{Agent: "a1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Text != "a durable fact" {
		t.Fatalf("cloud fallback candidates = %+v", got)
	}
}

// TestExtractionPromptIsExtractionNotDump: the prompt instructs the model to keep
// durable facts and DISCARD per-task noise + already-known facts (§3.2).
func TestExtractionPromptIsExtractionNotDump(t *testing.T) {
	in := ProposeInput{
		Current: &memory.Memory{Entries: []memory.Entry{{Text: "already known fact"}}},
		Signals: []Signal{{Task: "refactor", Summary: "did stuff", Files: []string{"a.go"}}},
	}
	pr := ExtractionPrompt(in)
	if !strings.Contains(pr, "DISCARD per-task noise") {
		t.Error("prompt missing extraction-not-dump instruction")
	}
	if !strings.Contains(pr, "already known fact") {
		t.Error("prompt does not feed current memory for de-dup")
	}
}

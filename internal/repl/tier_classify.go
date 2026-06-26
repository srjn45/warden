package repl

import (
	"context"
	"strings"

	"github.com/srjn45/warden/internal/llm"
)

// modelClassifier buckets a request's needed planning tier with one cheap
// local-model completion ("reply with only T0, T1, or T2"). Any model can do
// this reliably even when it can't plan the request itself, so it catches the
// cases the surface heuristic misses in both directions: a hard single-sentence
// ask the heuristic reads as trivial (no conjunctions), and a simple ask whose
// quoted "and"s the heuristic over-counts.
//
// It is best-effort and never blocks: an empty, unparseable, or failed reply
// falls through to a deterministic fallback Classifier (the heuristic), so
// classification still works with the model down and is bounded by the same
// per-call timeout as every other local-model call.
type modelClassifier struct {
	comp     llm.Completer
	fallback Classifier
}

func (m modelClassifier) NeededTier(ctx context.Context, line string) (Tier, error) {
	if m.comp == nil {
		return m.fallback.NeededTier(ctx, line)
	}
	raw, err := m.comp.Complete(ctx, classifyPrompt(line))
	if err != nil {
		return m.fallback.NeededTier(ctx, line)
	}
	if t, ok := parseTierWord(raw); ok {
		return t, nil
	}
	return m.fallback.NeededTier(ctx, line)
}

// classifyPrompt asks for a single-token tier verdict. It defines the three
// buckets in warden's terms so a small instruct model has a concrete rubric,
// and ends with "Answer:" so a well-behaved model emits just the token.
func classifyPrompt(line string) string {
	return "You triage requests to warden, an orchestrator of Claude coding agents. " +
		"Judge how hard it is to PLAN the right tool calls and reply with ONLY one token — T0, T1, or T2.\n" +
		"T0 = one trivial action (spawn a single agent, list agents, check a status).\n" +
		"T1 = a routine multi-step request (a couple of related actions in sequence).\n" +
		"T2 = a hard, ambiguous, or multi-agent plan (a pipeline, several coordinated agents, vague scope).\n\n" +
		"Request: " + line + "\nAnswer:"
}

// parseTierWord pulls a tier out of the model's reply. It takes the LAST of the
// three markers to appear, so a chatty model that echoes the rubric (which lists
// all three) before giving its verdict is read by its final answer, not the
// echoed list. No marker ⇒ (T0, false) so the caller uses its fallback.
func parseTierWord(raw string) (Tier, bool) {
	s := strings.ToLower(raw)
	best, bestIdx := T0, -1
	for _, c := range []struct {
		tok string
		t   Tier
	}{{"t0", T0}, {"t1", T1}, {"t2", T2}} {
		if i := strings.LastIndex(s, c.tok); i > bestIdx {
			best, bestIdx = c.t, i
		}
	}
	if bestIdx == -1 {
		return T0, false
	}
	return best, true
}

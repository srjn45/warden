package orchestrator

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/llm"
	"github.com/stretchr/testify/require"
)

// This is the NL-path eval harness: a labelled golden corpus plus a scorer that
// grades any Classifier against it. It lets a change to the heuristic, the
// classification prompt, the tier thresholds, or the model be measured instead
// of guessed at. It runs offline against the deterministic heuristic by default;
// set WARDEN_EVAL_OLLAMA=<model> to also score a live local model (see
// TestEval_LiveModelClassifier).
//
// The corpus is labelled by human judgement of how hard each request is to PLAN,
// NOT by what the heuristic happens to score — so the heuristic deliberately
// misses some, and the scorecard reports its true accuracy. That honesty is the
// point: it quantifies the gap the model classifier (local_llm_classifier: model)
// is there to close.

// classifyCase is one labelled request in the golden corpus.
type classifyCase struct {
	line string
	want Tier
}

// goldenClassify is the labelled corpus. Spread across the three tiers, with a
// few genuinely hard single-sentence asks (no conjunctions) that the surface
// heuristic cannot see — those are the motivating misses.
var goldenClassify = []classifyCase{
	{"list agents", T0},
	{"what's running?", T0},
	{"spawn an agent to fix the typo in README", T0},
	{"show me agent a3's output", T0},
	{"terminate a3", T0},
	{"spawn a dev agent to refactor the auth package", T0},
	{"fix the bug", T0},
	{"rename the variable foo to bar in config.go", T0},
	{"spawn an agent to review the PR and another to run the tests", T1},
	{"review the auth module and fix any bugs you find", T1},
	{"set up a pipeline that builds, tests, and deploys", T2},
	{"spawn one, and then a pipeline, and review and merge", T2},
	{"coordinate three agents to split the refactor across modules and merge their work", T2},
	{"untangle the auth race across all three worktrees", T2},
	{"spawn agents for each open issue and have them collaborate", T2},
	{"stand up a build/test/deploy pipeline and watch it to completion", T2},
}

// classifyScore is the outcome of grading a Classifier against a corpus.
type classifyScore struct {
	total, correct int
	misses         []classifyMiss
}

type classifyMiss struct {
	line      string
	want, got Tier
}

func (s classifyScore) accuracy() float64 {
	if s.total == 0 {
		return 0
	}
	return float64(s.correct) / float64(s.total)
}

// scorecard renders the misses as a readable block for a test log.
func (s classifyScore) scorecard() string {
	var b strings.Builder
	fmt.Fprintf(&b, "accuracy %d/%d = %.0f%%", s.correct, s.total, 100*s.accuracy())
	for _, m := range s.misses {
		fmt.Fprintf(&b, "\n  want %s got %s  %q", tierName(m.want), tierName(m.got), m.line)
	}
	return b.String()
}

// scoreClassifier grades a Classifier against the corpus. A classifier error
// counts as a miss (it couldn't tell), so the score reflects real usefulness.
func scoreClassifier(ctx context.Context, c Classifier, cases []classifyCase) classifyScore {
	s := classifyScore{total: len(cases)}
	for _, tc := range cases {
		got, err := c.NeededTier(ctx, tc.line)
		if err == nil && got == tc.want {
			s.correct++
			continue
		}
		s.misses = append(s.misses, classifyMiss{line: tc.line, want: tc.want, got: got})
	}
	return s
}

// TestEval_HeuristicClassifierBaseline records the deterministic heuristic's
// accuracy on the corpus and guards it from regressing. The floor is below the
// current score so a small honest dip doesn't break the build, but a real
// regression (someone breaking the scoring) trips it. The scorecard is logged so
// the misses are always visible.
func TestEval_HeuristicClassifierBaseline(t *testing.T) {
	score := scoreClassifier(context.Background(), heuristicClassifier{}, goldenClassify)
	t.Log("heuristic classifier — " + score.scorecard())
	require.GreaterOrEqual(t, score.accuracy(), 0.70,
		"the heuristic classifier regressed below its known baseline:\n%s", score.scorecard())
}

// TestEval_LiveModelClassifier scores a real local model against the same corpus
// and asserts it does at least as well as the heuristic — the justification for
// local_llm_classifier: model. Opt-in: set WARDEN_EVAL_OLLAMA=<model tag> (and,
// optionally, WARDEN_EVAL_OLLAMA_URL) to run it; skipped otherwise so CI stays
// model-free and deterministic.
func TestEval_LiveModelClassifier(t *testing.T) {
	model := os.Getenv("WARDEN_EVAL_OLLAMA")
	if model == "" {
		t.Skip("set WARDEN_EVAL_OLLAMA=<model tag> to score a live local model")
	}
	url := os.Getenv("WARDEN_EVAL_OLLAMA_URL")
	if url == "" {
		url = "http://localhost:11434"
	}
	comp := llm.NewOllama(url, model, 30*time.Second)
	cls := modelClassifier{comp: comp, fallback: heuristicClassifier{}}

	base := scoreClassifier(context.Background(), heuristicClassifier{}, goldenClassify)
	got := scoreClassifier(context.Background(), cls, goldenClassify)
	t.Logf("model %q — %s", model, got.scorecard())
	require.GreaterOrEqual(t, got.accuracy(), base.accuracy(),
		"the model classifier should match or beat the heuristic baseline (%.0f%%) to earn its round-trip", 100*base.accuracy())
}

// --- arg-shaping golden cases: lock in the sanitizer's behaviour ---

type shapeCase struct {
	name string
	in   ToolCall
	want map[string]any
}

var goldenShape = []shapeCase{
	{"drops fabricated repo/model/type", ToolCall{Name: "spawn_agent", Args: map[string]any{
		"prompt": "review auth", "repo": "/path/to/repo", "model": "gpt-4", "type": "frobnicate"}},
		map[string]any{"prompt": "review auth"}},
	{"canonicalises valid enums", ToolCall{Name: "spawn_agent", Args: map[string]any{
		"prompt": "x", "model": "Opus", "type": "PR-Review"}},
		map[string]any{"prompt": "x", "model": "opus", "type": "pr-review"}},
	{"keeps a real repo path", ToolCall{Name: "spawn_agent", Args: map[string]any{
		"prompt": "x", "repo": "/home/me/dev/warden"}},
		map[string]any{"prompt": "x", "repo": "/home/me/dev/warden"}},
	{"drops empty strings, keeps non-strings", ToolCall{Name: "spawn_agent", Args: map[string]any{
		"prompt": "x", "name": "   ", "worktree": true}},
		map[string]any{"prompt": "x", "worktree": true}},
}

// TestEval_SanitizerGolden grades sanitizeCall against the shaping corpus so the
// hallucination-scrubbing behaviour stays pinned as the code evolves.
func TestEval_SanitizerGolden(t *testing.T) {
	for _, tc := range goldenShape {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, sanitizeCall(tc.in).Args)
		})
	}
}

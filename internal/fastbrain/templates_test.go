package fastbrain

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func decide(t *testing.T, kind DecisionKind, out string) Response {
	t.Helper()
	r, err := NewEngine(fixed(out), nil).Decide(context.Background(), Request{Kind: kind, Tier: TierFast, Prompt: "p"})
	require.NoError(t, err)
	return r
}

func TestParseClassifyTask(t *testing.T) {
	ty, c := ParseClassifyTask(decide(t, KindClassifyTask, `{"type":"Test","confidence":0.8}`))
	require.Equal(t, "test", ty)
	require.InDelta(t, 0.8, c, 1e-9)
	ty, _ = ParseClassifyTask(decide(t, KindClassifyTask, `{"type":"bogus"}`))
	require.Equal(t, "other", ty)
	ty, _ = ParseClassifyTask(decide(t, KindClassifyTask, `garbage`))
	require.Equal(t, "other", ty)
}

func TestParseSummaryAndCommit(t *testing.T) {
	require.Equal(t, "a b", ParseSummary(decide(t, KindSummarizeCheck, `{"summary":" a b "}`)))
	require.Equal(t, "", ParseSummary(decide(t, KindSummarizeActivity, `nope`)))
	require.Equal(t, "", ParseSummary(decide(t, KindSummarizeActivity, `{"summary":5}`)))
	require.Equal(t, "feat: x", ParseCommitMessage(decide(t, KindCommitMessage, `{"message":"feat: x\n\nbody"}`)))
	require.Equal(t, "", ParseCommitMessage(decide(t, KindCommitMessage, `{}`)))
}

func TestParseCurateEntries(t *testing.T) {
	require.Equal(t, []string{"a", "b"}, ParseCurateEntries(decide(t, KindCurateExtract, `{"entries":["a"," ","b"]}`)))
	require.Empty(t, ParseCurateEntries(decide(t, KindCurateExtract, `x`)))
	require.Empty(t, ParseCurateEntries(decide(t, KindCurateExtract, `{"entries":"no"}`)))
}

func TestParseReplTurn(t *testing.T) {
	require.Equal(t, ReplTurn{}, ParseReplTurn(decide(t, KindReplTurn, `x`)))
	tn := ParseReplTurn(decide(t, KindReplTurn, `{"text":"hi","tool_calls":[{"name":"a"}]}`))
	require.Equal(t, "hi", tn.Text)
	require.Equal(t, map[string]any{}, tn.ToolCalls[0].Args)
}

func TestNewKindsRoutingAndSanitize(t *testing.T) {
	for _, k := range []DecisionKind{KindClassifyTask, KindSummarizeActivity, KindSummarizeCheck, KindCommitMessage, KindCurateExtract, KindReplTurn} {
		r := decide(t, k, "Sure!\n```json\n{\"a\":1}\n```\nbye")
		require.True(t, r.OK(), k)
		require.Equal(t, k, r.Kind)
		r, err := NewEngine(nil, nil).Decide(context.Background(), Request{Kind: k, Tier: TierFast, Prompt: "p"})
		require.NoError(t, err)
		require.Equal(t, StatusNoRunner, r.Status)
	}
	for _, p := range []string{ClassifyTaskPrompt("x"), SummarizeActivityPrompt("x"), SummarizeCheckPrompt("x"), CommitMessagePrompt("x"), CurateExtractPrompt("x")} {
		require.Contains(t, p, "JSON")
	}
}

func TestActivityBadgePromptIsDistinctFromSentencePrompt(t *testing.T) {
	b, s := ActivityBadgePrompt("editing"), SummarizeActivityPrompt("editing")
	require.NotEqual(t, b, s)
	require.Contains(t, b, "3 to 5 words")
	require.Contains(t, s, "max 12 words", "shared narrator prompt must stay a sentence")
	require.Contains(t, b, "editing")
}

func TestParseActivityBadge(t *testing.T) {
	p := func(out string) string { return ParseActivityBadge(decide(t, KindSummarizeActivity, out)) }
	require.Equal(t, "Fixing failing auth tests", p(`{"summary":" Fixing failing auth tests. "}`))
	require.Equal(t, "one two three four five", p(`{"summary":"one two three four five six seven"}`))
	require.Equal(t, "first line", p(`{"summary":"first line\nsecond"}`))
	require.Equal(t, "Running go test", p(`{"summary":"\"Running go test\""}`))
	// character cap never leaves a half word
	long := p(`{"summary":"Refactoring supercalifragilistic internationalization configuration modules"}`)
	require.LessOrEqual(t, len([]rune(long)), 40)
	require.Equal(t, "", p(`nope`))
	require.Equal(t, "", p(`{"summary":""}`))
}

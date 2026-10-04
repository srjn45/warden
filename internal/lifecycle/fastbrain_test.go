package lifecycle

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/fastbrain"
	"github.com/srjn45/warden/internal/store"
)

// fakeFB is a scripted fastbrain.Engine; only Decide is used by lifecycle.
type fakeFB struct {
	fastbrain.Engine
	json   string
	status fastbrain.Status
	calls  []fastbrain.Request
}

func (f *fakeFB) Decide(_ context.Context, req fastbrain.Request) (fastbrain.Response, error) {
	f.calls = append(f.calls, req)
	st := f.status
	if st == "" {
		st = fastbrain.StatusOK
	}
	r := fastbrain.Response{Kind: req.Kind, Tier: req.Tier, Status: st}
	if st == fastbrain.StatusOK {
		r.Output = fastbrain.Output{Raw: f.json, Parsed: []byte(f.json)}
	}
	return r, nil
}

func fbLifecycle(fb *fakeFB) (*Lifecycle, *FakeRunner) {
	fr := &FakeRunner{} // any claude -p call would be an unmatched command
	lc := New(fr, &FakeConfig{})
	lc.FastBrain = fb
	return lc, fr
}

func TestFastBrainClassifyMapping(t *testing.T) {
	cases := map[string]store.Type{
		"test": store.TypeTests, "implementation": store.TypeDevelopment,
		"review": store.TypePRReview, "refactor": store.TypeCode,
		"docs": store.TypeDocs, "other": store.TypeOther,
	}
	for in, want := range cases {
		fb := &fakeFB{json: `{"type":"` + in + `","confidence":0.9}`}
		lc, _ := fbLifecycle(fb)
		got, err := lc.Classify(context.Background(), "x")
		require.NoError(t, err)
		require.Equal(t, want, got, in)
		require.Equal(t, fastbrain.KindClassifyTask, fb.calls[0].Kind)
		require.Equal(t, fastbrain.TierFast, fb.calls[0].Tier)
	}
}

func TestFastBrainPreferredOverInternalAndLLM(t *testing.T) {
	fb := &fakeFB{json: `{"type":"docs"}`}
	lc, fr := fbLifecycle(fb)
	fc := &fakeCompleter{out: "tests"}
	lc.LLM = fc
	got, err := lc.Classify(context.Background(), "x")
	require.NoError(t, err)
	require.Equal(t, store.TypeDocs, got)
	require.Zero(t, fc.calls)
	require.Empty(t, fr.Calls)
}

func TestFastBrainFailOpen(t *testing.T) {
	for _, st := range []fastbrain.Status{fastbrain.StatusTimeout, fastbrain.StatusNoRunner} {
		lc, fr := fbLifecycle(&fakeFB{status: st})
		fc := &fakeCompleter{out: "tests"}
		lc.LLM = fc

		typ, err := lc.Classify(context.Background(), "x")
		require.NoError(t, err)
		require.Equal(t, store.TypeOther, typ)

		sum, err := lc.Summarize(context.Background(), &agentstore.Agent{ID: "a", Prompt: "do things"})
		require.NoError(t, err)
		require.Empty(t, sum)

		require.Equal(t, "fix-the-flaky-test", lc.GenerateName(context.Background(), "fix the flaky test please"))

		big := strings.Repeat("fail line\n", maxCheckOutputLines+20)
		require.Equal(t, truncateTail(big, maxCheckOutputLines), lc.summarizeCheckOutput(context.Background(), "go", big))

		require.Zero(t, fc.calls)
		for _, c := range fr.Calls {
			require.NotEqual(t, "claude", c.Argv[0], "fail-open must not fall through to claude -p")
		}
	}
}

func TestFastBrainSummarize(t *testing.T) {
	fb := &fakeFB{json: `{"summary":"Fixing parser bug"}`}
	lc, _ := fbLifecycle(fb)
	got, err := lc.Summarize(context.Background(), &agentstore.Agent{ID: "a", Prompt: "fix parser"})
	require.NoError(t, err)
	require.Equal(t, "Fixing parser bug", got)
	require.Equal(t, fastbrain.KindSummarizeActivity, fb.calls[0].Kind)
}

func TestFastBrainGenerateName(t *testing.T) {
	fb := &fakeFB{json: `{"name":"Parser Bug Fix"}`}
	lc, _ := fbLifecycle(fb)
	require.Equal(t, "parser-bug-fix", lc.GenerateName(context.Background(), "fix parser"))
	require.Equal(t, fastbrain.KindResolveAgentName, fb.calls[0].Kind)
}

func TestFastBrainSummarizeCheckOutput(t *testing.T) {
	fb := &fakeFB{json: `{"summary":"TestX: boom"}`}
	lc, _ := fbLifecycle(fb)
	big := strings.Repeat("fail line\n", maxCheckOutputLines+20)
	require.Equal(t, checkSummaryMarker+"TestX: boom", lc.summarizeCheckOutput(context.Background(), "go", big))
	require.Equal(t, fastbrain.KindSummarizeCheck, fb.calls[0].Kind)

	// Small output never reaches the model.
	fb.calls = nil
	require.Equal(t, "ok", lc.summarizeCheckOutput(context.Background(), "go", "ok"))
	require.Empty(t, fb.calls)
}

func TestFastBrainCommitMessage(t *testing.T) {
	files := []string{"internal/foo.go"}
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git diff --cached": {Out: "diff --git a/internal/foo.go b/internal/foo.go\n+retry"},
	}}
	fb := &fakeFB{json: `{"message":"feat(foo): add retry\nbody"}`}
	lc := New(fr, &FakeConfig{})
	lc.FastBrain = fb
	require.Equal(t, "feat(foo): add retry", lc.commitMessage(context.Background(), "/d", files))
	require.Equal(t, fastbrain.KindCommitMessage, fb.calls[0].Kind)

	fb.status = fastbrain.StatusTimeout
	require.Equal(t, deterministicCommitMessage(files), lc.commitMessage(context.Background(), "/d", files))
}

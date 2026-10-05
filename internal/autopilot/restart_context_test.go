package autopilot

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/lifecycle"
)

type fakeProber map[string]BranchFacts

func (f fakeProber) BranchFacts(_ context.Context, branch, _ string) (BranchFacts, error) {
	return f[branch], nil
}

func seedRestartLedger(t *testing.T, st *fakeStore) *Ledger {
	t.Helper()
	l := NewLedger(st, "run1")
	require.NoError(t, l.WriteTasks([]LedgerTask{
		{ID: "t1", State: LedgerLanded, Branch: "w/t1", PR: 11},
		{ID: "t2", State: LedgerPROpen, Branch: "w/t2", PR: 12},
		{ID: "t3", State: LedgerInProgress, Branch: "w/t3"},
		{ID: "t4", State: LedgerPending},
	}, "test"))
	require.NoError(t, l.AppendLanding(Landing{Branch: "w/t1", SHA: "abcdef0123456789abcdef", PR: 11, LandedAt: "2026-10-05T01:00:00Z"}))
	return l
}

func TestAssembleRestartContextMix(t *testing.T) {
	st := newFakeStore()
	l := seedRestartLedger(t, st)
	rc, err := AssembleRestartContext(context.Background(), RestartAssembleInput{
		Ledger:            l,
		IntegrationBranch: "autopilot/x",
		Prober: fakeProber{
			"w/t2": {HasUnmergedCommits: true, OpenPR: 12, PRBase: "autopilot/x"},
			"w/t3": {HasUnmergedCommits: false},
		},
		Reason: "stuck", ReasonKind: RestartReasonOperatorStop,
		CountRestart: true, PreviousCount: 2,
		Now: time.Date(2026, 10, 5, 2, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	require.Equal(t, 3, rc.RestartCount)
	require.Equal(t, []RestartFinishedTask{{ID: "t1", PR: 11, MergeSHA: "abcdef0123456789abcdef", Branch: "w/t1"}}, rc.FinishedTasks)
	require.Len(t, rc.Unfinished, 3)
	require.Equal(t, RestartUnfinishedTask{ID: "t2", PreviousBranch: "w/t2", HasUnmergedCommits: true, OpenPR: 12, PRBase: "autopilot/x"}, rc.Unfinished[0])
	require.False(t, rc.Unfinished[1].HasUnmergedCommits)
	require.Equal(t, RestartUnfinishedTask{ID: "t4"}, rc.Unfinished[2])
}

func TestAssembleRestartContextCountOnlyWhenOperator(t *testing.T) {
	l := seedRestartLedger(t, newFakeStore())
	rc, err := AssembleRestartContext(context.Background(), RestartAssembleInput{Ledger: l, PreviousCount: 4})
	require.NoError(t, err)
	require.Equal(t, 4, rc.RestartCount, "guardian HotSwap-style capture must not count (OQ-6)")
	require.Equal(t, RestartReasonUnknown, rc.ReasonKind)
}

func TestAssembleRestartContextReasonKinds(t *testing.T) {
	l := seedRestartLedger(t, newFakeStore())
	for _, k := range []string{RestartReasonOperatorStop, RestartReasonNeedsAttention, RestartReasonDegradedBackoff, RestartReasonOperatorForce, RestartReasonWatchdog} {
		rc, err := AssembleRestartContext(context.Background(), RestartAssembleInput{Ledger: l, ReasonKind: k, Reason: "why-" + k})
		require.NoError(t, err)
		require.Equal(t, k, rc.ReasonKind)
		out := RenderRestartContext(rc)
		require.Contains(t, out, k)
		require.Contains(t, out, "why-"+k)
	}
}

func TestAssembleRestartContextJournalBoundNewestFirst(t *testing.T) {
	st := newFakeStore()
	l := NewLedger(st, "run1")
	var js []JournalEntry
	for i := 0; i < 30; i++ { // written oldest-first to prove ordering is enforced
		js = append(js, JournalEntry{At: fmt.Sprintf("2026-10-05T00:%02d:00Z", i), Note: fmt.Sprintf("n%d", i)})
	}
	raw, err := marshalList(js)
	require.NoError(t, err)
	require.NoError(t, st.Set(l.JournalKey(), raw, "test"))

	rc, err := AssembleRestartContext(context.Background(), RestartAssembleInput{Ledger: l})
	require.NoError(t, err)
	require.Len(t, rc.Journal, 20)
	require.Equal(t, "n29", rc.Journal[0].Note)
	require.Equal(t, "n10", rc.Journal[19].Note)

	rc, err = AssembleRestartContext(context.Background(), RestartAssembleInput{Ledger: l, JournalLimit: 3})
	require.NoError(t, err)
	require.Len(t, rc.Journal, 3)
}

func TestCaptureRestartContextPersistsAndCounts(t *testing.T) {
	st := newFakeStore()
	l := seedRestartLedger(t, st)
	ctx := context.Background()
	_, err := l.CaptureRestartContext(ctx, RestartAssembleInput{CountRestart: true, ReasonKind: RestartReasonOperatorStop})
	require.NoError(t, err)
	rc, err := l.CaptureRestartContext(ctx, RestartAssembleInput{CountRestart: true, ReasonKind: RestartReasonOperatorForce})
	require.NoError(t, err)
	require.Equal(t, 2, rc.RestartCount)

	got, err := l.LoadRestartContext() // survives: read straight from the store
	require.NoError(t, err)
	require.Equal(t, 2, got.RestartCount)
	_, ok := st.m["autopilot.run1.restart_context"]
	require.True(t, ok)
	require.Equal(t, "plan.p1.restart_context", PlanRestartContextKey("p1"))
}

func TestDigestAndPromptContainSectionIffPresent(t *testing.T) {
	st := newFakeStore()
	l := seedRestartLedger(t, st)
	base := DigestInput{RunID: "run1", Ledger: l, Plan: Plan{Goal: "g"}}

	out, err := ComposeDigest(context.Background(), base)
	require.NoError(t, err)
	require.NotContains(t, out, "## Restart context")

	rc, err := l.CaptureRestartContext(context.Background(), RestartAssembleInput{CountRestart: true, ReasonKind: RestartReasonOperatorStop})
	require.NoError(t, err)
	base.RestartContext = rc
	out, err = ComposeDigest(context.Background(), base)
	require.NoError(t, err)
	require.Contains(t, out, "## Restart context")
	for _, want := range []string{
		"Finished tasks must not be redone",
		"create the worktree from that branch",
		"reuse it (push to it); never open a duplicate",
		"close the old PR with a comment",
	} {
		require.Contains(t, out, want)
	}

	require.Equal(t, "base", AppendRestartContext("base", ""))
	p := AppendRestartContext("base", RenderRestartContext(rc))
	require.Contains(t, p, "## Restart context")
	require.Equal(t, p, AppendRestartContext(p, RenderRestartContext(rc)), "no duplicate section")
}

func TestRestartTextIsFileBackedNotOnLaunchLine(t *testing.T) {
	st := newFakeStore()
	l := seedRestartLedger(t, st)
	rc, err := l.CaptureRestartContext(context.Background(), RestartAssembleInput{CountRestart: true, Reason: strings.Repeat("long ", 100)})
	require.NoError(t, err)
	prompt := AppendRestartContext("do the task", RenderRestartContext(rc))
	require.Greater(t, len(prompt), 1024)

	fr := &lifecycle.FakeRunner{}
	lc := lifecycle.New(fr, &lifecycle.FakeConfig{})
	lc.PromptsDir = "/state/prompts"
	lc.HintsDir = "/state/hints"
	_, err = lc.Spawn(context.Background(), lifecycle.SpawnRequest{Prompt: prompt, Cwd: "/work/project"})
	require.NoError(t, err)

	wrote := false
	for _, c := range fr.Calls {
		if len(c.Argv) >= 2 && c.Argv[0] == "tmux" && c.Argv[1] == "send-keys" {
			line := strings.Join(c.Argv, " ")
			require.NotContains(t, line, "Restart context", "restart text must not be typed on the launch line")
			require.Less(t, len(c.Argv[4]), 1024, c.Argv[4])
		}
		if strings.Contains(strings.Join(c.Argv, " "), "## Restart context") {
			wrote = true
		}
	}
	require.True(t, wrote, "restart section written to the prompt file")
}

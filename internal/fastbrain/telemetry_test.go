package fastbrain

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type detailedRunner struct{ fail bool }

func (d detailedRunner) Run(ctx context.Context, p string) (string, error) {
	raw, _, err := d.RunDetailed(ctx, p)
	return raw, err
}
func (d detailedRunner) RunDetailed(_ context.Context, _ string) (string, Selection, error) {
	sel := Selection{Runner: "Claude", Model: "Sonnet 5.5/x", Attempts: 1,
		Trail: []CandidateOutcome{{ID: "claude", Model: "m", Verdict: "selected"}}}
	if d.fail {
		return "", sel, errors.New("boom with /home/user/secret and tok_abc123")
	}
	return `{"ok":true}`, sel, nil
}

func TestTelemetryRecordsRedactedLabelsAndTrace(t *testing.T) {
	var mu sync.Mutex
	var events []string
	eng := NewEngineWithOptions(detailedRunner{}, detailedRunner{}, EngineOptions{
		Audit: func(a, tgt string, d map[string]string) { mu.Lock(); events = append(events, a+":"+tgt); mu.Unlock() },
	})
	req := Request{Kind: KindCommitMessage, Tier: TierFast, Prompt: "secret prompt /home/me/x"}
	_, _ = eng.Decide(context.Background(), req)
	_, _ = eng.Decide(context.Background(), req) // cache hit
	insp := eng.(Inspector)
	s := insp.Telemetry()
	require.EqualValues(t, 2, s.TotalDecisions)
	require.Len(t, s.Kinds, 1)
	k := s.Kinds[0]
	require.EqualValues(t, 2, k.Attempts)
	require.EqualValues(t, 1, k.Outcomes["ok"])
	require.EqualValues(t, 1, k.Outcomes[OutcomeCached])
	require.EqualValues(t, 1, k.Cached)
	require.Equal(t, "claude", s.Runners[0].Provider)
	require.Equal(t, "sonnet-5.5-x", s.Runners[0].Model)
	require.Len(t, s.Queues, 4)
	b, _ := json.Marshal(struct {
		S TelemetrySnapshot
		D []Decision
	}{s, insp.Decisions(10)})
	for _, bad := range []string{"secret", "/home", "tok_abc"} {
		require.False(t, strings.Contains(string(b), bad), bad)
	}
	d := insp.Decisions(10)
	require.Len(t, d, 2)
	require.Equal(t, "cached", d[0].FinalAction)
	require.Equal(t, "decided", d[1].FinalAction)
}

func TestTelemetryFailOpenAndUnknownKindBounded(t *testing.T) {
	eng := NewEngineWithOptions(detailedRunner{fail: true}, nil, EngineOptions{})
	r, _ := eng.Decide(context.Background(), Request{Kind: "weird_kind_123", Tier: TierFast, Prompt: "p"})
	require.Equal(t, StatusRunnerError, r.Status)
	s := eng.(Inspector).Telemetry()
	require.Equal(t, KindOther, s.Kinds[0].Kind)
	require.EqualValues(t, 1, s.Kinds[0].FailOpen)
	require.EqualValues(t, 1, s.Runners[0].Failed)
}

func TestPauseResumeKind(t *testing.T) {
	calls := 0
	rf := RunnerFunc(func(context.Context, string) (string, error) { calls++; return `{}`, nil })
	var acts []string
	eng := NewEngineWithOptions(rf, rf, EngineOptions{
		Audit: func(a, _ string, _ map[string]string) { acts = append(acts, a) },
	})
	insp := eng.(Inspector)
	require.ErrorIs(t, insp.SetKindPaused("nope", true, 0), ErrUnknownKind)
	require.NoError(t, insp.SetKindPaused(KindClassifyTask, true, 0))
	r, _ := eng.Decide(context.Background(), Request{Kind: KindClassifyTask, Tier: TierFast, Prompt: "a"})
	require.Equal(t, StatusDeferred, r.Status)
	require.Equal(t, ShedPaused, r.Admission.Reason)
	require.Zero(t, calls)
	var paused bool
	var until *time.Time
	for _, c := range insp.Telemetry().Controls {
		if c.Kind == KindClassifyTask {
			paused, until = c.Paused, c.Until
		}
	}
	require.True(t, paused)
	require.NotNil(t, until)
	require.NoError(t, insp.SetKindPaused(KindClassifyTask, false, 0))
	r, _ = eng.Decide(context.Background(), Request{Kind: KindClassifyTask, Tier: TierFast, Prompt: "a"})
	require.Equal(t, StatusOK, r.Status)
	require.Equal(t, []string{AuditControl, AuditControl}, acts)
}

func TestPauseExpiresAndConfigPauseSticks(t *testing.T) {
	now := time.Now()
	tel := newTelemetry(nil, nil, func() time.Time { return now })
	require.NoError(t, tel.setPaused(KindPRSummary, true, 48*time.Hour, "operator"))
	require.True(t, tel.paused(KindPRSummary))
	now = now.Add(MaxPauseTTL + time.Second)
	require.False(t, tel.paused(KindPRSummary))
	require.NoError(t, tel.setPaused(KindPRSummary, true, 0, "config"))
	now = now.Add(100 * time.Hour)
	require.True(t, tel.paused(KindPRSummary))
}

func TestCircuitTransitionAudited(t *testing.T) {
	state := CircuitClosed
	var got []string
	eng := NewEngineWithOptions(detailedRunner{}, detailedRunner{}, EngineOptions{
		HealthSnapshot: func() []RunnerHealth { return []RunnerHealth{{ID: "claude", State: state}} },
		Audit:          func(a, tgt string, d map[string]string) { got = append(got, a+":"+tgt+":"+d["to"]) },
	})
	_, _ = eng.Decide(context.Background(), Request{Kind: KindCommitMessage, Tier: TierFast, Prompt: "1"})
	require.Empty(t, got)
	state = CircuitOpen
	_, _ = eng.Decide(context.Background(), Request{Kind: KindCommitMessage, Tier: TierFast, Prompt: "2"})
	require.Equal(t, []string{"fastbrain_circuit:claude:open"}, got)
}

func TestAllKindsCoveredByInventory(t *testing.T) {
	for k := range decisionInventory {
		found := false
		for _, a := range AllKinds() {
			found = found || a == k
		}
		require.True(t, found, "kind %s missing from AllKinds", k)
	}
}

package dbtx

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/warden/internal/audit"
	"github.com/stretchr/testify/require"
)

func TestMetricsCollector_NormalCommit(t *testing.T) {
	collector := NewMetricsCollector()
	e := newEnv(t, WithTelemetry(collector))

	ctx := context.Background()
	tx, err := e.b.Begin(ctx)
	require.NoError(t, err)

	_, err = e.a.Insert(tx, doc("x", 1))
	require.NoError(t, err)

	res, err := tx.Commit(ctx, "project-child-creation")
	require.NoError(t, err)
	require.NotEmpty(t, res.TxID)

	require.Equal(t, uint64(1), collector.CommitsTotal())
	require.Equal(t, uint64(0), collector.ConflictsTotal())
	require.Equal(t, uint64(0), collector.RollbacksTotal())
	require.Equal(t, uint64(0), collector.RecoveriesTotal())
	require.Equal(t, uint64(0), collector.AbandonedTotal())
	require.Equal(t, uint64(0), collector.ErrorsTotal())

	snap := collector.Snapshot()
	require.Equal(t, uint64(1), snap.CommitsTotal)
	require.Equal(t, uint64(1), snap.ByOutcome[OutcomeSuccess])
	require.Equal(t, uint64(1), snap.Latency.Count)
	require.GreaterOrEqual(t, snap.Latency.SumMs, int64(0))

	intentSummary, ok := snap.ByIntent["project-child-creation"]
	require.True(t, ok)
	require.Equal(t, uint64(1), intentSummary.Commits)
	require.Equal(t, uint64(1), intentSummary.Latency.Count)
}

func TestMetricsCollector_Conflict(t *testing.T) {
	collector := NewMetricsCollector()
	e := newEnv(t, WithTelemetry(collector))

	ctx := context.Background()
	aid, _ := e.seed(t, "seed")
	collector.Reset()

	// Provoke conflict via ExpectedRev mismatch
	tx, err := e.b.Begin(ctx)
	require.NoError(t, err)
	err = e.a.Update(tx, aid, doc("conflict", 2), 999)
	require.NoError(t, err)

	_, err = tx.Commit(ctx, "autopilot-startup")
	require.Error(t, err)

	var be *Error
	require.True(t, errors.As(err, &be))
	require.Equal(t, ClassConflict, be.Class)

	require.Equal(t, uint64(0), collector.CommitsTotal())
	require.Equal(t, uint64(1), collector.ConflictsTotal())
	require.Equal(t, uint64(1), collector.ErrorsTotal())

	snap := collector.Snapshot()
	require.Equal(t, uint64(1), snap.ByOutcome[string(ClassConflict)])
	require.Equal(t, uint64(1), snap.ByClass[string(ClassConflict)])

	intentSummary := snap.ByIntent["autopilot-startup"]
	require.Equal(t, uint64(1), intentSummary.Conflicts)
	require.Equal(t, uint64(1), intentSummary.Errors)
}

func TestMetricsCollector_Rollback(t *testing.T) {
	collector := NewMetricsCollector()
	e := newEnv(t, WithTelemetry(collector))

	ctx := context.Background()

	// 1. Explicit Rollback on open transaction
	tx, err := e.b.Begin(ctx)
	require.NoError(t, err)
	tx.SetIntent("pipeline-linkage")
	_, _ = e.a.Insert(tx, doc("roll", 1))

	err = tx.Rollback()
	require.NoError(t, err)

	// Idempotent second rollback should not double-count
	err = tx.Rollback()
	require.NoError(t, err)

	require.Equal(t, uint64(1), collector.RollbacksTotal())
	snap := collector.Snapshot()
	require.Equal(t, uint64(1), snap.ByOutcome[OutcomeRolledBack])
	require.Equal(t, uint64(1), snap.ByIntent["pipeline-linkage"].Rollbacks)

	// 2. Rollback in Boundary.Run on build error
	customErr := errors.New("custom build failure")
	_, err = e.b.Run(ctx, "archive-restore", func(ctx context.Context, tx *Tx) error {
		_, _ = e.a.Insert(tx, doc("fail", 2))
		return customErr
	})
	require.ErrorIs(t, err, customErr)

	require.Equal(t, uint64(2), collector.RollbacksTotal())
	snap = collector.Snapshot()
	require.Equal(t, uint64(1), snap.ByIntent["archive-restore"].Rollbacks)
}

func TestMetricsCollector_Recovery(t *testing.T) {
	collector := NewMetricsCollector()
	e := newEnv(t, WithTelemetry(collector))

	// Inject outcome_unknown that resolves to COMMITTED via status lookup
	e.eng.commit = func(real Engine, key string, ops []engine.XTxOp) (*engine.XTxResult, error) {
		return nil, &engine.XTxError{Err: engine.ErrXTxOutcomeUnknown, Tx: "tx-rec-1"}
	}
	e.eng.status = func(real Engine, k string) (engine.XTxStatus, string) {
		return engine.XTxCommitted, "tx-rec-1"
	}

	ctx := context.Background()
	tx, err := e.b.Begin(ctx)
	require.NoError(t, err)
	_, _ = e.a.Insert(tx, doc("rec", 1))

	res, err := tx.Commit(ctx, "projection-update")
	require.NoError(t, err)
	require.True(t, res.Replayed)

	require.Equal(t, uint64(1), collector.CommitsTotal())
	require.Equal(t, uint64(1), collector.RecoveriesTotal())

	snap := collector.Snapshot()
	require.Equal(t, uint64(1), snap.RecoveriesTotal)
	require.Equal(t, uint64(1), snap.ByIntent["projection-update"].Recoveries)
}

func TestMetricsCollector_Abandoned(t *testing.T) {
	collector := NewMetricsCollector()
	e := newEnv(t, WithTelemetry(collector))

	// Inject outcome_unknown that remains UNKNOWN
	e.eng.commit = func(real Engine, key string, ops []engine.XTxOp) (*engine.XTxResult, error) {
		return nil, &engine.XTxError{Err: engine.ErrXTxOutcomeUnknown, Tx: "tx-abandoned-1"}
	}
	e.eng.status = func(real Engine, k string) (engine.XTxStatus, string) {
		return engine.XTxUnknown, ""
	}

	ctx := context.Background()
	tx, err := e.b.Begin(ctx)
	require.NoError(t, err)
	_, _ = e.a.Insert(tx, doc("ab", 1))

	_, err = tx.Commit(ctx, "audit-event-append")
	require.Error(t, err)

	require.Equal(t, uint64(1), collector.AbandonedTotal())
	require.Equal(t, uint64(1), collector.ErrorsTotal())
	require.Equal(t, uint64(1), collector.RecoveriesTotal()) // status lookup attempted and resolved UNKNOWN

	snap := collector.Snapshot()
	require.Equal(t, uint64(1), snap.AbandonedTotal)
	require.Equal(t, uint64(1), snap.ByIntent["audit-event-append"].Abandoned)
}

func TestSafeCardinality(t *testing.T) {
	collector := NewMetricsCollector()

	// Generate 5,000 distinct randomized intent strings
	rnd := rand.New(rand.NewSource(42))
	for i := 0; i < 5000; i++ {
		var intent string
		switch i % 5 {
		case 0:
			intent = fmt.Sprintf("op-%x-%x/step", rnd.Int63(), rnd.Int63())
		case 1:
			intent = fmt.Sprintf("plan-%x/launch", rnd.Int63())
		case 2:
			intent = fmt.Sprintf("random-user-input-%d-with-secrets-and-ids-%x", i, rnd.Int63())
		case 3:
			intent = fmt.Sprintf("deadbeef%016x", rnd.Int63())
		case 4:
			intent = "project-child-creation"
		}

		collector.Observe(Event{
			Intent:   intent,
			Outcome:  OutcomeSuccess,
			Duration: time.Duration(i%50) * time.Millisecond,
		})
	}

	snap := collector.Snapshot()

	// Cardinality guarantee: total distinct intent categories must NEVER exceed maxIntentBuckets (64)
	require.LessOrEqual(t, len(snap.ByIntent), maxIntentBuckets,
		"distinct intent labels must not exceed maxIntentBuckets")

	// Check that family prefixes were properly coalesced
	require.Contains(t, snap.ByIntent, "operation/step")
	require.Contains(t, snap.ByIntent, "plan/launch")
	require.Contains(t, snap.ByIntent, "project-child-creation")
	require.Contains(t, snap.ByIntent, "custom")

	// Reset should work cleanly
	collector.Reset()
	require.Equal(t, uint64(0), collector.CommitsTotal())
	require.Empty(t, collector.Snapshot().ByIntent)
}

func TestAuditObserver_Integration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	writer := audit.NewWriter(path)
	auditObs := NewAuditObserver(writer)
	collector := NewMetricsCollector()

	multi := MultiObserver(collector, auditObs)
	e := newEnv(t, WithObserver(multi))

	// 1. Normal commit -> metrics recorded, NOT audited (no spam)
	ctx := context.Background()
	tx, err := e.b.Begin(ctx)
	require.NoError(t, err)
	_, _ = e.a.Insert(tx, doc("x", 1))
	_, err = tx.Commit(ctx, "project-child-creation")
	require.NoError(t, err)

	events, err := audit.Read(path, audit.Filter{})
	require.NoError(t, err)
	require.Empty(t, events, "normal commit should not produce audit event")

	// 2. Recovery event -> audited with ActionTxRecovery
	e.eng.commit = func(real Engine, key string, ops []engine.XTxOp) (*engine.XTxResult, error) {
		return nil, &engine.XTxError{Err: engine.ErrXTxOutcomeUnknown, Tx: "tx-rec-99"}
	}
	e.eng.status = func(real Engine, k string) (engine.XTxStatus, string) {
		return engine.XTxCommitted, "tx-rec-99"
	}

	tx2, err := e.b.Begin(ctx)
	require.NoError(t, err)
	_, _ = e.a.Insert(tx2, doc("y", 2))
	_, err = tx2.Commit(ctx, "op-1234/recovery-step")
	require.NoError(t, err)

	events, err = audit.Read(path, audit.Filter{})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, audit.ActionTxRecovery, events[0].Action)
	require.Equal(t, "tx-rec-99", events[0].Target)
	require.Equal(t, "operation/recovery-step", events[0].Detail["intent"])
	require.Equal(t, "committed", events[0].Detail["outcome"])
	require.Equal(t, "true", events[0].Detail["resolved"])

	// 3. Durability error -> audited with ActionTxOutcome
	e.eng.status = nil
	e.eng.commit = func(real Engine, key string, ops []engine.XTxOp) (*engine.XTxResult, error) {
		return nil, &engine.XTxError{Err: engine.ErrXTxDurability, Tx: "tx-dur-1"}
	}
	tx3, err := e.b.Begin(ctx)
	require.NoError(t, err)
	_, _ = e.a.Insert(tx3, doc("z", 3))
	_, err = tx3.Commit(ctx, "pipeline-linkage")
	require.Error(t, err)

	events, err = audit.Read(path, audit.Filter{Action: audit.ActionTxOutcome})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, "durability", events[0].Detail["outcome"])
}

func TestObserver_ErrorResilience(t *testing.T) {
	// A panicking observer must NEVER break or disrupt transaction execution
	panickingObs := ObserverFunc(func(ev Event) {
		panic("observer intentionally exploded!")
	})

	collector := NewMetricsCollector()
	multi := MultiObserver(panickingObs, collector)

	e := newEnv(t, WithObserver(multi))
	ctx := context.Background()

	// Commit should succeed despite observer panic
	tx, err := e.b.Begin(ctx)
	require.NoError(t, err)
	_, err = e.a.Insert(tx, doc("safe", 100))
	require.NoError(t, err)

	res, err := tx.Commit(ctx, "resilience-test")
	require.NoError(t, err)
	require.NotEmpty(t, res.TxID)

	// Sibling observer in MultiObserver should still have received the event
	require.Equal(t, uint64(1), collector.CommitsTotal())

	// Rollback should also not panic
	tx2, err := e.b.Begin(ctx)
	require.NoError(t, err)
	tx2.SetIntent("resilience-rollback")
	_, _ = e.a.Insert(tx2, doc("roll", 200))
	require.NotPanics(t, func() {
		_ = tx2.Rollback()
	})
	require.Equal(t, uint64(1), collector.RollbacksTotal())
}

func TestHistogram_QuantilesAndBounds(t *testing.T) {
	var h hist

	// Empty histogram
	snap := h.snapshot()
	require.Equal(t, uint64(0), snap.Count)
	require.Equal(t, int64(0), snap.P50Ms)
	require.Equal(t, int64(0), snap.P95Ms)
	require.Equal(t, int64(0), snap.P99Ms)

	// Add durations across various buckets
	durations := []time.Duration{
		500 * time.Microsecond,  // 0ms -> bucket 1ms
		1500 * time.Microsecond, // 1ms -> bucket 1ms
		4 * time.Millisecond,    // 4ms -> bucket 5ms
		15 * time.Millisecond,   // 15ms -> bucket 25ms
		80 * time.Millisecond,   // 80ms -> bucket 100ms
		400 * time.Millisecond,  // 400ms -> bucket 500ms
		1200 * time.Millisecond, // 1200ms -> bucket 2500ms
		6000 * time.Millisecond, // 6000ms -> bucket +Inf
	}

	for _, d := range durations {
		h.add(d)
	}

	snap = h.snapshot()
	require.Equal(t, uint64(len(durations)), snap.Count)
	require.Equal(t, int64(0), snap.MinMs)
	require.Equal(t, int64(6000), snap.MaxMs)
	require.Greater(t, snap.SumMs, int64(0))
	require.Greater(t, snap.P50Ms, int64(0))
	require.GreaterOrEqual(t, snap.P95Ms, snap.P50Ms)
	require.GreaterOrEqual(t, snap.P99Ms, snap.P95Ms)
}

func TestNormalizeIntent_Cases(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"", "unspecified"},
		{"   ", "unspecified"},
		{"project-child-creation", "project-child-creation"},
		{"AUTOPILOT-STARTUP", "autopilot-startup"},
		{"pipeline-linkage", "pipeline-linkage"},
		{"op-12345/launch", "operation/launch"},
		{"op_abcdef/step", "operation/step"},
		{"plan-987/update", "plan/update"},
		{"agent-xyz/status", "agent/status"},
		{"proj-001/archive", "project/archive"},
		{"pipe-012/run", "pipeline/run"},
		{"sess-abc/close", "session/close"},
		{"tx-55/commit", "tx/commit"},
		{"workflow-1/step", "workflow/step"},
		{"op-12345/unreasonably_long_action_that_should_not_suffix", "operation"},
		{"deadbeef0123456789", "custom"},
		{"1234567890", "custom"},
		{"some.safe-intent_name", "some.safe-intent_name"},
		{"intent with spaces & special chars!", "intent_with_spaces___special_chars_"},
	}

	for i, c := range cases {
		t.Run(fmt.Sprintf("case_%d_%s", i, c.input), func(t *testing.T) {
			got := NormalizeIntent(c.input)
			require.Equal(t, c.want, got)
		})
	}
}

func TestNormalizeOutcome_Cases(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{OutcomeSuccess, OutcomeSuccess},
		{OutcomeRolledBack, OutcomeRolledBack},
		{OutcomeAbandoned, OutcomeAbandoned},
		{OutcomeNoChange, OutcomeNoChange},
		{string(ClassConflict), string(ClassConflict)},
		{string(ClassExhausted), string(ClassExhausted)},
		{string(ClassDurability), string(ClassDurability)},
		{string(ClassTooLarge), string(ClassTooLarge)},
		{string(ClassOutcomeUnknown), string(ClassOutcomeUnknown)},
		{string(ClassCollectionNotFound), string(ClassCollectionNotFound)},
		{string(ClassInternal), string(ClassInternal)},
		{"unexpected_error_xyz", OutcomeOther},
		{"random string", OutcomeOther},
	}

	for _, c := range cases {
		got := NormalizeOutcome(c.input)
		require.Equal(t, c.want, got)
	}
}

func TestDefaultObserver(t *testing.T) {
	collector, obs := DefaultObserver(nil)
	require.NotNil(t, collector)
	require.NotNil(t, obs)

	path := filepath.Join(t.TempDir(), "audit.jsonl")
	w := audit.NewWriter(path)
	collector, obs = DefaultObserver(w)
	require.NotNil(t, collector)
	require.NotNil(t, obs)

	obs.Observe(Event{Intent: "test", Outcome: OutcomeSuccess, Duration: time.Millisecond})
	require.Equal(t, uint64(1), collector.CommitsTotal())
}

func TestMetricsCollector_ConcurrentAccess(t *testing.T) {
	collector := NewMetricsCollector()
	const workers = 20
	const iters = 100

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				intent := fmt.Sprintf("op-%d/step", workerID%5)
				outcome := OutcomeSuccess
				if i%10 == 0 {
					outcome = string(ClassConflict)
				}
				collector.Observe(Event{
					Intent:   intent,
					Outcome:  outcome,
					Duration: time.Duration(i) * time.Millisecond,
				})
				_ = collector.Snapshot()
				_ = collector.CommitsTotal()
				_ = collector.ConflictsTotal()
			}
		}(w)
	}
	wg.Wait()

	snap := collector.Snapshot()
	require.Equal(t, uint64(workers*iters), snap.CommitsTotal+snap.ConflictsTotal)
	require.Equal(t, uint64(workers*iters), snap.Latency.Count)
}

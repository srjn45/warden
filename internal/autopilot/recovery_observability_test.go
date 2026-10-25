package autopilot

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// casFailStore refuses CAS so a repair cannot complete, modelling an
// unavailable supported-repair path.
type casFailStore struct{ *fakeStore }

func (casFailStore) CompareAndSet(string, string, string, string) error {
	return errors.New("store unavailable")
}

type casFailRuntime struct {
	Runtime
	store *fakeStore
}

func (r casFailRuntime) NewLedger(id string) *Ledger { return NewLedger(casFailStore{r.store}, id) }

func TestLedgerRepairJournalQuarantinesThenRebuilds(t *testing.T) {
	st := newFakeStore()
	l := NewLedger(st, "run")
	require.NoError(t, st.Set(l.TasksKey(), `[{"id":"t1","state":"done"}]`, ledgerWriter))
	require.NoError(t, st.Set(l.JournalKey(), "{not json", ledgerWriter))

	qkey, err := l.RepairJournal()
	require.NoError(t, err)
	require.NotEmpty(t, qkey)
	got, err := st.Get(qkey)
	require.NoError(t, err)
	require.Equal(t, "{not json", got, "original bytes retained")
	j, err := l.Journal()
	require.NoError(t, err)
	require.Empty(t, j)
	tasks, err := st.Get(l.TasksKey())
	require.NoError(t, err)
	require.Contains(t, tasks, "t1", "task ledger untouched")

	// Idempotent: a readable journal is never quarantined again.
	qkey, err = l.RepairJournal()
	require.NoError(t, err)
	require.Empty(t, qkey)
}

func TestLedgerRepairJournalMissingOrHealthyIsNoop(t *testing.T) {
	st := newFakeStore()
	l := NewLedger(st, "run")
	q, err := l.RepairJournal()
	require.NoError(t, err)
	require.Empty(t, q)
	require.NoError(t, st.Set(l.JournalKey(), "[]", ledgerWriter))
	q, err = l.RepairJournal()
	require.NoError(t, err)
	require.Empty(t, q)
}

func TestRecoveryRepairsCorruptJournalAutomatically(t *testing.T) {
	c, r, rt := recoveryFixture(t, "")
	l := NewLedger(rt.store, r.runID)
	require.NoError(t, rt.store.Set(l.JournalKey(), "invalid JSON", ledgerWriter))
	c.watchPlanTick(context.Background(), r.runID)
	waitRecoveryIdle(t, c)
	st := c.Status()
	require.NotNil(t, st.Runs[0].Brain)
	require.Equal(t, uint64(1), st.Recovery.JournalRepairs)
	require.Zero(t, st.Recovery.JournalRepairNeeded)
	_, err := l.Journal()
	require.NoError(t, err)
}

func TestRecoveryJournalRepairFailureIsDistinctDiagnostic(t *testing.T) {
	c, r, rt := recoveryFixture(t, "")
	l := NewLedger(rt.store, r.runID)
	require.NoError(t, rt.store.Set(l.JournalKey(), "invalid JSON", ledgerWriter))
	c.SetRuntime(casFailRuntime{Runtime: rt, store: rt.store})
	c.watchPlanTick(context.Background(), r.runID)
	waitRecoveryIdle(t, c)
	st := c.Status()
	require.Nil(t, st.Runs[0].Brain)
	require.Equal(t, "journal_repair_needed", st.Runs[0].RecoveryDiagnostic)
	require.Equal(t, uint64(1), st.Recovery.JournalRepairNeeded)
	require.NotContains(t, st.Runs[0].NeedsAttention, "plan_definition_repair_needed")
	raw, _ := rt.store.Get(l.JournalKey())
	require.Equal(t, "invalid JSON", raw, "failed repair leaves the journal untouched")
}

func TestRecoveryStatusDistinguishesAdoptedAndVerificationFailed(t *testing.T) {
	c, r, _ := recoveryFixture(t, "")
	c.watchPlanTick(context.Background(), r.runID)
	waitRecoveryIdle(t, c)
	require.Contains(t, []string{"launched", "adopted"}, c.Status().Runs[0].RecoveryDiagnostic)

	c2, r2, _ := recoveryFixture(t, "")
	c2.mu.Lock()
	r2.absPlanFile, r2.planFile = r2.repo+"/missing.yaml", r2.repo+"/missing.yaml"
	c2.mu.Unlock()
	c2.watchPlanTick(context.Background(), r2.runID)
	waitRecoveryIdle(t, c2)
	st := c2.Status()
	require.Equal(t, "verification_failed", st.Runs[0].RecoveryDiagnostic)
	require.Contains(t, st.Runs[0].NeedsAttention, "plan_definition_repair_needed")
	require.Equal(t, uint64(1), st.Recovery.VerificationFailures)
	require.Nil(t, st.Runs[0].Brain)
}

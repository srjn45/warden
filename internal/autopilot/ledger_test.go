package autopilot

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeStore is an in-memory CtxStore mirroring ctxstore.Store's Get/Set/CAS
// semantics (ErrLedgerMissing on absent Get, ErrLedgerConflict on CAS mismatch,
// expected "" = must-be-absent) so the ledger is exercised without a ScrivaDB.
type fakeStore struct {
	m map[string]string
}

func newFakeStore() *fakeStore { return &fakeStore{m: map[string]string{}} }

func (f *fakeStore) Get(key string) (string, error) {
	v, ok := f.m[key]
	if !ok {
		return "", ErrLedgerMissing
	}
	return v, nil
}

func (f *fakeStore) Set(key, value, _ string) error {
	f.m[key] = value
	return nil
}

func (f *fakeStore) CompareAndSet(key, expected, value, _ string) error {
	cur, ok := f.m[key]
	if expected == "" {
		if ok {
			return ErrLedgerConflict
		}
		f.m[key] = value
		return nil
	}
	if !ok || cur != expected {
		return ErrLedgerConflict
	}
	f.m[key] = value
	return nil
}

func TestLedgerKeysAreDotNamespaced(t *testing.T) {
	l := NewLedger(newFakeStore(), "ap-abc123")
	require.Equal(t, "autopilot.ap-abc123.tasks", l.TasksKey())
	require.Equal(t, "autopilot.ap-abc123.landings", l.LandingsKey())
	require.Equal(t, "autopilot.ap-abc123.journal", l.JournalKey())
	require.Equal(t, "autopilot.ap-abc123.tasks.api.state", l.TaskStateKey("api"))
	require.Equal(t, "autopilot.ap-abc123.tasks.api.branch", l.TaskBranchKey("api"))
	require.Equal(t, "autopilot.ap-abc123.integration_branch", l.IntegrationBranchKey())
}

func TestLedgerTasksRoundTrip(t *testing.T) {
	store := newFakeStore()
	l := NewLedger(store, "ap-run")

	// Absent key reads as an empty (not error) collection.
	got, err := l.Tasks()
	require.NoError(t, err)
	require.Empty(t, got)

	tasks := []LedgerTask{
		{ID: "api", State: LedgerInProgress, WorkerID: "A-1", Branch: "autopilot/api", PR: 7},
		{ID: "ui", State: LedgerPending},
	}
	require.NoError(t, l.WriteTasks(tasks, "brain"))

	got, err = l.Tasks()
	require.NoError(t, err)
	require.Equal(t, tasks, got)
}

func TestLedgerCAS(t *testing.T) {
	store := newFakeStore()
	l := NewLedger(store, "ap-run")

	first := []LedgerTask{{ID: "api", State: LedgerPending}}
	// CAS from absent (expected nil) creates the key.
	require.NoError(t, l.CASTasks(nil, first, "brain"))

	// CAS with a stale expectation loses the race.
	stale := []LedgerTask{{ID: "wrong", State: LedgerPending}}
	err := l.CASTasks(stale, []LedgerTask{{ID: "x", State: LedgerPending}}, "brain")
	require.ErrorIs(t, err, ErrLedgerConflict)

	// CAS with the current value succeeds.
	next := []LedgerTask{{ID: "api", State: LedgerLanded}}
	require.NoError(t, l.CASTasks(first, next, "brain"))

	got, err := l.Tasks()
	require.NoError(t, err)
	require.Equal(t, next, got)

	// Re-creating an existing key (expected nil) conflicts.
	require.ErrorIs(t, l.CASTasks(nil, next, "brain"), ErrLedgerConflict)
}

func TestLedgerAppendLanding(t *testing.T) {
	store := newFakeStore()
	l := NewLedger(store, "ap-run")

	got, err := l.Landings()
	require.NoError(t, err)
	require.Empty(t, got)

	require.NoError(t, l.AppendLanding(Landing{Branch: "autopilot/api", SHA: "deadbeefcafe0001", PR: 7, LandedAt: "t1"}))
	require.NoError(t, l.AppendLanding(Landing{Branch: "autopilot/ui", SHA: "deadbeefcafe0002", PR: 8, LandedAt: "t2"}))

	got, err = l.Landings()
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "autopilot/api", got[0].Branch)
	require.Equal(t, "autopilot/ui", got[1].Branch)
}

func TestLedgerWriteTasksRejectsInvalidState(t *testing.T) {
	l := NewLedger(newFakeStore(), "ap-run")

	err := l.WriteTasks([]LedgerTask{{ID: "api", State: "fixing"}}, "brain")
	require.ErrorIs(t, err, ErrInvalidLedgerState)
	got, err := l.Tasks()
	require.NoError(t, err)
	require.Empty(t, got, "rejected write must not persist")

	err = l.WriteTasks([]LedgerTask{{ID: "", State: LedgerPending}}, "brain")
	require.ErrorIs(t, err, ErrInvalidLedgerTask)

	err = l.CASTasks(nil, []LedgerTask{{ID: "api", State: "nope"}}, "brain")
	require.ErrorIs(t, err, ErrInvalidLedgerState)
	got, err = l.Tasks()
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestLedgerWriteTasksAcceptsEveryCanonicalState(t *testing.T) {
	l := NewLedger(newFakeStore(), "ap-run")
	var tasks []LedgerTask
	for i, st := range CanonicalLedgerStates {
		tasks = append(tasks, LedgerTask{ID: string(st), State: st, Note: string(rune('a' + i))})
	}
	require.NoError(t, l.WriteTasks(tasks, "brain"))
	got, err := l.Tasks()
	require.NoError(t, err)
	require.Equal(t, tasks, got)
}

func TestLedgerReadsPreEnumStatesWithoutValidating(t *testing.T) {
	store := newFakeStore()
	store.m["autopilot.ap-run.tasks"] = `[{"id":"api","state":"fixing"}]`
	l := NewLedger(store, "ap-run")

	got, err := l.Tasks()
	require.NoError(t, err)
	require.Equal(t, []LedgerTask{{ID: "api", State: "fixing"}}, got)

	// CAS expected is the last-read (possibly pre-enum) payload; only next is validated.
	require.NoError(t, l.CASTasks(got, []LedgerTask{{ID: "api", State: LedgerGated}}, "brain"))
}

func TestLedgerOptionalTaskStateOverlay(t *testing.T) {
	store := newFakeStore()
	l := NewLedger(store, "ap-run")

	_, err := l.TaskState("api")
	require.ErrorIs(t, err, ErrLedgerMissing)

	require.ErrorIs(t, l.WriteTaskState("api", "fixing", "brain"), ErrInvalidLedgerState)
	require.ErrorIs(t, l.WriteTaskState("", LedgerPending, "brain"), ErrInvalidLedgerTask)
	require.NoError(t, l.WriteTaskState("api", LedgerPROpen, "brain"))

	got, err := l.TaskState("api")
	require.NoError(t, err)
	require.Equal(t, LedgerPROpen, got)
	require.Equal(t, "pr_open", store.m[l.TaskStateKey("api")])
}

func TestLedgerIntegrationBranchRoundTrip(t *testing.T) {
	store := newFakeStore()
	l := NewLedger(store, "ap-run")
	got, err := l.IntegrationBranch()
	require.NoError(t, err)
	require.Empty(t, got)

	require.NoError(t, l.WriteIntegrationBranch("autopilot/ship", "daemon"))
	got, err = l.IntegrationBranch()
	require.NoError(t, err)
	require.Equal(t, "autopilot/ship", got)
}

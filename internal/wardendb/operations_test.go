package wardendb

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/dbtx"
)

// fakeClock is the journal's injectable time source.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func openJournal(t *testing.T, dataDir string, clk *fakeClock, opts ...Option) (*DB, *Journal) {
	t.Helper()
	db := mustOpen(t, dataDir, opts...)
	j, err := OpenJournal(db, WithOperationClock(clk.Now))
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	return db, j
}

var opSeq int

func testSpec() OperationSpec {
	opSeq++
	return OperationSpec{
		Kind: "tmux.session.create", SubjectType: "agent", SubjectID: "agent-1",
		IdempotencyKey: fmt.Sprintf("agent-1/gen-%d", opSeq),
		Intent:         map[string]any{"session": "wd-agent-1", "cols": 120},
	}
}

func mustBegin(t *testing.T, j *Journal, spec OperationSpec) Operation {
	t.Helper()
	op, created, err := j.Begin(context.Background(), spec)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if !created {
		t.Fatalf("Begin returned an existing operation %s", op.ID)
	}
	return op
}

func mustMove(t *testing.T, j *Journal, id string, to OperationState, u OperationUpdate) Operation {
	t.Helper()
	if to.Terminal() && u.Error == "" && u.Result == nil {
		u.Result = map[string]any{"ok": true}
	}
	if to == OperationRetryWait && u.NextAttemptAt.IsZero() {
		u.NextAttemptAt = j.now().Add(time.Minute)
	}
	op, err := j.Transition(context.Background(), id, to, u)
	if err != nil {
		t.Fatalf("Transition %s -> %s: %v", id, to, err)
	}
	if op.State != to {
		t.Fatalf("state is %s, want %s", op.State, to)
	}
	return op
}

func mustAcquire(t *testing.T, j *Journal, id, owner string, ttl time.Duration) Lease {
	t.Helper()
	l, _, err := j.Acquire(context.Background(), id, owner, ttl)
	if err != nil {
		t.Fatalf("Acquire %s: %v", id, err)
	}
	return l
}

// driveTo takes a fresh operation to state by a legal path and returns it with
// the lease it holds there, if any.
func driveTo(t *testing.T, j *Journal, clk *fakeClock, state OperationState) (Operation, *Lease) {
	t.Helper()
	op := mustBegin(t, j, testSpec())
	if state == OperationPending {
		return op, nil
	}
	if state == OperationCanceled {
		return mustMove(t, j, op.ID, OperationCanceled, OperationUpdate{}), nil
	}
	mustMove(t, j, op.ID, OperationReady, OperationUpdate{})
	if state == OperationReady {
		got, _ := j.Get(op.ID)
		return got, nil
	}
	lease := mustAcquire(t, j, op.ID, "worker", time.Minute)
	held := OperationUpdate{Lease: &lease}
	switch state {
	case OperationRunning:
	case OperationRetryWait, OperationReconciling, OperationCompensating, OperationSucceeded, OperationFailed:
		mustMove(t, j, op.ID, state, held)
	case OperationCompensated:
		mustMove(t, j, op.ID, OperationCompensating, held)
		mustMove(t, j, op.ID, OperationCompensated, held)
	case OperationManualIntervention:
		mustMove(t, j, op.ID, OperationReconciling, held)
		mustMove(t, j, op.ID, OperationManualIntervention, held)
	default:
		t.Fatalf("no path to %s", state)
	}
	got, err := j.Get(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != state {
		t.Fatalf("drove to %s, want %s", got.State, state)
	}
	if !state.Leased() {
		return got, nil
	}
	return got, &lease
}

func TestStateMachineMatchesSpec(t *testing.T) {
	// The "allowed next states" column of spec §2, verbatim.
	want := map[OperationState]string{
		OperationPending:            "ready canceled failed",
		OperationReady:              "running canceled failed",
		OperationRunning:            "succeeded retry_wait reconciling compensating failed",
		OperationRetryWait:          "ready failed canceled",
		OperationReconciling:        "succeeded ready compensating manual_intervention failed",
		OperationCompensating:       "compensated retry_wait manual_intervention failed",
		OperationSucceeded:          "",
		OperationCompensated:        "",
		OperationCanceled:           "",
		OperationFailed:             "",
		OperationManualIntervention: "",
	}
	if len(OperationStates()) != len(want) {
		t.Fatalf("%d states, spec has %d", len(OperationStates()), len(want))
	}
	for _, s := range OperationStates() {
		var next []string
		for _, n := range s.Next() {
			next = append(next, string(n))
		}
		if got := strings.Join(next, " "); got != want[s] {
			t.Errorf("%s -> %q, spec says %q", s, got, want[s])
		}
		if s.Terminal() != (want[s] == "") {
			t.Errorf("%s Terminal() = %v", s, s.Terminal())
		}
	}
	if OperationState("bogus").Valid() || OperationState("bogus").Terminal() {
		t.Error("an unknown state is valid or terminal")
	}
}

func TestBeginRecordsIntent(t *testing.T) {
	clk := newFakeClock()
	_, j := openJournal(t, t.TempDir(), clk)
	spec := testSpec()
	spec.DeadlineAt = clk.Now().Add(time.Hour)
	spec.Compensation = map[string]any{"policy": "kill-owned-session"}
	op := mustBegin(t, j, spec)

	if !strings.HasPrefix(op.ID, "op-") || len(op.ID) != 29 {
		t.Errorf("id %q is not op-<ULID>", op.ID)
	}
	if op.State != OperationPending || op.Attempt != 0 || op.SchemaVersion != OperationSchemaVersion {
		t.Errorf("unexpected initial row: %+v", op)
	}
	if !op.CreatedAt.Equal(clk.Now()) || !op.UpdatedAt.Equal(clk.Now()) || !op.DeadlineAt.Equal(spec.DeadlineAt) {
		t.Errorf("timestamps not from the injected clock: %+v", op)
	}
	if op.Kind != spec.Kind || op.SubjectType != spec.SubjectType || op.SubjectID != spec.SubjectID ||
		op.IdempotencyKey != spec.IdempotencyKey || op.Intent["session"] != "wd-agent-1" ||
		op.Compensation["policy"] != "kill-owned-session" {
		t.Errorf("intent not recorded: %+v", op)
	}
	got, err := j.Get(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != op.ID || got.IntentFingerprint != op.IntentFingerprint {
		t.Errorf("Get = %+v", got)
	}
	byKey, err := j.Lookup(spec.Kind, spec.IdempotencyKey)
	if err != nil || byKey.ID != op.ID {
		t.Errorf("Lookup = %v, %v", byKey.ID, err)
	}
	if _, err := j.Get("op-missing"); !errors.Is(err, ErrOperationNotFound) {
		t.Errorf("Get missing: %v", err)
	}
	if _, err := j.Lookup(spec.Kind, "missing"); !errors.Is(err, ErrOperationNotFound) {
		t.Errorf("Lookup missing: %v", err)
	}
}

func TestBeginIsIdempotent(t *testing.T) {
	clk := newFakeClock()
	_, j := openJournal(t, t.TempDir(), clk)
	spec := testSpec()
	first := mustBegin(t, j, spec)

	clk.Advance(time.Hour)
	again := spec
	again.Intent = map[string]any{"cols": 120, "session": "wd-agent-1"} // same intent, other key order
	again.DeadlineAt = clk.Now().Add(time.Hour)                         // not part of the intent
	second, created, err := j.Begin(context.Background(), again)
	if err != nil {
		t.Fatal(err)
	}
	if created || second.ID != first.ID {
		t.Fatalf("second Begin created=%v id=%s, want existing %s", created, second.ID, first.ID)
	}
	if !second.CreatedAt.Equal(first.CreatedAt) || second.AuditSeq != first.AuditSeq {
		t.Errorf("idempotent Begin rewrote the row: %+v", second)
	}
	all, err := j.Incomplete()
	if err != nil || len(all) != 1 {
		t.Fatalf("journal holds %d rows (%v), want 1", len(all), err)
	}

	// The key is unique per kind: the same key under another kind is new.
	other := spec
	other.Kind = "tmux.session.kill"
	if op := mustBegin(t, j, other); op.ID == first.ID {
		t.Error("another kind reused the operation")
	}

	// A terminal operation still owns its key.
	mustMove(t, j, first.ID, OperationCanceled, OperationUpdate{})
	done, created, err := j.Begin(context.Background(), spec)
	if err != nil || created || done.ID != first.ID || done.State != OperationCanceled {
		t.Errorf("Begin after terminal = %s created=%v err=%v", done.State, created, err)
	}
}

func TestBeginFingerprintConflict(t *testing.T) {
	_, j := openJournal(t, t.TempDir(), newFakeClock())
	spec := testSpec()
	first := mustBegin(t, j, spec)

	for name, mutate := range map[string]func(*OperationSpec){
		"intent":       func(s *OperationSpec) { s.Intent = map[string]any{"session": "other"} },
		"subject":      func(s *OperationSpec) { s.SubjectID = "agent-2" },
		"compensation": func(s *OperationSpec) { s.Compensation = map[string]any{"policy": "none"} },
		"depends_on":   func(s *OperationSpec) { s.DependsOn = []string{first.ID} },
	} {
		changed := spec
		mutate(&changed)
		_, created, err := j.Begin(context.Background(), changed)
		var ce *IdempotencyConflictError
		if !errors.As(err, &ce) || !errors.Is(err, ErrIdempotencyConflict) || created {
			t.Errorf("%s: err = %v, want *IdempotencyConflictError", name, err)
			continue
		}
		if ce.ExistingID != first.ID || ce.Existing != first.IntentFingerprint || ce.Got == ce.Existing ||
			ce.Kind != spec.Kind || ce.IdempotencyKey != spec.IdempotencyKey {
			t.Errorf("%s: conflict = %+v", name, ce)
		}
	}
	if all, _ := j.Incomplete(); len(all) != 1 {
		t.Errorf("a conflicting Begin wrote a row: %d rows", len(all))
	}
}

func TestBeginValidation(t *testing.T) {
	_, j := openJournal(t, t.TempDir(), newFakeClock())
	live := mustBegin(t, j, testSpec())

	bad := map[string]func(*OperationSpec){
		"no kind":         func(s *OperationSpec) { s.Kind = "" },
		"no subject type": func(s *OperationSpec) { s.SubjectType = "" },
		"no subject id":   func(s *OperationSpec) { s.SubjectID = "" },
		"no key":          func(s *OperationSpec) { s.IdempotencyKey = "" },
		"control char":    func(s *OperationSpec) { s.Kind = "a\nb" },
		"repeated dep":    func(s *OperationSpec) { s.DependsOn = []string{live.ID, live.ID} },
		"huge intent":     func(s *OperationSpec) { s.Intent = map[string]any{"blob": strings.Repeat("x", MaxOperationPayload)} },
		"unencodable":     func(s *OperationSpec) { s.Intent = map[string]any{"f": func() {}} },
		"live supersedes": func(s *OperationSpec) { s.Supersedes = live.ID },
	}
	for name, mutate := range bad {
		spec := testSpec()
		mutate(&spec)
		if _, _, err := j.Begin(context.Background(), spec); !errors.Is(err, ErrInvalidOperation) {
			t.Errorf("%s: err = %v, want ErrInvalidOperation", name, err)
		}
	}

	spec := testSpec()
	spec.DependsOn = []string{"op-missing"}
	var de *DependencyError
	if _, _, err := j.Begin(context.Background(), spec); !errors.As(err, &de) || de.DependsOn != "op-missing" {
		t.Errorf("missing dependency: %v", err)
	}
	spec = testSpec()
	spec.Supersedes = "op-missing"
	if _, _, err := j.Begin(context.Background(), spec); !errors.Is(err, ErrOperationNotFound) {
		t.Errorf("missing supersedes: %v", err)
	}

	// A terminal operation may be superseded by a successor.
	mustMove(t, j, live.ID, OperationFailed, OperationUpdate{Error: "boom"})
	spec = testSpec()
	spec.Supersedes = live.ID
	if next := mustBegin(t, j, spec); next.Supersedes != live.ID {
		t.Errorf("supersedes = %q", next.Supersedes)
	}
}

// TestEveryTransition drives a fresh operation to each state and attempts
// every target state from it: legal edges must apply, every other pair must
// fail with *TransitionError and leave the row untouched.
func TestEveryTransition(t *testing.T) {
	clk := newFakeClock()
	_, j := openJournal(t, t.TempDir(), clk)
	ctx := context.Background()

	for _, from := range OperationStates() {
		for _, to := range OperationStates() {
			t.Run(string(from)+"->"+string(to), func(t *testing.T) {
				op, lease := driveTo(t, j, clk, from)
				u := OperationUpdate{Lease: lease, Result: map[string]any{"ok": true}, NextAttemptAt: clk.Now().Add(time.Minute)}
				if from == OperationRetryWait {
					clk.Advance(2 * time.Minute) // the retry is due
				}

				// ready -> running is the claim: the only way into running.
				if from == OperationReady && to == OperationRunning {
					if _, err := j.Transition(ctx, op.ID, to, u); !errors.Is(err, ErrIllegalTransition) {
						t.Fatalf("Transition into running: %v, want ErrIllegalTransition", err)
					}
					l, got, err := j.Acquire(ctx, op.ID, "claimer", time.Minute)
					if err != nil {
						t.Fatal(err)
					}
					if got.State != OperationRunning || got.Attempt != op.Attempt+1 || l.Epoch != op.LeaseEpoch+1 {
						t.Fatalf("claim = %+v lease %+v", got, l)
					}
					return
				}

				got, err := j.Transition(ctx, op.ID, to, u)
				if from.CanTransition(to) {
					if err != nil {
						t.Fatalf("legal transition refused: %v", err)
					}
					if got.State != to {
						t.Fatalf("state = %s", got.State)
					}
					if to.Terminal() != !got.FinishedAt.IsZero() {
						t.Errorf("finished_at = %v for %s", got.FinishedAt, to)
					}
					if !to.Leased() && (got.LeaseOwner != "" || !got.LeaseExpiresAt.IsZero()) {
						t.Errorf("lease survived into %s: %+v", to, got)
					}
					if to.Leased() && got.LeaseOwner != lease.Owner {
						t.Errorf("lease lost entering %s", to)
					}
					if (to == OperationRetryWait) != !got.NextAttemptAt.IsZero() {
						t.Errorf("next_attempt_at = %v for %s", got.NextAttemptAt, to)
					}
					last := got.History[len(got.History)-1]
					if last.From != from || last.To != to || last.Event != OperationEventTransition {
						t.Errorf("history entry = %+v", last)
					}
					return
				}
				var te *TransitionError
				if !errors.As(err, &te) || !errors.Is(err, ErrIllegalTransition) {
					t.Fatalf("err = %v, want *TransitionError", err)
				}
				if te.ID != op.ID || te.From != from || te.To != to {
					t.Errorf("TransitionError = %+v", te)
				}
				after, _ := j.Get(op.ID)
				if after.State != from || after.AuditSeq != op.AuditSeq || !after.UpdatedAt.Equal(op.UpdatedAt) {
					t.Errorf("a refused transition changed the row: %+v", after)
				}
			})
		}
	}
}

func TestTransitionGuards(t *testing.T) {
	clk := newFakeClock()
	_, j := openJournal(t, t.TempDir(), clk)
	ctx := context.Background()

	if _, err := j.Transition(ctx, "op-missing", OperationReady, OperationUpdate{}); !errors.Is(err, ErrOperationNotFound) {
		t.Errorf("missing operation: %v", err)
	}
	op, _ := driveTo(t, j, clk, OperationPending)
	if _, err := j.Transition(ctx, op.ID, "bogus", OperationUpdate{}); !errors.Is(err, ErrInvalidOperation) {
		t.Errorf("unknown state: %v", err)
	}
	// A terminal row records a result or an error (spec §6 invariant 4).
	if _, err := j.Transition(ctx, op.ID, OperationCanceled, OperationUpdate{}); !errors.Is(err, ErrInvalidOperation) {
		t.Errorf("terminal without outcome: %v", err)
	}
	if _, err := j.Transition(ctx, op.ID, OperationReady, OperationUpdate{Result: map[string]any{"b": strings.Repeat("x", MaxOperationPayload)}}); !errors.Is(err, ErrInvalidOperation) {
		t.Errorf("oversized result: %v", err)
	}

	// retry_wait needs a due time and holds the row until then.
	op, lease := driveTo(t, j, clk, OperationRunning)
	if _, err := j.Transition(ctx, op.ID, OperationRetryWait, OperationUpdate{Lease: lease}); !errors.Is(err, ErrInvalidOperation) {
		t.Errorf("retry_wait without due time: %v", err)
	}
	due := clk.Now().Add(10 * time.Minute)
	waiting := mustMove(t, j, op.ID, OperationRetryWait, OperationUpdate{Lease: lease, NextAttemptAt: due, Error: "transient: api_key=hunter2secret"})
	if !waiting.NextAttemptAt.Equal(due) || strings.Contains(waiting.LastError, "hunter2secret") || !strings.Contains(waiting.LastError, "transient") {
		t.Errorf("retry row = %+v", waiting)
	}
	var te *TransitionError
	if _, err := j.Transition(ctx, op.ID, OperationReady, OperationUpdate{}); !errors.As(err, &te) || te.Reason == "" {
		t.Errorf("retry before due: %v", err)
	}
	clk.Advance(10 * time.Minute)
	ready := mustMove(t, j, op.ID, OperationReady, OperationUpdate{})
	if !ready.NextAttemptAt.IsZero() || ready.Attempt != 1 {
		t.Errorf("ready after retry = %+v", ready)
	}
	// The retry budget is durable: the next claim is attempt 2.
	if _, again, _ := j.Acquire(ctx, op.ID, "w", time.Minute); again.Attempt != 2 {
		t.Errorf("attempt = %d, want 2", again.Attempt)
	}

	// The observed fact is recorded with the outcome.
	op, lease = driveTo(t, j, clk, OperationRunning)
	seen := clk.Now().Add(-time.Second)
	done := mustMove(t, j, op.ID, OperationSucceeded, OperationUpdate{
		Lease: lease, Observation: map[string]any{"pane": "%3"}, ObservedAt: seen,
		ExternalRef: "tmux:default/wd-agent-1", Result: map[string]any{"session": "wd-agent-1"}, Reason: "observed",
	})
	if done.Observation["pane"] != "%3" || !done.ObservedAt.Equal(seen) || done.ExternalRef != "tmux:default/wd-agent-1" ||
		done.Result["session"] != "wd-agent-1" || !done.FinishedAt.Equal(clk.Now()) {
		t.Errorf("outcome = %+v", done)
	}
}

func TestDependenciesGateReady(t *testing.T) {
	clk := newFakeClock()
	_, j := openJournal(t, t.TempDir(), clk)
	ctx := context.Background()

	pred := mustBegin(t, j, testSpec())
	spec := testSpec()
	spec.Kind, spec.DependsOn = "cli.launch", []string{pred.ID}
	dep := mustBegin(t, j, spec)

	var de *DependencyError
	_, err := j.Transition(ctx, dep.ID, OperationReady, OperationUpdate{})
	if !errors.As(err, &de) || !errors.Is(err, ErrDependencyUnsatisfied) || de.DependsOn != pred.ID || de.State != OperationPending {
		t.Fatalf("pending predecessor: %v", err)
	}
	mustMove(t, j, pred.ID, OperationReady, OperationUpdate{})
	lease := mustAcquire(t, j, pred.ID, "w", time.Minute)
	mustMove(t, j, pred.ID, OperationSucceeded, OperationUpdate{Lease: &lease})
	mustMove(t, j, dep.ID, OperationReady, OperationUpdate{})

	// A failed predecessor blocks its dependent with a diagnostic.
	failed := mustBegin(t, j, testSpec())
	mustMove(t, j, failed.ID, OperationFailed, OperationUpdate{Error: "permission denied"})
	spec = testSpec()
	spec.DependsOn = []string{failed.ID}
	blocked := mustBegin(t, j, spec)
	if _, err := j.Transition(ctx, blocked.ID, OperationReady, OperationUpdate{}); !errors.As(err, &de) || de.State != OperationFailed {
		t.Fatalf("failed predecessor: %v", err)
	}
}

func TestProgressCheckpoints(t *testing.T) {
	clk := newFakeClock()
	_, j := openJournal(t, t.TempDir(), clk)
	ctx := context.Background()
	op, lease := driveTo(t, j, clk, OperationRunning)

	clk.Advance(time.Second)
	got, err := j.Progress(ctx, *lease, OperationProgress{
		Step: "create-session", Checkpoint: map[string]any{"issued": true},
		Observation: map[string]any{"exists": false}, ExternalRef: "tmux:default/wd",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Step != "create-session" || got.Checkpoint["issued"] != true || got.Observation["exists"] != false ||
		!got.ObservedAt.Equal(clk.Now()) || got.ExternalRef != "tmux:default/wd" || !got.UpdatedAt.Equal(clk.Now()) {
		t.Errorf("progress = %+v", got)
	}
	if got.State != OperationRunning || got.AuditSeq != op.AuditSeq {
		t.Errorf("progress changed state or history: %+v", got)
	}
	// A later step keeps what it does not set.
	got, err = j.Progress(ctx, *lease, OperationProgress{Step: "verify"})
	if err != nil || got.Step != "verify" || got.Checkpoint["issued"] != true {
		t.Errorf("second progress = %+v, %v", got, err)
	}
	if _, err := j.Progress(ctx, *lease, OperationProgress{Checkpoint: map[string]any{"b": strings.Repeat("x", MaxOperationPayload)}}); !errors.Is(err, ErrInvalidOperation) {
		t.Errorf("oversized checkpoint: %v", err)
	}

	// Progress is fenced, and a new attempt starts from a clean checkpoint.
	stale := *lease
	stale.Epoch++
	if _, err := j.Progress(ctx, stale, OperationProgress{Step: "x"}); !errors.Is(err, ErrLeaseFenced) {
		t.Errorf("stale progress: %v", err)
	}
	mustMove(t, j, op.ID, OperationRetryWait, OperationUpdate{Lease: lease, NextAttemptAt: clk.Now()})
	mustMove(t, j, op.ID, OperationReady, OperationUpdate{})
	if _, again, _ := j.Acquire(ctx, op.ID, "w2", time.Minute); again.Step != "" || again.Checkpoint != nil {
		t.Errorf("new attempt kept the old checkpoint: %+v", again)
	}
}

func TestLeaseAcquireRenewRelease(t *testing.T) {
	clk := newFakeClock()
	_, j := openJournal(t, t.TempDir(), clk)
	ctx := context.Background()

	for _, state := range []OperationState{OperationPending, OperationRetryWait, OperationSucceeded, OperationFailed} {
		op, _ := driveTo(t, j, clk, state)
		var le *LeaseError
		if _, _, err := j.Acquire(ctx, op.ID, "w", time.Minute); !errors.As(err, &le) || !errors.Is(err, ErrNotClaimable) || le.State != state {
			t.Errorf("Acquire in %s: %v", state, err)
		}
	}
	if _, _, err := j.Acquire(ctx, "op-missing", "w", time.Minute); !errors.Is(err, ErrOperationNotFound) {
		t.Errorf("Acquire missing: %v", err)
	}

	op, _ := driveTo(t, j, clk, OperationReady)
	for _, ttl := range []time.Duration{-time.Second, MaxLeaseTTL + time.Second} {
		if _, _, err := j.Acquire(ctx, op.ID, "w", ttl); !errors.Is(err, ErrInvalidOperation) {
			t.Errorf("ttl %s: %v", ttl, err)
		}
	}
	// An empty owner gets a random identity and a zero ttl the default.
	lease, claimed, err := j.Acquire(ctx, op.ID, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(lease.Owner, "lease-") || lease.Epoch != 1 || !lease.ExpiresAt.Equal(clk.Now().Add(DefaultLeaseTTL)) {
		t.Errorf("lease = %+v", lease)
	}
	if claimed.State != OperationRunning || claimed.Attempt != 1 || claimed.LeaseOwner != lease.Owner ||
		!claimed.LeaseExpiresAt.Equal(lease.ExpiresAt) || !claimed.LeaseLive(clk.Now()) {
		t.Errorf("claimed = %+v", claimed)
	}

	// A live lease excludes everyone, its own holder included.
	var le *LeaseError
	for _, owner := range []string{"intruder", lease.Owner} {
		_, _, err := j.Acquire(ctx, op.ID, owner, time.Minute)
		if !errors.As(err, &le) || !errors.Is(err, ErrLeaseHeld) || le.Holder != lease.Owner || le.HolderEpoch != 1 {
			t.Errorf("Acquire by %s on a live lease: %v", owner, err)
		}
	}

	clk.Advance(DefaultLeaseTTL - time.Second)
	renewed, err := j.Renew(ctx, lease, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if renewed.Epoch != lease.Epoch || !renewed.ExpiresAt.Equal(clk.Now().Add(10*time.Minute)) {
		t.Errorf("renewed = %+v", renewed)
	}
	clk.Advance(9 * time.Minute) // past the original expiry, inside the renewal
	if _, err := j.Progress(ctx, renewed, OperationProgress{Step: "still-mine"}); err != nil {
		t.Errorf("write under a renewed lease: %v", err)
	}
	if _, err := j.Renew(ctx, renewed, -time.Second); !errors.Is(err, ErrInvalidOperation) {
		t.Errorf("Renew with a bad ttl: %v", err)
	}

	// Release hands the row over at once; the releaser is fenced afterwards.
	if err := j.Release(ctx, renewed); err != nil {
		t.Fatal(err)
	}
	released, _ := j.Get(op.ID)
	if released.State != OperationRunning || released.LeaseOwner != "" || !released.LeaseExpiresAt.IsZero() || released.LeaseEpoch != 1 {
		t.Errorf("released = %+v", released)
	}
	if err := j.Release(ctx, renewed); !errors.Is(err, ErrLeaseFenced) {
		t.Errorf("double release: %v", err)
	}
	if _, err := j.Renew(ctx, renewed, time.Minute); !errors.Is(err, ErrLeaseFenced) {
		t.Errorf("renew after release: %v", err)
	}
	if _, err := j.Transition(ctx, op.ID, OperationSucceeded, OperationUpdate{Lease: &renewed, Result: map[string]any{"ok": true}}); !errors.Is(err, ErrLeaseFenced) {
		t.Errorf("write after release: %v", err)
	}
	next, taken, err := j.Acquire(ctx, op.ID, "successor", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if next.Epoch != 2 || taken.State != OperationReconciling || taken.Attempt != 1 {
		t.Errorf("takeover after release: lease %+v op %+v", next, taken)
	}
}

func TestLeaseExpiryTakeoverAndFencing(t *testing.T) {
	clk := newFakeClock()
	rec := &recorder{}
	_, j := openJournal(t, t.TempDir(), clk, observed(rec)...)
	ctx := context.Background()

	op, _ := driveTo(t, j, clk, OperationReady)
	old := mustAcquire(t, j, op.ID, "worker-a", time.Minute)

	// One tick before expiry the lease still excludes a taker.
	clk.Advance(time.Minute - time.Nanosecond)
	if _, _, err := j.Acquire(ctx, op.ID, "worker-b", time.Minute); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("takeover before expiry: %v", err)
	}
	clk.Advance(time.Nanosecond) // exactly at lease_expires_at: expired

	// The expired holder can no longer write, renew or report progress, even
	// though nobody has taken the row over yet.
	var le *LeaseError
	_, err := j.Transition(ctx, op.ID, OperationSucceeded, OperationUpdate{Lease: &old, Result: map[string]any{"ok": true}})
	if !errors.As(err, &le) || !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("write on an expired lease: %v", err)
	}
	if _, err := j.Renew(ctx, old, time.Minute); !errors.Is(err, ErrLeaseExpired) {
		t.Errorf("renew of an expired lease: %v", err)
	}
	if _, err := j.Progress(ctx, old, OperationProgress{Step: "late"}); !errors.Is(err, ErrLeaseExpired) {
		t.Errorf("progress on an expired lease: %v", err)
	}
	if rec.count(ActionOperationFenced, OperationsCollection) != 3 {
		t.Errorf("fenced counter = %d, want 3", rec.count(ActionOperationFenced, OperationsCollection))
	}
	if ev := rec.event(ActionOperationFenced); ev == nil || ev.Target != op.ID || ev.Detail["reason"] != "expired" || ev.Detail["lease_owner"] != "worker-a" {
		t.Errorf("fenced event = %+v", ev)
	}

	// Takeover: a new epoch, and a running row is reconciled, never rerun.
	fresh, taken, err := j.Acquire(ctx, op.ID, "worker-b", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Epoch != old.Epoch+1 || fresh.Owner != "worker-b" {
		t.Errorf("takeover lease = %+v", fresh)
	}
	if taken.State != OperationReconciling || taken.Attempt != 1 {
		t.Errorf("takeover left the row %s attempt %d, want reconciling attempt 1", taken.State, taken.Attempt)
	}
	last := taken.History[len(taken.History)-1]
	if last.Event != OperationEventTakeover || last.From != OperationRunning || last.To != OperationReconciling ||
		last.Owner != "worker-b" || !strings.Contains(last.Detail, "worker-a") {
		t.Errorf("takeover history = %+v", last)
	}

	// The stale owner is fenced from every write.
	stale := OperationUpdate{Lease: &old, Result: map[string]any{"ok": true}}
	if _, err := j.Transition(ctx, op.ID, OperationSucceeded, stale); !errors.As(err, &le) || !errors.Is(err, ErrLeaseFenced) {
		t.Fatalf("stale write: %v", err)
	}
	if le.Owner != "worker-a" || le.Epoch != old.Epoch || le.Holder != "worker-b" || le.HolderEpoch != fresh.Epoch {
		t.Errorf("LeaseError = %+v", le)
	}
	if _, err := j.Renew(ctx, old, time.Minute); !errors.Is(err, ErrLeaseFenced) {
		t.Errorf("stale renew: %v", err)
	}
	if err := j.Release(ctx, old); !errors.Is(err, ErrLeaseFenced) {
		t.Errorf("stale release: %v", err)
	}
	// The same owner name with an old epoch is just as stale.
	forged := fresh
	forged.Epoch = old.Epoch
	if _, err := j.Progress(ctx, forged, OperationProgress{Step: "x"}); !errors.Is(err, ErrLeaseFenced) {
		t.Errorf("old epoch under the holder's name: %v", err)
	}
	// A leased state takes no write without a lease.
	if _, err := j.Transition(ctx, op.ID, OperationSucceeded, OperationUpdate{Result: map[string]any{"ok": true}}); !errors.Is(err, ErrLeaseRequired) {
		t.Errorf("write without a lease: %v", err)
	}
	if got, _ := j.Get(op.ID); got.State != OperationReconciling || got.LeaseOwner != "worker-b" || got.LeaseEpoch != fresh.Epoch {
		t.Errorf("refused writes changed the row: %+v", got)
	}

	// The holder's write goes through.
	done := mustMove(t, j, op.ID, OperationSucceeded, OperationUpdate{Lease: &fresh})
	if done.LeaseOwner != "" || done.LeaseEpoch != fresh.Epoch {
		t.Errorf("done = %+v", done)
	}

	// An expired reconciling/compensating lease is taken over in place.
	for _, state := range []OperationState{OperationReconciling, OperationCompensating} {
		op, lease := driveTo(t, j, clk, state)
		clk.Advance(2 * time.Minute)
		l, got, err := j.Acquire(ctx, op.ID, "rescuer", time.Minute)
		if err != nil || got.State != state || l.Epoch != lease.Epoch+1 {
			t.Errorf("takeover in %s: lease %+v state %s err %v", state, l, got.State, err)
		}
	}
}

func TestAuditLinkage(t *testing.T) {
	clk := newFakeClock()
	rec := &recorder{}
	_, j := openJournal(t, t.TempDir(), clk, observed(rec)...)
	ctx := context.Background()

	op := mustBegin(t, j, testSpec())
	mustMove(t, j, op.ID, OperationReady, OperationUpdate{Reason: "prerequisites met"})
	lease := mustAcquire(t, j, op.ID, "worker", time.Minute)
	if _, err := j.Progress(ctx, lease, OperationProgress{Step: "s"}); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Renew(ctx, lease, time.Minute); err != nil {
		t.Fatal(err)
	}
	final := mustMove(t, j, op.ID, OperationFailed, OperationUpdate{Lease: &lease, Error: "denied: bearer abc.def.ghi"})

	// Every state change is one history entry and one audit event carrying
	// the same identifier; progress and renewals are not audited.
	want := []struct{ action, event, from, to string }{
		{ActionOperationBegin, OperationEventBegin, "", "pending"},
		{ActionOperationTransition, OperationEventTransition, "pending", "ready"},
		{ActionOperationLease, OperationEventClaim, "ready", "running"},
		{ActionOperationTransition, OperationEventTransition, "running", "failed"},
	}
	var events []audit.Event
	rec.mu.Lock()
	for _, ev := range rec.events {
		if strings.HasPrefix(ev.Action, "wardendb.operation.") {
			events = append(events, ev)
		}
	}
	rec.mu.Unlock()
	if len(events) != len(want) || len(final.History) != len(want) || final.AuditSeq != uint64(len(want)) {
		t.Fatalf("%d audit events, %d history entries, seq %d; want %d", len(events), len(final.History), final.AuditSeq, len(want))
	}
	ids := final.AuditIDs()
	for i, w := range want {
		ev, h := events[i], final.History[i]
		if ev.Action != w.action || ev.Target != op.ID || ev.Actor != "wardendb" || ev.Detail["event"] != w.event ||
			ev.Detail["from"] != w.from || ev.Detail["to"] != w.to || ev.Detail["kind"] != op.Kind ||
			ev.Detail["subject_id"] != op.SubjectID {
			t.Errorf("audit event %d = %+v", i, ev)
		}
		wantID := fmt.Sprintf("%s#%d", op.ID, i+1)
		if ev.Detail["audit_id"] != wantID || h.AuditID != wantID || ids[i] != wantID || h.Seq != uint64(i+1) {
			t.Errorf("entry %d: audit id %q, history %q, want %q", i, ev.Detail["audit_id"], h.AuditID, wantID)
		}
		if !ev.Time.Equal(h.At) {
			t.Errorf("entry %d: audit time %v, history %v", i, ev.Time, h.At)
		}
	}
	if events[2].Detail["lease_owner"] != "worker" || events[2].Detail["lease_epoch"] != "1" || events[2].Detail["attempt"] != "1" {
		t.Errorf("claim event = %+v", events[2])
	}
	if d := events[3].Detail["detail"]; strings.Contains(d, "abc.def.ghi") || !strings.Contains(d, "denied") {
		t.Errorf("audit detail not redacted: %q", d)
	}
	if strings.Contains(final.LastError, "abc.def.ghi") {
		t.Errorf("last_error not redacted: %q", final.LastError)
	}
	if rec.count(ActionOperationTransition, OperationsCollection) != 2 || rec.count(ActionOperationBegin, OperationsCollection) != 1 {
		t.Errorf("counters: transition %d begin %d", rec.count(ActionOperationTransition, OperationsCollection), rec.count(ActionOperationBegin, OperationsCollection))
	}

	// A refused change is not audited as a change.
	before := len(events)
	_, _ = j.Transition(ctx, op.ID, OperationReady, OperationUpdate{})
	_, _, _ = j.Begin(ctx, OperationSpec{Kind: op.Kind, SubjectType: "agent", SubjectID: "x", IdempotencyKey: op.IdempotencyKey})
	rec.mu.Lock()
	after := len(rec.events)
	rec.mu.Unlock()
	if n := rec.count(ActionOperationTransition, OperationsCollection) + rec.count(ActionOperationBegin, OperationsCollection); n != 3 || after < before {
		t.Errorf("refused changes were audited: %d", n)
	}
}

func TestHistoryIsBounded(t *testing.T) {
	clk := newFakeClock()
	_, j := openJournal(t, t.TempDir(), clk)
	op, _ := driveTo(t, j, clk, OperationReady)
	var last Operation
	for range MaxOperationHistory {
		lease := mustAcquire(t, j, op.ID, "w", time.Minute)
		mustMove(t, j, op.ID, OperationRetryWait, OperationUpdate{Lease: &lease, NextAttemptAt: clk.Now()})
		last = mustMove(t, j, op.ID, OperationReady, OperationUpdate{})
	}
	if len(last.History) != MaxOperationHistory || last.AuditSeq != uint64(2+3*MaxOperationHistory) {
		t.Fatalf("history %d entries, seq %d", len(last.History), last.AuditSeq)
	}
	if tail := last.History[len(last.History)-1]; tail.Seq != last.AuditSeq || last.History[0].Seq != last.AuditSeq-MaxOperationHistory+1 {
		t.Errorf("history window = %d..%d of %d", last.History[0].Seq, tail.Seq, last.AuditSeq)
	}
	if last.Attempt != MaxOperationHistory {
		t.Errorf("attempt = %d", last.Attempt)
	}
}

func TestJournalPersistsAcrossReopen(t *testing.T) {
	dataDir := t.TempDir()
	clk := newFakeClock()
	ctx := context.Background()

	db, j := openJournal(t, dataDir, clk)
	spec := testSpec()
	spec.Compensation = map[string]any{"policy": "kill"}
	op := mustBegin(t, j, spec)
	mustMove(t, j, op.ID, OperationReady, OperationUpdate{})
	lease := mustAcquire(t, j, op.ID, "worker", 10*time.Minute)
	before, err := j.Progress(ctx, lease, OperationProgress{Step: "issued", Checkpoint: map[string]any{"n": 3}, ExternalRef: "tmux:x"})
	if err != nil {
		t.Fatal(err)
	}
	done, _ := driveTo(t, j, clk, OperationSucceeded)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Get(op.ID); !errors.Is(err, ErrClosed) {
		t.Errorf("Get after Close: %v", err)
	}
	if _, _, err := j.Begin(ctx, testSpec()); !errors.Is(err, ErrClosed) {
		t.Errorf("Begin after Close: %v", err)
	}
	if _, err := j.Transition(ctx, op.ID, OperationFailed, OperationUpdate{Error: "x"}); !errors.Is(err, ErrClosed) {
		t.Errorf("Transition after Close: %v", err)
	}

	rec := &recorder{}
	_, j2 := openJournal(t, dataDir, clk, observed(rec)...)
	if rec.count(ActionRecovery, "") != 0 || rec.count(ActionIntegrityRefused, "") != 0 {
		t.Errorf("a clean shutdown was reported as recovery/integrity: %+v", rec.event(ActionRecovery))
	}
	got, err := j2.Get(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != OperationRunning || got.Attempt != 1 || got.Step != "issued" || got.Checkpoint["n"] != float64(3) ||
		got.ExternalRef != "tmux:x" || got.LeaseOwner != "worker" || got.LeaseEpoch != 1 ||
		!got.LeaseExpiresAt.Equal(before.LeaseExpiresAt) || !got.CreatedAt.Equal(before.CreatedAt) ||
		got.IntentFingerprint != before.IntentFingerprint || got.Compensation["policy"] != "kill" ||
		len(got.History) != len(before.History) || got.AuditSeq != before.AuditSeq {
		t.Errorf("reopened row = %+v\nbefore     = %+v", got, before)
	}
	if fin, err := j2.Get(done.ID); err != nil || fin.State != OperationSucceeded || !fin.FinishedAt.Equal(done.FinishedAt) {
		t.Errorf("reopened terminal row = %+v, %v", fin, err)
	}

	// Idempotency and its conflict detection survive the restart.
	again, created, err := j2.Begin(ctx, spec)
	if err != nil || created || again.ID != op.ID {
		t.Errorf("Begin after reopen = %s created=%v err=%v", again.ID, created, err)
	}
	spec.Intent = map[string]any{"session": "changed"}
	if _, _, err := j2.Begin(ctx, spec); !errors.Is(err, ErrIdempotencyConflict) {
		t.Errorf("conflict after reopen: %v", err)
	}

	// So does the lease: it still excludes others and still authorizes its
	// holder, and the fencing epoch keeps growing from where it was.
	if _, _, err := j2.Acquire(ctx, op.ID, "other", time.Minute); !errors.Is(err, ErrLeaseHeld) {
		t.Errorf("lease not held after reopen: %v", err)
	}
	if _, err := j2.Progress(ctx, lease, OperationProgress{Step: "resumed"}); err != nil {
		t.Errorf("holder refused after reopen: %v", err)
	}
	clk.Advance(11 * time.Minute)
	l, taken, err := j2.Acquire(ctx, op.ID, "other", time.Minute)
	if err != nil || l.Epoch != 2 || taken.State != OperationReconciling {
		t.Errorf("takeover after reopen: lease %+v state %s err %v", l, taken.State, err)
	}
}

func TestRecoveryQueriesAfterReopen(t *testing.T) {
	dataDir := t.TempDir()
	clk := newFakeClock()
	db, j := openJournal(t, dataDir, clk)

	ids := map[string]string{}
	add := func(name string, state OperationState) *Lease {
		op, lease := driveTo(t, j, clk, state)
		ids[name] = op.ID
		return lease
	}
	add("pending", OperationPending)
	add("ready", OperationReady)
	add("running-live", OperationRunning)
	add("reconciling-live", OperationReconciling)
	add("compensating-live", OperationCompensating)
	add("retry-later", OperationRetryWait) // due in one minute
	for _, s := range []OperationState{OperationSucceeded, OperationFailed, OperationCanceled, OperationCompensated, OperationManualIntervention} {
		add(string(s), s)
	}
	// The leases above run for a minute; these three are made to lapse.
	clk.Advance(-time.Hour)
	add("running-expired", OperationRunning)
	add("reconciling-expired", OperationReconciling)
	add("compensating-expired", OperationCompensating)
	add("retry-due", OperationRetryWait)
	released := add("running-released", OperationRunning)
	if err := j.Release(context.Background(), *released); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Hour)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	_, j2 := openJournal(t, dataDir, clk)
	check := func(query string, got []Operation, err error, want ...string) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		wantIDs := map[string]string{}
		for _, name := range want {
			wantIDs[ids[name]] = name
		}
		for i, op := range got {
			if _, ok := wantIDs[op.ID]; !ok {
				for name, id := range ids {
					if id == op.ID {
						t.Errorf("%s returned %s (%s)", query, name, op.State)
					}
				}
			}
			delete(wantIDs, op.ID)
			if i > 0 && got[i-1].ID >= op.ID {
				t.Errorf("%s is not ordered by id", query)
			}
		}
		for _, name := range wantIDs {
			t.Errorf("%s missed %s", query, name)
		}
	}

	inc, err := j2.Incomplete()
	check("Incomplete", inc, err,
		"pending", "ready", "running-live", "reconciling-live", "compensating-live", "retry-later",
		"running-expired", "reconciling-expired", "compensating-expired", "retry-due", "running-released")
	exp, err := j2.LeaseExpired()
	check("LeaseExpired", exp, err, "running-expired", "reconciling-expired", "compensating-expired", "running-released")
	rcn, err := j2.NeedsReconciliation()
	check("NeedsReconciliation", rcn, err, "running-expired", "running-released", "reconciling-live", "reconciling-expired")
	due, err := j2.Due()
	check("Due", due, err, "ready", "retry-due")
	term, err := j2.ListByState(OperationSucceeded, OperationFailed)
	check("ListByState", term, err, "succeeded", "failed")

	// The queries follow the clock: a live lease lapses, a retry falls due.
	clk.Advance(2 * time.Minute)
	exp, err = j2.LeaseExpired()
	check("LeaseExpired later", exp, err, "running-expired", "reconciling-expired", "compensating-expired",
		"running-released", "running-live", "reconciling-live", "compensating-live")
	due, err = j2.Due()
	check("Due later", due, err, "ready", "retry-due", "retry-later")

	// Recovery acts on what it found: the expired running row is claimed
	// into reconciling and leaves the lease-expired set.
	if _, got, err := j2.Acquire(context.Background(), ids["running-expired"], "reconciler", time.Hour); err != nil || got.State != OperationReconciling {
		t.Fatalf("recovery claim: %v %s", err, got.State)
	}
	exp, _ = j2.LeaseExpired()
	for _, op := range exp {
		if op.ID == ids["running-expired"] {
			t.Error("a re-leased row is still reported as lease-expired")
		}
	}
}

// TestStageCommitsWithJournalChange covers spec §1.1: the caller's internal
// change and the journal change are one commit.
func TestStageCommitsWithJournalChange(t *testing.T) {
	clk := newFakeClock()
	db, j := openJournal(t, t.TempDir(), clk)
	ctx := context.Background()
	if _, err := Register[agentRec](db, Spec{Name: "agents", Version: 1}); err != nil {
		t.Fatal(err)
	}
	agents, err := dbtx.Bind(j.Boundary(), "agents", dbtx.MapCodec{})
	if err != nil {
		t.Fatal(err)
	}
	count := func() uint64 {
		t.Helper()
		col, err := Lookup[agentRec](db, "agents")
		if err != nil {
			t.Fatal(err)
		}
		eng, _ := col.Engine()
		n, err := eng.Count(nil)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	reserve := func(_ context.Context, tx *dbtx.Tx) error {
		_, err := agents.Insert(tx, map[string]any{"name": "agent-1", "status": "starting"})
		return err
	}

	spec := testSpec()
	op, created, err := j.Begin(ctx, spec, reserve)
	if err != nil || !created || count() != 1 {
		t.Fatalf("Begin with a stage: created=%v agents=%d err=%v", created, count(), err)
	}
	// An idempotent replay stages nothing.
	if _, created, err := j.Begin(ctx, spec, reserve); err != nil || created || count() != 1 {
		t.Fatalf("replayed Begin: created=%v agents=%d err=%v", created, count(), err)
	}
	// A failing stage rolls the journal change back with it.
	boom := errors.New("validation failed")
	failing := func(context.Context, *dbtx.Tx) error { return boom }
	failed := testSpec()
	if _, _, err := j.Begin(ctx, failed, reserve, failing); !errors.Is(err, boom) {
		t.Fatalf("Begin with a failing stage: %v", err)
	}
	if _, err := j.Lookup(failed.Kind, failed.IdempotencyKey); !errors.Is(err, ErrOperationNotFound) || count() != 1 {
		t.Errorf("a failed Begin left state behind: %v, agents=%d", err, count())
	}
	if _, err := j.Transition(ctx, op.ID, OperationReady, OperationUpdate{}, reserve, failing); !errors.Is(err, boom) {
		t.Fatalf("Transition with a failing stage: %v", err)
	}
	if got, _ := j.Get(op.ID); got.State != OperationPending || count() != 1 {
		t.Errorf("a failed Transition left state behind: %s, agents=%d", got.State, count())
	}
	// And a passing one commits both.
	if _, err := j.Transition(ctx, op.ID, OperationReady, OperationUpdate{}, reserve); err != nil || count() != 2 {
		t.Errorf("Transition with a stage: agents=%d err=%v", count(), err)
	}
}

func TestOperationsCollectionIsStrictAndRegisteredOnce(t *testing.T) {
	db, j := openJournal(t, t.TempDir(), newFakeClock())
	infos, err := db.Registered()
	if err != nil || len(infos) != 1 || infos[0].Name != OperationsCollection || infos[0].Version != OperationSchemaVersion {
		t.Fatalf("Registered = %+v, %v", infos, err)
	}
	eng, err := j.col.Engine()
	if err != nil {
		t.Fatal(err)
	}
	if got := eng.Config().SyncMode; got != SyncMode {
		t.Errorf("operations sync mode = %v, want %v", got, SyncMode)
	}
	have := map[string]bool{}
	for _, f := range eng.ListIndexes() {
		have[f] = true
	}
	for _, f := range append([]string{"id", "idempotency_ref"}, operationIndexes...) {
		if !have[f] {
			t.Errorf("operations has no index on %q", f)
		}
	}
	if _, err := OpenJournal(db); !errors.Is(err, ErrDuplicateCollection) {
		t.Errorf("second OpenJournal: %v", err)
	}
}

func TestOperationSchemaVersionIsChecked(t *testing.T) {
	_, j := openJournal(t, t.TempDir(), newFakeClock())
	op := mustBegin(t, j, testSpec())
	doc, err := operationCodec{}.Encode(op)
	if err != nil {
		t.Fatal(err)
	}
	if doc["schema_version"] != float64(OperationSchemaVersion) || doc["state"] != "pending" || doc["idempotency_key"] != op.IdempotencyKey {
		t.Errorf("stored document = %v", doc)
	}
	for _, absent := range []string{"finished_at", "next_attempt_at", "lease_expires_at", "lease_owner", "deadline_at"} {
		if _, ok := doc[absent]; ok {
			t.Errorf("unset field %q is stored", absent)
		}
	}
	doc["schema_version"] = float64(OperationSchemaVersion + 1)
	if _, err := (operationCodec{}).Decode(doc); !errors.Is(err, ErrOperationSchema) {
		t.Errorf("newer schema version: %v", err)
	}
}

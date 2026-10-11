package dbtx

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
)

// fakeEngine wraps the real engine so individual failure classes can be
// injected; with no hooks it is a pass-through.
type fakeEngine struct {
	Engine
	commit func(real Engine, key string, ops []engine.XTxOp) (*engine.XTxResult, error)
	status func(real Engine, k string) (engine.XTxStatus, string)
	calls  atomic.Int32
}

func (f *fakeEngine) CommitXTx(key string, ops []engine.XTxOp) (*engine.XTxResult, error) {
	f.calls.Add(1)
	if f.commit != nil {
		return f.commit(f.Engine, key, ops)
	}
	return f.Engine.CommitXTx(key, ops)
}

func (f *fakeEngine) XTxStatus(k string) (engine.XTxStatus, string) {
	if f.status != nil {
		return f.status(f.Engine, k)
	}
	return f.Engine.XTxStatus(k)
}

type env struct {
	dir  string
	db   *scriva.DB
	eng  *fakeEngine
	b    *Boundary
	a, c *Coll[map[string]any]
}

func openDB(t *testing.T, dir string) *scriva.DB {
	t.Helper()
	db, err := scriva.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func newEnv(t *testing.T, opts ...Option) *env {
	t.Helper()
	e := &env{dir: t.TempDir()}
	e.db = openDB(t, e.dir)
	t.Cleanup(func() { e.db.Close() })
	e.mount(t, opts...)
	return e
}

func (e *env) mount(t *testing.T, opts ...Option) {
	t.Helper()
	// "a" has a unique index on field k.
	e.db.MustCollection("a", scriva.WithUniqueIndex("k"))
	e.db.MustCollection("c")
	e.eng = &fakeEngine{Engine: FromEngine(e.db.Engine())}
	var err error
	opts = append([]Option{WithBackoff(func(int) time.Duration { return time.Millisecond }),
		WithResolveWait(50 * time.Millisecond), WithDeadlineGrace(30 * time.Millisecond)}, opts...)
	if e.b, err = NewWithEngine(e.eng, opts...); err != nil {
		t.Fatal(err)
	}
	if e.a, err = Bind[map[string]any](e.b, "a", MapCodec{}); err != nil {
		t.Fatal(err)
	}
	if e.c, err = Bind[map[string]any](e.b, "c", MapCodec{}); err != nil {
		t.Fatal(err)
	}
}

func doc(k string, n int) map[string]any { return map[string]any{"k": k, "n": n} }

// seed commits one record into each of a and c, returning ids and revs.
func (e *env) seed(t *testing.T, intent string) (aid, cid uint64) {
	t.Helper()
	tx, _ := e.b.Begin(context.Background())
	aid, _ = e.a.Insert(tx, doc("seed", 0))
	cid, _ = e.c.Insert(tx, doc("seedc", 0))
	if _, err := tx.Commit(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	return
}

func wantErr(t *testing.T, err error, target error, class Class) *Error {
	t.Helper()
	var be *Error
	if !errors.As(err, &be) {
		t.Fatalf("want *Error, got %T %v", err, err)
	}
	if target != nil && !errors.Is(err, target) {
		t.Fatalf("want %v, got %v", target, err)
	}
	if be.Class != class {
		t.Fatalf("class = %s, want %s (%v)", be.Class, class, err)
	}
	return be
}

func TestMultiCollectionCommitAtomicSingleCall(t *testing.T) { // G2
	e := newEnv(t)
	tx, _ := e.b.Begin(context.Background())
	aid, err := e.a.Insert(tx, doc("x", 1))
	if err != nil {
		t.Fatal(err)
	}
	cid, _ := e.c.Insert(tx, doc("y", 2))
	if _, err := e.a.Get(aid); err == nil {
		t.Fatal("staged insert visible before commit")
	}
	res, err := tx.Commit(context.Background(), "op1/step1")
	if err != nil {
		t.Fatal(err)
	}
	if e.eng.calls.Load() != 1 {
		t.Fatalf("CommitXTx calls = %d", e.eng.calls.Load())
	}
	if !res.RevsAuthoritative || len(res.Ops) != 2 {
		t.Fatalf("bad result %+v", res)
	}
	if v, err := e.a.Get(aid); err != nil || v.Value["k"] != "x" || v.Rev == 0 {
		t.Fatalf("a: %+v %v", v, err)
	}
	if v, err := e.c.Get(cid); err != nil || v.Value["k"] != "y" {
		t.Fatalf("c: %+v %v", v, err)
	}
}

func TestRollbackLeavesNothing(t *testing.T) {
	e := newEnv(t)
	tx, _ := e.b.Begin(context.Background())
	aid, _ := e.a.Insert(tx, doc("x", 1))
	_ = tx.Rollback()
	if e.eng.calls.Load() != 0 {
		t.Fatal("engine called on rollback")
	}
	if _, err := e.a.Get(aid); err == nil {
		t.Fatal("rolled-back insert exists")
	}
	if _, err := e.c.Insert(tx, doc("z", 1)); !errors.Is(err, ErrTxFinished) {
		t.Fatalf("stage after rollback: %v", err)
	}
	_, err := tx.Commit(context.Background(), "i")
	wantErr(t, err, ErrTxFinished, ClassInternal)
}

func TestUseAfterCommitNestedEmptyKey(t *testing.T) { // G2
	e := newEnv(t)
	ctx := context.Background()
	tx, _ := e.b.Begin(ctx)
	e.a.Insert(tx, doc("x", 1))
	if _, err := tx.Begin(); !errors.Is(err, ErrNestedTx) {
		t.Fatalf("tx.Begin: %v", err)
	}
	if _, err := e.b.Begin(tx.Context()); !errors.Is(err, ErrNestedTx) {
		t.Fatalf("nested Begin: %v", err)
	}
	if _, err := tx.Commit(ctx, ""); !errors.Is(err, ErrKeyRequired) {
		t.Fatalf("empty intent: %v", err)
	}
	// Failed commit still finishes the tx.
	if _, err := tx.Commit(ctx, "i"); !errors.Is(err, ErrTxFinished) {
		t.Fatalf("recommit: %v", err)
	}
	// After finish, Begin from the old ctx is allowed again.
	tx2, err := e.b.Begin(tx.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx2.Commit(ctx, "i"); !errors.Is(err, ErrEmptyTx) {
		t.Fatalf("empty commit: %v", err)
	}
	tx3, _ := e.b.Begin(ctx)
	e.a.Insert(tx3, doc("ok", 1))
	if _, err := tx3.Commit(ctx, "i3"); err != nil {
		t.Fatal(err)
	}
	if err := e.a.Update(tx3, 1, nil, 0); !errors.Is(err, ErrTxFinished) {
		t.Fatalf("update after commit: %v", err)
	}
	if e.eng.calls.Load() != 1 {
		t.Fatalf("calls=%d", e.eng.calls.Load())
	}
}

func TestDuplicateAndUndeclared(t *testing.T) { // G3, G8
	e := newEnv(t)
	aid, _ := e.seed(t, "seed")
	tx, _ := e.b.Begin(context.Background())
	if err := e.a.Update(tx, aid, doc("u", 1), 0); err != nil {
		t.Fatal(err)
	}
	if err := e.a.Delete(tx, aid, 0); !errors.Is(err, ErrDuplicateOp) {
		t.Fatalf("dup: %v", err)
	}
	if err := e.a.Update(tx, 0, doc("u", 1), 0); !errors.Is(err, ErrInvalidOp) {
		t.Fatalf("zero id: %v", err)
	}
	other, _ := NewWithEngine(e.eng)
	foreign, _ := Bind[map[string]any](other, "a", MapCodec{})
	if err := foreign.Update(tx, aid, doc("u", 1), 0); !errors.Is(err, ErrUndeclared) {
		t.Fatalf("foreign handle: %v", err)
	}
	_, err := Bind[map[string]any](e.b, "nope", MapCodec{})
	wantErr(t, err, engine.ErrCollectionNotFound, ClassCollectionNotFound)
}

func TestInsertIDsComeFromReserveID(t *testing.T) { // G3
	e := newEnv(t)
	tx, _ := e.b.Begin(context.Background())
	id1, _ := e.a.Insert(tx, doc("1", 1))
	id2, _ := e.a.Insert(tx, doc("2", 1))
	if id1 == 0 || id2 <= id1 {
		t.Fatalf("ids %d %d", id1, id2)
	}
}

func TestConflictExpectedRevAndUnique(t *testing.T) { // G6
	e := newEnv(t)
	aid, _ := e.seed(t, "seed")
	cur, _ := e.a.Get(aid)
	ctx := context.Background()

	tx, _ := e.b.Begin(ctx)
	e.a.Update(tx, aid, doc("seed", 9), cur.Rev+7)
	_, err := tx.Commit(ctx, "rev")
	be := wantErr(t, err, engine.ErrXTxConflict, ClassConflict)
	if !be.SafeToRetry {
		t.Fatal("conflict must be retry-safe")
	}
	if v, _ := e.a.Get(aid); v.Value["n"].(float64) != 0 {
		t.Fatal("conflicting tx wrote")
	}

	tx, _ = e.b.Begin(ctx)
	e.a.Insert(tx, doc("seed", 5)) // duplicate unique k
	e.c.Insert(tx, doc("never", 5))
	_, err = tx.Commit(ctx, "uniq")
	wantErr(t, err, engine.ErrDuplicateKey, ClassConflict)
	// Atomic: the c participant did not land either.
	tx, _ = e.b.Begin(ctx)
	e.c.Insert(tx, doc("probe", 1))
	if _, err := tx.Commit(ctx, "probe"); err != nil {
		t.Fatal(err)
	}
}

func TestLimitsPreflight(t *testing.T) { // G9
	e := newEnv(t)
	ctx := context.Background()
	tx, _ := e.b.Begin(ctx)
	for i := 0; i < MaxOps+1; i++ {
		if _, err := e.a.Insert(tx, doc(fmt.Sprint(i), i)); err != nil {
			t.Fatal(err)
		}
	}
	_, err := tx.Commit(ctx, "big")
	be := wantErr(t, err, ErrTooLarge, ClassTooLarge)
	if be.SafeToRetry {
		t.Fatal("too-large must not retry")
	}

	// 17 participants
	var hs []*Coll[map[string]any]
	for i := 0; i < MaxParticipants+1; i++ {
		name := fmt.Sprintf("p%d", i)
		e.db.MustCollection(name)
		h, err := Bind[map[string]any](e.b, name, MapCodec{})
		if err != nil {
			t.Fatal(err)
		}
		hs = append(hs, h)
	}
	tx, _ = e.b.Begin(ctx)
	for _, h := range hs {
		h.Insert(tx, doc("x", 1))
	}
	_, err = tx.Commit(ctx, "wide")
	wantErr(t, err, ErrTooLarge, ClassTooLarge)

	// key > 128 bytes
	tx, _ = e.b.Begin(ctx)
	e.a.Insert(tx, doc("x", 1))
	long := make([]byte, 120)
	for i := range long {
		long[i] = 'k'
	}
	_, err = tx.Commit(ctx, string(long))
	wantErr(t, err, ErrTooLarge, ClassTooLarge)

	if e.eng.calls.Load() != 0 {
		t.Fatalf("engine called %d times for over-limit tx", e.eng.calls.Load())
	}
	// exactly at the op limit is accepted
	tx, _ = e.b.Begin(ctx)
	for i := 0; i < MaxOps; i++ {
		e.a.Insert(tx, doc(fmt.Sprint("ok", i), i))
	}
	if _, err := tx.Commit(ctx, "atlimit"); err != nil {
		t.Fatal(err)
	}
}

func TestIdempotentReplayAndKeyPerPayload(t *testing.T) { // G5
	e := newEnv(t)
	aid, _ := e.seed(t, "seed")
	ctx := context.Background()
	commit := func(n int) (*Result, error) {
		tx, _ := e.b.Begin(ctx)
		e.a.Update(tx, aid, doc("seed", n), 0)
		return tx.Commit(ctx, "op/upd")
	}
	r1, err := commit(1)
	if err != nil {
		t.Fatal(err)
	}
	before := e.eng.calls.Load()
	r2, err := commit(1) // identical op set → replay, no engine call
	if err != nil {
		t.Fatal(err)
	}
	if e.eng.calls.Load() != before || !r2.Replayed || r2.RevsAuthoritative || r2.TxID != r1.TxID || r2.Ops[0].Rev != 0 {
		t.Fatalf("replay: %+v calls %d→%d", r2, before, e.eng.calls.Load())
	}
	r3, err := commit(2) // same intent, different payload → distinct key
	if err != nil || r3.Replayed || r3.Key == r1.Key {
		t.Fatalf("payload change: %+v %v", r3, err)
	}
	if v, _ := e.a.Get(aid); v.Value["n"].(float64) != 2 {
		t.Fatal("second payload not applied")
	}
}

func TestOutcomeUnknownCommittedIsSuccessViaStatus(t *testing.T) { // G4
	e := newEnv(t)
	e.eng.commit = func(real Engine, k string, ops []engine.XTxOp) (*engine.XTxResult, error) {
		if _, err := real.CommitXTx(k, ops); err != nil {
			return nil, err
		}
		return nil, &engine.XTxError{Err: engine.ErrXTxOutcomeUnknown}
	}
	ctx := context.Background()
	tx, _ := e.b.Begin(ctx)
	id, _ := e.a.Insert(tx, doc("x", 1))
	res, err := tx.Commit(ctx, "ou")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Replayed || res.RevsAuthoritative || res.TxID == "" {
		t.Fatalf("%+v", res)
	}
	if v, err := e.a.Get(id); err != nil || v.Rev == 0 {
		t.Fatalf("re-read: %v", err)
	}
}

func TestOutcomeUnknownNotApplied(t *testing.T) { // G4
	e := newEnv(t)
	var n atomic.Int32
	e.eng.commit = func(real Engine, k string, ops []engine.XTxOp) (*engine.XTxResult, error) {
		if n.Add(1) == 1 {
			return nil, &engine.XTxError{Err: engine.ErrXTxOutcomeUnknown} // nothing applied
		}
		return real.CommitXTx(k, ops)
	}
	ctx := context.Background()
	tx, _ := e.b.Begin(ctx)
	e.a.Insert(tx, doc("x", 1))
	_, err := tx.Commit(ctx, "ou2")
	be := wantErr(t, err, engine.ErrXTxOutcomeUnknown, ClassOutcomeUnknown)
	if be.Outcome != OutcomeUnknown || !be.SafeToRetry {
		t.Fatalf("%+v", be)
	}

	// Run rebuilds (fresh read + restage) and the retry commits.
	n.Store(0)
	builds := 0
	res, err := e.b.Run(ctx, "ou3", func(ctx context.Context, tx *Tx) error {
		builds++
		_, err := e.a.Insert(tx, doc("y", 1))
		return err
	})
	if err != nil || builds != 2 || !res.RevsAuthoritative {
		t.Fatalf("builds=%d res=%+v err=%v", builds, res, err)
	}
}

func TestOutcomePendingNeverSuccess(t *testing.T) { // G4
	e := newEnv(t)
	e.eng.commit = func(Engine, string, []engine.XTxOp) (*engine.XTxResult, error) {
		return nil, &engine.XTxError{Err: engine.ErrXTxOutcomeUnknown}
	}
	e.eng.status = func(real Engine, k string) (engine.XTxStatus, string) { return engine.XTxPending, "" }
	ctx := context.Background()
	res, err := e.b.Run(ctx, "pend", func(ctx context.Context, tx *Tx) error {
		_, err := e.a.Insert(tx, doc("x", 1))
		return err
	})
	if res != nil {
		t.Fatal("success reported without confirmation")
	}
	be := wantErr(t, err, nil, ClassOutcomeUnknown)
	if be.Outcome != OutcomePending || be.SafeToRetry || e.eng.calls.Load() != 1 {
		t.Fatalf("%+v calls=%d", be, e.eng.calls.Load())
	}
}

func TestRetryWithRebuildOnConflict(t *testing.T) { // G6
	e := newEnv(t)
	aid, _ := e.seed(t, "seed")
	ctx := context.Background()
	builds := 0
	res, err := e.b.Run(ctx, "bump", func(ctx context.Context, tx *Tx) error {
		builds++
		cur, err := e.a.Get(aid)
		if err != nil {
			return err
		}
		if builds == 1 { // concurrent writer sneaks in after our read
			bg := context.Background() // separate goroutine/context in real life
			w, _ := e.b.Begin(bg)
			e.a.Update(w, aid, doc("seed", 100), 0)
			if _, err := w.Commit(bg, "intruder"); err != nil {
				t.Fatal(err)
			}
		}
		n := int(cur.Value["n"].(float64))
		return e.a.Update(tx, aid, doc("seed", n+1), cur.Rev)
	})
	if err != nil || builds != 2 || res == nil {
		t.Fatalf("builds=%d err=%v", builds, err)
	}
	if v, _ := e.a.Get(aid); v.Value["n"].(float64) != 101 {
		t.Fatalf("lost update: %v", v.Value)
	}
}

func TestRetryBoundedAndNonRetryableNotRetried(t *testing.T) { // G6
	e := newEnv(t, WithMaxAttempts(3))
	e.eng.commit = func(Engine, string, []engine.XTxOp) (*engine.XTxResult, error) {
		return nil, &engine.XTxError{Err: engine.ErrXTxConflict}
	}
	ctx := context.Background()
	builds := 0
	_, err := e.b.Run(ctx, "b", func(ctx context.Context, tx *Tx) error {
		builds++
		_, err := e.a.Insert(tx, doc("x", 1))
		return err
	})
	be := wantErr(t, err, engine.ErrXTxConflict, ClassConflict)
	if builds != 3 || be.Attempts != 3 {
		t.Fatalf("builds=%d attempts=%d", builds, be.Attempts)
	}

	for name, raw := range map[string]error{
		"untyped":    errors.New("engine: mystery"),
		"durability": &engine.XTxError{Err: engine.ErrXTxDurability},
		"toolarge":   &engine.XTxError{Err: engine.ErrXTxTooLarge},
		"readonly":   &engine.XTxError{Err: engine.ErrReadOnly},
	} {
		raw := raw
		e.eng.commit = func(Engine, string, []engine.XTxOp) (*engine.XTxResult, error) { return nil, raw }
		builds = 0
		_, err := e.b.Run(ctx, "n", func(ctx context.Context, tx *Tx) error {
			builds++
			_, err := e.a.Insert(tx, doc("x", 1))
			return err
		})
		var be *Error
		if !errors.As(err, &be) {
			t.Fatalf("%s: %v", name, err)
		}
		wantBuilds := 1
		if name == "durability" {
			wantBuilds = 3 // retry-safe per engine, bounded
		}
		if builds != wantBuilds {
			t.Fatalf("%s: builds=%d", name, builds)
		}
		if name == "untyped" && be.Class != ClassInternal {
			t.Fatalf("untyped class %s", be.Class)
		}
		if name == "readonly" && !errors.Is(err, ErrAtomicUnavailable) {
			t.Fatalf("readonly: %v", err)
		}
	}
}

func TestAtomicUnavailableNoFallback(t *testing.T) { // hard constraint
	e := newEnv(t)
	e.eng.commit = func(Engine, string, []engine.XTxOp) (*engine.XTxResult, error) {
		return nil, &engine.XTxError{Err: engine.ErrXTxUnsupported}
	}
	ctx := context.Background()
	tx, _ := e.b.Begin(ctx)
	id, _ := e.a.Insert(tx, doc("x", 1))
	_, err := tx.Commit(ctx, "na")
	wantErr(t, err, ErrAtomicUnavailable, ClassInternal)
	if _, gerr := e.a.Get(id); gerr == nil {
		t.Fatal("partial/sequential write leaked")
	}
	if e.eng.calls.Load() != 1 {
		t.Fatal("fallback attempted")
	}
}

func TestDeadlineResolvesBeforeReporting(t *testing.T) { // G7
	e := newEnv(t)
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	e.eng.commit = func(real Engine, k string, ops []engine.XTxOp) (*engine.XTxResult, error) {
		defer wg.Done()
		<-release
		return real.CommitXTx(k, ops)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	tx, _ := e.b.Begin(ctx)
	id, _ := e.a.Insert(tx, doc("x", 1))
	_, err := tx.Commit(ctx, "dl")
	be := wantErr(t, err, ErrCommitDeadline, ClassOutcomeUnknown)
	if be.SafeToRetry || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%+v", be)
	}
	// The abandoned goroutine commits afterwards: that is exactly why the
	// deadline error is "unknown", and status now says COMMITTED.
	close(release)
	wg.Wait()
	deadline := time.Now().Add(time.Second)
	for {
		if o, _ := e.b.Outcome(be.Key); o == OutcomeCommitted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("status never COMMITTED")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := e.a.Get(id); err != nil {
		t.Fatal(err)
	}
}

func TestDeadlineGraceFinishes(t *testing.T) { // G7
	e := newEnv(t, WithDeadlineGrace(500*time.Millisecond))
	e.eng.commit = func(real Engine, k string, ops []engine.XTxOp) (*engine.XTxResult, error) {
		time.Sleep(40 * time.Millisecond)
		return real.CommitXTx(k, ops)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	tx, _ := e.b.Begin(ctx)
	e.a.Insert(tx, doc("x", 1))
	if res, err := tx.Commit(ctx, "dl2"); err != nil || !res.RevsAuthoritative {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestStagingAgeAndCancelledCtx(t *testing.T) { // G7
	e := newEnv(t, WithMaxStagingAge(time.Millisecond))
	ctx := context.Background()
	tx, _ := e.b.Begin(ctx)
	id, _ := e.a.Insert(tx, doc("x", 1))
	time.Sleep(10 * time.Millisecond)
	_, err := tx.Commit(ctx, "age")
	wantErr(t, err, ErrStagingExpired, ClassInternal)
	if _, gerr := e.a.Get(id); gerr == nil || e.eng.calls.Load() != 0 {
		t.Fatal("expired staging reached the engine")
	}

	e2 := newEnv(t)
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	tx, _ = e2.b.Begin(ctx)
	e2.a.Insert(tx, doc("x", 1))
	if _, err := tx.Commit(cctx, "c"); err == nil || e2.eng.calls.Load() != 0 {
		t.Fatalf("cancelled ctx: %v", err)
	}
}

func TestReopenShowsAllParticipants(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tx, _ := e.b.Begin(ctx)
	aid, _ := e.a.Insert(tx, doc("x", 1))
	cid, _ := e.c.Insert(tx, doc("y", 2))
	if _, err := tx.Commit(ctx, "reopen"); err != nil {
		t.Fatal(err)
	}
	if err := e.db.Close(); err != nil {
		t.Fatal(err)
	}
	e.db = openDB(t, e.dir)
	e.mount(t)
	if v, err := e.a.Get(aid); err != nil || v.Value["k"] != "x" {
		t.Fatalf("a after reopen: %v %v", v, err)
	}
	if v, err := e.c.Get(cid); err != nil || v.Value["k"] != "y" {
		t.Fatalf("c after reopen: %v %v", v, err)
	}
	if o, _ := e.b.Outcome("reopen/"); o != OutcomeUnknown {
		t.Fatalf("partial key: %s", o)
	}
}

type recObserver struct {
	mu sync.Mutex
	ev []Event
}

func (r *recObserver) Observe(e Event) { r.mu.Lock(); r.ev = append(r.ev, e); r.mu.Unlock() }

func TestObserverSeam(t *testing.T) {
	obs := &recObserver{}
	e := newEnv(t, WithObserver(obs))
	ctx := context.Background()
	tx, _ := e.b.Begin(ctx)
	e.a.Insert(tx, doc("x", 1))
	e.c.Insert(tx, doc("y", 1))
	if _, err := tx.Commit(ctx, "obs"); err != nil {
		t.Fatal(err)
	}
	tx, _ = e.b.Begin(ctx)
	e.a.Update(tx, 1, doc("x", 2), 99)
	tx.Commit(ctx, "obs2")
	if len(obs.ev) != 2 || obs.ev[0].Outcome != "committed" || obs.ev[0].TxID == "" ||
		obs.ev[0].Participants != 2 || obs.ev[1].Outcome != string(ClassConflict) {
		t.Fatalf("%+v", obs.ev)
	}
}

func TestClassifier(t *testing.T) { // G6
	cases := []struct {
		err  error
		want Class
	}{
		{&engine.XTxError{Err: engine.ErrXTxConflict}, ClassConflict},
		{&engine.XTxError{Err: engine.ErrXTxConflict, Cause: engine.ErrDuplicateKey}, ClassConflict},
		{engine.ErrResourceExhausted, ClassExhausted},
		{engine.ErrQuotaExceeded, ClassExhausted},
		{&engine.XTxError{Err: engine.ErrXTxTooLarge}, ClassTooLarge},
		{&engine.XTxError{Err: engine.ErrXTxDurability}, ClassDurability},
		{&engine.XTxError{Err: engine.ErrXTxOutcomeUnknown}, ClassOutcomeUnknown},
		{&engine.XTxError{Err: engine.ErrXTxInProgress}, ClassOutcomeUnknown},
		{fmt.Errorf("w: %w", engine.ErrCollectionNotFound), ClassCollectionNotFound},
		{errors.New("engine: cannot commit empty transaction"), ClassInternal},
		{engine.ErrReservedName, ClassInternal},
		{nil, ClassInternal},
	}
	for i, c := range cases {
		if got := Classify(c.err); got != c.want {
			t.Errorf("case %d: %s want %s", i, got, c.want)
		}
	}
	if ClassOutcomeUnknown.Retryable() || ClassTooLarge.Retryable() || ClassInternal.Retryable() || ClassCollectionNotFound.Retryable() {
		t.Error("fail-closed classes retryable")
	}
}

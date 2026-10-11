package crashtest

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srjn45/scriva/query"
	"github.com/srjn45/warden/internal/dbtx"
	"github.com/srjn45/warden/internal/dbtx/workflows"
)

// acceptable reports whether a concurrent Run error is a legitimate typed
// outcome (a lost race) rather than a silent failure or an untyped leak.
func acceptable(err error) bool {
	var be *dbtx.Error
	if errors.As(err, &be) {
		return true
	}
	if strings.HasPrefix(err.Error(), "collection: get: id ") && strings.HasSuffix(err.Error(), " not found") {
		return true
	}
	for _, d := range []error{
		workflows.ErrProjectMissing, workflows.ErrProjectClosed, workflows.ErrProjectBusy,
		workflows.ErrPlanMissing, workflows.ErrPlanNotReady, workflows.ErrPlanLinked,
		workflows.ErrAgentMissing, workflows.ErrExists,
	} {
		if errors.Is(err, d) {
			return true
		}
	}
	return false
}

// sampler runs the cross-collection checkers while writers are active.
//
// Audit gap I2: scriva has no cross-collection read snapshot, so a reader
// that scans several collections while a commit lands can observe a torn
// VIEW of an atomic commit. Gated sampling (writers hold gate.RLock around
// each Run, the sampler takes gate.Lock) is a valid correctness probe: any
// violation is a real bug. Ungated sampling is informational only and its
// violation count is logged, never asserted.
type sampler struct {
	t     testing.TB
	db    interface{ Close() error }
	gate  *sync.RWMutex
	check func() error
	stop  chan struct{}
	wg    sync.WaitGroup

	gatedRuns, ungatedRuns, ungatedViolations atomic.Int64
	failure                                   atomic.Value // string
}

func startSampler(t testing.TB, gate *sync.RWMutex, check func() error) *sampler {
	s := &sampler{t: t, gate: gate, check: check, stop: make(chan struct{})}
	s.wg.Add(2)
	go func() {
		defer s.wg.Done()
		for {
			select {
			case <-s.stop:
				return
			default:
			}
			gate.Lock()
			err := check()
			gate.Unlock()
			s.gatedRuns.Add(1)
			if err != nil {
				s.failure.CompareAndSwap(nil, err.Error())
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	go func() {
		defer s.wg.Done()
		for {
			select {
			case <-s.stop:
				return
			default:
			}
			s.ungatedRuns.Add(1)
			if err := check(); err != nil {
				s.ungatedViolations.Add(1)
			}
			time.Sleep(time.Millisecond)
		}
	}()
	return s
}

func (s *sampler) finish() {
	close(s.stop)
	s.wg.Wait()
	if f, _ := s.failure.Load().(string); f != "" {
		s.t.Fatalf("gated mid-run checker violation: %s", f)
	}
	s.t.Logf("sampler: gated=%d (violations 0), ungated=%d (violations=%d, informational: audit gap I2, no cross-collection read snapshot)",
		s.gatedRuns.Load(), s.ungatedRuns.Load(), s.ungatedViolations.Load())
}

// runBounded fails the test if fn does not finish in d (deadlock detector).
func runBounded(t testing.TB, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s: did not finish within %s (deadlock/livelock)", what, d)
	}
}

// TestConcurrentSameWorkflow (scenario 5a): N goroutines run the SAME
// workflow (same intent) on one DB; interferers bump the fenced records
// meanwhile. Exactly one logical effect must land: the final state is the
// POST snapshot, never a double application or a lost interferer write.
func TestConcurrentSameWorkflow(t *testing.T) {
	n := scale(6, 16)
	for _, w := range workflows.All() {
		w := w
		t.Run(w.Name, func(t *testing.T) {
			t.Parallel()
			b := prepare(t, w)
			dir := b.fresh(t)
			s := mustOpen(t, dir, nil, dbtx.WithMaxAttempts(8))
			defer s.close()
			var gate sync.RWMutex
			smp := startSampler(t, &gate, func() error { return w.Check(s.db) })

			var wg sync.WaitGroup
			var ok, typed atomic.Int64
			errs := make(chan error, 2*n)
			runBounded(t, 60*time.Second, w.Name, func() {
				for i := 0; i < n; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						gate.RLock()
						defer gate.RUnlock()
						// Each goroutine needs its own Env: BeforeCommit and
						// the repos are per-run state in the helper; the
						// boundary underneath is shared.
						e, err := workflows.NewEnv(s.db, s.b)
						if err != nil {
							errs <- err
							return
						}
						switch err := w.Run(bg, e); {
						case err == nil:
							ok.Add(1)
						case acceptable(err):
							typed.Add(1)
						default:
							errs <- fmt.Errorf("untyped error from concurrent run: %w", err)
						}
					}()
				}
				wg.Wait()
			})
			smp.finish()
			close(errs)
			for err := range errs {
				t.Error(err)
			}
			if ok.Load() == 0 {
				t.Fatalf("no concurrent run succeeded (typed failures: %d)", typed.Load())
			}
			// Converge anything that lost a race, then compare exactly.
			if err := w.Run(bg, s.e); err != nil {
				t.Fatalf("converging run: %v", err)
			}
			if got := snapshot(t, s.db); got != b.post {
				t.Fatalf("after %d concurrent runs state != POST (lost update or double apply):\n--- post\n%s--- got\n%s", n, b.post, got)
			}
			if err := checkAll(s.db); err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: %d goroutines: %d succeeded, %d typed failures", w.Name, n, ok.Load(), typed.Load())
		})
	}
}

// TestConcurrentAllWorkflows (scenario 5c): every workflow, twice each, runs
// concurrently against one database seeded with all of them. Gated mid-run
// sampling must never see a violation; the converged state must match a
// sequential reference run.
func TestConcurrentAllWorkflows(t *testing.T) {
	all := workflows.All()
	build := func(dir string) *stack {
		s := mustOpen(t, dir, nil, dbtx.WithMaxAttempts(8))
		for _, w := range all {
			if err := w.Seed(bg, s.e); err != nil {
				t.Fatalf("%s seed: %v", w.Name, err)
			}
		}
		warmJournal(t, s)
		return s
	}
	refDir := filepath.Join(t.TempDir(), "ref")
	ref := build(refDir)
	for _, w := range all {
		if err := w.Run(bg, ref.e); err != nil {
			t.Fatalf("%s sequential run: %v", w.Name, err)
		}
	}
	want := snapshot(t, ref.db)
	ref.close()

	s := build(filepath.Join(t.TempDir(), "conc"))
	defer s.close()
	var gate sync.RWMutex
	smp := startSampler(t, &gate, func() error { return checkAll(s.db) })
	var wg sync.WaitGroup
	errs := make(chan error, 4*len(all))
	runBounded(t, 90*time.Second, "all workflows", func() {
		for _, w := range all {
			for i := 0; i < 2; i++ {
				w := w
				wg.Add(1)
				go func() {
					defer wg.Done()
					gate.RLock()
					defer gate.RUnlock()
					e, err := workflows.NewEnv(s.db, s.b)
					if err != nil {
						errs <- err
						return
					}
					if err := w.Run(bg, e); err != nil && !acceptable(err) {
						errs <- fmt.Errorf("%s: %w", w.Name, err)
					}
				}()
			}
		}
		wg.Wait()
	})
	smp.finish()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	for _, w := range all {
		if err := w.Run(bg, s.e); err != nil {
			t.Fatalf("%s converging run: %v", w.Name, err)
		}
	}
	if err := checkAll(s.db); err != nil {
		t.Fatal(err)
	}
	if got := snapshot(t, s.db); got != want {
		t.Fatalf("concurrent end state differs from sequential reference:\n--- want\n%s--- got\n%s", want, got)
	}
}

// TestInverseParticipantOrderNoDeadlock (scenario 5d): two goroutines stage
// the same two collections in opposite orders, many times, with a contended
// fenced record in each. The engine takes no locks in staging order, so this
// must finish (bounded) with every pair intact and no lost counter update.
func TestInverseParticipantOrderNoDeadlock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	s := mustOpen(t, dir, nil, dbtx.WithMaxAttempts(64))
	defer s.close()
	warmJournal(t, s) // creates warm_a / warm_b
	a, err := dbtx.Bind[map[string]any](s.b, "warm_a", dbtx.MapCodec{})
	if err != nil {
		t.Fatal(err)
	}
	c, err := dbtx.Bind[map[string]any](s.b, "warm_b", dbtx.MapCodec{})
	if err != nil {
		t.Fatal(err)
	}
	var ida, idc uint64
	if _, err := s.b.Run(bg, "seed-counters", func(_ context.Context, tx *dbtx.Tx) error {
		var err error
		if ida, err = a.Insert(tx, map[string]any{"counter": float64(0)}); err != nil {
			return err
		}
		idc, err = c.Insert(tx, map[string]any{"counter": float64(0)})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	per := scale(25, 150)
	var wg sync.WaitGroup
	var done atomic.Int64
	worker := func(name string, reverse bool) {
		defer wg.Done()
		for i := 0; i < per; i++ {
			intent := fmt.Sprintf("inc/%s/%d", name, i)
			_, err := s.b.Run(bg, intent, func(_ context.Context, tx *dbtx.Tx) error {
				va, err := a.Get(ida)
				if err != nil {
					return err
				}
				vc, err := c.Get(idc)
				if err != nil {
					return err
				}
				upA := func() error {
					m := map[string]any{"counter": va.Value["counter"].(float64) + 1}
					return a.Update(tx, ida, m, va.Rev)
				}
				upC := func() error {
					m := map[string]any{"counter": vc.Value["counter"].(float64) + 1}
					return c.Update(tx, idc, m, vc.Rev)
				}
				first, second := upA, upC
				if reverse {
					first, second = upC, upA
				}
				if err := first(); err != nil {
					return err
				}
				if err := second(); err != nil {
					return err
				}
				_, err = a.Insert(tx, map[string]any{"tag": intent})
				if err != nil {
					return err
				}
				_, err = c.Insert(tx, map[string]any{"tag": intent})
				return err
			})
			if err != nil {
				t.Errorf("%s #%d: %v", name, i, err)
				return
			}
			done.Add(1)
		}
	}
	runBounded(t, 90*time.Second, "inverse-order writers", func() {
		wg.Add(2)
		go worker("fwd", false)
		go worker("rev", true)
		wg.Wait()
	})
	if t.Failed() {
		return
	}
	want := float64(2 * per)
	va, _ := a.Get(ida)
	vc, _ := c.Get(idc)
	if va.Value["counter"].(float64) != want || vc.Value["counter"].(float64) != want {
		t.Fatalf("lost update: counters a=%v c=%v want %v", va.Value["counter"], vc.Value["counter"], want)
	}
	tags := func(coll string) map[string]bool {
		out := map[string]bool{}
		col, err := s.db.Collection(coll)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := col.Scan(query.MatchAll)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if tg, ok := r.Data["tag"].(string); ok {
				out[tg] = true
			}
		}
		return out
	}
	ta, tc := tags("warm_a"), tags("warm_b")
	if len(ta) != 2*per || len(tc) != 2*per {
		t.Fatalf("tag counts a=%d c=%d want %d", len(ta), len(tc), 2*per)
	}
	for k := range ta {
		if !tc[k] {
			t.Fatalf("torn pair: tag %s in warm_a but not warm_b", k)
		}
	}
}

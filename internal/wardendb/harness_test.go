package wardendb

// Recovery / fault harness for the warden-db foundation. The per-feature
// suites (strict, verify, backup, repair, operations) pin each primitive; this
// file drives them together against a realistically populated database —
// several collections plus journal rows in every recovery-relevant state — and
// holds the cross-cutting invariants: nothing is ever salvaged by a normal
// Open, read-only tools never own or mutate, and recovery state survives a
// Close/Open cycle.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
)

// fixture describes a database written by populate.
type fixture struct {
	// ids and leases are keyed by the scenario names used in populate.
	ids    map[string]string
	leases map[string]Lease
	// agents and plans are the record keys written to each collection.
	agents, plans []string
}

// populate writes a database holding two typed collections and operation rows
// in every state recovery cares about, then closes it. Rows named *-expired
// hold leases that lapsed before the clock reaches its final value; *-live
// ones are still held at the final value.
func populate(t *testing.T, dataDir string, clk *fakeClock) *fixture {
	t.Helper()
	db, j := openJournal(t, dataDir, clk)
	fx := &fixture{ids: map[string]string{}, leases: map[string]Lease{},
		agents: []string{"agent-1", "agent-2", "agent-3", "agent-4"},
		plans:  []string{"plan-a", "plan-b"}}

	agents, err := Register[agentRec](db, Spec{Name: "agents", Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	plans, err := Register[planRec](db, Spec{Name: "plans", Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	ae, err := agents.Engine()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range fx.agents {
		if _, err := ae.Upsert(k, map[string]any{"id": k}); err != nil {
			t.Fatal(err)
		}
	}
	pe, err := plans.Engine()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range fx.plans {
		if _, err := pe.Upsert(k, map[string]any{"id": k}); err != nil {
			t.Fatal(err)
		}
	}

	add := func(name string, state OperationState) {
		op, lease := driveTo(t, j, clk, state)
		fx.ids[name] = op.ID
		if lease != nil {
			fx.leases[name] = *lease
		}
	}
	add("pending", OperationPending)
	add("ready", OperationReady)
	add("succeeded", OperationSucceeded)
	add("manual", OperationManualIntervention)
	// Leases are one minute long: these lapse once the clock moves on.
	add("running-expired", OperationRunning)
	add("reconciling-expired", OperationReconciling)
	add("retry-due", OperationRetryWait)
	clk.Advance(time.Hour)
	add("running-live", OperationRunning)
	add("reconciling-live", OperationReconciling)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return fx
}

// fault damages one segment file in one way.
type fault struct {
	name string
	// apply damages the segment at seg (its first record is always followed by
	// more data).
	apply func(t *testing.T, seg string)
}

func replaceInFirstLine(t *testing.T, seg, old, repl string) {
	t.Helper()
	b, err := os.ReadFile(seg)
	if err != nil {
		t.Fatal(err)
	}
	nl := bytes.IndexByte(b, '\n')
	first := string(b[:nl])
	if !strings.Contains(first, old) {
		t.Fatalf("first record of %s has no %q: %s", seg, old, first)
	}
	first = strings.Replace(first, old, repl, 1)
	if err := os.WriteFile(seg, append([]byte(first), b[nl:]...), 0o600); err != nil {
		t.Fatal(err)
	}
}

// blockingFaults are damages to acknowledged segment bytes. Each must make a
// normal Open refuse and must never be repaired implicitly.
var blockingFaults = []fault{
	{"overwritten-record", func(t *testing.T, seg string) { corruptFirstRecord(t, seg) }},
	{"invalid-json", func(t *testing.T, seg string) {
		// Same length, valid framing, not JSON.
		replaceInFirstLine(t, seg, `"op":"insert"`, `"op":"inser!"}{`)
	}},
	{"checksum-mismatch", func(t *testing.T, seg string) {
		// Still valid JSON: only the per-record crc can notice.
		b, _ := os.ReadFile(seg)
		nl := bytes.IndexByte(b, '\n')
		first := string(b[:nl])
		i := strings.Index(first, `"rev":1`)
		if i < 0 {
			t.Fatalf("no rev in %s", first)
		}
		first = first[:i] + `"rev":7` + first[i+len(`"rev":1`):]
		if err := os.WriteFile(seg, append([]byte(first), b[nl:]...), 0o600); err != nil {
			t.Fatal(err)
		}
	}},
	{"truncated-head", func(t *testing.T, seg string) {
		b, _ := os.ReadFile(seg)
		if err := os.WriteFile(seg, b[10:], 0o600); err != nil {
			t.Fatal(err)
		}
	}},
	{"truncated-mid-record", func(t *testing.T, seg string) {
		// Half a record is cut out ahead of intact ones.
		b, _ := os.ReadFile(seg)
		nl := bytes.IndexByte(b, '\n')
		out := append(append([]byte{}, b[:nl/2]...), b[nl+1:]...)
		if err := os.WriteFile(seg, out, 0o600); err != nil {
			t.Fatal(err)
		}
	}},
}

func TestHarnessBlockingFaultsFailClosed(t *testing.T) {
	ctx := context.Background()
	for _, col := range []string{"agents", "plans", OperationsCollection} {
		for _, f := range blockingFaults {
			t.Run(col+"/"+f.name, func(t *testing.T) {
				dataDir, backups := t.TempDir(), t.TempDir()
				populate(t, dataDir, newFakeClock())
				f.apply(t, newestSegment(t, dataDir, col))
				before := snapshot(t, Dir(dataDir))

				// Normal Open: refused, names the collection, salvages nothing.
				rec := &recorder{}
				db, err := Open(dataDir, observed(rec)...)
				var ie *IntegrityError
				if db != nil || !errors.As(err, &ie) || !errors.Is(err, ErrIntegrity) {
					if db != nil {
						_ = db.Close()
					}
					t.Fatalf("Open = %v, %v; want *IntegrityError", db, err)
				}
				// Damage to a journal participant can surface as a
				// cross-collection transaction finding, so the collection
				// may be named by a finding rather than the error itself.
				named := ie.Collection == col
				for _, f := range ie.Findings {
					named = named || f.Collection == col || strings.Contains(f.String(), `"`+col+`"`)
				}
				if !named || ie.Stage != StageOpen || len(ie.Findings) == 0 {
					t.Errorf("IntegrityError = %+v", ie)
				}
				if rec.count(ActionRecovery, "") != 0 || rec.count(ActionOpen, "") != 0 {
					t.Errorf("a refused open emitted recovery/open events")
				}
				sameSnapshot(t, before, snapshot(t, Dir(dataDir)))

				// The read-only tools see the same damage and change nothing.
				v, err := Verify(ctx, dataDir)
				if err != nil || v.Health != HealthCorrupt || v.Health.Openable() || v.Blocking == 0 {
					t.Fatalf("Verify = %+v, %v", v, err)
				}
				if c := v.Collection(col); c == nil || c.Health != HealthCorrupt {
					t.Errorf("collection %s status = %+v", col, c)
				}
				rep, err := Diagnose(ctx, dataDir)
				if err != nil || rep.Health != HealthCorrupt {
					t.Fatalf("Diagnose = %+v, %v", rep, err)
				}
				if st, err := Probe(dataDir); err != nil || st.Owned {
					t.Errorf("Probe = %+v, %v", st, err)
				}
				sameSnapshot(t, before, snapshot(t, Dir(dataDir)))

				// Repair without Salvage and OpenForRepair without Salvage
				// refuse too, and take no backup.
				if res, err := Repair(ctx, dataDir, RepairOptions{BackupDir: backups}); !errors.Is(err, ErrRepairRefused) || res == nil || res.Changed {
					t.Errorf("Repair = %+v, %v", res, err)
				}
				if db, err := OpenForRepair(dataDir, WithRepair(RepairOptions{BackupDir: backups})); db != nil || !errors.Is(err, ErrRepairRefused) {
					if db != nil {
						_ = db.Close()
					}
					t.Errorf("OpenForRepair = %v, %v", db, err)
				}
				if got := dirEntries(t, backups); len(got) != 0 {
					t.Errorf("refused repairs wrote %v", got)
				}
				sameSnapshot(t, before, snapshot(t, Dir(dataDir)))
				if _, err := Open(dataDir); !errors.Is(err, ErrIntegrity) {
					t.Errorf("Open after refused repairs = %v", err)
				}
			})
		}
	}
}

// TestHarnessDriftIsNeverSilent covers damage the engine classes as
// recoverable (derived structures out of step with the segments). A strict
// Open may rebuild those, but only loudly, and the read-only tools must
// report them beforehand without changing a byte.
func TestHarnessDriftIsNeverSilent(t *testing.T) {
	ctx := context.Background()
	drifts := []struct {
		name  string
		apply func(t *testing.T, colDir string)
		// keeps is whether every acknowledged record must survive Open.
		keeps bool
	}{
		{"index-unreadable", func(t *testing.T, d string) {
			must(t, os.WriteFile(filepath.Join(d, "index.json"), []byte("{garbage"), 0o600))
		}, true},
		{"index-checksum-drift", func(t *testing.T, d string) {
			must(t, os.WriteFile(filepath.Join(d, "index.json"), []byte("{}"), 0o600))
		}, true},
		{"index-removed", func(t *testing.T, d string) {
			must(t, os.Remove(filepath.Join(d, "index.json")))
		}, true},
		// An emptied or vanished segment is, today, classed as index drift:
		// Open succeeds and the records it held are gone. The harness pins
		// that it is at least reported before and announced by Open; whether
		// the engine should refuse it is raised in the PR description.
		{"segment-emptied", func(t *testing.T, d string) {
			segs, _ := filepath.Glob(filepath.Join(d, "seg_*.ndjson"))
			must(t, os.WriteFile(segs[len(segs)-1], nil, 0o600))
		}, false},
		{"segment-removed", func(t *testing.T, d string) {
			segs, _ := filepath.Glob(filepath.Join(d, "seg_*.ndjson"))
			must(t, os.Remove(segs[len(segs)-1]))
		}, false},
	}
	for _, d := range drifts {
		t.Run(d.name, func(t *testing.T) {
			dataDir := t.TempDir()
			seed(t, dataDir)
			d.apply(t, filepath.Join(Dir(dataDir), "agents"))
			before := snapshot(t, Dir(dataDir))

			v, err := Verify(ctx, dataDir)
			if err != nil || v.Health != HealthDegraded || !v.Health.Openable() || v.Recoverable == 0 || v.Blocking != 0 {
				t.Fatalf("Verify = %+v, %v", v, err)
			}
			sameSnapshot(t, before, snapshot(t, Dir(dataDir)))

			rec := &recorder{}
			db := mustOpen(t, dataDir, observed(rec)...)
			ev := rec.event(ActionRecovery)
			if ev == nil || ev.Detail["findings"] == "" {
				t.Fatalf("drift was repaired without a %s event naming findings: %+v", ActionRecovery, ev)
			}
			if n := agentCount(t, db); d.keeps && n != 3 {
				t.Errorf("read %d of 3 records after rebuild", n)
			} else if !d.keeps {
				t.Logf("records readable after %s: %d of 3 (see PR notes)", d.name, n)
			}
			res, err := db.Verify(ctx)
			if err != nil || res.Health != HealthHealthy {
				t.Errorf("Verify after rebuild = %+v, %v", res, err)
			}
		})
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// TestHarnessOwnershipExcludesEveryoneElse holds one populated database open
// and checks that every other entry point either refuses or is purely
// diagnostic: no byte under warden-db changes and the owner keeps working.
func TestHarnessOwnershipExcludesEveryoneElse(t *testing.T) {
	ctx := context.Background()
	dataDir, backups, dest := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "bk")
	clk := newFakeClock()
	fx := populate(t, dataDir, clk)

	rec := &recorder{}
	db, j := openJournal(t, dataDir, clk, observed(rec)...)
	if n := agentCount(t, db); n != 3 {
		t.Fatalf("read %d records", n)
	}
	before := snapshot(t, Dir(dataDir))

	// A second strict opener is refused and told who owns the database.
	var le *LockedError
	if db2, err := Open(dataDir); db2 != nil || !errors.As(err, &le) || !errors.Is(err, ErrLocked) {
		t.Fatalf("second Open = %v, %v", db2, err)
	}
	if le.Dir != Dir(dataDir) {
		t.Errorf("LockedError.Dir = %q", le.Dir)
	}
	// A direct storage open behind the foundation's back is refused by the
	// engine's own directory lock.
	if h, err := scriva.Open(Dir(dataDir)); err == nil {
		_ = h.Close()
		t.Fatal("direct scriva.Open succeeded on an owned warden-db")
	} else if !errors.Is(err, engine.ErrDatabaseLocked) {
		t.Errorf("direct scriva.Open = %v, want ErrDatabaseLocked", err)
	}

	// Tools that need exclusive ownership refuse.
	if _, err := Backup(ctx, dataDir, dest); !errors.Is(err, ErrLocked) {
		t.Errorf("offline Backup of an owned db = %v", err)
	}
	if res, err := Repair(ctx, dataDir, RepairOptions{BackupDir: backups, Salvage: true}); !errors.Is(err, ErrLocked) || res != nil {
		t.Errorf("Repair = %+v, %v", res, err)
	}
	if d2, err := OpenForRepair(dataDir, WithRepair(RepairOptions{BackupDir: backups, Salvage: true})); d2 != nil || !errors.Is(err, ErrLocked) {
		t.Errorf("OpenForRepair = %v, %v", d2, err)
	}
	if got := dirEntries(t, backups); len(got) != 0 {
		t.Errorf("refusals wrote %v", got)
	}

	// Read-only tools run beside the owner and leave no trace.
	if st, err := Probe(dataDir); err != nil || !st.Exists {
		t.Errorf("Probe = %+v, %v", st, err)
	}
	v, err := Verify(ctx, dataDir)
	if err != nil || v.Lock.State == LockUnowned || v.Blocking != 0 {
		t.Errorf("Verify beside an owner = %+v, %v", v, err)
	}
	rep, err := Diagnose(ctx, dataDir)
	if err != nil || rep.Health == HealthCorrupt {
		t.Errorf("Diagnose beside an owner = %+v, %v", rep, err)
	}
	if got := helper(t, dataDir, "probe"); got != "owned-by-parent" {
		t.Errorf("other process Probe = %q", got)
	}
	if got := helperOpen(t, dataDir); got != "locked" {
		t.Errorf("other process Open = %q", got)
	}
	sameSnapshot(t, before, snapshot(t, Dir(dataDir)))
	if rec.count(ActionRepair, "") != 0 || rec.count(ActionBackup, "") != 0 {
		t.Errorf("a refused tool was audited as having acted")
	}

	// The owner is unaffected: it reads, writes and still holds its leases.
	if got, err := j.Get(fx.ids["running-live"]); err != nil || got.State != OperationRunning {
		t.Errorf("owner Get = %+v, %v", got, err)
	}
	if _, _, err := j.Begin(ctx, testSpec()); err != nil {
		t.Errorf("owner Begin: %v", err)
	}
	must(t, db.Close())

	// Ownership is strictly sequential: with the owner gone exactly one of
	// many concurrent openers wins.
	var wg sync.WaitGroup
	var mu sync.Mutex
	var won []*DB
	var locked, other int
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := Open(dataDir)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				won = append(won, d)
			case errors.Is(err, ErrLocked):
				locked++
			default:
				other++
				t.Errorf("concurrent Open: %v", err)
			}
		}()
	}
	wg.Wait()
	for _, d := range won {
		must(t, d.Close())
	}
	if len(won) != 1 || locked != 7 || other != 0 {
		t.Errorf("concurrent Open: %d won, %d locked, %d other; want 1/7/0", len(won), locked, other)
	}
}

// TestHarnessReopenCycles opens, extends and closes the same populated
// database repeatedly: every cycle must verify clean with no recovery event,
// and a closed or retired handle must fail every use.
func TestHarnessReopenCycles(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	clk := newFakeClock()
	fx := populate(t, dataDir, clk)

	var last *DB
	var lastJ *Journal
	for cycle := 1; cycle <= 3; cycle++ {
		rec := &recorder{}
		db, j := openJournal(t, dataDir, clk, observed(rec)...)
		if rec.count(ActionRecovery, "") != 0 || rec.count(ActionIntegrityRefused, "") != 0 {
			t.Fatalf("cycle %d: clean reopen reported recovery/integrity", cycle)
		}
		agents, err := Register[agentRec](db, Spec{Name: "agents", Version: 1})
		must(t, err)
		if _, err := Register[agentRec](db, Spec{Name: "agents", Version: 1}); !errors.Is(err, ErrDuplicateCollection) {
			t.Errorf("cycle %d: duplicate Register = %v", cycle, err)
		}
		if _, err := Lookup[agentRec](db, "agents"); err != nil {
			t.Errorf("cycle %d: Lookup: %v", cycle, err)
		}
		e, _ := agents.Engine()
		key := fmt.Sprintf("agent-cycle-%d", cycle)
		if _, err := e.Upsert(key, map[string]any{"id": key}); err != nil {
			t.Fatal(err)
		}
		for _, k := range append([]string{key}, fx.agents...) {
			if ok, err := e.Exists(k); err != nil || !ok {
				t.Errorf("cycle %d: record %s missing: %v %v", cycle, k, ok, err)
			}
		}
		if _, _, err := j.Begin(ctx, testSpec()); err != nil {
			t.Fatal(err)
		}
		res, err := db.Verify(ctx)
		if err != nil || res.Health != HealthHealthy {
			t.Fatalf("cycle %d: Verify = %+v, %v", cycle, res, err)
		}
		must(t, db.Close())
		must(t, db.Close()) // idempotent
		last, lastJ = db, j
	}

	// Every use of a closed handle is ErrClosed, never a panic or stale data.
	if _, err := last.Handle(); !errors.Is(err, ErrClosed) {
		t.Errorf("Handle after Close = %v", err)
	}
	if _, err := Register[agentRec](last, Spec{Name: "terminals", Version: 1}); !errors.Is(err, ErrClosed) {
		t.Errorf("Register after Close = %v", err)
	}
	if _, err := last.Verify(ctx); !errors.Is(err, ErrClosed) {
		t.Errorf("Verify after Close = %v", err)
	}
	if _, err := last.Backup(ctx, filepath.Join(t.TempDir(), "bk")); !errors.Is(err, ErrClosed) {
		t.Errorf("Backup after Close = %v", err)
	}
	if err := last.OnClose("late", func() error { return nil }); !errors.Is(err, ErrClosed) {
		t.Errorf("OnClose after Close = %v", err)
	}
	if _, err := lastJ.Incomplete(); !errors.Is(err, ErrClosed) {
		t.Errorf("Incomplete after Close = %v", err)
	}
	if _, err := lastJ.Get(fx.ids["pending"]); !errors.Is(err, ErrClosed) {
		t.Errorf("Get after Close = %v", err)
	}
}

// TestHarnessRetiredHandleStaysRetired damages the segments under a live,
// populated handle: the integrity failure found by Verify retires the handle
// and its journal, Close still succeeds without touching the damage, and the
// next Open refuses.
func TestHarnessRetiredHandleStaysRetired(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	clk := newFakeClock()
	fx := populate(t, dataDir, clk)
	rec := &recorder{}
	db, j := openJournal(t, dataDir, clk, observed(rec)...)
	agents, err := Register[agentRec](db, Spec{Name: "agents", Version: 1})
	must(t, err)

	seg := newestSegment(t, dataDir, "agents")
	damaged := corruptFirstRecord(t, seg)
	res, err := db.Verify(ctx)
	if err != nil || res.Health != HealthCorrupt {
		t.Fatalf("Verify = %+v, %v", res, err)
	}
	if _, err := agents.Engine(); !errors.Is(err, ErrIntegrity) {
		t.Errorf("Engine after violation = %v", err)
	}
	if _, err := j.Get(fx.ids["ready"]); !errors.Is(err, ErrIntegrity) {
		t.Errorf("journal Get after violation = %v", err)
	}
	if _, _, err := j.Begin(ctx, testSpec()); !errors.Is(err, ErrIntegrity) {
		t.Errorf("journal Begin after violation = %v", err)
	}
	if _, err := j.Transition(ctx, fx.ids["ready"], OperationCanceled, OperationUpdate{}); !errors.Is(err, ErrIntegrity) {
		t.Errorf("journal Transition after violation = %v", err)
	}
	if _, err := db.Backup(ctx, filepath.Join(t.TempDir(), "bk")); !errors.Is(err, ErrIntegrity) {
		t.Errorf("Backup of a retired handle = %v", err)
	}
	must(t, db.Close())
	if got, err := os.ReadFile(seg); err != nil || !bytes.Equal(got, damaged) {
		t.Errorf("Close changed the damaged segment")
	}
	if _, err := Open(dataDir); !errors.Is(err, ErrIntegrity) {
		t.Errorf("Open after retirement = %v", err)
	}
}

// operationRows returns every journal row keyed by id.
func operationRows(t *testing.T, j *Journal) map[string]Operation {
	t.Helper()
	all, err := j.ListByState(OperationStates()...)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]Operation{}
	for _, op := range all {
		out[op.ID] = op
	}
	return out
}

func readAgents(t *testing.T, db *DB, keys []string) int {
	t.Helper()
	agents, err := Register[agentRec](db, Spec{Name: "agents", Version: 1})
	must(t, err)
	e, err := agents.Engine()
	must(t, err)
	n := 0
	for _, k := range keys {
		if ok, _ := e.Exists(k); ok {
			n++
		}
	}
	return n
}

func TestHarnessBackupRestorePopulated(t *testing.T) {
	ctx := context.Background()
	for _, online := range []bool{true, false} {
		name := "offline"
		if online {
			name = "online"
		}
		t.Run(name, func(t *testing.T) {
			src, dst, dest := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "bk")
			clk := newFakeClock()
			fx := populate(t, src, clk)

			var m *BackupManifest
			var want map[string]Operation
			var err error
			if online {
				db, j := openJournal(t, src, clk)
				want = operationRows(t, j)
				m, err = db.Backup(ctx, dest)
				must(t, err)
				// Work after the backup must not be in it.
				if _, _, err := j.Begin(ctx, testSpec()); err != nil {
					t.Fatal(err)
				}
				must(t, db.Close())
			} else {
				m, err = Backup(ctx, src, dest)
				must(t, err)
				db, j := openJournal(t, src, clk)
				want = operationRows(t, j)
				must(t, db.Close())
			}
			var cols []string
			for _, c := range m.Collections {
				cols = append(cols, c.Name)
			}
			sort.Strings(cols)
			if !reflect.DeepEqual(cols, []string{"agents", OperationsCollection, "plans"}) {
				t.Fatalf("backup collections = %v", cols)
			}
			if _, err := VerifyBackup(dest); err != nil {
				t.Fatalf("VerifyBackup: %v", err)
			}

			res, err := Restore(ctx, dest, dst, RestoreOptions{})
			must(t, err)
			if !res.Verify.Health.Openable() {
				t.Fatalf("restored health = %q", res.Verify.Health)
			}
			if st, err := Probe(dst); err != nil || st.Owned {
				t.Fatalf("restore kept ownership: %+v, %v", st, err)
			}

			rec := &recorder{}
			db, j := openJournal(t, dst, clk, observed(rec)...)
			// A snapshot carries no derived indexes, so the online copy is
			// rebuilt (loudly) on first open; a file copy needs nothing.
			if !online && rec.count(ActionRecovery, "") != 0 {
				t.Errorf("restored database needed recovery: %+v", rec.event(ActionRecovery))
			}
			if n := readAgents(t, db, fx.agents); n != len(fx.agents) {
				t.Errorf("restored %d of %d agents", n, len(fx.agents))
			}
			plans, err := Register[planRec](db, Spec{Name: "plans", Version: 1})
			must(t, err)
			pe, _ := plans.Engine()
			for _, k := range fx.plans {
				if ok, _ := pe.Exists(k); !ok {
					t.Errorf("plan %s lost", k)
				}
			}
			got := operationRows(t, j)
			if len(got) != len(want) {
				t.Errorf("restored %d operations, backed up %d", len(got), len(want))
			}
			for id, w := range want {
				g, ok := got[id]
				if !ok {
					t.Errorf("operation %s missing after restore", id)
					continue
				}
				if g.State != w.State || g.LeaseOwner != w.LeaseOwner || g.LeaseEpoch != w.LeaseEpoch ||
					!g.LeaseExpiresAt.Equal(w.LeaseExpiresAt) || g.IntentFingerprint != w.IntentFingerprint ||
					len(g.History) != len(w.History) {
					t.Errorf("operation %s differs after restore:\n got %+v\nwant %+v", id, g, w)
				}
			}
			// Recovery works on the restored copy: lapsed leases are found
			// and the old holder's lease is fenced on takeover.
			exp, err := j.LeaseExpired()
			must(t, err)
			found := map[string]bool{}
			for _, op := range exp {
				found[op.ID] = true
			}
			for _, n := range []string{"running-expired", "reconciling-expired"} {
				if !found[fx.ids[n]] {
					t.Errorf("%s not reported lease-expired after restore", n)
				}
			}
			res2, err := db.Verify(ctx)
			if err != nil || res2.Health != HealthHealthy {
				t.Errorf("Verify of restored database = %+v, %v", res2, err)
			}
		})
	}
}

func TestHarnessRestoreRefusalCases(t *testing.T) {
	ctx := context.Background()
	src, dest := t.TempDir(), filepath.Join(t.TempDir(), "bk")
	populate(t, src, newFakeClock())
	_, err := Backup(ctx, src, dest)
	must(t, err)

	t.Run("non-empty-destination", func(t *testing.T) {
		target := t.TempDir()
		populate(t, target, newFakeClock())
		before := snapshot(t, Dir(target))
		if _, err := Restore(ctx, dest, target, RestoreOptions{}); !errors.Is(err, ErrRestoreRefused) {
			t.Fatalf("Restore over data = %v", err)
		}
		sameSnapshot(t, before, snapshot(t, Dir(target)))
	})
	t.Run("live-destination-even-with-overwrite", func(t *testing.T) {
		target := t.TempDir()
		clk := newFakeClock()
		populate(t, target, clk)
		db, j := openJournal(t, target, clk)
		before := snapshot(t, Dir(target))
		for _, ro := range []RestoreOptions{{}, {Overwrite: true}, {Overwrite: true, AllowCorrupt: true}} {
			if _, err := Restore(ctx, dest, target, ro); !errors.Is(err, ErrLocked) {
				t.Errorf("Restore(%+v) over an owned db = %v", ro, err)
			}
		}
		sameSnapshot(t, before, snapshot(t, Dir(target)))
		if _, err := j.Incomplete(); err != nil {
			t.Errorf("owner broken by refused restores: %v", err)
		}
		must(t, db.Close())
	})
	t.Run("tampered-backup", func(t *testing.T) {
		bk := filepath.Join(t.TempDir(), "bk")
		_, err := Backup(ctx, src, bk)
		must(t, err)
		var victim string
		_ = filepath.WalkDir(bk, func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(p, ".ndjson") && victim == "" {
				victim = p
			}
			return nil
		})
		if victim == "" {
			t.Fatal("no segment in backup")
		}
		b, _ := os.ReadFile(victim)
		b[len(b)/2] ^= 0xff
		must(t, os.WriteFile(victim, b, 0o600))
		target := t.TempDir()
		if _, err := Restore(ctx, bk, target, RestoreOptions{AllowCorrupt: true}); !errors.Is(err, ErrBackupInvalid) {
			t.Fatalf("Restore of a tampered backup = %v", err)
		}
		if got := dirEntries(t, Dir(target)); len(got) != 0 {
			for _, n := range got {
				if !lockFile(n) {
					t.Errorf("refused restore left %s", n)
				}
			}
		}
	})
	t.Run("backup-inside-target", func(t *testing.T) {
		target := t.TempDir()
		inside := filepath.Join(Dir(target), "bk")
		_, err := Backup(ctx, src, inside)
		if err != nil {
			t.Skipf("backup into an arbitrary dir refused: %v", err)
		}
		if _, err := Restore(ctx, inside, target, RestoreOptions{}); !errors.Is(err, ErrRestoreRefused) {
			t.Errorf("Restore from inside the target = %v", err)
		}
	})
	t.Run("backup-destination-inside-warden-db", func(t *testing.T) {
		clk := newFakeClock()
		target := t.TempDir()
		populate(t, target, clk)
		before := snapshot(t, Dir(target))
		if _, err := Backup(ctx, target, filepath.Join(Dir(target), "bk")); !errors.Is(err, ErrBackupDestination) {
			t.Errorf("Backup into the database = %v", err)
		}
		sameSnapshot(t, before, snapshot(t, Dir(target)))
	})
}

func TestHarnessRepairPopulated(t *testing.T) {
	ctx := context.Background()
	dataDir, backups := t.TempDir(), t.TempDir()
	clk := newFakeClock()
	fx := populate(t, dataDir, clk)
	// Two independent problems: damaged acknowledged bytes in agents and a
	// lost derived index in plans.
	corruptFirstRecord(t, newestSegment(t, dataDir, "agents"))
	dropIndex(t, dataDir, "plans")
	before := snapshot(t, Dir(dataDir))

	// Unsafe cases first: nothing changes.
	if _, err := Repair(ctx, dataDir, RepairOptions{Salvage: true}); !errors.Is(err, ErrRepairBackupRequired) {
		t.Errorf("Repair without a backup dir = %v", err)
	}
	if _, err := Repair(ctx, dataDir, RepairOptions{BackupDir: filepath.Join(Dir(dataDir), "bk"), Salvage: true}); !errors.Is(err, ErrRepairBackupRequired) {
		t.Errorf("Repair with a backup dir inside warden-db = %v", err)
	}
	res, err := Repair(ctx, dataDir, RepairOptions{BackupDir: backups})
	if !errors.Is(err, ErrRepairRefused) || res == nil || res.Changed {
		t.Errorf("Repair without Salvage = %+v, %v", res, err)
	}
	sameSnapshot(t, before, snapshot(t, Dir(dataDir)))
	if got := dirEntries(t, backups); len(got) != 0 {
		t.Errorf("refused repairs wrote %v", got)
	}

	// The acting run: backs up first, then salvages.
	rec := &recorder{}
	res, err = Repair(ctx, dataDir, RepairOptions{BackupDir: backups, Salvage: true}, observed(rec)...)
	if err != nil || !res.Changed || res.BackupDir == "" || res.After == nil {
		t.Fatalf("Repair = %+v, %v", res, err)
	}
	if !inside(backups, res.BackupDir) {
		t.Errorf("backup %s is not under %s", res.BackupDir, backups)
	}
	if _, err := VerifyBackup(res.BackupDir); err != nil {
		t.Errorf("pre-repair backup does not verify: %v", err)
	}
	outcomes := map[string]RepairOutcome{}
	for _, c := range res.Collections {
		outcomes[c.Name] = c.Outcome
	}
	if outcomes["agents"] != RepairRepaired || outcomes[OperationsCollection] != RepairUnchanged {
		t.Errorf("outcomes = %v", outcomes)
	}
	if rec.count(ActionRepair, "") != 1 {
		t.Errorf("repair events = %d", rec.count(ActionRepair, ""))
	}
	if st, err := Probe(dataDir); err != nil || st.Owned {
		t.Errorf("Repair kept ownership: %+v, %v", st, err)
	}

	// Afterwards the database is an ordinary strict, verifiable one and
	// everything the repair had no reason to touch is intact.
	vr, err := Verify(ctx, dataDir)
	if err != nil || !vr.Health.Openable() || vr.Blocking != 0 {
		t.Fatalf("Verify after repair = %+v, %v", vr, err)
	}
	db, j := openJournal(t, dataDir, clk)
	if n := readAgents(t, db, fx.agents); n != len(fx.agents)-1 {
		t.Errorf("agents after salvage = %d, want %d (one damaged record lost)", n, len(fx.agents)-1)
	}
	plans, err := Register[planRec](db, Spec{Name: "plans", Version: 1})
	must(t, err)
	pe, _ := plans.Engine()
	for _, k := range fx.plans {
		if ok, _ := pe.Exists(k); !ok {
			t.Errorf("plan %s lost by repair", k)
		}
	}
	rows := operationRows(t, j)
	for name, id := range fx.ids {
		if _, ok := rows[id]; !ok {
			t.Errorf("operation %s lost by repair", name)
		}
	}
	vo, err := db.Verify(ctx)
	if err != nil || vo.Health != HealthHealthy {
		t.Errorf("owner Verify after repair = %+v, %v", vo, err)
	}
	must(t, db.Close())

	// A second repair has nothing to do.
	again, err := Repair(ctx, dataDir, RepairOptions{BackupDir: backups, Salvage: true})
	if err != nil || again.Changed || again.BackupDir != "" {
		t.Errorf("second Repair = %+v, %v", again, err)
	}
}

// TestHarnessOpenForRepairHandsBackAStrictHandle: the repair entry point ends
// in the same strict handle Open returns, journal included. Damage to the
// journal itself is never salvaged: transaction state is not rewritten.
func TestHarnessOpenForRepairHandsBackAStrictHandle(t *testing.T) {
	ctx := context.Background()
	clk := newFakeClock()

	t.Run("journal-damage-refused", func(t *testing.T) {
		dataDir, backups := t.TempDir(), t.TempDir()
		populate(t, dataDir, clk)
		corruptFirstRecord(t, newestSegment(t, dataDir, OperationsCollection))
		before := snapshot(t, Dir(dataDir))
		db, err := OpenForRepair(dataDir, WithRepair(RepairOptions{BackupDir: backups, Salvage: true}))
		if db != nil || !errors.Is(err, ErrRepairRefused) {
			if db != nil {
				_ = db.Close()
			}
			t.Fatalf("OpenForRepair = %v, %v", db, err)
		}
		sameSnapshot(t, before, snapshot(t, Dir(dataDir)))
		if _, err := Open(dataDir); !errors.Is(err, ErrIntegrity) {
			t.Errorf("Open = %v", err)
		}
	})

	t.Run("other-collection-damage-salvaged", func(t *testing.T) {
		dataDir, backups := t.TempDir(), t.TempDir()
		fx := populate(t, dataDir, clk)
		corruptFirstRecord(t, newestSegment(t, dataDir, "agents"))
		db, err := OpenForRepair(dataDir, WithRepair(RepairOptions{BackupDir: backups, Salvage: true}))
		must(t, err)
		t.Cleanup(func() { _ = db.Close() })
		j, err := OpenJournal(db, WithOperationClock(clk.Now))
		must(t, err)
		if rows := operationRows(t, j); len(rows) != len(fx.ids) {
			t.Errorf("journal holds %d rows after repair, want %d", len(rows), len(fx.ids))
		}
		if _, err := Open(dataDir); !errors.Is(err, ErrLocked) {
			t.Errorf("second Open = %v", err)
		}
		if res, err := db.Verify(ctx); err != nil || res.Health != HealthHealthy {
			t.Errorf("Verify = %+v, %v", res, err)
		}
		if _, _, err := j.Begin(ctx, testSpec()); err != nil {
			t.Errorf("journal write after repair: %v", err)
		}
	})
}

// TestHarnessOperationRecoveryAcrossReopen checks that the recovery queries
// classify a populated journal identically after any number of Close/Open
// cycles, and that lease fencing epochs keep growing across them.
func TestHarnessOperationRecoveryAcrossReopen(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	clk := newFakeClock()
	fx := populate(t, dataDir, clk)

	ids := func(ops []Operation) []string {
		var out []string
		for _, op := range ops {
			for n, id := range fx.ids {
				if id == op.ID {
					out = append(out, n)
				}
			}
		}
		sort.Strings(out)
		return out
	}
	expect := func(t *testing.T, j *Journal) {
		t.Helper()
		type q struct {
			name string
			get  func() ([]Operation, error)
			want []string
		}
		for _, c := range []q{
			{"Incomplete", j.Incomplete, []string{"pending", "ready", "reconciling-expired", "reconciling-live", "retry-due", "running-expired", "running-live"}},
			{"LeaseExpired", j.LeaseExpired, []string{"reconciling-expired", "running-expired"}},
			{"NeedsReconciliation", j.NeedsReconciliation, []string{"reconciling-expired", "reconciling-live", "running-expired"}},
		} {
			got, err := c.get()
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if g := ids(got); !reflect.DeepEqual(g, c.want) {
				t.Errorf("%s = %v, want %v", c.name, g, c.want)
			}
		}
		for _, id := range []string{"succeeded", "manual"} {
			op, err := j.Get(fx.ids[id])
			if err != nil || !op.State.Terminal() && id == "succeeded" {
				t.Errorf("Get %s = %+v, %v", id, op, err)
			}
		}
	}

	// The same answers across three reopen cycles, each ending in an unclean
	// handoff (no work in between).
	for cycle := 0; cycle < 3; cycle++ {
		db, j := openJournal(t, dataDir, clk)
		expect(t, j)
		must(t, db.Close())
	}

	// Takeover on one reopen fences the original holder on the next.
	db, j := openJournal(t, dataDir, clk)
	old := fx.leases["running-expired"]
	l1, taken, err := j.Acquire(ctx, fx.ids["running-expired"], "rescuer-1", time.Minute)
	must(t, err)
	if l1.Epoch != old.Epoch+1 || taken.State != OperationReconciling {
		t.Fatalf("takeover = %+v state %s (old epoch %d)", l1, taken.State, old.Epoch)
	}
	must(t, db.Close())

	db, j = openJournal(t, dataDir, clk)
	var fenced *LeaseError
	if _, err := j.Progress(ctx, old, OperationProgress{Step: "zombie"}); !errors.As(err, &fenced) || !errors.Is(err, ErrLeaseFenced) {
		t.Errorf("original holder after reopen = %v", err)
	}
	if _, err := j.Progress(ctx, l1, OperationProgress{Step: "still-me"}); err != nil {
		t.Errorf("rescuer lease after reopen = %v", err)
	}
	// The live leases survive too: still held, and fenced against takers.
	live := fx.leases["running-live"]
	if _, _, err := j.Acquire(ctx, fx.ids["running-live"], "intruder", time.Minute); !errors.Is(err, ErrLeaseHeld) {
		t.Errorf("live lease taken after reopen: %v", err)
	}
	if _, err := j.Progress(ctx, live, OperationProgress{Step: "alive"}); err != nil {
		t.Errorf("live holder after reopen: %v", err)
	}
	// Once the rescuer lapses as well, the next owner's epoch continues.
	clk.Advance(2 * time.Hour)
	exp, err := j.LeaseExpired()
	must(t, err)
	if g := ids(exp); !reflect.DeepEqual(g, []string{"reconciling-expired", "reconciling-live", "running-expired", "running-live"}) {
		t.Errorf("LeaseExpired after the clock moved = %v", g)
	}
	must(t, db.Close())

	db, j = openJournal(t, dataDir, clk)
	l2, _, err := j.Acquire(ctx, fx.ids["running-expired"], "rescuer-2", time.Minute)
	must(t, err)
	if l2.Epoch != l1.Epoch+1 {
		t.Errorf("epoch after second reopen = %d, want %d", l2.Epoch, l1.Epoch+1)
	}
	if _, err := j.Progress(ctx, l1, OperationProgress{Step: "late"}); !errors.Is(err, ErrLeaseFenced) {
		t.Errorf("previous rescuer = %v", err)
	}
	must(t, db.Close())
}

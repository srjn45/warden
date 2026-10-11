package wardendb

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/ownerlock"
)

type agentRec struct {
	ID string `json:"id"`
}

type planRec struct {
	ID string `json:"id"`
}

func mustOpen(t *testing.T, dataDir string, opts ...Option) *DB {
	t.Helper()
	db, err := Open(dataDir, opts...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestOpenOwnsDedicatedDirectory(t *testing.T) {
	dataDir := t.TempDir()
	db := mustOpen(t, dataDir)
	want := filepath.Join(dataDir, DirName)
	if db.Dir() != want || Dir(dataDir) != want {
		t.Fatalf("dir = %q / %q, want %q", db.Dir(), Dir(dataDir), want)
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != DirName {
		t.Fatalf("Open touched the data dir outside %s: %v", DirName, entries)
	}
	if _, err := db.Handle(); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if _, err := Open(""); err == nil {
		t.Fatal("Open(\"\") succeeded")
	}
}

func TestDoubleOpenSameProcessRejected(t *testing.T) {
	dataDir := t.TempDir()
	mustOpen(t, dataDir, WithOwner(ownerlock.Info{Kind: ownerlock.KindDaemon, Version: "test"}))

	_, err := Open(dataDir)
	var locked *LockedError
	if !errors.As(err, &locked) || !errors.Is(err, ErrLocked) {
		t.Fatalf("second Open err = %v, want *LockedError", err)
	}
	if locked.Dir != Dir(dataDir) && !strings.HasSuffix(locked.Dir, DirName) {
		t.Fatalf("LockedError.Dir = %q", locked.Dir)
	}
	if locked.Owner == nil || locked.Owner.PID != os.Getpid() || locked.Owner.Version != "test" {
		t.Fatalf("LockedError.Owner = %+v", locked.Owner)
	}
}

// TestHelperOpen is the child half of TestDoubleOpenOtherProcessRejected.
func TestHelperOpen(t *testing.T) {
	dataDir := os.Getenv("WARDENDB_HELPER_DATADIR")
	if dataDir == "" {
		t.Skip("helper process only")
	}
	if os.Getenv("WARDENDB_HELPER_MODE") == "probe" {
		st, err := Probe(dataDir)
		switch {
		case err != nil:
			os.Stdout.WriteString("RESULT:error " + err.Error() + "\n")
		case st.Owned && st.Owner != nil && st.Owner.PID == os.Getppid():
			os.Stdout.WriteString("RESULT:owned-by-parent\n")
		default:
			os.Stdout.WriteString("RESULT:unowned\n")
		}
		return
	}
	db, err := Open(dataDir)
	if err == nil {
		_ = db.Close()
		os.Stdout.WriteString("RESULT:opened\n")
		return
	}
	if errors.Is(err, ErrLocked) {
		os.Stdout.WriteString("RESULT:locked\n")
		return
	}
	os.Stdout.WriteString("RESULT:error " + err.Error() + "\n")
}

func helperOpen(t *testing.T, dataDir string) string {
	t.Helper()
	return helper(t, dataDir, "open")
}

func helper(t *testing.T, dataDir, mode string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperOpen$", "-test.v")
	cmd.Env = append(os.Environ(), "WARDENDB_HELPER_DATADIR="+dataDir, "WARDENDB_HELPER_MODE="+mode)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper: %v\n%s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "RESULT:") {
			return strings.TrimPrefix(line, "RESULT:")
		}
	}
	t.Fatalf("helper printed no result:\n%s", out)
	return ""
}

func TestDoubleOpenOtherProcessRejected(t *testing.T) {
	dataDir := t.TempDir()
	db := mustOpen(t, dataDir)
	if got := helperOpen(t, dataDir); got != "locked" {
		t.Fatalf("other process Open while owned = %q, want locked", got)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if got := helperOpen(t, dataDir); got != "opened" {
		t.Fatalf("other process Open after Close = %q, want opened", got)
	}
}

func TestRegistryRules(t *testing.T) {
	db := mustOpen(t, t.TempDir())

	agents, err := Register[agentRec](db, Spec{Name: "agents", Version: 1})
	if err != nil {
		t.Fatalf("Register agents: %v", err)
	}
	if agents.Name() != "agents" || agents.Version() != 1 {
		t.Fatalf("accessor = %s v%d", agents.Name(), agents.Version())
	}
	col, err := agents.Engine()
	if err != nil || col == nil || col.Name() != "agents" {
		t.Fatalf("Engine() = %v, %v", col, err)
	}

	for _, tc := range []struct {
		name string
		spec Spec
		want error
	}{
		{"unknown", Spec{Name: "not_in_contract", Version: 1}, ErrUnknownCollection},
		{"duplicate", Spec{Name: "agents", Version: 1}, ErrDuplicateCollection},
		{"empty name", Spec{Version: 1}, ErrInvalidSpec},
		{"zero version", Spec{Name: "plans"}, ErrInvalidSpec},
	} {
		if _, err := Register[agentRec](db, tc.spec); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	// A rejected registration leaves nothing behind.
	if _, err := Register[planRec](db, Spec{Name: "plans", Version: 3}); err != nil {
		t.Fatalf("Register plans after rejected attempt: %v", err)
	}

	if got, err := Lookup[agentRec](db, "agents"); err != nil || got.Name() != "agents" {
		t.Fatalf("Lookup agents = %v, %v", got, err)
	}
	if _, err := Lookup[planRec](db, "agents"); !errors.Is(err, ErrTypeMismatch) {
		t.Fatalf("Lookup wrong type err = %v", err)
	}
	if _, err := Lookup[agentRec](db, "projects"); !errors.Is(err, ErrNotRegistered) {
		t.Fatalf("Lookup unregistered err = %v", err)
	}

	infos, err := db.Registered()
	if err != nil {
		t.Fatal(err)
	}
	want := []CollectionInfo{
		{Name: "agents", Version: 1, RecordType: "wardendb.agentRec"},
		{Name: "plans", Version: 3, RecordType: "wardendb.planRec"},
	}
	if !reflect.DeepEqual(infos, want) {
		t.Fatalf("Registered = %+v, want %+v", infos, want)
	}
}

// The registry must match the collection contract §2 exactly.
func TestCatalogueMatchesContract(t *testing.T) {
	got := Catalogue()
	if len(got) != 29 {
		t.Fatalf("catalogue has %d collections, contract has 29 (26 + operations + plan_events + plan_notes)", len(got))
	}
	seen := map[string]bool{}
	for _, n := range got {
		if seen[n] {
			t.Fatalf("duplicate catalogue entry %q", n)
		}
		seen[n] = true
	}
	got[0] = "mutated"
	if Catalogue()[0] != "agents" {
		t.Fatal("Catalogue returned its backing array")
	}
}

func TestCloseOrdering(t *testing.T) {
	dataDir := t.TempDir()
	var trace []string
	db, err := Open(dataDir, func(c *config) { c.trace = func(s string) { trace = append(trace, s) } })
	if err != nil {
		t.Fatal(err)
	}

	agents, err := Register[agentRec](db, Spec{Name: "agents", Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	var plans *Collection[planRec]
	check := func(label string, wantAgents, wantPlans error) func() error {
		return func() error {
			if _, err := agents.Engine(); !errors.Is(err, wantAgents) {
				t.Errorf("%s: agents.Engine err = %v, want %v", label, err, wantAgents)
			}
			if _, err := plans.Engine(); !errors.Is(err, wantPlans) {
				t.Errorf("%s: plans.Engine err = %v, want %v", label, err, wantPlans)
			}
			// The lock is released last: still owned while closers run.
			if _, err := Open(dataDir); !errors.Is(err, ErrLocked) {
				t.Errorf("%s: Open during Close err = %v, want ErrLocked", label, err)
			}
			return nil
		}
	}
	// first sits between the two collections: plans is already retired when
	// it runs, agents is not.
	if err := db.OnClose("first", check("first", nil, ErrClosed)); err != nil {
		t.Fatal(err)
	}
	if plans, err = Register[planRec](db, Spec{Name: "plans", Version: 1}); err != nil {
		t.Fatal(err)
	}
	if err := db.OnClose("last", check("last", nil, nil)); err != nil {
		t.Fatal(err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	want := []string{"closer:last", "collection:plans", "closer:first", "collection:agents", "handle", "lock"}
	if !reflect.DeepEqual(trace, want) {
		t.Fatalf("close order = %v\nwant        %v", trace, want)
	}
}

func TestCloseRunsEveryStepAndJoinsErrors(t *testing.T) {
	dataDir := t.TempDir()
	db, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	errA, errB := errors.New("a failed"), errors.New("b failed")
	ran := 0
	_ = db.OnClose("a", func() error { ran++; return errA })
	_ = db.OnClose("b", func() error { ran++; return errB })

	err = db.Close()
	if !errors.Is(err, errA) || !errors.Is(err, errB) || ran != 2 {
		t.Fatalf("Close err = %v (ran %d), want both closer errors", err, ran)
	}
	// Idempotent: closers do not run again and the result is stable.
	if again := db.Close(); !errors.Is(again, errA) || ran != 2 {
		t.Fatalf("second Close err = %v (ran %d)", again, ran)
	}
	// A failing closer still releases the handle and the lock.
	mustOpen(t, dataDir)
}

func TestUseAfterClose(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	agents, err := Register[agentRec](db, Spec{Name: "agents", Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	if _, err := agents.Engine(); !errors.Is(err, ErrClosed) {
		t.Errorf("Engine after Close err = %v", err)
	}
	if _, err := db.Handle(); !errors.Is(err, ErrClosed) {
		t.Errorf("Handle after Close err = %v", err)
	}
	if _, err := Register[planRec](db, Spec{Name: "plans", Version: 1}); !errors.Is(err, ErrClosed) {
		t.Errorf("Register after Close err = %v", err)
	}
	if _, err := Lookup[agentRec](db, "agents"); !errors.Is(err, ErrClosed) {
		t.Errorf("Lookup after Close err = %v", err)
	}
	if _, err := db.Registered(); !errors.Is(err, ErrClosed) {
		t.Errorf("Registered after Close err = %v", err)
	}
	if err := db.OnClose("late", func() error { return nil }); !errors.Is(err, ErrClosed) {
		t.Errorf("OnClose after Close err = %v", err)
	}

	var nilDB *DB
	if err := nilDB.Close(); err != nil {
		t.Errorf("nil Close: %v", err)
	}
	if _, err := nilDB.Handle(); !errors.Is(err, ErrClosed) {
		t.Errorf("nil Handle err = %v", err)
	}
}

func TestReopenAfterClosePreservesData(t *testing.T) {
	dataDir := t.TempDir()
	db, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	agents, err := Register[agentRec](db, Spec{Name: "agents", Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	col, _ := agents.Engine()
	if _, err := col.Upsert("agent-1", map[string]any{"id": "agent-1"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db2 := mustOpen(t, dataDir)
	// The registry is per handle: nothing is registered until declared again.
	if infos, _ := db2.Registered(); len(infos) != 0 {
		t.Fatalf("reopened registry = %+v, want empty", infos)
	}
	agents2, err := Register[agentRec](db2, Spec{Name: "agents", Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	col2, err := agents2.Engine()
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := col2.Exists("agent-1"); err != nil || !ok {
		t.Fatalf("record lost across reopen: %v, %v", ok, err)
	}
}

func TestProbeIsReadOnly(t *testing.T) {
	dataDir := t.TempDir()

	st, err := Probe(dataDir)
	if err != nil || st.Exists || st.Owned || st.Dir != Dir(dataDir) {
		t.Fatalf("Probe on empty data dir = %+v, %v", st, err)
	}
	if entries, _ := os.ReadDir(dataDir); len(entries) != 0 {
		t.Fatalf("Probe created %v", entries)
	}

	db := mustOpen(t, dataDir)
	// This process is the owner, so Probe must neither report a foreign owner
	// nor disturb the held lock.
	st, err = Probe(dataDir)
	if err != nil || !st.Exists || st.Owned {
		t.Fatalf("Probe while self-owned = %+v, %v", st, err)
	}
	if _, err := Open(dataDir); !errors.Is(err, ErrLocked) {
		t.Fatalf("Open after Probe err = %v, want ErrLocked", err)
	}
	// A diagnostic process sees the owner without disturbing it.
	if got := helper(t, dataDir, "probe"); got != "owned-by-parent" {
		t.Fatalf("Probe from another process = %q, want owned-by-parent", got)
	}
	if _, err := db.Handle(); err != nil {
		t.Fatalf("Handle after foreign Probe: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if got := helper(t, dataDir, "probe"); got != "unowned" {
		t.Fatalf("Probe from another process after Close = %q, want unowned", got)
	}

	// Probing an unowned database must not take ownership.
	if st, err = Probe(dataDir); err != nil || !st.Exists || st.Owned {
		t.Fatalf("Probe after Close = %+v, %v", st, err)
	}
	mustOpen(t, dataDir)
}

package schema

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func touch(t *testing.T, dir, rel string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestConstantsStartAtOne(t *testing.T) {
	if SchemaVersion != 1 || MinSchema != 1 || LegacyBaseline != 1 {
		t.Fatalf("SchemaVersion=%d MinSchema=%d LegacyBaseline=%d, want all 1", SchemaVersion, MinSchema, LegacyBaseline)
	}
	if MinSchema > SchemaVersion || LegacyBaseline > SchemaVersion {
		t.Fatal("MinSchema and LegacyBaseline must not exceed SchemaVersion")
	}
}

func TestDecideGuardTable(t *testing.T) {
	// binary writes 14, migrates from 11.
	cases := []struct {
		name       string
		data       int
		inProgress bool
		want       Verdict
	}{
		{"equal", 14, false, VerdictStart},
		{"data newer", 15, false, VerdictDataNewer},
		{"older at MinSchema", 11, false, VerdictNeedsMigration},
		{"older within MinSchema", 13, false, VerdictNeedsMigration},
		{"below MinSchema", 10, false, VerdictUnsupported},
		{"in_progress, equal", 14, true, VerdictInterrupted},
		{"in_progress, older", 12, true, VerdictInterrupted},
		{"in_progress, newer", 15, true, VerdictInterrupted},
		{"in_progress, below min", 3, true, VerdictInterrupted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Decide(tc.data, 14, 11, tc.inProgress); got != tc.want {
				t.Fatalf("Decide(%d, 14, 11, %v) = %v, want %v", tc.data, tc.inProgress, got, tc.want)
			}
		})
	}
}

func TestCheckRefusalMessages(t *testing.T) {
	cases := []struct {
		name    string
		ledger  Ledger
		verdict Verdict
		want    []string
	}{
		{"data newer", Ledger{SchemaVersion: 15, BinaryVersion: "9.30.0"}, VerdictDataNewer,
			[]string{"newer warden (9.30.0)", "wd update", "schema 15", "schema 14"}},
		{"needs migration", Ledger{SchemaVersion: 12}, VerdictNeedsMigration,
			[]string{"`warden migrate`", "`wd update`"}},
		{"below min", Ledger{SchemaVersion: 4}, VerdictUnsupported,
			[]string{"unsupported direct upgrade", "schema 11"}},
		{"interrupted", Ledger{SchemaVersion: 13, InProgress: &InProgress{Migration: "0014-x", Step: "run"}}, VerdictInterrupted,
			[]string{"0014-x", "step run", "warden migrate --resume", "warden migrate --restore"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := check("/data", &tc.ledger, 14, 11)
			var ge *GuardError
			if !errors.As(err, &ge) {
				t.Fatalf("err = %v, want *GuardError", err)
			}
			if ge.Verdict != tc.verdict {
				t.Fatalf("verdict = %v, want %v", ge.Verdict, tc.verdict)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("message missing %q:\n%s", w, err)
				}
			}
		})
	}
	if err := check("/data", &Ledger{SchemaVersion: 14}, 14, 11); err != nil {
		t.Fatalf("equal schema must start, got %v", err)
	}
}

func TestBootFreshDirStampedAtCurrent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-yet-created")
	res, err := Boot(dir, "9.29.0")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stamped || res.StampErr != nil {
		t.Fatalf("stamped=%v stampErr=%v, want a clean stamp", res.Stamped, res.StampErr)
	}
	l, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if l.SchemaVersion != SchemaVersion || l.BinaryVersion != "9.29.0" || l.InProgress != nil {
		t.Fatalf("ledger = %+v", l)
	}
	if len(l.History) != 1 || l.History[0].Migration != MigrationBaselineFresh ||
		l.History[0].From != 0 || l.History[0].To != SchemaVersion {
		t.Fatalf("history = %+v", l.History)
	}
}

func TestBootLegacyInstallStampedAndBoots(t *testing.T) {
	dir := t.TempDir()
	for _, s := range LegacySentinels {
		touch(t, dir, s)
	}
	res, err := Boot(dir, "9.29.0")
	if err != nil {
		t.Fatalf("an existing install must boot after being stamped: %v", err)
	}
	if !res.Stamped {
		t.Fatal("legacy install was not stamped")
	}
	l, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if l.SchemaVersion != LegacyBaseline {
		t.Fatalf("schema_version = %d, want legacy baseline %d", l.SchemaVersion, LegacyBaseline)
	}
	h := l.History[0]
	if h.Migration != MigrationBaselineLegacy || len(h.Sentinels) != len(LegacySentinels) {
		t.Fatalf("history[0] = %+v", h)
	}
	// Sentinels are read, never retired here.
	for _, s := range LegacySentinels {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(s))); err != nil {
			t.Errorf("sentinel %s was removed: %v", s, err)
		}
	}
}

func TestInferBaseline(t *testing.T) {
	t.Run("empty dir is fresh", func(t *testing.T) {
		b := InferBaseline(t.TempDir())
		if !b.Fresh || b.Version != SchemaVersion || len(b.Sentinels) != 0 {
			t.Fatalf("baseline = %+v", b)
		}
	})
	t.Run("unrelated files stay fresh", func(t *testing.T) {
		dir := t.TempDir()
		touch(t, dir, "config.yaml")
		touch(t, dir, ".warden-owner.lock")
		touch(t, dir, "tui.log")
		if b := InferBaseline(dir); !b.Fresh {
			t.Fatalf("baseline = %+v, want fresh", b)
		}
	})
	t.Run("each sentinel alone marks legacy", func(t *testing.T) {
		for _, s := range LegacySentinels {
			dir := t.TempDir()
			touch(t, dir, s)
			b := InferBaseline(dir)
			if b.Fresh || b.Version != LegacyBaseline || !reflect.DeepEqual(b.Sentinels, []string{s}) {
				t.Errorf("%s: baseline = %+v", s, b)
			}
		}
	})
	t.Run("store data without sentinels is legacy", func(t *testing.T) {
		dir := t.TempDir()
		touch(t, dir, "agents-db/active/data.log")
		b := InferBaseline(dir)
		if b.Fresh || b.Version != LegacyBaseline || len(b.Sentinels) != 0 {
			t.Fatalf("baseline = %+v", b)
		}
	})
}

func TestEnsureStampsExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	touch(t, dir, ".sessions-filedb-imported")

	first, stamped, err := Ensure(dir, "9.29.0")
	if err != nil || !stamped {
		t.Fatalf("first Ensure: stamped=%v err=%v", stamped, err)
	}
	before, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatal(err)
	}

	// The dir now looks different (another sentinel, a newer binary, a later
	// clock): none of it may re-stamp or rewrite the ledger.
	touch(t, dir, ".pipelines-filedb-imported")
	old := now
	now = func() time.Time { return old().Add(48 * time.Hour) }
	defer func() { now = old }()

	second, stamped, err := Ensure(dir, "9.31.0")
	if err != nil || stamped {
		t.Fatalf("second Ensure: stamped=%v err=%v, want untouched", stamped, err)
	}
	after, _ := os.ReadFile(Path(dir))
	if string(before) != string(after) {
		t.Fatalf("ledger rewritten:\n%s\n---\n%s", before, after)
	}
	if second.BinaryVersion != first.BinaryVersion || len(second.History) != 1 {
		t.Fatalf("second = %+v", second)
	}
}

func TestEnsureConcurrentStampWritesOnce(t *testing.T) {
	dir := t.TempDir()
	const n = 16
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		stamps int
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, stamped, err := Ensure(dir, "9.29.0")
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			if stamped {
				stamps++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if stamps != 1 {
		t.Fatalf("%d goroutines stamped, want exactly 1", stamps)
	}
	if l, err := Load(dir); err != nil || len(l.History) != 1 {
		t.Fatalf("ledger = %+v err=%v", l, err)
	}
}

func TestSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	l := &Ledger{SchemaVersion: 1, BinaryVersion: "9.29.0"}
	if err := Save(dir, l); err != nil {
		t.Fatal(err)
	}
	l.InProgress = &InProgress{Migration: "0002-x", Step: "run", Snapshot: "backups/pre"}
	l.History = append(l.History, HistoryEntry{From: 1, To: 2, Migration: "0002-x", Backup: "backups/pre"})
	if err := Save(dir, l); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.InProgress == nil || got.InProgress.Snapshot != "backups/pre" || len(got.History) != 1 {
		t.Fatalf("round trip = %+v", got)
	}
	// No temp file is left behind, and the ledger is private.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != FileName {
		t.Fatalf("dir entries = %v, want only %s", entries, FileName)
	}
	if fi, _ := os.Stat(Path(dir)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestSaveFailureKeepsPreviousLedger(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	if err := Save(dir, &Ledger{SchemaVersion: 1, BinaryVersion: "old"}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(Path(dir))
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if err := Save(dir, &Ledger{SchemaVersion: 2, BinaryVersion: "new"}); err == nil {
		t.Fatal("Save into a read-only dir succeeded")
	}
	after, _ := os.ReadFile(Path(dir))
	if string(before) != string(after) {
		t.Fatal("a failed Save changed the committed ledger")
	}
}

func TestLedgerWireFormat(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := Ensure(dir, "9.29.0"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(Path(dir))
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"schema_version", "binary_version", "history", "in_progress"} {
		if _, ok := m[k]; !ok {
			t.Errorf("ledger missing key %q:\n%s", k, raw)
		}
	}
	if string(m["in_progress"]) != "null" {
		t.Errorf("in_progress = %s, want null", m["in_progress"])
	}
	// An empty history is [] on disk, not null.
	if err := Save(dir, &Ledger{SchemaVersion: 1}); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(Path(dir))
	if !strings.Contains(string(raw), `"history": []`) {
		t.Errorf("empty history not encoded as []:\n%s", raw)
	}
}

func TestLoadErrors(t *testing.T) {
	if _, err := Load(t.TempDir()); !errors.Is(err, ErrNoLedger) {
		t.Fatalf("missing ledger: err = %v, want ErrNoLedger", err)
	}
	for name, body := range map[string]string{
		"truncated":    `{"schema_version": 1,`,
		"zero version": `{"schema_version": 0}`,
		"empty":        ``,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			touch(t, dir, ".sessions-filedb-imported")
			if err := os.WriteFile(Path(dir), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			var ce *CorruptError
			if _, err := Load(dir); !errors.As(err, &ce) {
				t.Fatalf("Load err = %v, want *CorruptError", err)
			}
			// Boot refuses and leaves the damaged file for the operator.
			if _, err := Boot(dir, "9.29.0"); !errors.As(err, &ce) {
				t.Fatalf("Boot err = %v, want *CorruptError", err)
			}
			if got, _ := os.ReadFile(Path(dir)); string(got) != body {
				t.Fatal("a corrupt ledger was overwritten")
			}
		})
	}
}

func TestBootRefusesAndLeavesLedgerUntouched(t *testing.T) {
	cases := map[string]struct {
		ledger  Ledger
		verdict Verdict
	}{
		"data newer":  {Ledger{SchemaVersion: SchemaVersion + 1, BinaryVersion: "99.0.0"}, VerdictDataNewer},
		"in_progress": {Ledger{SchemaVersion: SchemaVersion, InProgress: &InProgress{Migration: "0002-x"}}, VerdictInterrupted},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := Save(dir, &tc.ledger); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(Path(dir))
			_, err := Boot(dir, "9.29.0")
			var ge *GuardError
			if !errors.As(err, &ge) || ge.Verdict != tc.verdict {
				t.Fatalf("Boot err = %v, want verdict %v", err, tc.verdict)
			}
			if after, _ := os.ReadFile(Path(dir)); string(before) != string(after) {
				t.Fatal("a refused boot modified the ledger")
			}
		})
	}
}

func TestBootEqualLedgerStartsWithoutRewriting(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, &Ledger{SchemaVersion: SchemaVersion, BinaryVersion: "9.20.0"}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(Path(dir))
	res, err := Boot(dir, "9.29.0")
	if err != nil || res.Stamped {
		t.Fatalf("Boot: stamped=%v err=%v", res.Stamped, err)
	}
	if after, _ := os.ReadFile(Path(dir)); string(before) != string(after) {
		t.Fatal("boot rewrote an existing ledger")
	}
}

// An install that predates the ledger must keep booting even when the stamp
// itself cannot be written.
func TestBootUnwritableLegacyDirStillBoots(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	touch(t, dir, ".agents-from-sessions-imported")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	res, err := Boot(dir, "9.29.0")
	if err != nil {
		t.Fatalf("Boot refused an existing install: %v", err)
	}
	var se *StampError
	if res.Stamped || !errors.As(res.StampErr, &se) {
		t.Fatalf("stamped=%v stampErr=%v, want an unstamped boot with StampError", res.Stamped, res.StampErr)
	}
	if res.Ledger == nil || res.Ledger.SchemaVersion != LegacyBaseline {
		t.Fatalf("ledger = %+v", res.Ledger)
	}
}

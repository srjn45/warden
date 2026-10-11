package wardendb

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// agentCount registers "agents" on db and returns how many of the seeded
// records it can read.
func agentCount(t *testing.T, db *DB) int {
	t.Helper()
	agents, err := Register[agentRec](db, Spec{Name: "agents", Version: 1})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	col, err := agents.Engine()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, id := range []string{"agent-1", "agent-2", "agent-3"} {
		if _, err := col.GetByKey(id); err == nil {
			n++
		}
	}
	return n
}

func TestHealthRules(t *testing.T) {
	cases := []struct {
		name                  string
		exists                bool
		lock                  LockState
		blocking, recoverable int
		want                  Health
	}{
		{"missing", false, LockUnowned, 0, 0, HealthMissing},
		{"missing wins over everything", false, LockOwnedByOther, 3, 3, HealthMissing},
		{"clean", true, LockUnowned, 0, 0, HealthHealthy},
		{"clean, own handle", true, LockOwnedBySelf, 0, 0, HealthHealthy},
		{"recoverable only", true, LockUnowned, 0, 2, HealthDegraded},
		{"recoverable, own handle", true, LockOwnedBySelf, 0, 1, HealthDegraded},
		{"blocking", true, LockUnowned, 1, 0, HealthCorrupt},
		{"blocking and recoverable", true, LockUnowned, 1, 5, HealthCorrupt},
		{"owned by other", true, LockOwnedByOther, 0, 0, HealthLocked},
		{"owned by other hides recoverable", true, LockOwnedByOther, 0, 4, HealthLocked},
		{"open elsewhere", true, LockOpenElsewhere, 0, 0, HealthLocked},
		{"corrupt wins over locked", true, LockOwnedByOther, 1, 0, HealthCorrupt},
	}
	for _, c := range cases {
		if got := healthOf(c.exists, c.lock, c.blocking, c.recoverable); got != c.want {
			t.Errorf("%s: healthOf = %q, want %q", c.name, got, c.want)
		}
	}
	for h, want := range map[Health]bool{
		HealthHealthy: true, HealthDegraded: true, HealthCorrupt: false, HealthMissing: false, HealthLocked: false,
	} {
		if h.Openable() != want {
			t.Errorf("%q.Openable() = %v, want %v", h, !want, want)
		}
	}
}

func TestClassOf(t *testing.T) {
	cases := []struct {
		f    Finding
		want FindingClass
	}{
		{Finding{Severity: severityRepairableIndex, Code: "index-missing"}, ClassDerived},
		{Finding{Severity: severityDataCorruption, Code: "record-corrupt"}, ClassSegmentDamage},
		{Finding{Severity: severityConflict, Code: "duplicate-id"}, ClassConflict},
		{Finding{Severity: severityDataCorruption, Code: "xtx-journal-corrupt"}, ClassTransaction},
		{Finding{Severity: "info", Code: codeXTxUndecided}, ClassCrashLeftover},
		{Finding{Severity: "info", Code: "torn-tail"}, ClassCrashLeftover},
	}
	for _, c := range cases {
		if got := ClassOf(c.f); got != c.want {
			t.Errorf("ClassOf(%s/%s) = %q, want %q", c.f.Severity, c.f.Code, got, c.want)
		}
		if ClassOf(c.f).RepairPolicy() == "" {
			t.Errorf("%q has no repair policy", c.want)
		}
	}
}

func TestVerifyMissingDirectory(t *testing.T) {
	dataDir := t.TempDir()
	res, err := Verify(context.Background(), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if res.Exists || res.Health != HealthMissing || len(res.Collections) != 0 {
		t.Errorf("result = %+v, want missing", res)
	}
	if _, err := os.Stat(Dir(dataDir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Verify created the directory: %v", err)
	}
}

func TestVerifyCleanDatabaseIsHealthy(t *testing.T) {
	dataDir := t.TempDir()
	seed(t, dataDir)

	// Unowned, read-only.
	res, err := Verify(context.Background(), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if res.Health != HealthHealthy || res.Online || res.Lock.State != LockUnowned || res.Blocking != 0 || res.Recoverable != 0 {
		t.Fatalf("unowned verify = %+v", res)
	}
	agents := res.Collection("agents")
	if agents == nil || agents.Health != HealthHealthy || agents.LiveRecords != 3 || agents.Segments == 0 || agents.Registered {
		t.Fatalf("agents status = %+v", agents)
	}
	if res.FinishedAt.Before(res.StartedAt) || res.StartedAt.IsZero() {
		t.Errorf("timestamps: %v .. %v", res.StartedAt, res.FinishedAt)
	}

	// Owned.
	rec := &recorder{}
	db := mustOpen(t, dataDir, observed(rec)...)
	if n := agentCount(t, db); n != 3 {
		t.Fatalf("read %d records", n)
	}
	res, err = db.Verify(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Health != HealthHealthy || !res.Online || res.Lock.State != LockOwnedBySelf {
		t.Fatalf("owned verify = %+v", res)
	}
	agents = res.Collection("agents")
	if agents == nil || !agents.Online || !agents.Registered || agents.LiveRecords != 3 {
		t.Fatalf("agents status = %+v", agents)
	}
	if ev := rec.event(ActionVerify); ev == nil || ev.Detail["health"] != string(HealthHealthy) {
		t.Errorf("verify event = %+v", ev)
	}
	// A clean verification leaves the handle usable.
	if _, err := db.Handle(); err != nil {
		t.Errorf("Handle after clean Verify: %v", err)
	}
}

func TestVerifyCorruptSegment(t *testing.T) {
	dataDir := t.TempDir()
	seed(t, dataDir)
	seg := newestSegment(t, dataDir, "agents")
	corruptFirstRecord(t, seg)
	before := snapshot(t, Dir(dataDir))

	res, err := Verify(context.Background(), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if res.Health != HealthCorrupt || res.Blocking == 0 || res.Health.Openable() {
		t.Fatalf("verify = health %q, blocking %d", res.Health, res.Blocking)
	}
	agents := res.Collection("agents")
	if agents == nil || agents.Health != HealthCorrupt || len(agents.Blocking) == 0 {
		t.Fatalf("agents status = %+v", agents)
	}
	f := agents.Blocking[0]
	if f.Collection != "agents" || f.Segment != filepath.Base(seg) || f.Code == "" || ClassOf(f) != ClassSegmentDamage {
		t.Errorf("finding = %+v (class %q)", f, ClassOf(f))
	}
	sameSnapshot(t, before, snapshot(t, Dir(dataDir)))

	// The verdict agrees with the strict Open.
	if _, err := Open(dataDir); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("Open err = %v, want ErrIntegrity", err)
	}
	sameSnapshot(t, before, snapshot(t, Dir(dataDir)))
}

func TestVerifyIsReadOnlyAndTakesNoOwnership(t *testing.T) {
	dataDir := t.TempDir()
	seed(t, dataDir)
	// Remove the ownership bookkeeping so that creating it would show up.
	for _, name := range []string{".warden-owner.lock", "LOCK"} {
		_ = os.Remove(filepath.Join(Dir(dataDir), name))
	}
	list := func() []string {
		ents, err := os.ReadDir(Dir(dataDir))
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range ents {
			out = append(out, e.Name())
		}
		return out
	}
	names, before := list(), snapshot(t, Dir(dataDir))

	if _, err := Verify(context.Background(), dataDir); err != nil {
		t.Fatal(err)
	}
	if _, err := Diagnose(context.Background(), dataDir); err != nil {
		t.Fatal(err)
	}
	sameSnapshot(t, before, snapshot(t, Dir(dataDir)))
	if got := list(); strings.Join(got, ",") != strings.Join(names, ",") {
		t.Errorf("directory entries changed: %v -> %v", names, got)
	}
	st, err := Probe(dataDir)
	if err != nil || st.Owned {
		t.Fatalf("Probe after Verify = %+v, %v", st, err)
	}

	// Nothing is held: the database opens, and while it is open a read-only
	// verification says so without disturbing the owner.
	db := mustOpen(t, dataDir)
	res, err := Verify(context.Background(), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if res.Health != HealthLocked || res.Lock.State != LockOpenElsewhere {
		t.Errorf("verify of an open database = health %q, lock %q", res.Health, res.Lock.State)
	}
	if n := agentCount(t, db); n != 3 {
		t.Errorf("owner reads %d records after a concurrent Verify", n)
	}
}

func TestVerifyOnOwnedHandleRetiresItOnCorruption(t *testing.T) {
	dataDir := t.TempDir()
	seed(t, dataDir)
	// A second collection directory the handle never opens through Register is
	// still verified, at rest.
	rec := &recorder{}
	db := mustOpen(t, dataDir, observed(rec)...)
	if _, err := Register[planRec](db, Spec{Name: "plans", Version: 1}); err != nil {
		t.Fatal(err)
	}
	res, err := db.Verify(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Health != HealthHealthy || res.Collection("agents") == nil || res.Collection("plans") == nil {
		t.Fatalf("verify = %+v", res)
	}
	if res.Collection("agents").Registered || !res.Collection("plans").Registered {
		t.Errorf("registered flags: agents=%v plans=%v", res.Collection("agents").Registered, res.Collection("plans").Registered)
	}

	// Damage the bytes under the open handle.
	corruptFirstRecord(t, newestSegment(t, dataDir, "agents"))
	res, err = db.Verify(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Health != HealthCorrupt || res.Blocking == 0 {
		t.Fatalf("verify after damage = health %q, blocking %d", res.Health, res.Blocking)
	}
	var ie *IntegrityError
	if _, err := db.Handle(); !errors.As(err, &ie) || ie.Stage != StageVerify {
		t.Fatalf("Handle after corrupt Verify = %v", err)
	}
	if _, err := db.Verify(context.Background()); !errors.Is(err, ErrIntegrity) {
		t.Errorf("Verify on a retired handle = %v", err)
	}
	if ev := rec.event(ActionIntegrityRefused); ev == nil || ev.Detail["stage"] != StageVerify {
		t.Error("no integrity violation was audited")
	}
}

func TestReportIsSerializableAndCarriesNoPayload(t *testing.T) {
	dataDir := t.TempDir()
	seed(t, dataDir)
	corruptFirstRecord(t, newestSegment(t, dataDir, "agents"))

	rep, err := Diagnose(context.Background(), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Dir != Dir(dataDir) || rep.SchemaVersion != SchemaVersion || rep.Health != HealthCorrupt ||
		!rep.Exists || rep.Online || rep.Lock.State != LockUnowned || rep.Blocking == 0 ||
		rep.GeneratedAt.IsZero() || rep.VerifyStartedAt.IsZero() || rep.VerifyFinishedAt.IsZero() {
		t.Fatalf("report = %+v", rep)
	}
	if len(rep.Collections) != 1 || rep.Collections[0].Name != "agents" || len(rep.Collections[0].Findings) == 0 {
		t.Fatalf("collections = %+v", rep.Collections)
	}
	f := rep.Collections[0].Findings[0]
	if !f.Blocking || f.Code == "" || f.Class != ClassSegmentDamage || f.Segment == "" {
		t.Errorf("finding = %+v", f)
	}

	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Health != rep.Health || len(back.Collections) != 1 {
		t.Errorf("round trip = %+v", back)
	}
	out := string(b)
	for _, leak := range []string{"agent-1", "agent-2", "agent-3", "####", "message", "command", "addr"} {
		if strings.Contains(strings.ToLower(out), leak) {
			t.Errorf("report leaks %q: %s", leak, out)
		}
	}

	// A live owner is identified, without its command line or address.
	clean := t.TempDir()
	db := mustOpen(t, clean)
	live, err := db.Diagnose(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !live.Online || live.Lock.State != LockOwnedBySelf || live.Health != HealthHealthy {
		t.Errorf("live report = %+v", live)
	}
}

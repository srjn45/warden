package wardendb

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"

	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/dbtx"
)

// recorder is an Auditor and a Metrics sink.
type recorder struct {
	mu       sync.Mutex
	events   []audit.Event
	counts   map[string]int
	observed map[string]int
}

func (r *recorder) Log(ev audit.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recorder) Inc(name, collection string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.counts == nil {
		r.counts = map[string]int{}
	}
	r.counts[name+"|"+collection]++
}

func (r *recorder) Observe(name, collection string, _ time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.observed == nil {
		r.observed = map[string]int{}
	}
	r.observed[name+"|"+collection]++
}

func (r *recorder) count(name, collection string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counts[name+"|"+collection]
}

func (r *recorder) observations(name, collection string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.observed[name+"|"+collection]
}

// event returns the last audited event for action, or nil.
func (r *recorder) event(action string) *audit.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.events) - 1; i >= 0; i-- {
		if r.events[i].Action == action {
			ev := r.events[i]
			return &ev
		}
	}
	return nil
}

func observed(rec *recorder) []Option {
	return []Option{WithAuditor(rec), WithMetrics(rec)}
}

// snapshot reads every data file under dir. The two lock files are skipped:
// they are ownership bookkeeping, not data.
func snapshot(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		switch d.Name() {
		case ".warden-owner.lock", "LOCK":
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		out[rel] = b
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sameSnapshot(t *testing.T, before, after map[string][]byte) {
	t.Helper()
	names := func(m map[string][]byte) []string {
		var s []string
		for k := range m {
			s = append(s, k)
		}
		sort.Strings(s)
		return s
	}
	for _, name := range names(before) {
		got, ok := after[name]
		if !ok {
			t.Errorf("%s was removed", name)
		} else if !bytes.Equal(before[name], got) {
			t.Errorf("%s was modified (%d -> %d bytes)", name, len(before[name]), len(got))
		}
	}
	for _, name := range names(after) {
		if _, ok := before[name]; !ok {
			t.Errorf("%s was created", name)
		}
	}
}

// seed writes three records to "agents" and closes the database.
func seed(t *testing.T, dataDir string) {
	t.Helper()
	db, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	agents, err := Register[agentRec](db, Spec{Name: "agents", Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	col, _ := agents.Engine()
	for _, id := range []string{"agent-1", "agent-2", "agent-3"} {
		if _, err := col.Upsert(id, map[string]any{"id": id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// newestSegment returns the path of a collection's last segment file.
func newestSegment(t *testing.T, dataDir, collection string) string {
	t.Helper()
	segs, err := filepath.Glob(filepath.Join(Dir(dataDir), collection, "seg_*.ndjson"))
	if err != nil || len(segs) == 0 {
		t.Fatalf("no segments for %s: %v", collection, err)
	}
	sort.Strings(segs)
	return segs[len(segs)-1]
}

// corruptFirstRecord damages the first record of the segment in place, without
// changing the file length, and returns the bytes it left behind.
func corruptFirstRecord(t *testing.T, seg string) []byte {
	t.Helper()
	b, err := os.ReadFile(seg)
	if err != nil {
		t.Fatal(err)
	}
	nl := bytes.IndexByte(b, '\n')
	if nl < 8 || nl == len(b)-1 {
		t.Fatalf("segment %s has no complete leading record followed by more data", seg)
	}
	for i := 0; i < nl; i++ {
		b[i] = '#'
	}
	if err := os.WriteFile(seg, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestStrictPolicyAppliedToEveryCollection(t *testing.T) {
	db := mustOpen(t, t.TempDir())
	for _, name := range []string{"agents", "plans"} {
		c, err := Register[agentRec](db, Spec{Name: name, Version: 1})
		if err != nil {
			t.Fatal(err)
		}
		col, _ := c.Engine()
		cfg := col.Config()
		if cfg.SyncMode != engine.SyncModeAlways {
			t.Errorf("%s sync mode = %q, want always", name, cfg.SyncMode)
		}
		if cfg.IntegrityPolicy != engine.PolicyFail && cfg.IntegrityPolicy != "" {
			t.Errorf("%s integrity policy = %q, want fail", name, cfg.IntegrityPolicy)
		}
		if cfg.OnIntegrity == nil || cfg.OnCompaction == nil || cfg.OnSegmentPoisoned == nil {
			t.Errorf("%s is missing an integrity/compaction callback", name)
		}
		if cfg.CompactInterval != DefaultCompactInterval {
			t.Errorf("%s compact interval = %s, want %s", name, cfg.CompactInterval, DefaultCompactInterval)
		}
		if cfg.CompactDirtyPct != DefaultCompactDirtyRatio {
			t.Errorf("%s dirty ratio = %v, want %v", name, cfg.CompactDirtyPct, DefaultCompactDirtyRatio)
		}
	}
}

func TestSpecCannotWeakenPolicy(t *testing.T) {
	db := mustOpen(t, t.TempDir())
	c, err := Register[agentRec](db, Spec{Name: "agents", Version: 1, Options: []scriva.CollectionOption{
		scriva.WithCollectionSyncMode(engine.SyncModeNone),
		scriva.WithCollectionCompactInterval(time.Second),
		scriva.WithMaxRecords(7),
		nil,
	}})
	if err != nil {
		t.Fatal(err)
	}
	col, _ := c.Engine()
	cfg := col.Config()
	if cfg.SyncMode != engine.SyncModeAlways {
		t.Errorf("sync mode = %q, want the policy to override the spec", cfg.SyncMode)
	}
	if cfg.CompactInterval != DefaultCompactInterval {
		t.Errorf("compact interval = %s, want the policy to override the spec", cfg.CompactInterval)
	}
	// Options outside the policy still apply.
	if cfg.MaxRecords != 7 {
		t.Errorf("max records = %d, want 7", cfg.MaxRecords)
	}
}

func TestCollectionOpenedBehindRegisterIsRefused(t *testing.T) {
	rec := &recorder{}
	db := mustOpen(t, t.TempDir(), observed(rec)...)
	h, err := db.Handle()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Collection("plans", scriva.WithCollectionSyncMode(engine.SyncModeNone)); err != nil {
		t.Fatal(err)
	}
	_, err = Register[planRec](db, Spec{Name: "plans", Version: 1})
	var pe *PolicyError
	if !errors.As(err, &pe) || !errors.Is(err, ErrPolicy) {
		t.Fatalf("Register err = %v, want *PolicyError", err)
	}
	if pe.Collection != "plans" || pe.Setting != "sync mode" || pe.Got != "none" || pe.Want != "always" {
		t.Errorf("PolicyError = %+v", pe)
	}
	if rec.count(ActionPolicyRefused, "plans") != 1 || rec.event(ActionPolicyRefused) == nil {
		t.Errorf("policy refusal not reported")
	}
	if infos, _ := db.Registered(); len(infos) != 0 {
		t.Errorf("refused collection was registered: %+v", infos)
	}
	// A policy refusal is the caller's mistake, not corruption: the handle
	// stays usable.
	if _, err := Register[agentRec](db, Spec{Name: "agents", Version: 1}); err != nil {
		t.Errorf("Register after a policy refusal: %v", err)
	}
}

func TestCompactionLimits(t *testing.T) {
	for name, l := range map[string]CompactionLimits{
		"interval too short": {Interval: time.Second},
		"interval too long":  {Interval: 48 * time.Hour},
		"negative interval":  {Interval: -time.Minute},
		"ratio too low":      {DirtyRatio: 0.01},
		"ratio too high":     {DirtyRatio: 0.99},
		"negative ratio":     {DirtyRatio: -0.5},
	} {
		t.Run(name, func(t *testing.T) {
			dataDir := t.TempDir()
			db, err := Open(dataDir, WithCompaction(l))
			if !errors.Is(err, ErrInvalidOption) {
				_ = db.Close()
				t.Fatalf("Open err = %v, want ErrInvalidOption", err)
			}
			if _, err := os.Stat(Dir(dataDir)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a rejected option still created the directory: %v", err)
			}
		})
	}

	rec := &recorder{}
	want := CompactionLimits{Interval: 2 * time.Hour, DirtyRatio: 0.5}
	db := mustOpen(t, t.TempDir(), append(observed(rec), WithCompaction(want))...)
	c, err := Register[agentRec](db, Spec{Name: "agents", Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	col, _ := c.Engine()
	if cfg := col.Config(); cfg.CompactInterval != want.Interval || cfg.CompactDirtyPct != want.DirtyRatio {
		t.Errorf("limits = %s / %v, want %s / %v", cfg.CompactInterval, cfg.CompactDirtyPct, want.Interval, want.DirtyRatio)
	}
	ev := rec.event(ActionOpen)
	if ev == nil || ev.Detail["compact_interval"] != "2h0m0s" || ev.Detail["compact_dirty_ratio"] != "0.5" {
		t.Errorf("open event does not carry the limits: %+v", ev)
	}
}

func TestOpenRefusesCorruptCollection(t *testing.T) {
	dataDir := t.TempDir()
	seed(t, dataDir)
	corruptFirstRecord(t, newestSegment(t, dataDir, "agents"))
	before := snapshot(t, Dir(dataDir))

	rec := &recorder{}
	for attempt := 1; attempt <= 2; attempt++ {
		db, err := Open(dataDir, observed(rec)...)
		var ie *IntegrityError
		if !errors.As(err, &ie) || !errors.Is(err, ErrIntegrity) {
			_ = db.Close()
			t.Fatalf("attempt %d: Open err = %v, want *IntegrityError", attempt, err)
		}
		if db != nil {
			t.Fatalf("attempt %d: Open returned a handle with an integrity error", attempt)
		}
		if ie.Stage != StageOpen || ie.Collection != "agents" || ie.Dir != Dir(dataDir) {
			t.Errorf("attempt %d: IntegrityError = %+v", attempt, ie)
		}
		if ie.Detail == "" || ie.Total < 1 || len(ie.Findings) < 1 || ie.Findings[0].Collection != "agents" {
			t.Errorf("attempt %d: IntegrityError carries no finding: %+v", attempt, ie)
		}
		// Refusing must not change, salvage or truncate anything.
		sameSnapshot(t, before, snapshot(t, Dir(dataDir)))
		// The ownership lock was released, so the directory stays diagnosable.
		if st, err := Probe(dataDir); err != nil || st.Owned {
			t.Errorf("attempt %d: Probe after refusal = %+v, %v", attempt, st, err)
		}
	}
	if got := rec.count(ActionIntegrityRefused, "agents"); got != 2 {
		t.Errorf("integrity refusals counted = %d, want 2", got)
	}
	ev := rec.event(ActionIntegrityRefused)
	if ev == nil || ev.Target != "agents" || ev.Detail["stage"] != StageOpen || ev.Detail["dir"] != Dir(dataDir) {
		t.Errorf("integrity refusal event = %+v", ev)
	}
	if rec.count(ActionOpen, "") != 0 {
		t.Errorf("a refused open was reported as an open")
	}
}

func TestRegisterRefusesCorruptCollection(t *testing.T) {
	dataDir := t.TempDir()
	seed(t, dataDir)

	rec := &recorder{}
	db := mustOpen(t, dataDir, observed(rec)...)
	plans, err := Register[planRec](db, Spec{Name: "plans", Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	// The directory verified clean at Open; the damage appears afterwards.
	seg := newestSegment(t, dataDir, "agents")
	damaged := corruptFirstRecord(t, seg)

	_, err = Register[agentRec](db, Spec{Name: "agents", Version: 1})
	var ie *IntegrityError
	if !errors.As(err, &ie) || !errors.Is(err, ErrIntegrity) {
		t.Fatalf("Register err = %v, want *IntegrityError", err)
	}
	if ie.Stage != StageRegister || ie.Collection != "agents" || ie.Detail == "" {
		t.Errorf("IntegrityError = %+v", ie)
	}
	if got, err := os.ReadFile(seg); err != nil || !bytes.Equal(got, damaged) {
		t.Errorf("the refused segment was modified (%d -> %d bytes, %v)", len(damaged), len(got), err)
	}
	if rec.count(ActionIntegrityRefused, "agents") != 1 || rec.count(ActionRegister, "agents") != 0 {
		t.Errorf("refusal not reported, or reported as a registration")
	}

	// The violation retires the whole handle, including what was already
	// handed out.
	if _, err := db.Handle(); !errors.Is(err, ErrIntegrity) {
		t.Errorf("Handle after violation = %v", err)
	}
	if _, err := plans.Engine(); !errors.Is(err, ErrIntegrity) {
		t.Errorf("Engine after violation = %v", err)
	}
	if _, err := Lookup[planRec](db, "plans"); !errors.Is(err, ErrIntegrity) {
		t.Errorf("Lookup after violation = %v", err)
	}
	if _, err := Register[agentRec](db, Spec{Name: "terminals", Version: 1}); !errors.Is(err, ErrIntegrity) {
		t.Errorf("Register after violation = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Errorf("Close of a retired handle: %v", err)
	}
	if got, err := os.ReadFile(seg); err != nil || !bytes.Equal(got, damaged) {
		t.Errorf("Close modified the refused segment")
	}
	// And the next Open refuses it up front.
	if _, err := Open(dataDir); !errors.Is(err, ErrIntegrity) {
		t.Errorf("reopen err = %v, want ErrIntegrity", err)
	}
}

func TestTornTailIsRecoveredLoudly(t *testing.T) {
	dataDir := t.TempDir()
	seed(t, dataDir)
	// A crash mid-append leaves an incomplete last line that was never
	// acknowledged to any writer.
	f, err := os.OpenFile(newestSegment(t, dataDir, "agents"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"id":99,"op":"ins`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	rec := &recorder{}
	db := mustOpen(t, dataDir, observed(rec)...)
	if rec.count(ActionRecovery, "") != 1 {
		t.Fatalf("a torn tail was reconciled without an %s event", ActionRecovery)
	}
	if ev := rec.event(ActionRecovery); ev == nil || ev.Detail["findings"] == "" {
		t.Errorf("recovery event names no finding: %+v", ev)
	}
	agents, err := Register[agentRec](db, Spec{Name: "agents", Version: 1})
	if err != nil {
		t.Fatalf("Register over a recovered tail: %v", err)
	}
	col, _ := agents.Engine()
	for _, id := range []string{"agent-1", "agent-2", "agent-3"} {
		if ok, err := col.Exists(id); err != nil || !ok {
			t.Errorf("acknowledged record %s lost: %v, %v", id, ok, err)
		}
	}
}

func TestCleanReopenAfterTransactionHasNoFalsePositive(t *testing.T) {
	dataDir := t.TempDir()
	db, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"agents", "plans"} {
		if _, err := Register[agentRec](db, Spec{Name: name, Version: 1}); err != nil {
			t.Fatal(err)
		}
	}
	h, _ := db.Handle()
	b, err := dbtx.New(h)
	if err != nil {
		t.Fatal(err)
	}
	agents, err := dbtx.Bind[map[string]any](b, "agents", dbtx.MapCodec{})
	if err != nil {
		t.Fatal(err)
	}
	plans, err := dbtx.Bind[map[string]any](b, "plans", dbtx.MapCodec{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Run(context.Background(), "seed", func(_ context.Context, tx *dbtx.Tx) error {
		if _, err := agents.Insert(tx, map[string]any{"name": "a"}); err != nil {
			return err
		}
		_, err := plans.Insert(tx, map[string]any{"name": "p"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	rec := &recorder{}
	db2 := mustOpen(t, dataDir, observed(rec)...)
	if rec.count(ActionRecovery, "") != 0 {
		t.Errorf("a clean shutdown was reported as a recovery: %+v", rec.event(ActionRecovery))
	}
	for _, name := range []string{"agents", "plans"} {
		if _, err := Register[agentRec](db2, Spec{Name: name, Version: 1}); err != nil {
			t.Errorf("Register %s after a clean transaction: %v", name, err)
		}
	}
}

func TestLifecycleEventsAndMetrics(t *testing.T) {
	dataDir := t.TempDir()
	rec := &recorder{}
	db, err := Open(dataDir, observed(rec)...)
	if err != nil {
		t.Fatal(err)
	}
	ev := rec.event(ActionOpen)
	if ev == nil || ev.Actor != "wardendb" || ev.Target != Dir(dataDir) ||
		ev.Detail["sync_mode"] != "always" || ev.Detail["integrity_policy"] != "fail" {
		t.Fatalf("open event = %+v", ev)
	}
	if rec.count(ActionOpen, "") != 1 || rec.observations(MetricOpenDuration, "") != 1 {
		t.Errorf("open not counted or timed")
	}

	if _, err := Register[agentRec](db, Spec{Name: "agents", Version: 3}); err != nil {
		t.Fatal(err)
	}
	ev = rec.event(ActionRegister)
	if ev == nil || ev.Target != "agents" || ev.Detail["version"] != "3" || rec.count(ActionRegister, "agents") != 1 {
		t.Errorf("register event = %+v", ev)
	}

	// A second opener is refused, and the refusal names the owner.
	_, err = Open(dataDir, observed(rec)...)
	var le *LockedError
	if !errors.As(err, &le) {
		t.Fatalf("second Open err = %v, want *LockedError", err)
	}
	ev = rec.event(ActionLockRefused)
	if ev == nil || rec.count(ActionLockRefused, "") != 1 || ev.Detail["owner_pid"] == "" {
		t.Errorf("lock refusal event = %+v", ev)
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if rec.count(ActionClose, "") != 1 || rec.event(ActionClose) == nil {
		t.Errorf("close not reported")
	}
	if err := db.Close(); err != nil || rec.count(ActionClose, "") != 1 {
		t.Errorf("a repeated Close was reported again (%v)", err)
	}
}

func TestCompactionIsReported(t *testing.T) {
	rec := &recorder{}
	db := mustOpen(t, t.TempDir(), observed(rec)...)
	c, err := Register[agentRec](db, Spec{Name: "agents", Version: 1, Options: []scriva.CollectionOption{
		scriva.WithCollectionSegmentMaxSize(512),
	}})
	if err != nil {
		t.Fatal(err)
	}
	col, _ := c.Engine()
	for i := 0; i < 60; i++ {
		if _, err := col.Upsert("agent-1", map[string]any{"id": "agent-1", "n": i}); err != nil {
			t.Fatal(err)
		}
	}
	if err := col.CompactNow(); err != nil {
		t.Fatal(err)
	}
	if rec.count(ActionCompaction, "agents") < 1 || rec.observations(MetricCompactionDuration, "agents") < 1 {
		t.Fatalf("compaction pass was not reported")
	}
	if ev := rec.event(ActionCompaction); ev == nil || ev.Target != "agents" || ev.Detail["duration_ms"] == "" {
		t.Errorf("compaction event = %+v", ev)
	}
	// Compaction must leave the collection verifiably intact.
	if ok, err := col.Exists("agent-1"); err != nil || !ok {
		t.Errorf("record lost by compaction: %v, %v", ok, err)
	}
	if ie := db.mon.verifyCollection(col); ie != nil {
		t.Errorf("collection does not verify after compaction: %v", ie)
	}
}

func TestPoisonedSegmentRetiresHandle(t *testing.T) {
	rec := &recorder{}
	db := mustOpen(t, t.TempDir(), observed(rec)...)
	agents, err := Register[agentRec](db, Spec{Name: "agents", Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	col, _ := agents.Engine()
	// The engine reports a failed write it could not roll back through the
	// callback wardendb installed.
	cause := errors.New("disk full")
	col.Config().OnSegmentPoisoned("agents", "seg_000001.ndjson", cause)

	_, err = agents.Engine()
	var ie *IntegrityError
	if !errors.As(err, &ie) || ie.Stage != StageRuntime || ie.Collection != "agents" || !errors.Is(err, cause) {
		t.Fatalf("Engine after poisoning = %v", err)
	}
	if _, err := db.Handle(); !errors.Is(err, ErrIntegrity) {
		t.Errorf("Handle after poisoning = %v", err)
	}
	if ev := rec.event(ActionIntegrityRefused); ev == nil || ev.Detail["stage"] != StageRuntime {
		t.Errorf("runtime violation event = %+v", ev)
	}
}

func TestDefaultSinksAreInert(t *testing.T) {
	dataDir := t.TempDir()
	seed(t, dataDir)
	corruptFirstRecord(t, newestSegment(t, dataDir, "agents"))
	// No auditor, no metrics: every emit path must still be safe.
	if _, err := Open(dataDir); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("Open err = %v, want ErrIntegrity", err)
	}
}

func TestOpenForRepairIsReservedAndInert(t *testing.T) {
	dataDir := t.TempDir()
	db, err := OpenForRepair(dataDir)
	if db != nil || !errors.Is(err, ErrRepairNotImplemented) {
		t.Fatalf("OpenForRepair = %v, %v", db, err)
	}
	if _, err := os.Stat(Dir(dataDir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("OpenForRepair touched the directory: %v", err)
	}
}

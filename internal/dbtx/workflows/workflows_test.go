package workflows

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/warden/internal/dbtx"
)

type rig struct {
	t    *testing.T
	dir  string
	opts []dbtx.Option
	db   *scriva.DB
	e    *Env
}

func newRig(t *testing.T, opts ...dbtx.Option) *rig {
	t.Helper()
	r := &rig{t: t, dir: t.TempDir(), opts: opts}
	r.open()
	t.Cleanup(func() {
		if r.db != nil {
			r.db.Close()
		}
	})
	return r
}

func (r *rig) open() {
	r.t.Helper()
	db, err := scriva.Open(r.dir)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := Setup(db); err != nil {
		r.t.Fatal(err)
	}
	opts := append([]dbtx.Option{dbtx.WithBackoff(func(int) time.Duration { return time.Millisecond })}, r.opts...)
	b, err := dbtx.New(db, opts...)
	if err != nil {
		r.t.Fatal(err)
	}
	e, err := NewEnv(db, b)
	if err != nil {
		r.t.Fatal(err)
	}
	r.db, r.e = db, e
}

func (r *rig) reopen() {
	r.t.Helper()
	if err := r.db.Close(); err != nil {
		r.t.Fatal(err)
	}
	r.open()
}

func (r *rig) snap() string {
	r.t.Helper()
	s, err := Snapshot(r.db)
	if err != nil {
		r.t.Fatal(err)
	}
	return s
}

func (r *rig) mustCheck(w Workflow, when string) {
	r.t.Helper()
	if err := w.Check(r.db); err != nil {
		r.t.Fatalf("%s: checker %s failed: %v", when, w.Name, err)
	}
}

func seeded(t *testing.T, w Workflow, opts ...dbtx.Option) *rig {
	t.Helper()
	r := newRig(t, opts...)
	if err := w.Seed(context.Background(), r.e); err != nil {
		t.Fatalf("seed: %v", err)
	}
	r.mustCheck(w, "after seed")
	return r
}

var ctx = context.Background()

func TestSetupIdempotentAndAllTable(t *testing.T) {
	r := newRig(t)
	if err := Setup(r.db); err != nil {
		t.Fatal(err)
	}
	ws := All()
	if len(ws) != 7 {
		t.Fatalf("All() = %d workflows, want 7", len(ws))
	}
	seen := map[string]bool{}
	for _, w := range ws {
		if w.Name == "" || w.Seed == nil || w.Run == nil || w.Check == nil || w.Interfere == nil || len(w.Collections) < 2 || seen[w.Name] {
			t.Fatalf("incomplete workflow %+v", w)
		}
		seen[w.Name] = true
	}
}

// Every workflow, with the others' state also present in the same DB, commits
// atomically, survives reopen, and is a no-op when re-run.
func TestHappyPathReopenAndRerun(t *testing.T) {
	r := newRig(t)
	for _, w := range All() {
		if err := w.Seed(ctx, r.e); err != nil {
			t.Fatalf("%s seed: %v", w.Name, err)
		}
	}
	for _, w := range All() {
		before := r.snap()
		if err := w.Run(ctx, r.e); err != nil {
			t.Fatalf("%s run: %v", w.Name, err)
		}
		if r.snap() == before {
			t.Fatalf("%s: run changed nothing", w.Name)
		}
		for _, c := range All() {
			r.mustCheck(c, "after "+w.Name)
		}
		// Re-run converges to a no-op.
		after := r.snap()
		if err := w.Run(ctx, r.e); err != nil {
			t.Fatalf("%s rerun: %v", w.Name, err)
		}
		if r.snap() != after {
			t.Fatalf("%s: rerun changed state", w.Name)
		}
	}
	want := r.snap()
	r.reopen()
	if got := r.snap(); got != want {
		t.Fatalf("state differs after reopen:\n%s\n---\n%s", want, got)
	}
	for _, w := range All() {
		r.mustCheck(w, "after reopen")
	}
}

func TestRollbackLeavesNothing(t *testing.T) {
	boom := errors.New("injected failure after staging")
	for _, w := range All() {
		t.Run(w.Name, func(t *testing.T) {
			r := seeded(t, w)
			before := r.snap()
			r.e.BeforeCommit = func(int) error { return boom }
			if err := w.Run(ctx, r.e); !errors.Is(err, boom) {
				t.Fatalf("err = %v, want injected failure", err)
			}
			if r.snap() != before {
				t.Fatal("rollback left visible state")
			}
			r.mustCheck(w, "after rollback")
			r.reopen()
			if r.snap() != before {
				t.Fatal("state changed across reopen")
			}
			// The same workflow still succeeds afterwards.
			r.e.BeforeCommit = nil
			if err := w.Run(ctx, r.e); err != nil {
				t.Fatal(err)
			}
			r.mustCheck(w, "after retry")
		})
	}
}

func TestConflictRetryConverges(t *testing.T) {
	for _, w := range All() {
		t.Run(w.Name, func(t *testing.T) {
			r := seeded(t, w)
			attempts := 0
			r.e.BeforeCommit = func(a int) error {
				attempts = a
				if a == 1 {
					return w.Interfere(r.e)
				}
				return nil
			}
			if err := w.Run(ctx, r.e); err != nil {
				t.Fatalf("run: %v", err)
			}
			if attempts != 2 {
				t.Fatalf("attempts = %d, want 2 (one conflict, one rebuild)", attempts)
			}
			r.mustCheck(w, "after converge")
			// Target state holds: a further run is a no-op.
			r.e.BeforeCommit = nil
			s := r.snap()
			if err := w.Run(ctx, r.e); err != nil || r.snap() != s {
				t.Fatalf("not converged: err=%v changed=%v", err, r.snap() != s)
			}
			r.reopen()
			r.mustCheck(w, "after reopen")
		})
	}
}

func TestConflictExhaustedIsTypedAndPartialFree(t *testing.T) {
	for _, w := range All() {
		t.Run(w.Name, func(t *testing.T) {
			r := seeded(t, w, dbtx.WithMaxAttempts(2))
			var last string
			r.e.BeforeCommit = func(int) error {
				if err := w.Interfere(r.e); err != nil {
					return err
				}
				last = r.snap() // everything except our own (unapplied) writes
				return nil
			}
			err := w.Run(ctx, r.e)
			var be *dbtx.Error
			if !errors.As(err, &be) || be.Class != dbtx.ClassConflict || be.Attempts != 2 {
				t.Fatalf("err = %v, want conflict after 2 attempts", err)
			}
			if r.snap() != last {
				t.Fatal("failed transaction left partial state")
			}
			r.mustCheck(w, "after exhausted conflict")
		})
	}
}

// plant writes a deliberately partial state with plain single-collection
// writes; the checker must then fail.
func TestCheckersDetectPlantedPartialState(t *testing.T) {
	type plantCase struct {
		name  string
		wf    string
		run   bool // run the workflow before planting
		plant func(t *testing.T, e *Env)
	}
	must := func(t *testing.T, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	cases := []plantCase{
		{"child without project", "project-child-creation", true, func(t *testing.T, e *Env) {
			_, _, err := e.Agents.col.Insert(map[string]any{"id": "orphan", "project_id": "ghost", "status": "running"})
			must(t, err)
		}},
		{"child without counter bump", "project-child-creation", true, func(t *testing.T, e *Env) {
			_, _, err := e.Agents.col.Insert(map[string]any{"id": "extra", "project_id": W1Project, "status": "running"})
			must(t, err)
		}},
		{"run without plan flip or manager", "autopilot-startup", false, func(t *testing.T, e *Env) {
			_, _, err := e.Runs.col.Insert(map[string]any{"id": "run-x", "project_id": W2Project, "plan_id": W2Plan,
				"manager_agent_id": "agent-nope", "status": "running", "live_plan": W2Plan})
			must(t, err)
		}},
		{"plan flipped without run record", "autopilot-startup", true, func(t *testing.T, e *Env) {
			r, _, _ := e.Runs.ByKey(W2Run)
			must(t, e.Runs.col.Delete(r.ID))
		}},
		{"manager slot missing", "autopilot-startup", true, func(t *testing.T, e *Env) {
			a, _, _ := e.Agents.ByKey(W2Manager)
			must(t, e.Agents.col.Delete(a.ID))
		}},
		{"plan side only of pipeline link", "pipeline-linkage", true, func(t *testing.T, e *Env) {
			p, _, _ := e.Pipelines.ByKey(W3Pipeline)
			must(t, e.Pipelines.col.Delete(p.ID))
		}},
		{"pipeline side only of link", "pipeline-linkage", false, func(t *testing.T, e *Env) {
			_, _, err := e.Pipelines.col.Insert(map[string]any{"id": "pipe-x", "project_id": W3Project,
				"plan_id": W3Plan, "status": "running", "jobs": 0})
			must(t, err)
		}},
		{"agent in live and archive", "archive-restore", true, func(t *testing.T, e *Env) {
			_, _, err := e.Agents.col.Insert(map[string]any{"id": W4AgentA, "project_id": W4Project, "status": "running"})
			must(t, err)
		}},
		{"live deleted without archive row", "archive-restore", false, func(t *testing.T, e *Env) {
			a, _, _ := e.Agents.ByKey(W4AgentA)
			must(t, e.Agents.col.Delete(a.ID))
		}},
		{"closed project with open child", "project-closure", true, func(t *testing.T, e *Env) {
			a, _, _ := e.Agents.ByKey(W5AgentA)
			d, _ := e.Agents.col.Get(a.ID)
			d.Data["status"] = "running"
			_, err := e.Agents.col.Update(a.ID, d.Data)
			must(t, err)
		}},
		{"closed project with running pipeline", "project-closure", true, func(t *testing.T, e *Env) {
			p, _, _ := e.Pipelines.ByKey(W5Pipeline)
			d, _ := e.Pipelines.col.Get(p.ID)
			d.Data["status"] = "running"
			_, err := e.Pipelines.col.Update(p.ID, d.Data)
			must(t, err)
		}},
		{"stale projection", "projection-update", true, func(t *testing.T, e *Env) {
			p, _, _ := e.Projects.ByKey(W6Project)
			d, _ := e.Projects.col.Get(p.ID)
			d.Data["name"] = "sneaky"
			_, err := e.Projects.col.Update(p.ID, d.Data)
			must(t, err)
		}},
		{"audit event deleted", "audit-event-append", true, func(t *testing.T, e *Env) {
			ev, _, _ := e.Events.ByKey(eventID(W7Plan, 2))
			must(t, e.Events.col.Delete(ev.ID))
		}},
		{"plan changed without event", "audit-event-append", true, func(t *testing.T, e *Env) {
			p, _, _ := e.Plans.ByKey(W7Plan)
			d, _ := e.Plans.col.Get(p.ID)
			d.Data["revision"] = float64(3)
			d.Data["status"] = "running"
			_, err := e.Plans.col.Update(p.ID, d.Data)
			must(t, err)
		}},
		{"event without its change", "audit-event-append", true, func(t *testing.T, e *Env) {
			_, _, err := e.Events.col.Insert(map[string]any{"id": eventID(W7Plan, 9), "kind": "plan.ready",
				"subject": W7Plan, "subject_rev": 9, "to": "ready"})
			must(t, err)
		}},
	}
	byName := map[string]Workflow{}
	for _, w := range All() {
		byName[w.Name] = w
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := byName[c.wf]
			r := seeded(t, w)
			if c.run {
				if err := w.Run(ctx, r.e); err != nil {
					t.Fatal(err)
				}
			}
			r.mustCheck(w, "valid state before planting")
			c.plant(t, r.e)
			err := w.Check(r.db)
			if err == nil {
				t.Fatal("checker did not detect the planted partial state")
			}
			if !strings.Contains(err.Error(), "violation") {
				t.Fatalf("unexpected checker error: %v", err)
			}
		})
	}
}

// --- workflow-specific semantics -------------------------------------------

func TestAutopilotIntentThenOutcome(t *testing.T) {
	w := All()[1]
	r := seeded(t, w)
	if err := w.Run(ctx, r.e); err != nil {
		t.Fatal(err)
	}
	m, _, _ := r.e.Agents.ByKey(W2Manager)
	op, ok, _ := r.e.Ops.ByKey(launchOpID(W2Manager))
	if !ok || op.Value.State != "pending" || m.Value.Status != "starting" {
		t.Fatalf("intent not journaled with the startup: agent=%+v op=%+v", m.Value, op.Value)
	}
	r.mustCheck(w, "intent state")
	if err := FinishAgentLaunch(ctx, r.e, W2Run); err != nil {
		t.Fatal(err)
	}
	m, _, _ = r.e.Agents.ByKey(W2Manager)
	op, _, _ = r.e.Ops.ByKey(launchOpID(W2Manager))
	if op.Value.State != "succeeded" || m.Value.Status != "running" {
		t.Fatalf("outcome not recorded: agent=%+v op=%+v", m.Value, op.Value)
	}
	r.mustCheck(w, "outcome state")
	r.reopen()
	r.mustCheck(w, "after reopen")
}

func TestAutopilotSecondRunOnSamePlanRefused(t *testing.T) {
	w := All()[1]
	r := seeded(t, w)
	// A competing run wins between our read and commit.
	r.e.BeforeCommit = func(a int) error {
		if a != 1 {
			return nil
		}
		r.e.BeforeCommit = nil
		defer func() { r.e.BeforeCommit = nil }()
		return StartAutopilot(ctx, r.e, "run-rival", W2Plan, "agent-rival")
	}
	err := w.Run(ctx, r.e)
	if !errors.Is(err, ErrPlanNotReady) {
		t.Fatalf("err = %v, want ErrPlanNotReady after conflict+rebuild", err)
	}
	if _, ok, _ := r.e.Runs.ByKey(W2Run); ok {
		t.Fatal("losing run was written")
	}
	if _, ok, _ := r.e.Agents.ByKey(W2Manager); ok {
		t.Fatal("losing manager slot was written")
	}
	r.mustCheck(w, "after loser")
}

func TestLivePlanUniqueIndexIsSparse(t *testing.T) {
	r := newRig(t)
	col := r.e.Runs.col
	ins := func(id, live string) error {
		d := map[string]any{"id": id, "project_id": "p", "plan_id": "pl", "manager_agent_id": "m", "status": "stopped"}
		if live != "" {
			d["live_plan"] = live
		}
		_, _, err := col.Insert(d)
		return err
	}
	for i, id := range []string{"r1", "r2", "r3"} { // many terminal runs share a plan
		if err := ins(id, ""); err != nil {
			t.Fatalf("terminal run %d: %v", i, err)
		}
	}
	if err := ins("r4", "pl"); err != nil {
		t.Fatal(err)
	}
	if err := ins("r5", "pl"); !errors.Is(err, engine.ErrDuplicateKey) {
		t.Fatalf("second live run err = %v, want ErrDuplicateKey", err)
	}
}

func TestArchiveRestoreIsExactInverse(t *testing.T) {
	w := All()[3]
	r := seeded(t, w)
	agents := func() string {
		rows, err := r.e.Agents.All()
		if err != nil {
			t.Fatal(err)
		}
		var parts []string
		for _, a := range rows {
			parts = append(parts, a.Value.ID+"|"+a.Value.ProjectID+"|"+a.Value.Status+"|"+a.Value.ArchivedFrom)
		}
		sort.Strings(parts)
		return strings.Join(parts, ";")
	}
	counters := func() [2]int {
		p, _, _ := r.e.Projects.ByKey(W4Project)
		return [2]int{p.Value.LiveAgents, p.Value.ArchivedAgents}
	}
	liveBefore, cBefore := agents(), counters()
	if err := ArchiveAgent(ctx, r.e, W4AgentA); err != nil {
		t.Fatal(err)
	}
	r.mustCheck(w, "archived")
	if _, ok, _ := r.e.Agents.ByKey(W4AgentA); ok {
		t.Fatal("still live")
	}
	if _, ok, _ := r.e.Archive.ByKey(W4AgentA); !ok {
		t.Fatal("not in archive")
	}
	if err := RestoreAgent(ctx, r.e, W4AgentA); err != nil {
		t.Fatal(err)
	}
	r.mustCheck(w, "restored")
	if agents() != liveBefore || counters() != cBefore {
		t.Fatalf("restore is not the inverse: %s vs %s, %v vs %v", agents(), liveBefore, counters(), cBefore)
	}
	if rows, _ := r.e.Archive.All(); len(rows) != 0 {
		t.Fatal("archive not empty after restore")
	}
	r.reopen()
	r.mustCheck(w, "after reopen")
}

func TestCloseProjectTooLargeFailsTypedAndChangesNothing(t *testing.T) {
	w := All()[4]
	r := seeded(t, w)
	var rows []map[string]any
	for i := 0; i < dbtx.MaxOps; i++ {
		rows = append(rows, map[string]any{"id": "bulk-" + strconv.Itoa(i), "project_id": W5Project, "status": "running"})
	}
	if _, _, err := r.e.Agents.col.InsertMany(rows, time.Time{}); err != nil {
		t.Fatal(err)
	}
	before := r.snap()
	err := CloseProject(ctx, r.e, W5Project)
	var be *dbtx.Error
	if !errors.As(err, &be) || be.Class != dbtx.ClassTooLarge {
		t.Fatalf("err = %v, want too_large", err)
	}
	if r.snap() != before {
		t.Fatal("oversized close changed state")
	}
	r.mustCheck(w, "after too-large")
}

func TestCloseProjectRefusedWithLiveRun(t *testing.T) {
	r := seeded(t, All()[1])
	if err := All()[1].Run(ctx, r.e); err != nil {
		t.Fatal(err)
	}
	before := r.snap()
	if err := CloseProject(ctx, r.e, W2Project); !errors.Is(err, ErrProjectBusy) {
		t.Fatalf("err = %v, want ErrProjectBusy", err)
	}
	if r.snap() != before {
		t.Fatal("refused close changed state")
	}
}

func TestChildCreatedDuringCloseIsStoppedByRetry(t *testing.T) {
	w := All()[4]
	r := seeded(t, w)
	r.e.BeforeCommit = func(a int) error {
		if a != 1 {
			return nil
		}
		saved := r.e.BeforeCommit
		r.e.BeforeCommit = nil
		defer func() { r.e.BeforeCommit = saved }()
		return CreateProjectWithAgent(ctx, r.e, W5Project, "agent-w5-late")
	}
	if err := w.Run(ctx, r.e); err != nil {
		t.Fatal(err)
	}
	late, ok, _ := r.e.Agents.ByKey("agent-w5-late")
	if !ok || late.Value.Status != "stopped" {
		t.Fatalf("late child = %+v ok=%v, want stopped", late.Value, ok)
	}
	r.mustCheck(w, "after close")
	r.mustCheck(All()[0], "children")
}

func TestChildCreationRefusedWhenProjectClosedMeanwhile(t *testing.T) {
	r := seeded(t, All()[4])
	r.e.BeforeCommit = func(a int) error {
		if a != 1 {
			return nil
		}
		saved := r.e.BeforeCommit
		r.e.BeforeCommit = nil
		defer func() { r.e.BeforeCommit = saved }()
		return CloseProject(ctx, r.e, W5Project)
	}
	err := CreateProjectWithAgent(ctx, r.e, W5Project, "agent-w5-new")
	if !errors.Is(err, ErrProjectClosed) {
		t.Fatalf("err = %v, want ErrProjectClosed", err)
	}
	if _, ok, _ := r.e.Agents.ByKey("agent-w5-new"); ok {
		t.Fatal("child written into a closed project")
	}
	r.mustCheck(All()[4], "closure")
	r.mustCheck(All()[0], "children")
}

func TestProjectionTracksEveryProjectWrite(t *testing.T) {
	w := All()[5]
	r := seeded(t, w)
	if err := w.Run(ctx, r.e); err != nil {
		t.Fatal(err)
	}
	// Later writes by other workflows keep the existing view fresh.
	if err := CreateProjectWithAgent(ctx, r.e, W6Project, "agent-w6b"); err != nil {
		t.Fatal(err)
	}
	if err := ArchiveAgent(ctx, r.e, W6Agent); err != nil {
		t.Fatal(err)
	}
	r.mustCheck(w, "after other writers")
	v, ok, _ := r.e.Views.ByKey(W6Project)
	p, _, _ := r.e.Projects.ByKey(W6Project)
	if !ok || v.Value.SourceVersion != p.Value.Version || v.Value.LiveAgents != 1 || v.Value.ArchivedAgents != 1 {
		t.Fatalf("view = %+v, project = %+v", v.Value, p.Value)
	}
}

func TestMissingCollectionIsTypedAtBind(t *testing.T) {
	db, err := scriva.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	b, _ := dbtx.New(db)
	if _, err := NewEnv(db, b); err == nil || dbtx.Classify(err) != dbtx.ClassCollectionNotFound {
		t.Fatalf("NewEnv without Setup err = %v, want collection_not_found", err)
	}
}

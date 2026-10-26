package planstore

import (
	"context"
	"fmt"
	"testing"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
	"github.com/srjn45/warden/internal/wardendb"
	"github.com/stretchr/testify/require"
)

func newSchema3PlanFixture(t *testing.T) (*Store, *wardendb.DB) {
	t.Helper()
	db, err := wardendb.Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	projects, err := wardendb.Register[map[string]any](db, wardendb.Spec{Name: "projects", Version: 3, Options: []scriva.CollectionOption{scriva.WithUniqueIndex("id")}})
	require.NoError(t, err)
	col, err := projects.Engine()
	require.NoError(t, err)
	_, _, err = col.InsertWithKey("p", map[string]any{"id": "p", "name": "fixture", "status": "open", "path": ""})
	require.NoError(t, err)
	s, err := NewSchema3StoreFromDB(db)
	require.NoError(t, err)
	return s, db
}

func schema3RawPlans(t *testing.T, db *wardendb.DB) *engine.Collection {
	t.Helper()
	col, err := wardendb.Lookup[planHeader](db, plansCollection)
	require.NoError(t, err)
	eng, err := col.Engine()
	require.NoError(t, err)
	return eng
}

func TestSchema3MaterializedPlanPreservesDefinitionAndExecutionEvidence(t *testing.T) {
	ctx := context.Background()
	s, db := newSchema3PlanFixture(t)
	p := &Plan{
		ID: "plan", ProjectID: "p", Name: "ordered", Goal: "full definition", Constraints: []string{"constraint"}, DoneWhen: []string{"done"},
		Tasks: []PlanTask{{ID: "z", Prompt: "first"}, {ID: "a", Prompt: "second", After: []string{"z"}}}, Status: PlanStatusInProgress,
		TaskProgress: map[string]string{"z": "done", "a": "in_progress"}, TaskOutcomes: map[string]TaskOutcome{"z": {TaskID: "z", Status: "done", Note: "verified", VerifiedChecks: []string{"test"}}},
		ActiveExecution:  &PlanExecution{ID: "current", PlanID: "plan", ExecutionMode: PlanModeManual, Snapshot: &ExecutionSnapshot{Name: "original snapshot", Tasks: []PlanTask{{ID: "old", Prompt: "immutable original"}}}},
		ExecutionHistory: []PlanExecution{{ID: "past", PlanID: "plan", ExecutorID: "historical-executor", TaskDispatch: map[string]TaskDispatch{"old": {ExecutionID: "past", State: "landed", Branch: "historical-branch"}}}},
	}
	require.NoError(t, s.Create(ctx, p))
	got, err := s.Get(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, p.Tasks, got.Tasks)
	require.Equal(t, p.ContentHash, got.ContentHash)
	require.Equal(t, p.Revision, got.Revision)
	require.Equal(t, p.TaskProgress, got.TaskProgress)
	require.Equal(t, p.TaskOutcomes, got.TaskOutcomes)
	require.Equal(t, p.ActiveExecution.Snapshot, got.ActiveExecution.Snapshot)
	require.Equal(t, p.ExecutionHistory, got.ExecutionHistory)
	require.Equal(t, got.TaskProgress, got.ActiveExecution.TaskProgress)
	raw, err := schema3RawPlans(t, db).GetByKey(p.ID)
	require.NoError(t, err)
	for _, field := range []string{"tasks", "task_progress", "task_outcomes", "pipeline_id", "autopilot_run_id", "orchestrator_id"} {
		require.NotContains(t, raw.Data, field)
	}
	require.NotContains(t, raw.Data["active_execution"], "task_progress")
	tasks, err := wardendb.Lookup[TaskRecord](db, tasksCollection)
	require.NoError(t, err)
	eng, err := tasks.Engine()
	require.NoError(t, err)
	rows, err := eng.Scan(query.MatchAll)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	first, err := eng.GetByKey(taskKey("plan", "z"))
	require.NoError(t, err)
	require.Equal(t, "0", fmt.Sprint(first.Data["seq"]))
	require.Equal(t, "p", first.Data["project_id"])
	require.NoError(t, s.Update(ctx, "plan", func(next *Plan) error { next.TaskProgress["a"] = "done"; return nil }))
	got, err = s.Get(ctx, "plan")
	require.NoError(t, err)
	require.Equal(t, "done", got.TaskProgress["a"])
	require.Equal(t, p.ContentHash, got.ContentHash)
}

func TestSchema3PlanMutationRejectsOwnershipAndInvalidDefinitionsAtomically(t *testing.T) {
	ctx := context.Background()
	s, _ := newSchema3PlanFixture(t)
	p := &Plan{ID: "plan", ProjectID: "p", Name: "pending", Status: PlanStatusPending, Tasks: []PlanTask{{ID: "z", Prompt: "original"}, {ID: "a", Prompt: "after", After: []string{"z"}}}}
	require.NoError(t, s.Create(ctx, p))
	before, err := s.Get(ctx, "plan")
	require.NoError(t, err)
	for _, mutate := range []func(*Plan){
		func(next *Plan) { next.ProjectID = "other" },
		func(next *Plan) { next.ID = "replacement" },
		func(next *Plan) { next.Tasks[0].After = []string{"a"} },
		func(next *Plan) { next.TaskProgress = map[string]string{"missing": "done"} },
		func(next *Plan) { next.TaskOutcomes = map[string]TaskOutcome{"missing": {Note: "unowned"}} },
	} {
		require.Error(t, s.UpdateIf(ctx, "plan", before.Revision, func(next *Plan) error { mutate(next); return nil }))
		after, err := s.Get(ctx, "plan")
		require.NoError(t, err)
		require.Equal(t, before, after)
	}
	require.NoError(t, s.UpdateIf(ctx, "plan", before.Revision, func(next *Plan) error {
		next.Tasks = []PlanTask{{ID: "a", Prompt: "first now"}, {ID: "new", Prompt: "new task", After: []string{"a"}}}
		next.TaskProgress = map[string]string{"a": "pending", "new": "pending"}
		return nil
	}))
	after, err := s.Get(ctx, "plan")
	require.NoError(t, err)
	require.Equal(t, before.Revision+1, after.Revision)
	require.Equal(t, "a", after.Tasks[0].ID)
	require.Equal(t, "new", after.Tasks[1].ID)
}

func TestSchema3PlanCodecRejectsRetiredFieldsEvenWhenNull(t *testing.T) {
	for _, field := range []string{"tasks", "task_progress", "task_outcomes", "pipeline_id", "autopilot_run_id", "orchestrator_id"} {
		t.Run(field, func(t *testing.T) {
			s, db := newSchema3PlanFixture(t)
			p := &Plan{ID: "plan", ProjectID: "p", Name: "fixture", Status: PlanStatusPending}
			require.NoError(t, s.Create(context.Background(), p))
			eng := schema3RawPlans(t, db)
			r, err := eng.GetByKey("plan")
			require.NoError(t, err)
			r.Data[field] = nil
			delete(r.Data, engine.KeyField)
			_, err = eng.UpdateByKey("plan", r.Data)
			require.NoError(t, err)
			_, err = s.Get(context.Background(), "plan")
			require.ErrorContains(t, err, "forbidden persisted field")
		})
	}
}

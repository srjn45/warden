package planstore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeResp struct {
	out string
	err error
}

type fakeRunner struct {
	mu        sync.Mutex
	calls     [][]string
	responses map[string]fakeResp
}

func (f *fakeRunner) Run(_ context.Context, _ string, name string, args ...string) (string, error) {
	argv := append([]string{name}, args...)
	f.mu.Lock()
	f.calls = append(f.calls, append([]string(nil), argv...))
	f.mu.Unlock()
	key := strings.Join(argv, " ")
	if f.responses != nil {
		if r, ok := f.responses[key]; ok {
			return r.out, r.err
		}
		for k, r := range f.responses {
			if strings.HasPrefix(key, k) {
				return r.out, r.err
			}
		}
	}
	return "", nil
}

func (f *fakeRunner) called(name string, contain ...string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if len(c) == 0 || c[0] != name {
			continue
		}
		joined := strings.Join(c, " ")
		ok := true
		for _, part := range contain {
			if !strings.Contains(joined, part) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func newTestService(t *testing.T) (*PlanService, *Store, string, *fakeRunner) {
	t.Helper()
	store := newTestStore(t)
	root := t.TempDir()
	fake := &fakeRunner{responses: map[string]fakeResp{}}
	svc := NewPlanService(store, WithProjectRoot(func(string) string { return root }), WithRunner(fake))
	return svc, store, root, fake
}

func sampleCreate(name string) CreateRequest {
	return CreateRequest{
		Name: name,
		Goal: "do the thing",
		Tasks: []TaskSpec{
			{ID: "t1", Prompt: "first"},
			{ID: "t2", Prompt: "second", After: []string{"t1"}},
		},
		Constraints: []string{"stay in lane"},
		DoneWhen:    []string{"tests pass"},
	}
}

func TestPlanService_Create(t *testing.T) {
	svc, _, root, _ := newTestService(t)
	ctx := context.Background()

	p, err := svc.Create(ctx, "proj-1", sampleCreate("My Feature"))
	require.NoError(t, err)
	require.Equal(t, "My Feature", p.Name)
	require.Equal(t, PlanStatusPending, p.Status)
	require.Equal(t, "do the thing", p.Goal)
	require.Equal(t, []string{"stay in lane"}, p.Constraints)
	require.Equal(t, []string{"tests pass"}, p.DoneWhen)
	require.Len(t, p.Tasks, 2)
	require.Equal(t, []string{"t1"}, p.Tasks[1].After)
	require.Equal(t, "", p.FilePath)
	require.Equal(t, int64(1), p.Revision)
	require.NotEmpty(t, p.ContentHash)
	require.Equal(t, ComputeContentHash(p), p.ContentHash)
	require.Equal(t, map[string]string{"t1": "pending", "t2": "pending"}, p.TaskProgress)

	_, err = os.Stat(filepath.Join(root, "plans"))
	require.True(t, os.IsNotExist(err), "Create must not create a plans/ directory")
}

func TestPlanService_Create_validation(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()

	_, err := svc.Create(ctx, "proj-1", CreateRequest{Name: "  ", Tasks: []TaskSpec{{ID: "t1", Prompt: "p"}}})
	require.Error(t, err)
	var ve *ValidationError
	require.ErrorAs(t, err, &ve)
	require.Equal(t, "name", ve.Field)

	_, err = svc.Create(ctx, "proj-1", CreateRequest{Name: "ok"})
	require.ErrorAs(t, err, &ve)
	require.Equal(t, "tasks", ve.Field)

	_, err = svc.Create(ctx, "proj-1", CreateRequest{
		Name:  "ok",
		Tasks: []TaskSpec{{ID: "t1", Prompt: ""}},
	})
	require.ErrorAs(t, err, &ve)

	_, err = svc.Create(ctx, "proj-1", CreateRequest{
		Name:  "ok",
		Tasks: []TaskSpec{{ID: "t1", Prompt: "p"}, {ID: "t2", Prompt: "p", After: []string{"missing"}}},
	})
	require.ErrorAs(t, err, &ve)
	require.Contains(t, ve.Error(), "after-ref")

	_, err = svc.Create(ctx, "proj-1", CreateRequest{
		Name: "cycle",
		Tasks: []TaskSpec{
			{ID: "a", Prompt: "p", After: []string{"b"}},
			{ID: "b", Prompt: "p", After: []string{"a"}},
		},
	})
	require.ErrorAs(t, err, &ve)
	require.Contains(t, ve.Error(), "cycle")
}

func TestPlanService_Create_autoChainsFlatTasks(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()
	p, err := svc.Create(ctx, "proj-1", CreateRequest{
		Name:  "flat",
		Goal:  "g",
		Tasks: []TaskSpec{{ID: "a", Prompt: "1"}, {ID: "b", Prompt: "2"}, {ID: "c", Prompt: "3"}},
	})
	require.NoError(t, err)
	require.Empty(t, p.Tasks[0].After)
	require.Equal(t, []string{"a"}, p.Tasks[1].After)
	require.Equal(t, []string{"b"}, p.Tasks[2].After)
}

func TestPlanService_UpdateTaskStatus_gatesOnDeps(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()
	p, err := svc.Create(ctx, "proj-1", sampleCreate("gated"))
	require.NoError(t, err)

	_, err = svc.UpdateTaskStatus(ctx, p.ID, "t2", "in_progress")
	require.ErrorIs(t, err, ErrTaskDepsUnmet)

	_, err = svc.UpdateTaskStatus(ctx, p.ID, "t1", "done")
	require.NoError(t, err)
	_, err = svc.UpdateTaskStatus(ctx, p.ID, "t2", "in_progress")
	require.NoError(t, err)
}

func TestPlanService_Create_duplicate(t *testing.T) {
	svc, store, root, _ := newTestService(t)
	ctx := context.Background()

	req := sampleCreate("Dup Plan")
	id := PlanID("proj-1", "Dup Plan")
	require.NoError(t, store.Create(ctx, &Plan{
		ID: id, ProjectID: "proj-1", Name: "Dup Plan",
		Status: PlanStatusPending, Tasks: []PlanTask{{ID: "t1", Prompt: "x"}},
	}))

	_, err := svc.Create(ctx, "proj-1", req)
	require.ErrorIs(t, err, ErrExists)
	_, err = os.Stat(filepath.Join(root, "plans"))
	require.True(t, os.IsNotExist(err))
}

func TestPlanService_Update_notPending(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()

	p, err := svc.Create(ctx, "proj-1", sampleCreate("Lock Me"))
	require.NoError(t, err)
	_, err = svc.Transition(ctx, p.ID, PlanStatusInProgress, TransitionOptions{})
	require.NoError(t, err)

	name := "nope"
	_, err = svc.Update(ctx, p.ID, UpdateRequest{Name: &name})
	require.ErrorIs(t, err, ErrNotPending)
}

func TestPlanService_Update_pending(t *testing.T) {
	svc, _, root, _ := newTestService(t)
	ctx := context.Background()

	p, err := svc.Create(ctx, "proj-1", sampleCreate("Edit Me"))
	require.NoError(t, err)
	require.Equal(t, int64(1), p.Revision)
	oldHash := p.ContentHash

	newName := "Edited"
	newGoal := "new goal"
	tasks := []TaskSpec{{ID: "only", Prompt: "just this"}}
	got, err := svc.Update(ctx, p.ID, UpdateRequest{Name: &newName, Goal: &newGoal, Tasks: &tasks})
	require.NoError(t, err)
	require.Equal(t, "Edited", got.Name)
	require.Equal(t, "new goal", got.Goal)
	require.Equal(t, PlanStatusPending, got.Status)
	require.Equal(t, "", got.FilePath)
	require.Equal(t, int64(2), got.Revision)
	require.NotEqual(t, oldHash, got.ContentHash)
	require.Len(t, got.Tasks, 1)
	require.Equal(t, "only", got.Tasks[0].ID)
	require.Equal(t, "pending", got.TaskProgress["only"])
	_, err = os.Stat(filepath.Join(root, "plans"))
	require.True(t, os.IsNotExist(err))
}

func TestPlanService_Update_revisionConflict(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()

	p, err := svc.Create(ctx, "proj-1", sampleCreate("Race"))
	require.NoError(t, err)

	stale := int64(0)
	goal := "stale write"
	_, err = svc.Update(ctx, p.ID, UpdateRequest{Goal: &goal, ExpectedRevision: &stale})
	require.ErrorIs(t, err, ErrRevisionConflict)
	var conflict *RevisionConflictError
	require.ErrorAs(t, err, &conflict)
	require.Equal(t, p.ID, conflict.PlanID)
	require.Equal(t, int64(0), conflict.Expected)
	require.Equal(t, int64(1), conflict.Actual)

	got, err := svc.Get(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, "do the thing", got.Goal)
	require.Equal(t, int64(1), got.Revision)
}

func TestPlanService_Transition_stateMachine(t *testing.T) {
	ctx := context.Background()

	t.Run("valid pending to in_progress and archived", func(t *testing.T) {
		svc, _, root, _ := newTestService(t)
		p, err := svc.Create(ctx, "proj-1", sampleCreate("Go Live"))
		require.NoError(t, err)
		got, err := svc.Transition(ctx, p.ID, PlanStatusInProgress, TransitionOptions{ExecutionMode: PlanModeManual})
		require.NoError(t, err)
		require.Equal(t, PlanStatusInProgress, got.Status)
		require.Equal(t, PlanModeManual, got.ExecutionMode)
		require.NotNil(t, got.StartedAt)
		require.Equal(t, int64(2), got.Revision)
		_, err = os.Stat(filepath.Join(root, "plans"))
		require.True(t, os.IsNotExist(err))
	})

	t.Run("valid in_progress back to pending", func(t *testing.T) {
		svc, _, _, _ := newTestService(t)
		p, err := svc.Create(ctx, "proj-1", sampleCreate("Bounce"))
		require.NoError(t, err)
		_, err = svc.Transition(ctx, p.ID, PlanStatusInProgress, TransitionOptions{})
		require.NoError(t, err)
		got, err := svc.Transition(ctx, p.ID, PlanStatusPending, TransitionOptions{})
		require.NoError(t, err)
		require.Equal(t, PlanStatusPending, got.Status)
	})

	t.Run("valid pending to archived", func(t *testing.T) {
		svc, _, _, _ := newTestService(t)
		p, err := svc.Create(ctx, "proj-1", sampleCreate("Shelf"))
		require.NoError(t, err)
		got, err := svc.Transition(ctx, p.ID, PlanStatusArchived, TransitionOptions{})
		require.NoError(t, err)
		require.Equal(t, PlanStatusArchived, got.Status)
		require.NotNil(t, got.ArchivedAt)
	})

	t.Run("valid completed to archived", func(t *testing.T) {
		svc, store, _, fake := newTestService(t)
		p, err := svc.Create(ctx, "proj-1", sampleCreate("Finish"))
		require.NoError(t, err)
		_, err = svc.Transition(ctx, p.ID, PlanStatusInProgress, TransitionOptions{})
		require.NoError(t, err)
		_, err = svc.UpdateTaskStatus(ctx, p.ID, "t1", "done")
		require.NoError(t, err)
		_, err = svc.UpdateTaskStatus(ctx, p.ID, "t2", "skipped")
		require.NoError(t, err)
		p, err = store.Get(ctx, p.ID)
		require.NoError(t, err)
		fake.responses["gh pr list"] = fakeResp{out: "[]"}
		got, err := svc.Transition(ctx, p.ID, PlanStatusCompleted, TransitionOptions{})
		require.NoError(t, err)
		require.Equal(t, PlanStatusCompleted, got.Status)
		require.NotNil(t, got.CompletedAt)
		got, err = svc.Transition(ctx, got.ID, PlanStatusArchived, TransitionOptions{})
		require.NoError(t, err)
		require.Equal(t, PlanStatusArchived, got.Status)
		require.NotNil(t, got.ArchivedAt)
	})
}

func TestPlanService_Transition_invalid(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()

	p, err := svc.Create(ctx, "proj-1", sampleCreate("No Jump"))
	require.NoError(t, err)
	_, err = svc.Transition(ctx, p.ID, PlanStatusCompleted, TransitionOptions{})
	require.ErrorIs(t, err, ErrInvalidTransition)
	var it *InvalidTransitionError
	require.ErrorAs(t, err, &it)
	require.Equal(t, PlanStatusPending, it.From)
	require.Equal(t, PlanStatusCompleted, it.To)

	_, err = svc.Transition(ctx, p.ID, PlanStatusArchived, TransitionOptions{})
	require.NoError(t, err)
	_, err = svc.Transition(ctx, p.ID, PlanStatusPending, TransitionOptions{})
	require.ErrorIs(t, err, ErrInvalidTransition)
}

func TestPlanService_Transition_incompleteTasks(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()

	p, err := svc.Create(ctx, "proj-1", sampleCreate("WIP"))
	require.NoError(t, err)
	_, err = svc.Transition(ctx, p.ID, PlanStatusInProgress, TransitionOptions{})
	require.NoError(t, err)
	_, err = svc.UpdateTaskStatus(ctx, p.ID, "t1", "done")
	require.NoError(t, err)

	_, err = svc.Transition(ctx, p.ID, PlanStatusCompleted, TransitionOptions{})
	require.ErrorIs(t, err, ErrTasksIncomplete)
	var te *TasksIncompleteError
	require.ErrorAs(t, err, &te)
	require.Equal(t, []string{"t2"}, te.TaskIDs)
}

func TestPlanService_Transition_unmergedBranches(t *testing.T) {
	svc, store, _, fake := newTestService(t)
	ctx := context.Background()

	p, err := svc.Create(ctx, "proj-1", sampleCreate("PRs Open"))
	require.NoError(t, err)
	_, err = svc.Transition(ctx, p.ID, PlanStatusInProgress, TransitionOptions{})
	require.NoError(t, err)
	_, err = svc.UpdateTaskStatus(ctx, p.ID, "t1", "done")
	require.NoError(t, err)
	_, err = svc.UpdateTaskStatus(ctx, p.ID, "t2", "done")
	require.NoError(t, err)

	require.NoError(t, store.Update(ctx, p.ID, func(pl *Plan) error {
		pl.Branches = []string{"feat/prs-open", "feat/other"}
		return nil
	}))
	fake.responses["gh pr list --head feat/prs-open --state open --json number"] = fakeResp{out: `[{"number":12}]`}
	fake.responses["gh pr list --head feat/other --state open --json number"] = fakeResp{out: `[]`}

	_, err = svc.Transition(ctx, p.ID, PlanStatusCompleted, TransitionOptions{})
	require.ErrorIs(t, err, ErrBranchesUnmerged)
	var be *BranchesUnmergedError
	require.ErrorAs(t, err, &be)
	require.Equal(t, []string{"feat/prs-open"}, be.Branches)
}

func TestPlanService_Transition_completeWhenMerged(t *testing.T) {
	svc, store, root, fake := newTestService(t)
	ctx := context.Background()

	p, err := svc.Create(ctx, "proj-1", sampleCreate("All Green"))
	require.NoError(t, err)
	_, err = svc.Transition(ctx, p.ID, PlanStatusInProgress, TransitionOptions{})
	require.NoError(t, err)
	_, err = svc.UpdateTaskStatus(ctx, p.ID, "t1", "done")
	require.NoError(t, err)
	_, err = svc.UpdateTaskStatus(ctx, p.ID, "t2", "skipped")
	require.NoError(t, err)
	require.NoError(t, store.Update(ctx, p.ID, func(pl *Plan) error {
		pl.Branches = []string{"feat/all-green"}
		return nil
	}))
	fake.responses["gh pr list --head feat/all-green --state open --json number"] = fakeResp{out: `[]`}

	got, err := svc.Transition(ctx, p.ID, PlanStatusCompleted, TransitionOptions{})
	require.NoError(t, err)
	require.Equal(t, PlanStatusCompleted, got.Status)
	_, err = os.Stat(filepath.Join(root, "plans"))
	require.True(t, os.IsNotExist(err), "complete must not create plans/")
	require.False(t, fake.called("git", "worktree", "remove"), "Transition must not clean up worktrees")
}

func TestPlanService_UpdateTaskStatus(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()
	p, err := svc.Create(ctx, "proj-1", sampleCreate("Tasks"))
	require.NoError(t, err)

	got, err := svc.UpdateTaskStatus(ctx, p.ID, "t1", "in_progress")
	require.NoError(t, err)
	require.Equal(t, "in_progress", got.TaskProgress["t1"])
	require.Equal(t, "pending", got.TaskProgress["t2"])
	require.Equal(t, int64(1), got.Revision, "task progress must not bump revision")

	_, err = svc.UpdateTaskStatus(ctx, p.ID, "t1", "bogus")
	require.ErrorIs(t, err, ErrInvalidTaskStatus)
}

func TestPlanService_CleanupWorktrees(t *testing.T) {
	svc, _, root, fake := newTestService(t)
	ctx := context.Background()
	fake.responses["git worktree list --porcelain"] = fakeResp{out: strings.Join([]string{
		"worktree " + root,
		"HEAD abc",
		"branch refs/heads/main",
		"",
		"worktree " + filepath.Join(root, ".worktrees", "worker-1"),
		"HEAD def",
		"branch refs/heads/worker-1",
		"",
	}, "\n")}

	err := svc.CleanupWorktrees(ctx, &Plan{
		ProjectID: "proj-1",
		Branches:  []string{"worker-1"},
	})
	require.NoError(t, err)
	require.True(t, fake.called("git", "worktree", "remove"))
	require.True(t, fake.called("git", "branch", "-d", "worker-1"))
	require.True(t, fake.called("git", "push", "origin", "--delete", "worker-1"))
}

func TestPlan_BranchesRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	p := &Plan{
		ID:        "plan-branches",
		ProjectID: "proj-1",
		Name:      "with-branches",
		Status:    PlanStatusInProgress,
		Branches:  []string{"feat/a", "feat/b"},
	}
	require.NoError(t, s.Create(ctx, p))
	got, err := s.Get(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"feat/a", "feat/b"}, got.Branches)
}

func TestMatchPlanBranches(t *testing.T) {
	got := matchPlanBranches("My Feature", "plan-abcd1234", []string{
		"main",
		"master",
		"autopilot/my-feature",
		"feat/my-feature",
		"my-feature",
		"worker-plan-abcd1234",
		"unrelated",
	})
	require.Equal(t, []string{"feat/my-feature", "my-feature", "worker-plan-abcd1234"}, got)
}

func TestPlanService_NoPlansDir_fullLifecycle(t *testing.T) {
	svc, store, root, fake := newTestService(t)
	ctx := context.Background()
	fake.responses["gh pr list"] = fakeResp{out: "[]"}

	// No plans/ directory at all.
	_, err := os.Stat(filepath.Join(root, "plans"))
	require.True(t, os.IsNotExist(err))

	p, err := svc.Create(ctx, "proj-1", sampleCreate("DB Only"))
	require.NoError(t, err)

	listed, err := svc.List(ctx, "proj-1", "")
	require.NoError(t, err)
	require.Len(t, listed, 1)

	got, err := svc.Get(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, "DB Only", got.Name)
	require.Equal(t, "do the thing", got.Goal)

	goal := "updated goal"
	got, err = svc.Update(ctx, p.ID, UpdateRequest{Goal: &goal, ExpectedRevision: &got.Revision})
	require.NoError(t, err)
	require.Equal(t, "updated goal", got.Goal)

	got, err = svc.Transition(ctx, p.ID, PlanStatusInProgress, TransitionOptions{ExecutionMode: PlanModeManual})
	require.NoError(t, err)
	require.Equal(t, PlanStatusInProgress, got.Status)

	_, err = svc.UpdateTaskStatus(ctx, p.ID, "t1", "done")
	require.NoError(t, err)
	_, err = svc.UpdateTaskStatus(ctx, p.ID, "t2", "done")
	require.NoError(t, err)

	got, err = svc.Transition(ctx, p.ID, PlanStatusCompleted, TransitionOptions{})
	require.NoError(t, err)
	require.Equal(t, PlanStatusCompleted, got.Status)

	got, err = svc.Transition(ctx, p.ID, PlanStatusArchived, TransitionOptions{})
	require.NoError(t, err)
	require.Equal(t, PlanStatusArchived, got.Status)
	require.NotNil(t, got.ArchivedAt)

	_, err = os.Stat(filepath.Join(root, "plans"))
	require.True(t, os.IsNotExist(err), "full lifecycle must never create plans/")

	// Detail still served from store after archive.
	final, err := store.Get(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, "updated goal", final.Goal)
	require.Len(t, final.Tasks, 2)
}

func TestPlanService_EditedYAMLDoesNotMutateCanonical(t *testing.T) {
	svc, store, root, _ := newTestService(t)
	ctx := context.Background()

	p, err := svc.Create(ctx, "proj-1", sampleCreate("Canonical"))
	require.NoError(t, err)
	before, err := store.Get(ctx, p.ID)
	require.NoError(t, err)

	// Operator drops a replica YAML that disagrees with the canonical record.
	rel := filepath.Join("plans", "pending", "canonical.yaml")
	abs := filepath.Join(root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
	require.NoError(t, LegacyWritePlanYAMLAtomic(abs, LegacyPlanDocument{
		Version: 1,
		Name:    "Canonical",
		Goal:    "hijacked from disk",
		Tasks:   []LegacyPlanTask{{ID: "evil", Prompt: "should not win"}},
	}))
	require.NoError(t, store.Update(ctx, p.ID, func(pl *Plan) error {
		pl.FilePath = rel // last-export metadata only
		return nil
	}))

	got, err := svc.Get(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, before.Goal, got.Goal)
	require.Equal(t, before.ContentHash, got.ContentHash)
	require.Len(t, got.Tasks, 2)
	require.Equal(t, "t1", got.Tasks[0].ID)

	listed, err := svc.List(ctx, "proj-1", PlanStatusPending)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, before.Goal, listed[0].Goal)

	// Rewrite the YAML again after a service update — still inert.
	goal := "canonical update"
	got, err = svc.Update(ctx, p.ID, UpdateRequest{Goal: &goal})
	require.NoError(t, err)
	require.Equal(t, "canonical update", got.Goal)
	require.NoError(t, os.WriteFile(abs, []byte("name: Canonical\ngoal: disk again\ntasks:\n  - id: x\n    prompt: y\n"), 0o644))
	got, err = svc.Get(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, "canonical update", got.Goal)
	require.Equal(t, "t1", got.Tasks[0].ID)
}

func TestPlanService_AddTask_pending(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()

	p, err := svc.Create(ctx, "proj-1", sampleCreate("Add Task"))
	require.NoError(t, err)
	require.Equal(t, int64(1), p.Revision)
	oldHash := p.ContentHash

	got, err := svc.AddTask(ctx, p.ID, TaskSpec{ID: "t3", Prompt: "third", After: []string{"t2"}}, nil)
	require.NoError(t, err)
	require.Len(t, got.Tasks, 3)
	require.Equal(t, "t3", got.Tasks[2].ID)
	require.Equal(t, []string{"t2"}, got.Tasks[2].After)
	require.Equal(t, "pending", got.TaskProgress["t3"])
	require.Equal(t, int64(2), got.Revision)
	require.NotEqual(t, oldHash, got.ContentHash)
	require.Equal(t, ComputeContentHash(got), got.ContentHash)
}

func TestPlanService_AddTask_validation(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()
	p, err := svc.Create(ctx, "proj-1", sampleCreate("Add Bad"))
	require.NoError(t, err)

	var ve *ValidationError

	_, err = svc.AddTask(ctx, p.ID, TaskSpec{ID: "", Prompt: "x"}, nil)
	require.ErrorAs(t, err, &ve)
	require.Equal(t, "task.id", ve.Field)

	_, err = svc.AddTask(ctx, p.ID, TaskSpec{ID: "t3", Prompt: "  "}, nil)
	require.ErrorAs(t, err, &ve)
	require.Equal(t, "task.prompt", ve.Field)

	_, err = svc.AddTask(ctx, p.ID, TaskSpec{ID: "t1", Prompt: "dup"}, nil)
	require.ErrorAs(t, err, &ve)
	require.Contains(t, ve.Error(), "duplicate")

	_, err = svc.AddTask(ctx, p.ID, TaskSpec{ID: "t3", Prompt: "x", After: []string{"missing"}}, nil)
	require.ErrorAs(t, err, &ve)
	require.Contains(t, ve.Error(), "after-ref")

	_, err = svc.AddTask(ctx, p.ID, TaskSpec{ID: "t0", Prompt: "cycle", After: []string{"t2"}}, nil)
	require.NoError(t, err)
	// t1 → t2 → t0 → t2 would require editing edges; create a cycle via UpdateTask instead.
	got, err := svc.Get(ctx, p.ID)
	require.NoError(t, err)
	_, err = svc.UpdateTask(ctx, got.ID, "t2", nil, &[]string{"t0"}, nil)
	require.ErrorAs(t, err, &ve)
	require.Contains(t, ve.Error(), "cycle")

	unchanged, err := svc.Get(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, got.Revision, unchanged.Revision)
	require.Equal(t, []string{"t1"}, unchanged.Tasks[1].After)
}

func TestPlanService_AddTask_notPending(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()

	for _, status := range []PlanStatus{PlanStatusInProgress, PlanStatusCompleted, PlanStatusArchived} {
		t.Run(string(status), func(t *testing.T) {
			p, err := svc.Create(ctx, "proj-1", sampleCreate("Lock-"+string(status)))
			require.NoError(t, err)
			switch status {
			case PlanStatusInProgress:
				_, err = svc.Transition(ctx, p.ID, PlanStatusInProgress, TransitionOptions{})
			case PlanStatusCompleted:
				_, err = svc.Transition(ctx, p.ID, PlanStatusInProgress, TransitionOptions{})
				require.NoError(t, err)
				_, err = svc.UpdateTaskStatus(ctx, p.ID, "t1", "done")
				require.NoError(t, err)
				_, err = svc.UpdateTaskStatus(ctx, p.ID, "t2", "done")
				require.NoError(t, err)
				_, err = svc.Transition(ctx, p.ID, PlanStatusCompleted, TransitionOptions{})
			case PlanStatusArchived:
				_, err = svc.Transition(ctx, p.ID, PlanStatusArchived, TransitionOptions{})
			}
			require.NoError(t, err)

			_, err = svc.AddTask(ctx, p.ID, TaskSpec{ID: "extra", Prompt: "nope"}, nil)
			require.ErrorIs(t, err, ErrNotPending)
		})
	}
}

func TestPlanService_AddTask_revisionConflict(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()
	p, err := svc.Create(ctx, "proj-1", sampleCreate("Add Race"))
	require.NoError(t, err)

	stale := int64(0)
	_, err = svc.AddTask(ctx, p.ID, TaskSpec{ID: "t3", Prompt: "third"}, &stale)
	require.ErrorIs(t, err, ErrRevisionConflict)
	var conflict *RevisionConflictError
	require.ErrorAs(t, err, &conflict)
	require.Equal(t, p.ID, conflict.PlanID)
	require.Equal(t, int64(0), conflict.Expected)
	require.Equal(t, int64(1), conflict.Actual)

	got, err := svc.Get(ctx, p.ID)
	require.NoError(t, err)
	require.Len(t, got.Tasks, 2)
	require.Equal(t, int64(1), got.Revision)
}

func TestPlanService_UpdateTask_pending(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()
	p, err := svc.Create(ctx, "proj-1", sampleCreate("Edit Task"))
	require.NoError(t, err)
	oldHash := p.ContentHash

	prompt := "first revised"
	got, err := svc.UpdateTask(ctx, p.ID, "t1", &prompt, nil, nil)
	require.NoError(t, err)
	require.Equal(t, "first revised", got.Tasks[0].Prompt)
	require.Equal(t, []string{"t1"}, got.Tasks[1].After)
	require.Equal(t, int64(2), got.Revision)
	require.NotEqual(t, oldHash, got.ContentHash)

	after := []string{}
	got, err = svc.UpdateTask(ctx, got.ID, "t2", nil, &after, nil)
	require.NoError(t, err)
	require.Empty(t, got.Tasks[1].After)
	require.Equal(t, int64(3), got.Revision)
}

func TestPlanService_UpdateTask_validationAndNotPending(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()
	p, err := svc.Create(ctx, "proj-1", sampleCreate("Update Bad"))
	require.NoError(t, err)

	var ve *ValidationError
	empty := "  "
	_, err = svc.UpdateTask(ctx, p.ID, "t1", &empty, nil, nil)
	require.ErrorAs(t, err, &ve)
	require.Equal(t, "task.prompt", ve.Field)

	_, err = svc.UpdateTask(ctx, p.ID, "missing", nil, &[]string{"t1"}, nil)
	require.ErrorAs(t, err, &ve)
	require.Contains(t, ve.Error(), "unknown task")

	_, err = svc.UpdateTask(ctx, p.ID, "t2", nil, &[]string{"nope"}, nil)
	require.ErrorAs(t, err, &ve)
	require.Contains(t, ve.Error(), "after-ref")

	_, err = svc.Transition(ctx, p.ID, PlanStatusInProgress, TransitionOptions{})
	require.NoError(t, err)
	prompt := "locked"
	_, err = svc.UpdateTask(ctx, p.ID, "t1", &prompt, nil, nil)
	require.ErrorIs(t, err, ErrNotPending)
}

func TestPlanService_UpdateTask_revisionConflict(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()
	p, err := svc.Create(ctx, "proj-1", sampleCreate("Update Race"))
	require.NoError(t, err)

	stale := int64(99)
	prompt := "stale"
	_, err = svc.UpdateTask(ctx, p.ID, "t1", &prompt, nil, &stale)
	require.ErrorIs(t, err, ErrRevisionConflict)

	got, err := svc.Get(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, "first", got.Tasks[0].Prompt)
	require.Equal(t, int64(1), got.Revision)
}

func TestPlanService_RemoveTask_pending(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()
	p, err := svc.Create(ctx, "proj-1", sampleCreate("Remove Task"))
	require.NoError(t, err)
	oldHash := p.ContentHash

	// Leaf task can be removed.
	got, err := svc.RemoveTask(ctx, p.ID, "t2", nil)
	require.NoError(t, err)
	require.Len(t, got.Tasks, 1)
	require.Equal(t, "t1", got.Tasks[0].ID)
	_, ok := got.TaskProgress["t2"]
	require.False(t, ok)
	require.Equal(t, "pending", got.TaskProgress["t1"])
	require.Equal(t, int64(2), got.Revision)
	require.NotEqual(t, oldHash, got.ContentHash)
}

func TestPlanService_RemoveTask_blockedByDependents(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()
	p, err := svc.Create(ctx, "proj-1", sampleCreate("Remove Blocked"))
	require.NoError(t, err)

	_, err = svc.RemoveTask(ctx, p.ID, "t1", nil)
	var ve *ValidationError
	require.ErrorAs(t, err, &ve)
	require.Contains(t, ve.Error(), "depends on it")

	got, err := svc.Get(ctx, p.ID)
	require.NoError(t, err)
	require.Len(t, got.Tasks, 2)
	require.Equal(t, int64(1), got.Revision)
}

func TestPlanService_RemoveTask_notPendingAndConflict(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()

	p, err := svc.Create(ctx, "proj-1", sampleCreate("Remove Lock"))
	require.NoError(t, err)
	_, err = svc.Transition(ctx, p.ID, PlanStatusInProgress, TransitionOptions{})
	require.NoError(t, err)
	_, err = svc.RemoveTask(ctx, p.ID, "t2", nil)
	require.ErrorIs(t, err, ErrNotPending)

	p2, err := svc.Create(ctx, "proj-1", sampleCreate("Remove Race"))
	require.NoError(t, err)
	stale := int64(0)
	_, err = svc.RemoveTask(ctx, p2.ID, "t2", &stale)
	require.ErrorIs(t, err, ErrRevisionConflict)
	got, err := svc.Get(ctx, p2.ID)
	require.NoError(t, err)
	require.Len(t, got.Tasks, 2)
}

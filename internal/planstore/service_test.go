package planstore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
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
	require.Equal(t, filepath.Join("plans", "pending", "my-feature.yaml"), p.FilePath)
	require.Equal(t, map[string]string{"t1": "pending", "t2": "pending"}, p.TaskProgress)

	abs := filepath.Join(root, p.FilePath)
	require.FileExists(t, abs)
	raw, err := os.ReadFile(abs)
	require.NoError(t, err)
	var doc planDocument
	require.NoError(t, yaml.Unmarshal(raw, &doc))
	require.Equal(t, "My Feature", doc.Name)
	require.Equal(t, "do the thing", doc.Goal)
	require.Len(t, doc.Tasks, 2)
	require.Equal(t, []string{"t1"}, doc.Tasks[1].After)

	entries, err := os.ReadDir(filepath.Join(root, "plans", "pending"))
	require.NoError(t, err)
	for _, e := range entries {
		require.False(t, strings.HasSuffix(e.Name(), ".tmp"), "temp file left behind: %s", e.Name())
	}
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
}

func TestPlanService_Create_dbFailureRemovesTemp(t *testing.T) {
	svc, store, root, _ := newTestService(t)
	ctx := context.Background()

	req := sampleCreate("Dup Plan")
	id := PlanID("proj-1", "Dup Plan")
	require.NoError(t, store.Create(ctx, &Plan{
		ID: id, ProjectID: "proj-1", Name: "Dup Plan",
		FilePath: "plans/pending/other.yaml", Status: PlanStatusPending,
	}))

	_, err := svc.Create(ctx, "proj-1", req)
	require.ErrorIs(t, err, ErrExists)

	pending := filepath.Join(root, "plans", "pending")
	if entries, readErr := os.ReadDir(pending); readErr == nil {
		for _, e := range entries {
			require.False(t, strings.Contains(e.Name(), ".tmp"), "temp file left behind after DB failure: %s", e.Name())
			require.NotEqual(t, "dup-plan.yaml", e.Name(), "final YAML must not be placed on DB failure")
		}
	}
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

	newName := "Edited"
	newGoal := "new goal"
	tasks := []TaskSpec{{ID: "only", Prompt: "just this"}}
	got, err := svc.Update(ctx, p.ID, UpdateRequest{Name: &newName, Goal: &newGoal, Tasks: &tasks})
	require.NoError(t, err)
	require.Equal(t, "Edited", got.Name)
	require.Equal(t, PlanStatusPending, got.Status)
	// Filename is derived on create only.
	require.Equal(t, filepath.Join("plans", "pending", "edit-me.yaml"), got.FilePath)

	raw, err := os.ReadFile(filepath.Join(root, got.FilePath))
	require.NoError(t, err)
	var doc planDocument
	require.NoError(t, yaml.Unmarshal(raw, &doc))
	require.Equal(t, "Edited", doc.Name)
	require.Equal(t, "new goal", doc.Goal)
	require.Len(t, doc.Tasks, 1)
	require.Equal(t, "only", doc.Tasks[0].ID)
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
		require.FileExists(t, filepath.Join(root, "plans", "in_progress", "go-live.yaml"))
		require.NoFileExists(t, filepath.Join(root, "plans", "pending", "go-live.yaml"))
	})

	t.Run("valid in_progress back to pending", func(t *testing.T) {
		svc, _, root, _ := newTestService(t)
		p, err := svc.Create(ctx, "proj-1", sampleCreate("Bounce"))
		require.NoError(t, err)
		_, err = svc.Transition(ctx, p.ID, PlanStatusInProgress, TransitionOptions{})
		require.NoError(t, err)
		got, err := svc.Transition(ctx, p.ID, PlanStatusPending, TransitionOptions{})
		require.NoError(t, err)
		require.Equal(t, PlanStatusPending, got.Status)
		require.FileExists(t, filepath.Join(root, "plans", "pending", "bounce.yaml"))
	})

	t.Run("valid pending to archived", func(t *testing.T) {
		svc, _, root, _ := newTestService(t)
		p, err := svc.Create(ctx, "proj-1", sampleCreate("Shelf"))
		require.NoError(t, err)
		got, err := svc.Transition(ctx, p.ID, PlanStatusArchived, TransitionOptions{})
		require.NoError(t, err)
		require.Equal(t, PlanStatusArchived, got.Status)
		require.FileExists(t, filepath.Join(root, "plans", "archived", "shelf.yaml"))
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
	require.FileExists(t, filepath.Join(root, "plans", "completed", "all-green.yaml"))
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
		FilePath:  "plans/in_progress/with-branches.yaml",
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

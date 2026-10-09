package planstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/srjn45/scriva/engine"
	"github.com/stretchr/testify/require"
)

func TestStoreUsesPerWriteDurability(t *testing.T) {
	s := newTestStore(t)
	require.Equal(t, engine.SyncModeAlways, s.col.Config().SyncMode)
	require.Equal(t, engine.SyncModeAlways, s.events.Config().SyncMode)
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

func TestCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	p := &Plan{
		ID:        "plan-aabbccdd",
		ProjectID: "proj-1",
		Name:      "feature-x",
		FilePath:  "plans/pending/feature-x.yaml",
		Status:    PlanStatusPending,
	}

	// Create
	require.NoError(t, s.Create(ctx, p))
	require.ErrorIs(t, s.Create(ctx, p), ErrExists)

	// Get
	got, err := s.Get(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, p.Name, got.Name)
	require.Equal(t, p.Status, got.Status)
	require.False(t, got.CreatedAt.IsZero())

	// Update
	require.NoError(t, s.Update(ctx, p.ID, func(pl *Plan) error {
		pl.Status = PlanStatusInProgress
		return nil
	}))
	got, err = s.Get(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, PlanStatusInProgress, got.Status)

	// List
	list, err := s.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)

	// Delete
	require.NoError(t, s.Delete(ctx, p.ID))
	_, err = s.Get(ctx, p.ID)
	require.ErrorIs(t, err, ErrNotFound)
	require.ErrorIs(t, s.Delete(ctx, p.ID), ErrNotFound)
}

func TestListByProject(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	plans := []*Plan{
		{ID: "plan-aa000001", ProjectID: "proj-a", Name: "p1", FilePath: "plans/pending/p1.yaml", Status: PlanStatusPending},
		{ID: "plan-aa000002", ProjectID: "proj-a", Name: "p2", FilePath: "plans/in_progress/p2.yaml", Status: PlanStatusInProgress},
		{ID: "plan-bb000001", ProjectID: "proj-b", Name: "p3", FilePath: "plans/pending/p3.yaml", Status: PlanStatusPending},
	}
	for _, p := range plans {
		require.NoError(t, s.Create(ctx, p))
	}

	got, err := s.ListByProject(ctx, "proj-a")
	require.NoError(t, err)
	require.Len(t, got, 2)

	got, err = s.ListByProjectAndStatus(ctx, "proj-a", PlanStatusPending)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "p1", got[0].Name)

	got, err = s.ListByProject(ctx, "proj-b")
	require.NoError(t, err)
	require.Len(t, got, 1)
}

func TestPlanID_stableAcrossMoves(t *testing.T) {
	id1 := PlanID("proj-x", "brain-consult")
	id2 := PlanID("proj-x", "brain-consult")
	require.Equal(t, id1, id2)
	require.True(t, len(id1) > 5)
	require.Equal(t, "plan-", id1[:5])

	// Different names produce different IDs.
	require.NotEqual(t, PlanID("proj-x", "a"), PlanID("proj-x", "b"))
	// Same name in different projects produces different IDs.
	require.NotEqual(t, PlanID("proj-x", "a"), PlanID("proj-y", "a"))
}

// writePlanFile creates a YAML plan file at dir/subpath with the given name
// field. Pass name="" to write a file with no name: key.
func writePlanFile(t *testing.T, root, subpath, planName string) {
	t.Helper()
	abs := filepath.Join(root, subpath)
	require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
	var content string
	if planName != "" {
		content = "name: " + planName + "\ngoal: test\n"
	} else {
		content = "goal: test\n"
	}
	require.NoError(t, os.WriteFile(abs, []byte(content), 0o644))
}

func TestScanProject_basic(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	root := t.TempDir()

	writePlanFile(t, root, "plans/pending/feature-x.yaml", "feature-x")
	writePlanFile(t, root, "plans/in_progress/brain-consult.yaml", "brain-consult")
	writePlanFile(t, root, "plans/completed/old-thing.yaml", "old-thing")

	n, err := ScanProject(ctx, s, "proj-1", root)
	require.NoError(t, err)
	require.Equal(t, 3, n.Upserted)
	require.Equal(t, ScanDeprecationNotice, n.Notice)

	list, err := s.ListByProject(ctx, "proj-1")
	require.NoError(t, err)
	require.Len(t, list, 3)

	id := PlanID("proj-1", "brain-consult")
	p, err := s.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, PlanStatusInProgress, p.Status)
	require.Equal(t, "plans/in_progress/brain-consult.yaml", p.FilePath)
}

func TestScanProject_idempotent(t *testing.T) {
	// A second scan on an unchanged filesystem must produce zero updates.
	s := newTestStore(t)
	ctx := context.Background()
	root := t.TempDir()

	writePlanFile(t, root, "plans/pending/feature-x.yaml", "feature-x")

	// First scan: creates.
	n, err := ScanProject(ctx, s, "proj-1", root)
	require.NoError(t, err)
	require.Equal(t, 1, n.Upserted)

	// Set an execution link only (file stays in pending/).
	id := PlanID("proj-1", "feature-x")
	require.NoError(t, s.Update(ctx, id, func(p *Plan) error {
		p.AutopilotRunID = "ap-run-42"
		return nil
	}))

	// Second scan: no filesystem changes → zero upserts.
	n, err = ScanProject(ctx, s, "proj-1", root)
	require.NoError(t, err)
	require.Equal(t, 0, n.Upserted)

	// Execution link must be preserved.
	got, err := s.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "ap-run-42", got.AutopilotRunID)
	require.Equal(t, PlanStatusPending, got.Status)
}

func TestScanProject_gitMv(t *testing.T) {
	// Simulate git mv plans/pending/brain-consult.yaml plans/in_progress/brain-consult.yaml
	s := newTestStore(t)
	ctx := context.Background()
	root := t.TempDir()

	// Initial state: file is in pending/.
	writePlanFile(t, root, "plans/pending/brain-consult.yaml", "brain-consult")
	n, err := ScanProject(ctx, s, "proj-1", root)
	require.NoError(t, err)
	require.Equal(t, 1, n.Upserted)

	id := PlanID("proj-1", "brain-consult")

	// Simulate the daemon linking an execution entity.
	require.NoError(t, s.Update(ctx, id, func(p *Plan) error {
		p.PipelineID = "pipe-99"
		return nil
	}))

	// git mv: remove pending file, create in_progress file.
	require.NoError(t, os.Remove(filepath.Join(root, "plans/pending/brain-consult.yaml")))
	writePlanFile(t, root, "plans/in_progress/brain-consult.yaml", "brain-consult")

	// Second scan: stub record → update FilePath + Status, preserve PipelineID.
	n, err = ScanProject(ctx, s, "proj-1", root)
	require.NoError(t, err)
	require.Equal(t, 1, n.Upserted)

	got, err := s.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, PlanStatusInProgress, got.Status)
	require.Equal(t, "plans/in_progress/brain-consult.yaml", got.FilePath)
	require.Equal(t, "pipe-99", got.PipelineID, "execution link must be preserved across git-mv")

	// Scan must not produce a duplicate.
	list, err := s.ListByProject(ctx, "proj-1")
	require.NoError(t, err)
	require.Len(t, list, 1, "git-mv must not create a duplicate record")
}

func TestScanProject_flatFile(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	root := t.TempDir()

	// Flat plans/*.yaml → treated as pending.
	writePlanFile(t, root, "plans/my-flat-plan.yaml", "my-flat-plan")

	n, err := ScanProject(ctx, s, "proj-1", root)
	require.NoError(t, err)
	require.Equal(t, 1, n.Upserted)

	id := PlanID("proj-1", "my-flat-plan")
	got, err := s.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, PlanStatusPending, got.Status)
	require.Equal(t, "plans/my-flat-plan.yaml", got.FilePath)
}

func TestScanProject_noNameFieldFallsBackToStem(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	root := t.TempDir()

	// File with no name: key — falls back to filename stem.
	writePlanFile(t, root, "plans/pending/stem-only.yaml", "")

	n, err := ScanProject(ctx, s, "proj-1", root)
	require.NoError(t, err)
	require.Equal(t, 1, n.Upserted)

	id := PlanID("proj-1", "stem-only")
	got, err := s.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "stem-only", got.Name)
}

func TestScanProject_noPlansDirIsNoop(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	root := t.TempDir() // no plans/ directory at all

	n, err := ScanProject(ctx, s, "proj-1", root)
	require.NoError(t, err)
	require.Equal(t, 0, n.Upserted)
	require.Equal(t, ScanDeprecationNotice, n.Notice)
}

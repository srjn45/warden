package planstore

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func sampleCanonicalPlan() *Plan {
	return &Plan{
		ID:        "plan-cafebabe",
		ProjectID: "proj-1",
		Name:      "ship-feature",
		Goal:      "land the feature",
		Constraints: []string{
			"stay on integration branch",
			"no force-push to main",
		},
		DoneWhen: []string{"PR green", "landed"},
		Tasks: []PlanTask{
			{ID: "analyze", Prompt: "read the code", After: nil},
			{ID: "implement", Prompt: "write the code", After: []string{"analyze"}},
			{ID: "review", Prompt: "review the PR", After: []string{"implement"}},
		},
		Status:   PlanStatusPending,
		FilePath: "plans/pending/ship-feature.yaml",
	}
}

func TestCanonicalPlan_storeRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	archived := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	exported := archived.Add(-time.Hour)
	p := sampleCanonicalPlan()
	p.ArchivedAt = &archived
	p.RepoExport = &RepoExportMeta{
		SchemaVersion: 1,
		Revision:      1,
		ContentHash:   "sha256:placeholder",
		ExportedAt:    &exported,
		Lifecycle:     PlanStatusPending,
		FilePath:      "plans/pending/ship-feature.yaml",
		ExecutionSummaryRef: &ExecutionSummaryRef{
			PlanID:      p.ID,
			ExecutionID: "pe-deadbeef",
			ContentHash: "sha256:placeholder",
		},
	}
	p.ActiveExecution = &PlanExecution{
		ID:            "pe-deadbeef",
		PlanID:        p.ID,
		ExecutionMode: PlanModeManual,
		StartedAt:     exported,
	}

	require.NoError(t, s.Create(ctx, p))
	require.Equal(t, int64(1), p.Revision)
	require.NotEmpty(t, p.ContentHash)
	require.Equal(t, ComputeContentHash(p), p.ContentHash)

	got, err := s.Get(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, p.Name, got.Name)
	require.Equal(t, p.Goal, got.Goal)
	require.Equal(t, p.Constraints, got.Constraints)
	require.Equal(t, p.DoneWhen, got.DoneWhen)
	require.Equal(t, p.Tasks, got.Tasks)
	require.Equal(t, p.Status, got.Status)
	require.Equal(t, int64(1), got.Revision)
	require.Equal(t, p.ContentHash, got.ContentHash)
	require.NotNil(t, got.ArchivedAt)
	require.True(t, got.ArchivedAt.Equal(archived))
	require.NotNil(t, got.RepoExport)
	require.Equal(t, 1, got.RepoExport.SchemaVersion)
	require.Equal(t, PlanStatusPending, got.RepoExport.Lifecycle)
	require.Equal(t, "plans/pending/ship-feature.yaml", got.RepoExport.FilePath)
	require.NotNil(t, got.RepoExport.ExecutionSummaryRef)
	require.Equal(t, "pe-deadbeef", got.RepoExport.ExecutionSummaryRef.ExecutionID)
	require.NotNil(t, got.ActiveExecution)
	require.Equal(t, "pe-deadbeef", got.ActiveExecution.ID)

	// RepoExport must round-trip independently of ExecutionHistory.
	require.Nil(t, got.ExecutionHistory)
}

func TestComputeContentHash_determinism(t *testing.T) {
	a := sampleCanonicalPlan()
	b := sampleCanonicalPlan()

	h1 := ComputeContentHash(a)
	h2 := ComputeContentHash(b)
	require.Equal(t, h1, h2)
	require.True(t, len(h1) > len("sha256:"))
	require.Equal(t, "sha256:", h1[:7])

	// Nil vs empty slices must hash identically.
	a.Constraints = nil
	b.Constraints = []string{}
	a.DoneWhen = nil
	b.DoneWhen = []string{}
	a.Tasks[0].After = nil
	b.Tasks[0].After = []string{}
	require.Equal(t, ComputeContentHash(a), ComputeContentHash(b))

	// Definition mutation changes the hash.
	b.Goal = "different goal"
	require.NotEqual(t, ComputeContentHash(a), ComputeContentHash(b))

	// Task after-order is significant.
	c := sampleCanonicalPlan()
	c.Tasks[1].After = []string{"review"} // was analyze→implement
	require.NotEqual(t, ComputeContentHash(a), ComputeContentHash(c))

	// Lifecycle / execution fields must NOT participate in the hash.
	d := sampleCanonicalPlan()
	base := ComputeContentHash(d)
	d.Status = PlanStatusInProgress
	d.AutopilotRunID = "ap-1"
	d.TaskProgress = map[string]string{"analyze": "done"}
	d.Revision = 99
	d.FilePath = "plans/in_progress/ship-feature.yaml"
	d.RepoExport = &RepoExportMeta{SchemaVersion: 1, FilePath: "x.yaml"}
	require.Equal(t, base, ComputeContentHash(d))
}

func TestUpdateIf_revisionConflict(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	p := sampleCanonicalPlan()
	require.NoError(t, s.Create(ctx, p))
	require.Equal(t, int64(1), p.Revision)

	// Stale expected revision → structured conflict, no mutation.
	err := s.UpdateIf(ctx, p.ID, 0, func(pl *Plan) error {
		pl.Goal = "should not land"
		return nil
	})
	require.ErrorIs(t, err, ErrRevisionConflict)
	var conflict *RevisionConflictError
	require.ErrorAs(t, err, &conflict)
	require.Equal(t, p.ID, conflict.PlanID)
	require.Equal(t, int64(0), conflict.Expected)
	require.Equal(t, int64(1), conflict.Actual)

	got, err := s.Get(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, "land the feature", got.Goal)
	require.Equal(t, int64(1), got.Revision)

	// Matching expected revision + definition edit → bump + rehash.
	oldHash := got.ContentHash
	require.NoError(t, s.UpdateIf(ctx, p.ID, 1, func(pl *Plan) error {
		pl.Goal = "land the feature, for real"
		return nil
	}))
	got, err = s.Get(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, "land the feature, for real", got.Goal)
	require.Equal(t, int64(2), got.Revision)
	require.NotEqual(t, oldHash, got.ContentHash)
	require.Equal(t, ComputeContentHash(got), got.ContentHash)

	// Lifecycle-only change also bumps revision; hash stays stable when
	// definition is unchanged.
	hashBefore := got.ContentHash
	require.NoError(t, s.UpdateIf(ctx, p.ID, 2, func(pl *Plan) error {
		pl.Status = PlanStatusInProgress
		now := time.Now().UTC()
		pl.StartedAt = &now
		return nil
	}))
	got, err = s.Get(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, PlanStatusInProgress, got.Status)
	require.Equal(t, int64(3), got.Revision)
	require.Equal(t, hashBefore, got.ContentHash)

	// Execution-only mutation does not bump revision.
	require.NoError(t, s.UpdateIf(ctx, p.ID, 3, func(pl *Plan) error {
		pl.TaskProgress = map[string]string{"analyze": "done"}
		pl.AutopilotRunID = "ap-99"
		return nil
	}))
	got, err = s.Get(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, int64(3), got.Revision)
	require.Equal(t, "ap-99", got.AutopilotRunID)
	require.Equal(t, "done", got.TaskProgress["analyze"])
}

func TestCanonicalPlan_absentFieldMigration(t *testing.T) {
	// Simulate a pre-canonical ScrivaDB document that lacks the new fields.
	legacy := map[string]any{
		"id":         "plan-legacy01",
		"project_id": "proj-legacy",
		"name":       "old-plan",
		"file_path":  "plans/pending/old-plan.yaml",
		"status":     "pending",
		"created_at": "2026-08-01T00:00:00Z",
		"updated_at": "2026-08-01T00:00:00Z",
	}

	p, err := decodeRecord(legacy)
	require.NoError(t, err)
	require.Equal(t, "plan-legacy01", p.ID)
	require.Equal(t, "old-plan", p.Name)
	require.Equal(t, PlanStatusPending, p.Status)

	// Absent new fields decode as zero / nil — no error, no panic.
	require.Equal(t, "", p.Goal)
	require.Nil(t, p.Constraints)
	require.Nil(t, p.DoneWhen)
	require.Nil(t, p.Tasks)
	require.Equal(t, int64(0), p.Revision)
	require.Equal(t, "", p.ContentHash)
	require.Nil(t, p.ArchivedAt)
	require.Nil(t, p.RepoExport)
	require.Nil(t, p.ActiveExecution)
	require.Nil(t, p.ExecutionHistory)

	// Persist via Create so Revision/ContentHash initialize for new writes.
	s := newTestStore(t)
	ctx := context.Background()
	require.NoError(t, s.Create(ctx, p))
	got, err := s.Get(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, int64(1), got.Revision)
	require.Equal(t, ComputeContentHash(got), got.ContentHash)

	// Round-trip the legacy map through JSON to prove omitempty still works
	// the other direction for zero values on a freshly decoded struct.
	raw, err := json.Marshal(map[string]any{
		"id":         "plan-legacy02",
		"project_id": "proj-legacy",
		"name":       "older-plan",
		"status":     "completed",
	})
	require.NoError(t, err)
	var minimal Plan
	require.NoError(t, json.Unmarshal(raw, &minimal))
	require.Equal(t, int64(0), minimal.Revision)
	require.Nil(t, minimal.RepoExport)
	require.Empty(t, minimal.Tasks)
}

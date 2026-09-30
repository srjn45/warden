package planstore

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestComputeTaskSummary_FromCanonicalTasks(t *testing.T) {
	p := &Plan{
		Tasks: []PlanTask{
			{ID: "a", Prompt: "one"},
			{ID: "b", Prompt: "two"},
			{ID: "c", Prompt: "three"},
		},
		TaskProgress: map[string]string{
			"a": "done",
			"b": "in_progress",
		},
	}
	s := ComputeTaskSummary(p)
	require.Equal(t, 3, s.Total)
	require.Equal(t, 1, s.Done)
	require.Equal(t, 1, s.InProgress)
	require.Equal(t, 1, s.Pending)
	require.Equal(t, "1/3", s.String())
}

func TestComputeTaskSummary_NoTasksUsesProgressKeys(t *testing.T) {
	p := &Plan{
		TaskProgress: map[string]string{"x": "done", "y": "skipped"},
		TaskOutcomes: map[string]TaskOutcome{"z": {TaskID: "z", Status: "done"}},
	}
	s := ComputeTaskSummary(p)
	require.Equal(t, 3, s.Total)
	require.Equal(t, 2, s.Done)
	require.Equal(t, 1, s.Skipped)
}

func TestComputeExportStatus(t *testing.T) {
	require.Equal(t, ExportStatusNone, ComputeExportStatus(nil))
	require.Equal(t, ExportStatusNone, ComputeExportStatus(&Plan{Revision: 2}))

	p := &Plan{
		Revision:    3,
		ContentHash: "sha256:abc",
		RepoExport: &RepoExportMeta{
			Revision:    3,
			ContentHash: "sha256:abc",
		},
	}
	require.Equal(t, ExportStatusCurrent, ComputeExportStatus(p))

	p.Revision = 4
	require.Equal(t, ExportStatusStale, ComputeExportStatus(p))
}

func TestExecutorID_PrefersActiveExecution(t *testing.T) {
	p := &Plan{
		AutopilotRunID: "ap-1",
		ActiveExecution: &PlanExecution{
			ExecutorID: "sess-live",
		},
	}
	require.Equal(t, "sess-live", ExecutorID(p))
	p.ActiveExecution = nil
	require.Equal(t, "ap-1", ExecutorID(p))
}

func TestFindRelatedPlans_HeuristicScoring(t *testing.T) {
	anchor := &Plan{
		ID:        "plan-anchor",
		ProjectID: "/proj",
		Name:      "canonical plans repo sync",
		Goal:      "make ScrivaDB the sole store for plan definitions",
		Branches:  []string{"feature/plans"},
		Status:    PlanStatusInProgress,
	}
	candidates := []*Plan{
		{
			ID: "plan-same-title", ProjectID: "/proj", Name: "canonical plans discovery",
			Goal: "discover plans from ScrivaDB", Status: PlanStatusPending,
		},
		{
			ID: "plan-shared-branch", ProjectID: "/proj", Name: "unrelated name",
			Goal: "something else entirely", Branches: []string{"feature/plans"},
			Status: PlanStatusCompleted,
		},
		{
			ID: "plan-other-proj", ProjectID: "/other", Name: "canonical plans repo sync",
			Goal: "make ScrivaDB the sole store", Status: PlanStatusPending,
		},
		{
			ID: "plan-noise", ProjectID: "/proj", Name: "docs polish",
			Goal: "fix typos", Status: PlanStatusPending,
		},
		anchor,
	}
	res := FindRelatedPlans(anchor, candidates, 5)
	require.True(t, res.Heuristic)
	require.Contains(t, res.Disclaimer, "not authoritative")
	require.Equal(t, "plan-anchor", res.AnchorID)
	require.NotEmpty(t, res.Hits)

	ids := map[string]bool{}
	for _, h := range res.Hits {
		ids[h.PlanID] = true
		require.NotEqual(t, "plan-anchor", h.PlanID)
		require.NotEqual(t, "plan-other-proj", h.PlanID, "cross-project candidates must be ignored")
		require.NotEqual(t, "plan-noise", h.PlanID, "same-project-only noise must be filtered")
	}
	require.True(t, ids["plan-same-title"])
	require.True(t, ids["plan-shared-branch"], "completed plans remain for historical lookup")
	require.GreaterOrEqual(t, res.Hits[0].Score, res.Hits[len(res.Hits)-1].Score)
}

func TestFindRelatedPlans_NoYAMLAuthority(t *testing.T) {
	// Two DB plans; a filesystem replica path must not invent a third hit.
	anchor := &Plan{
		ID: "plan-a", ProjectID: "/p", Name: "feature alpha", Goal: "ship alpha",
		FilePath: "plans/pending/feature-alpha.yaml", Status: PlanStatusPending,
	}
	other := &Plan{
		ID: "plan-b", ProjectID: "/p", Name: "feature alpha followup", Goal: "ship alpha next",
		FilePath: "plans/pending/feature-alpha.yaml", // same inert path; still one DB plan
		Status:   PlanStatusPending,
	}
	res := FindRelatedPlans(anchor, []*Plan{other}, 10)
	require.Len(t, res.Hits, 1)
	require.Equal(t, "plan-b", res.Hits[0].PlanID)
}

func TestFormatPlanDefinition_DBOnly(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	p := &Plan{
		ID: "plan-db", ProjectID: "/p", Name: "db-native", Goal: "no yaml",
		Status: PlanStatusPending, Revision: 2, ContentHash: "sha256:x",
		Tasks:     []PlanTask{{ID: "t1", Prompt: "implement", After: []string{}}},
		CreatedAt: now, UpdatedAt: now,
	}
	text := FormatPlanDefinition(p)
	require.Contains(t, text, "Plan ID: plan-db")
	require.Contains(t, text, "Revision: 2")
	require.Contains(t, text, "Export-Status: none")
	require.Contains(t, text, "- t1: implement")
	require.NotContains(t, text, "plans/pending")
}

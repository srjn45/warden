package daemon

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/planstore"
)

func TestPlansProjection_NoPlansDirIdentical(t *testing.T) {
	ts, ps, root := crudPlanServer(t)
	// Explicitly ensure no plans/ directory exists.
	_, err := os.Stat(filepath.Join(root, "plans"))
	require.True(t, os.IsNotExist(err))

	require.NoError(t, ps.Create(t.Context(), &planstore.Plan{
		ID: planstore.PlanID(root, "db-native"), ProjectID: root, Name: "db-native",
		Goal: "discover from ScrivaDB", Status: planstore.PlanStatusPending,
		Tasks: []planstore.PlanTask{{ID: "t1", Prompt: "implement"}},
	}))
	p, err := ps.Get(t.Context(), planstore.PlanID(root, "db-native"))
	require.NoError(t, err)
	require.NotEmpty(t, p.ID)

	q := url.Values{"project_id": {root}}
	resp, err := http.Get(crudPlansURL(ts.URL, "", q))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var listed []oapi.Plan
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&listed))
	require.Len(t, listed, 1)
	require.Equal(t, p.ID, listed[0].Id)
	require.Equal(t, oapi.None, listed[0].ExportStatus)
	require.Equal(t, 1, listed[0].TaskSummary.Total)
	require.Equal(t, 1, listed[0].TaskSummary.Pending)
	require.Equal(t, int64(1), listed[0].Revision)

	// Stale/deleted YAML replica must not appear as a second plan.
	require.NoError(t, os.MkdirAll(filepath.Join(root, "plans", "pending"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "plans", "pending", "db-native.yaml"),
		[]byte("name: db-native\ngoal: stale replica\ntasks:\n  - id: ghost\n    prompt: should not list\n"), 0o644))

	resp2, err := http.Get(crudPlansURL(ts.URL, "", q))
	require.NoError(t, err)
	defer resp2.Body.Close()
	var listed2 []oapi.Plan
	require.NoError(t, json.NewDecoder(resp2.Body).Decode(&listed2))
	require.Len(t, listed2, 1, "YAML replica must not create a second list entry")
	require.Equal(t, listed[0].Id, listed2[0].Id)
	require.Equal(t, "discover from ScrivaDB", listed2[0].Goal)
}

func TestPlansProjection_ExportStatusAndRelated(t *testing.T) {
	ts, ps, root := crudPlanServer(t)
	now := time.Now().UTC()

	anchor := &planstore.Plan{
		ID: planstore.PlanID(root, "anchor-feat"), ProjectID: root, Name: "anchor feature sync",
		Goal: "ship canonical plans", Status: planstore.PlanStatusInProgress, Revision: 2,
		ContentHash: "sha256:live", Branches: []string{"feat/anchor"},
		Tasks:          []planstore.PlanTask{{ID: "a", Prompt: "one"}, {ID: "b", Prompt: "two"}},
		TaskProgress:   map[string]string{"a": "done", "b": "in_progress"},
		AutopilotRunID: "ap-9",
		RepoExport: &planstore.RepoExportMeta{
			Revision: 1, ContentHash: "sha256:old", FilePath: "plans/in_progress/anchor.yaml",
			ExportedAt: &now, Lifecycle: planstore.PlanStatusInProgress,
		},
		CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, ps.Create(t.Context(), anchor))

	related := &planstore.Plan{
		ID: planstore.PlanID(root, "related-feat"), ProjectID: root, Name: "related feature discovery",
		Goal: "discover canonical plans", Status: planstore.PlanStatusCompleted, Revision: 1,
		Branches:  []string{"feat/anchor"},
		Tasks:     []planstore.PlanTask{{ID: "r1", Prompt: "done work"}},
		CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, ps.Create(t.Context(), related))

	noise := &planstore.Plan{
		ID: planstore.PlanID(root, "noise"), ProjectID: root, Name: "docs polish",
		Goal: "fix typos only", Status: planstore.PlanStatusPending, Revision: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, ps.Create(t.Context(), noise))

	gotResp, err := http.Get(crudPlansURL(ts.URL, "/"+url.PathEscape(anchor.ID), nil))
	require.NoError(t, err)
	defer gotResp.Body.Close()
	require.Equal(t, http.StatusOK, gotResp.StatusCode)
	var got oapi.Plan
	require.NoError(t, json.NewDecoder(gotResp.Body).Decode(&got))
	require.Equal(t, oapi.Stale, got.ExportStatus)
	require.Equal(t, "ap-9", got.ExecutorId)
	require.Equal(t, 2, got.TaskSummary.Total)
	require.Equal(t, 1, got.TaskSummary.Done)
	require.Equal(t, 1, got.TaskSummary.InProgress)
	require.Equal(t, "plans/in_progress/anchor.yaml", got.RepoExport.FilePath)

	relResp, err := http.Get(crudPlansURL(ts.URL, "/"+url.PathEscape(anchor.ID)+"/related", nil))
	require.NoError(t, err)
	defer relResp.Body.Close()
	require.Equal(t, http.StatusOK, relResp.StatusCode)
	var res oapi.RelatedPlansResult
	require.NoError(t, json.NewDecoder(relResp.Body).Decode(&res))
	require.True(t, res.Heuristic)
	require.Contains(t, res.Disclaimer, "not authoritative")
	require.Equal(t, anchor.ID, res.AnchorId)
	require.NotEmpty(t, res.Hits)
	ids := map[string]bool{}
	for _, h := range res.Hits {
		ids[h.PlanId] = true
	}
	require.True(t, ids[related.ID], "completed related plan must remain available")
	require.False(t, ids[noise.ID], "noise-only same-project plan must be filtered")
	require.False(t, ids[anchor.ID])
}

func TestPlansProjection_StaleYAMLNotSecondPlan(t *testing.T) {
	ts, ps, root := crudPlanServer(t)
	p := &planstore.Plan{
		ID: planstore.PlanID(root, "only-one"), ProjectID: root, Name: "only-one",
		Goal: "canonical", Status: planstore.PlanStatusPending, Revision: 1,
		FilePath: "plans/pending/only-one.yaml",
		Tasks:    []planstore.PlanTask{{ID: "t1", Prompt: "from db"}},
	}
	require.NoError(t, ps.Create(t.Context(), p))

	// Write a divergent YAML with a different goal — must not affect list/get.
	require.NoError(t, os.MkdirAll(filepath.Join(root, "plans", "pending"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "plans", "pending", "only-one.yaml"),
		[]byte("name: only-one\ngoal: from yaml\ntasks:\n  - id: yaml-task\n    prompt: ghost\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "plans", "pending", "orphan.yaml"),
		[]byte("name: orphan\ngoal: never imported\ntasks:\n  - id: o1\n    prompt: x\n"), 0o644))

	q := url.Values{"project_id": {root}}
	resp, err := http.Get(crudPlansURL(ts.URL, "", q))
	require.NoError(t, err)
	defer resp.Body.Close()
	var listed []oapi.Plan
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&listed))
	require.Len(t, listed, 1)
	require.Equal(t, "canonical", listed[0].Goal)
	require.Len(t, listed[0].Tasks, 1)
	require.Equal(t, "t1", listed[0].Tasks[0].Id)
}

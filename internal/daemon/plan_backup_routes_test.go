package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/planbackup"
	"github.com/srjn45/warden/internal/planstore"
)

func TestPlanBackupRoutes_exportRestoreRoundTrip(t *testing.T) {
	ps, err := planstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = ps.Close() })

	plan := &planstore.Plan{
		ID:        planstore.PlanID("/proj", "backup-route"),
		ProjectID: "/proj",
		Name:      "Backup Route",
		Goal:      "api round trip",
		Status:    planstore.PlanStatusPending,
		Revision:  1,
		Tasks:     []planstore.PlanTask{{ID: "t1", Prompt: "do"}},
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	plan.ContentHash = planstore.ComputeContentHash(plan)
	require.NoError(t, ps.Create(context.Background(), plan))
	require.NoError(t, ps.AppendEvent(context.Background(), &planstore.PlanExecutionEvent{
		ID: "ev-route-1", PlanID: plan.ID, ExecutionID: "pe-route",
		Kind: planstore.EventKindExecutionStarted, OccurredAt: time.Now().UTC(),
	}))

	srv := &Server{store: newFakeStore(), life: &fakeLife{}, plans: ps}
	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)

	exportBody := []byte(`{"plan_ids":["` + plan.ID + `"]}`)
	resp, err := http.Post(ts.URL+"/api/v1/plans/export_backup", "application/json", bytes.NewReader(exportBody))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var bundle planbackup.Bundle
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&bundle))
	require.Equal(t, planbackup.SchemaVersion, bundle.SchemaVersion)
	require.Len(t, bundle.Entries, 1)
	require.Len(t, bundle.Entries[0].Events, 1)

	ps2, err := planstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = ps2.Close() })
	srv2 := &Server{store: newFakeStore(), life: &fakeLife{}, plans: ps2}
	ts2 := httptest.NewServer(srv2.router())
	t.Cleanup(ts2.Close)

	restorePayload, err := json.Marshal(map[string]any{"bundle": bundle, "dry_run": true})
	require.NoError(t, err)
	resp, err = http.Post(ts2.URL+"/api/v1/plans/restore_backup", "application/json", bytes.NewReader(restorePayload))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	restorePayload, err = json.Marshal(map[string]any{"bundle": bundle})
	require.NoError(t, err)
	resp, err = http.Post(ts2.URL+"/api/v1/plans/restore_backup", "application/json", bytes.NewReader(restorePayload))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var res planbackup.RestoreResult
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&res))
	require.Equal(t, planbackup.OutcomeRestored, res.Entries[0].Outcome)

	got, err := ps2.Get(context.Background(), plan.ID)
	require.NoError(t, err)
	require.Equal(t, plan.Name, got.Name)
	events, err := ps2.ListAllEvents(context.Background(), plan.ID)
	require.NoError(t, err)
	require.Len(t, events, 1)

	resp, err = http.Post(ts2.URL+"/api/v1/plans/restore_backup", "application/json", bytes.NewReader(restorePayload))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&res))
	require.Equal(t, planbackup.OutcomeSkipped, res.Entries[0].Outcome)
}

package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/insights"
	"github.com/srjn45/warden/internal/store"
)

// TestTypeRoleDeprecationAcceptance is the plan-213eaa87 acceptance gate for the
// Type→Role migration: pre-migration Type-only records load with Role backfill,
// history accepts both --role (canonical) and --type (deprecated alias), and
// display/insights helpers classify by Role.
func TestTypeRoleDeprecationAcceptance(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// --- Seed: legacy FileStore records (Type set, Role absent) ------------
	dir := t.TempDir()
	legacy, err := store.NewFileStore(dir)
	require.NoError(t, err)
	require.NoError(t, legacy.Insert(ctx, &store.Session{
		ID: "legacy-dev", Status: store.StatusWorking, Type: store.TypeDevelopment,
		Subject: "pre-migration development work", UpdatedAt: now, CreatedAt: now,
	}))
	require.NoError(t, legacy.Insert(ctx, &store.Session{
		ID: "legacy-review", Status: store.StatusWorking, Type: store.TypePRReview,
		Subject: "pre-migration review", UpdatedAt: now, CreatedAt: now,
	}))
	require.NoError(t, legacy.Insert(ctx, &store.Session{
		ID: "legacy-explicit", Status: store.StatusWorking,
		Type: store.TypeDevelopment, Role: "orchestrator",
		Subject: "role already set", UpdatedAt: now, CreatedAt: now,
	}))
	require.NoError(t, legacy.Close(ctx))

	// --- Open agentstore (import + lazy Role backfill) ---------------------
	agents, err := agentstore.New(dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, agents.Close()) })

	dev, err := agents.Get(ctx, "legacy-dev")
	require.NoError(t, err)
	require.Equal(t, "implementer", dev.Role, "Type=development must backfill Role=implementer")
	require.Equal(t, store.TypeDevelopment, dev.Type, "Type retained during alias window")

	review, err := agents.Get(ctx, "legacy-review")
	require.NoError(t, err)
	require.Equal(t, "reviewer", review.Role)

	explicit, err := agents.Get(ctx, "legacy-explicit")
	require.NoError(t, err)
	require.Equal(t, "orchestrator", explicit.Role, "explicit Role must not be overwritten")

	// Shared helpers agree with the persisted backfill.
	require.Equal(t, "implementer", store.DisplayRole("", store.TypeDevelopment))
	require.Equal(t, "orchestrator", store.EffectiveRole("orchestrator", store.TypeDevelopment))

	// Insights FromSession classifies Type-only records by Role.
	rec := insights.FromSession(&store.Session{ID: "x", Type: store.TypeDevelopment, Status: store.StatusDone}, nil)
	require.Equal(t, "implementer", rec.Role)
	require.Equal(t, "implementer", rec.Type, "Type JSON dual-emitted as Role mirror")

	// --- Archive two Type-only agents for history filter coverage ----------
	require.NoError(t, agents.Archive(ctx, "legacy-dev"))
	require.NoError(t, agents.Archive(ctx, "legacy-review"))

	srv := &Server{store: agents}
	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)

	// Active list: explicit-role agent still visible with Role intact.
	listResp, err := http.Get(ts.URL + "/api/v1/sessions")
	require.NoError(t, err)
	defer listResp.Body.Close()
	require.Equal(t, http.StatusOK, listResp.StatusCode)
	var listed struct {
		Sessions []store.Session `json:"sessions"`
	}
	require.NoError(t, json.NewDecoder(listResp.Body).Decode(&listed))
	require.Len(t, listed.Sessions, 1)
	require.Equal(t, "legacy-explicit", listed.Sessions[0].ID)
	require.Equal(t, "orchestrator", listed.Sessions[0].Role)

	// GET one archived record via history: Role present after backfill.
	code, hist := getHistory(t, srv, "")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, hist.Sessions, 2)
	byID := map[string]*store.Session{}
	for _, s := range hist.Sessions {
		byID[s.ID] = s
	}
	require.Equal(t, "implementer", byID["legacy-dev"].Role)
	require.Equal(t, "reviewer", byID["legacy-review"].Role)

	// Canonical --role filter (mapped server-side from effective role).
	code, hist = getHistory(t, srv, "?role=implementer")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, hist.Sessions, 1)
	require.Equal(t, "legacy-dev", hist.Sessions[0].ID)

	code, hist = getHistory(t, srv, "?role=reviewer")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, hist.Sessions, 1)
	require.Equal(t, "legacy-review", hist.Sessions[0].ID)

	// Deprecated --type alias maps onto role (development→implementer).
	code, hist = getHistory(t, srv, "?type=development")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, hist.Sessions, 1)
	require.Equal(t, "legacy-dev", hist.Sessions[0].ID)

	code, hist = getHistory(t, srv, "?type=pr-review")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, hist.Sessions, 1)
	require.Equal(t, "legacy-review", hist.Sessions[0].ID)
}

// TestTypeOnlyRecordRoundTripThroughRealStore proves Insert(Type, Role="") →
// Get returns the backfilled Role without a bulk rewrite (lazy migration).
func TestTypeOnlyRecordRoundTripThroughRealStore(t *testing.T) {
	s, err := agentstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	ctx := context.Background()

	require.NoError(t, s.Insert(ctx, &agentstore.Agent{
		ID: "type-only", Status: store.StatusWorking, Type: store.TypeSpike,
		// Role intentionally omitted
	}))
	got, err := s.Get(ctx, "type-only")
	require.NoError(t, err)
	require.Equal(t, "general", got.Role)
	require.Equal(t, store.TypeSpike, got.Type)

	listed, err := s.List(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, "general", listed[0].Role)
}

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/store"
)

func TestRoleFromDeprecatedType(t *testing.T) {
	require.Equal(t, "implementer", roleFromDeprecatedType("development"))
	require.Equal(t, "reviewer", roleFromDeprecatedType("pr-review"))
	require.Equal(t, "general", roleFromDeprecatedType("analysis"))
	require.Equal(t, "general", roleFromDeprecatedType("spike"))
	require.Equal(t, "", roleFromDeprecatedType(""))
	require.Equal(t, "", roleFromDeprecatedType("span-out"))
}

func TestResolveRoleCanonicalWinsOverType(t *testing.T) {
	require.Equal(t, "worker", resolveRoleCanonical("worker", "development"))
	require.Equal(t, "implementer", resolveRoleCanonical("", "development"))
	require.Equal(t, "reviewer", resolveRoleCanonical("", "pr-review"))
}

func TestEffectiveSessionRole(t *testing.T) {
	require.Equal(t, "worker", effectiveSessionRole("worker", store.TypeDevelopment))
	require.Equal(t, "implementer", effectiveSessionRole("", store.TypeDevelopment))
	require.Equal(t, "general", effectiveSessionRole("", store.TypeSpike))
}

// TestSpawnTypeAliasMapsToRole covers the Type→Role terminology migration on
// POST /api/v1/spawn: type alone populates Role; explicit role wins when both
// are provided.
func TestSpawnTypeAliasMapsToRole(t *testing.T) {
	fl := &fakeLife{}
	ts := lifeServer(t, newFakeStore(), fl)
	defer ts.Close()

	body, _ := json.Marshal(map[string]any{
		"type":   "development",
		"repo":   t.TempDir(),
		"prompt": "legacy type body",
	})
	resp, err := http.Post(ts.URL+"/api/v1/spawn", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.NotNil(t, fl.spawned)
	require.Equal(t, "implementer", fl.spawned.Role, "deprecated type must map onto role")
	require.Equal(t, "development", string(fl.spawned.Type), "type still forwarded during alias window")
}

func TestSpawnRoleCanonicalWinsOverType(t *testing.T) {
	fl := &fakeLife{}
	ts := lifeServer(t, newFakeStore(), fl)
	defer ts.Close()

	body, _ := json.Marshal(map[string]any{
		"role":   "orchestrator",
		"type":   "development",
		"repo":   t.TempDir(),
		"prompt": "role wins",
	})
	resp, err := http.Post(ts.URL+"/api/v1/spawn", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.Equal(t, "orchestrator", fl.spawned.Role, "canonical role must win over type alias")
}

func TestHandleHistoryTypeFilterMapsToRole(t *testing.T) {
	fs := newFakeStore()
	ctx := context.Background()
	now := time.Now()
	require.NoError(t, fs.Insert(ctx, &agentstore.Agent{ID: "dev", Type: store.TypeDevelopment, UpdatedAt: now}))
	require.NoError(t, fs.Archive(ctx, "dev"))
	require.NoError(t, fs.Insert(ctx, &agentstore.Agent{ID: "ana", Type: store.TypeAnalysis, UpdatedAt: now}))
	require.NoError(t, fs.Archive(ctx, "ana"))
	srv := &Server{store: fs}

	code, sr := getHistory(t, srv, "?type=development")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, sr.Sessions, 1)
	require.Equal(t, "dev", sr.Sessions[0].ID)

	code, sr = getHistory(t, srv, "?role=general")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, sr.Sessions, 1)
	require.Equal(t, "ana", sr.Sessions[0].ID)
}

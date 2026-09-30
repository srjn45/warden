package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSpawnAiCliCanonicalWins covers the Backend→AI CLI terminology migration
// on POST /api/v1/spawn: ai_cli wins when both ai_cli and backend are provided,
// and the Session response dual-emits both keys during the alias window.
func TestSpawnAiCliCanonicalWins(t *testing.T) {
	fl := &fakeLife{}
	ts := lifeServer(t, newFakeStore(), fl)
	defer ts.Close()

	body, _ := json.Marshal(map[string]any{
		"role":    "worker",
		"prompt":  "migrate me",
		"cwd":     t.TempDir(),
		"ai_cli":  "aider",
		"backend": "claude",
	})
	resp, err := http.Post(ts.URL+"/api/v1/spawn", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.NotNil(t, fl.spawned)
	require.Equal(t, "aider", fl.spawned.AiCli, "canonical ai_cli must win over backend alias")

	var sess map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&sess))
	require.Equal(t, "aider", sess["ai_cli"])
	require.Equal(t, "aider", sess["backend"], "alias window dual-emits backend")
}

// TestSpawnBackendAliasAloneStillWorks verifies the deprecated backend-only
// spawn body still populates AiCli during the alias window.
func TestSpawnBackendAliasAloneStillWorks(t *testing.T) {
	fl := &fakeLife{}
	ts := lifeServer(t, newFakeStore(), fl)
	defer ts.Close()

	body, _ := json.Marshal(map[string]any{
		"role":    "worker",
		"prompt":  "legacy body",
		"cwd":     t.TempDir(),
		"backend": "codex",
	})
	resp, err := http.Post(ts.URL+"/api/v1/spawn", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.Equal(t, "codex", fl.spawned.AiCli)
}

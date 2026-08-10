package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// listSessionsKind fetches /api/v1/sessions with an optional ?kind= filter and
// returns the ids in the response.
func listSessionsKind(t *testing.T, url string) []string {
	t.Helper()
	resp, err := http.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out struct {
		Sessions []store.Session `json:"sessions"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	ids := make([]string, 0, len(out.Sessions))
	for _, s := range out.Sessions {
		ids = append(ids, s.ID)
	}
	return ids
}

// The ?kind= filter on GET /api/v1/sessions narrows by session kind; omitted
// returns everything (agents + terminals).
func TestListSessionsKindFilter(t *testing.T) {
	fs := newFakeStore()
	require.NoError(t, fs.Insert(context.Background(), &store.Session{ID: "a1", Status: store.StatusWorking}))
	require.NoError(t, fs.Insert(context.Background(), &store.Session{ID: "t1", Kind: store.KindTerminal, Status: store.StatusWorking}))
	ts := testServer(t, fs)
	defer ts.Close()

	require.ElementsMatch(t, []string{"a1", "t1"}, listSessionsKind(t, ts.URL+"/api/v1/sessions"),
		"no filter returns agents and terminals alike")
	require.Equal(t, []string{"t1"}, listSessionsKind(t, ts.URL+"/api/v1/sessions?kind=terminal"),
		"kind=terminal returns only terminals")
	require.Equal(t, []string{"a1"}, listSessionsKind(t, ts.URL+"/api/v1/sessions?kind=agent"),
		"kind=agent returns only agents (kind empty or agent)")
}

// GET /api/v1/capabilities advertises the terminal-sessions flag for version-skew
// negotiation.
func TestGetCapabilities(t *testing.T) {
	ts := testServer(t, newFakeStore())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/v1/capabilities")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out struct {
		Capabilities []string `json:"capabilities"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.Contains(t, out.Capabilities, "terminal-sessions")
}

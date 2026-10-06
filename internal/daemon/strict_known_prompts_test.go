package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/knownprompts"
	"github.com/srjn45/warden/internal/poller"
)

func knownServer(t *testing.T) (*httptest.Server, *knownprompts.Store, string) {
	t.Helper()
	ks, err := knownprompts.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = ks.Close() })
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	srv := &Server{poller: &poller.Poller{Known: ks}, audit: audit.NewWriter(auditPath)}
	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)
	return ts, ks, auditPath
}

func learnTwo(t *testing.T, ks *knownprompts.Store) []knownprompts.Entry {
	t.Helper()
	for _, q := range []string{"Allow the thing?", "Trust this other folder?"} {
		_, _, err := ks.Learn(context.Background(), "claude", "1.0", knownprompts.Reading{
			Question: q, Options: []string{"Yes", "No"}, Affirmative: 1,
		})
		require.NoError(t, err)
	}
	return ks.List()
}

func doReq(t *testing.T, method, url string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func TestKnownPromptsListForgetOne(t *testing.T) {
	ts, ks, auditPath := knownServer(t)
	es := learnTwo(t, ks)

	resp, out := doReq(t, http.MethodGet, ts.URL+"/api/v1/known-prompts")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Len(t, out["prompts"], 2)

	resp, _ = doReq(t, http.MethodDelete, ts.URL+"/api/v1/known-prompts/"+es[0].ID)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Len(t, ks.List(), 1)

	resp, _ = doReq(t, http.MethodDelete, ts.URL+"/api/v1/known-prompts/"+es[0].ID)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	evs, err := audit.Read(auditPath, audit.Filter{})
	require.NoError(t, err)
	require.Len(t, evs, 1)
	require.Equal(t, audit.ActionKnownPromptForget, evs[0].Action)
	require.Equal(t, es[0].ID, evs[0].Target)
}

func TestKnownPromptsForgetAll(t *testing.T) {
	ts, ks, auditPath := knownServer(t)
	learnTwo(t, ks)

	resp, out := doReq(t, http.MethodDelete, ts.URL+"/api/v1/known-prompts")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.EqualValues(t, 2, out["removed"])
	require.Empty(t, ks.List())

	evs, err := audit.Read(auditPath, audit.Filter{})
	require.NoError(t, err)
	require.Len(t, evs, 1)
	require.Equal(t, audit.ActionKnownPromptForgetAll, evs[0].Action)
	require.Equal(t, "2", evs[0].Detail["removed"])
}

func TestKnownPromptsAbsentStore(t *testing.T) {
	srv := &Server{}
	ts := httptest.NewServer(srv.router())
	defer ts.Close()
	resp, out := doReq(t, http.MethodGet, ts.URL+"/api/v1/known-prompts")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Empty(t, out["prompts"])
	resp, _ = doReq(t, http.MethodDelete, ts.URL+"/api/v1/known-prompts/x")
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

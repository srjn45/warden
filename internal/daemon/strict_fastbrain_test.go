package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/fastbrain"
)

func TestFastBrainMetricsDecisionsAndControls(t *testing.T) {
	rf := fastbrain.RunnerFunc(func(context.Context, string) (string, error) { return `{"ok":true}`, nil })
	eng := fastbrain.NewEngine(rf, rf)
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	srv := &Server{fastBrain: eng, audit: audit.NewWriter(auditPath)}
	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)

	_, err := eng.Decide(context.Background(), fastbrain.Request{Kind: fastbrain.KindCommitMessage, Tier: fastbrain.TierFast, Prompt: "TOPSECRET diff"})
	require.NoError(t, err)

	resp, out := doReq(t, http.MethodGet, ts.URL+"/api/v1/fastbrain/metrics")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.EqualValues(t, 1, out["total_decisions"])
	require.Len(t, out["controls"], len(fastbrain.AllKinds()))

	resp, out = doReq(t, http.MethodGet, ts.URL+"/api/v1/fastbrain/decisions?limit=5")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	b, _ := json.Marshal(out)
	require.NotContains(t, string(b), "TOPSECRET")
	require.Len(t, out["decisions"], 1)

	put := func(kind, body string) (*http.Response, map[string]any) {
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/fastbrain/controls/"+kind, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		r, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer r.Body.Close()
		var o map[string]any
		_ = json.NewDecoder(r.Body).Decode(&o)
		return r, o
	}
	r, _ := put("nonsense", `{"paused":true}`)
	require.Equal(t, http.StatusNotFound, r.StatusCode)
	r, _ = put("commit_message", `{"paused":true,"ttl_seconds":60}`)
	require.Equal(t, http.StatusOK, r.StatusCode)
	res, _ := eng.Decide(context.Background(), fastbrain.Request{Kind: fastbrain.KindCommitMessage, Tier: fastbrain.TierFast, Prompt: "x"})
	require.Equal(t, fastbrain.StatusDeferred, res.Status)
	r, _ = put("commit_message", `{"paused":false}`)
	require.Equal(t, http.StatusOK, r.StatusCode)

	evs, err := audit.Read(auditPath, audit.Filter{Action: audit.ActionFastBrainControl})
	require.NoError(t, err)
	require.Len(t, evs, 2)
	require.Equal(t, "commit_message", evs[0].Target)
	require.Equal(t, fastbrain.AuditControl, audit.ActionFastBrainControl)
	require.Equal(t, fastbrain.AuditCircuit, audit.ActionFastBrainCircuit)
	require.Equal(t, fastbrain.AuditAbandoned, audit.ActionFastBrainAbandoned)
}

func TestFastBrainEndpointsWithoutEngine(t *testing.T) {
	ts := httptest.NewServer((&Server{}).router())
	t.Cleanup(ts.Close)
	resp, _ := doReq(t, http.MethodGet, ts.URL+"/api/v1/fastbrain/metrics")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp, _ = doReq(t, http.MethodGet, ts.URL+"/api/v1/fastbrain/decisions")
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

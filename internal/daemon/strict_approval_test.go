package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/poller"
	"github.com/stretchr/testify/require"
)

func policyServer(t *testing.T) (*httptest.Server, *poller.Poller, *approval.Policy) {
	t.Helper()
	pl := poller.New(nil, time.Minute)
	var persisted approval.Policy
	srv := &Server{poller: pl, autoApprovePersist: func(p approval.Policy) error {
		persisted = p
		return nil
	}}
	return httptest.NewServer(srv.router()), pl, &persisted
}

func TestGetAutoApprovePolicy(t *testing.T) {
	ts, pl, _ := policyServer(t)
	defer ts.Close()
	pl.SetAutoApprovePolicy(approval.Policy{Enabled: true, Rules: approval.Rules{Allow: []approval.Rule{{Tool: "Read"}}}})

	resp, err := http.Get(ts.URL + "/api/v1/auto-approve/policy")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var got approval.Policy
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.True(t, got.Enabled)
	require.Len(t, got.Rules.Allow, 1)
	require.Equal(t, "Read", got.Rules.Allow[0].Tool)
}

func TestPutAutoApprovePolicy(t *testing.T) {
	ts, pl, persisted := policyServer(t)
	defer ts.Close()

	want := approval.Policy{
		Enabled: true,
		Rules:   approval.Rules{Allow: []approval.Rule{{Regex: `^Bash\(git status\)$`}}},
		Agents:  map[string]approval.Policy{"reviewer": {Enabled: true, Rules: approval.Rules{Allow: []approval.Rule{{Tool: "Grep"}}}}},
	}
	body, _ := json.Marshal(want)
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/auto-approve/policy", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Live policy updated.
	live := pl.AutoApprovePolicySnapshot()
	require.True(t, live.Enabled)
	require.Equal(t, `^Bash\(git status\)$`, live.Rules.Allow[0].Regex)
	require.Contains(t, live.Agents, "reviewer")
	// Persisted via the hook.
	require.True(t, persisted.Enabled)
	require.Contains(t, persisted.Agents, "reviewer")
}

func TestPutAutoApprovePolicyBadRegex(t *testing.T) {
	ts, _, _ := policyServer(t)
	defer ts.Close()
	body, _ := json.Marshal(approval.Policy{Rules: approval.Rules{Allow: []approval.Rule{{Regex: "(unterminated"}}}})
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/auto-approve/policy", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

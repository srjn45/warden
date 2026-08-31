package daemon

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// errBoom is a plain (non-degradation) read error used to exercise the
// whole-scan "read" failure fallback.
var errBoom = errors.New("engine read failed")

// degradedErr builds a representative typed degradation error for the fake store.
func degradedErr() error {
	return &store.DegradedScanError{Failures: []store.ScanFailure{{
		Collection: "active", Key: "corrupt-1", Class: store.DegradeDecode, Detail: "boom",
	}}}
}

// TestListSessionsDegradedReturns503 verifies the complete-or-error contract at
// the REST boundary: a degraded active scan is a 503, never a 200 with a partial
// (or empty) fleet the TUI would treat as authoritative.
func TestListSessionsDegradedReturns503(t *testing.T) {
	fs := newFakeStore()
	fs.data["A-1"] = &store.Session{ID: "A-1", Status: store.StatusWorking}
	fs.listErr = degradedErr()
	ts := testServer(t, fs)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/sessions")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

// TestListApprovalsDegradedReturns503 verifies the approval queue fails closed on
// a degraded scan too (rather than building a queue from a partial fleet).
func TestListApprovalsDegradedReturns503(t *testing.T) {
	fs := newFakeStore()
	fs.listErr = degradedErr()
	srv := &Server{store: fs, approvals: true}
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/approvals")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

// storeHealth fetches GET /api/v1/store/health, asserts it is always 200, and
// returns the decoded verdict.
func storeHealth(t *testing.T, url string) struct {
	Healthy      bool `json:"healthy"`
	Degraded     bool `json:"degraded"`
	FailureCount int  `json:"failure_count"`
	Failures     []struct {
		Collection string `json:"collection"`
		Key        string `json:"key"`
		Class      string `json:"class"`
		Detail     string `json:"detail"`
	} `json:"failures"`
} {
	t.Helper()
	resp, err := http.Get(url + "/api/v1/store/health")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "store-health is always 200 so a monitor can tell degraded from unreachable")
	var out struct {
		Healthy      bool `json:"healthy"`
		Degraded     bool `json:"degraded"`
		FailureCount int  `json:"failure_count"`
		Failures     []struct {
			Collection string `json:"collection"`
			Key        string `json:"key"`
			Class      string `json:"class"`
			Detail     string `json:"detail"`
		} `json:"failures"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out
}

// TestStoreHealthHealthy verifies a clean store reports healthy with no failures.
func TestStoreHealthHealthy(t *testing.T) {
	fs := newFakeStore()
	fs.data["A-1"] = &store.Session{ID: "A-1", Status: store.StatusWorking}
	ts := testServer(t, fs)
	defer ts.Close()

	h := storeHealth(t, ts.URL)
	require.True(t, h.Healthy)
	require.False(t, h.Degraded)
	require.Equal(t, 0, h.FailureCount)
	require.Empty(t, h.Failures)
}

// TestStoreHealthDegraded verifies a degraded store reports healthy=false with the
// typed per-record diagnostics mapped onto the wire type.
func TestStoreHealthDegraded(t *testing.T) {
	fs := newFakeStore()
	fs.listErr = degradedErr()
	ts := testServer(t, fs)
	defer ts.Close()

	h := storeHealth(t, ts.URL)
	require.False(t, h.Healthy)
	require.True(t, h.Degraded)
	require.Equal(t, 1, h.FailureCount)
	require.Len(t, h.Failures, 1)
	require.Equal(t, "active", h.Failures[0].Collection)
	require.Equal(t, "corrupt-1", h.Failures[0].Key)
	require.Equal(t, "decode", h.Failures[0].Class)
}

// TestStoreHealthNonDegradedError verifies a plain (non-degradation) read error
// still yields a concrete 200 verdict — a single whole-scan "read" failure —
// rather than bubbling to a 500.
func TestStoreHealthNonDegradedError(t *testing.T) {
	fs := newFakeStore()
	fs.listErr = errBoom
	ts := testServer(t, fs)
	defer ts.Close()

	h := storeHealth(t, ts.URL)
	require.False(t, h.Healthy)
	require.Equal(t, 1, h.FailureCount)
	require.Equal(t, "read", h.Failures[0].Class)
}

// TestStoreHealthCapabilityAdvertised verifies feature detection: the store-health
// capability is advertised so a client knows the endpoint exists.
func TestStoreHealthCapabilityAdvertised(t *testing.T) {
	ts := testServer(t, newFakeStore())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/v1/capabilities")
	require.NoError(t, err)
	defer resp.Body.Close()
	var out struct {
		Capabilities []string `json:"capabilities"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.Contains(t, out.Capabilities, "store-health")
}

func TestHistorySurfacesArchiveDegradation(t *testing.T) {
	fs := newFakeStore()
	fs.closed["closed-1"] = &store.Session{ID: "closed-1"}
	fs.closedSkipped = 2
	ts := testServer(t, fs)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/history")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var body struct {
		Sessions       []store.Session `json:"sessions"`
		Degraded       bool            `json:"degraded"`
		SkippedRecords int             `json:"skipped_records"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.True(t, body.Degraded)
	require.Equal(t, 2, body.SkippedRecords)
	require.Len(t, body.Sessions, 1)
}

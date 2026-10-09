package daemon

// Cockpit-level baseline for the agent-store lock-contention incident; see
// docs/specs/2026-10-09-agent-store-lock-contention-contract.md §2 (request
// paths) and §3 (defect). Uses the REAL agentstore.Store plus its slow-write
// seam: a single slow store write must currently stall the 1 s Cockpit poll
// routes. A later task flips these assertions (the poll must answer from a
// snapshot) — the failing baseline is the signal.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/store"
)

func TestBaselineCockpitPollStallsBehindSlowStoreWrite(t *testing.T) {
	st, err := agentstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(t, st.Insert(context.Background(), &agentstore.Agent{ID: "a1", Name: "one", Status: store.StatusWorking}))

	entered, release := make(chan struct{}), make(chan struct{})
	var once = make(chan struct{}, 1)
	once <- struct{}{}
	restore := agentstore.SetWriteSeam(func(op string) {
		select {
		case <-once:
			close(entered)
			<-release
		default:
		}
	})
	defer restore()
	releaseOnce := func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}
	defer releaseOnce()

	srv := &Server{store: st, approvals: true}
	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)

	// The poller/hook path: any status write (here the same call the poller makes).
	go func() { _ = st.UpdateStatus(context.Background(), "a1", store.StatusIdle) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("write seam never reached")
	}

	// Cockpit 1 s poll targets that read the active store (contract §2) answer from the snapshot without stalling.
	for _, path := range []string{"/api/v1/sessions?all=true", "/api/v1/tree", "/api/v1/store/health", "/api/v1/approvals", "/api/v1/sessions/a1", "/healthz"} {
		ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		cancel()
		require.NoError(t, err, "%s must answer from snapshot without stalling while store write is parked", path)
		require.Equal(t, http.StatusOK, resp.StatusCode, "%s status code", path)
		if resp != nil {
			resp.Body.Close()
		}
	}

	// Releasing the write completes cleanly.
	releaseOnce()
	resp, err := http.Get(ts.URL + "/api/v1/sessions?all=true")
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

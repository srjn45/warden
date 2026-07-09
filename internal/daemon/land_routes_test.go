package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/ctxstore"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// stubLandHost is a scriptable autopilot.LandHost the land route test injects via
// Server.landHostFn, so the handler's resolution / ledger / mapping are exercised
// without a live GitHub.
type stubLandHost struct {
	pr       autopilot.PRInfo
	prFound  bool
	ci       autopilot.GateState
	mergeSHA string
	merges   *int // incremented per Merge call (shared across host rebuilds)
}

func (h stubLandHost) FindPR(context.Context, string) (autopilot.PRInfo, bool, error) {
	return h.pr, h.prFound, nil
}
func (h stubLandHost) GateCI(context.Context, string, string, string) (autopilot.GateState, string, error) {
	return h.ci, "", nil
}
func (h stubLandHost) GateLocal(context.Context, string) (autopilot.GateState, string, error) {
	return autopilot.GateGreen, "", nil
}
func (h stubLandHost) Merge(context.Context, int, string, bool) (string, error) {
	*h.merges++
	return h.mergeSHA, nil
}

// newLandServer wires a Server with autopilot enabled on a one-plan repo, a
// ctx-store ledger, and the injected land host. It returns the server, the run
// id, and the merge counter.
func newLandServer(t *testing.T, host *stubLandHost) (*httptest.Server, *Server, string) {
	t.Helper()
	dir := t.TempDir()
	plan := filepath.Join(dir, "plan.yaml")
	require.NoError(t, os.WriteFile(plan, []byte("version: 1\ngoal: ship\n"), 0o644))

	cs, err := ctxstore.New(t.TempDir())
	require.NoError(t, err)
	srv := &Server{store: newFakeStore(), life: &fakeLife{}, cstore: cs, hub: newHub(), done: make(chan struct{})}
	srv.landHostFn = func(string) autopilot.LandHost { return *host }
	srv.SetAutopilotController(autopilot.NewController(autopilot.ControllerConfig{
		Plans:             []string{plan},
		IntegrationBranch: "autopilot/integration",
		Gate:              "ci",
		Strategy:          "squash",
		DeleteBranch:      true,
		Backends:          autopilot.BackendLadder{Free: []string{"claude"}},
	}, &apFakeEnv{repo: dir}))

	var st autopilot.Status
	code := apPostJSON(t, httptest.NewServer(srv.router()).URL+"/api/v1/autopilot", `{"enabled":true}`, &st)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, st.Runs, 1)
	runID := st.Runs[0].RunID

	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)
	return ts, srv, runID
}

// addWorker inserts an autopilot-owned worker session on branch for runID.
func addWorker(t *testing.T, srv *Server, runID, id, branch string) {
	t.Helper()
	require.NoError(t, srv.store.Insert(context.Background(), &store.Session{
		ID:       id,
		Branch:   branch,
		Worktree: t.TempDir(),
		Status:   store.StatusWorking,
		Tags:     []string{"autopilot", "run:" + runID},
	}))
}

func postLand(t *testing.T, url, ref string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(url+"/api/v1/autopilot/land", "application/json",
		strings.NewReader(`{"agent_or_branch":"`+ref+`"}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	var body map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	return resp.StatusCode, body
}

func TestLandRouteSuccessWritesLedger(t *testing.T) {
	merges := 0
	host := &stubLandHost{
		pr:       autopilot.PRInfo{Number: 7, BaseRef: "autopilot/integration", HeadSHA: "sha-head", Mergeable: true},
		prFound:  true,
		ci:       autopilot.GateGreen,
		mergeSHA: "sha-merge",
		merges:   &merges,
	}
	ts, srv, runID := newLandServer(t, host)
	addWorker(t, srv, runID, "W-1", "autopilot/api")

	code, body := postLand(t, ts.URL, "W-1")
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "sha-merge", body["sha"])
	require.Equal(t, false, body["already_landed"])
	require.Equal(t, 1, merges)

	// The daemon wrote the landing authoritatively into the run ledger, keyed on
	// the PR HEAD SHA (the idempotency key), not the merge commit.
	ledger := autopilot.NewLedger(ctxLedgerStore{cs: srv.cstore}, runID)
	landings, err := ledger.Landings()
	require.NoError(t, err)
	require.Len(t, landings, 1)
	require.Equal(t, "sha-head", landings[0].SHA)
	require.Equal(t, "autopilot/api", landings[0].Branch)

	// Re-issuing the same head is idempotent: the recorded head SHA is a no-op
	// (no second merge, no duplicate landing).
	merges = 0
	code, body = postLand(t, ts.URL, "W-1")
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, true, body["already_landed"])
	require.Equal(t, 0, merges, "an already-recorded head must not merge again")

	landings, err = ledger.Landings()
	require.NoError(t, err)
	require.Len(t, landings, 1, "no duplicate landing on an idempotent re-issue")
}

func TestLandRouteAlreadyMergedPR(t *testing.T) {
	merges := 0
	host := &stubLandHost{
		pr:      autopilot.PRInfo{Number: 9, BaseRef: "autopilot/integration", HeadSHA: "h", Merged: true, MergeCommit: "m"},
		prFound: true,
		merges:  &merges,
	}
	ts, srv, runID := newLandServer(t, host)
	addWorker(t, srv, runID, "W-1", "autopilot/api")

	code, body := postLand(t, ts.URL, "W-1")
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, true, body["already_landed"])
	require.Equal(t, "m", body["sha"])
	require.Equal(t, 0, merges)
}

func TestLandRouteNotOwned(t *testing.T) {
	merges := 0
	host := &stubLandHost{merges: &merges}
	ts, _, _ := newLandServer(t, host)

	// A branch with no owning autopilot session → not_owned (never reaches gh).
	code, body := postLand(t, ts.URL, "some/foreign-branch")
	require.Equal(t, http.StatusConflict, code)
	require.Equal(t, "not_owned", body["kind"])
	require.Equal(t, 0, merges)
}

func TestLandRouteGateRed(t *testing.T) {
	merges := 0
	host := &stubLandHost{
		pr:      autopilot.PRInfo{Number: 7, BaseRef: "autopilot/integration", HeadSHA: "h", Mergeable: true},
		prFound: true,
		ci:      autopilot.GateRed,
		merges:  &merges,
	}
	ts, srv, runID := newLandServer(t, host)
	addWorker(t, srv, runID, "W-1", "autopilot/api")

	code, body := postLand(t, ts.URL, "W-1")
	require.Equal(t, http.StatusConflict, code)
	require.Equal(t, "gate_red", body["kind"])
	require.Equal(t, 0, merges, "a red gate must not merge")
}

func TestLandRouteWrongBaseRejectsMainPR(t *testing.T) {
	merges := 0
	host := &stubLandHost{
		pr:      autopilot.PRInfo{Number: 7, BaseRef: "main", HeadSHA: "h", Mergeable: true},
		prFound: true,
		ci:      autopilot.GateGreen,
		merges:  &merges,
	}
	ts, srv, runID := newLandServer(t, host)
	addWorker(t, srv, runID, "W-1", "autopilot/api")

	code, body := postLand(t, ts.URL, "W-1")
	require.Equal(t, http.StatusConflict, code)
	require.Equal(t, "wrong_base", body["kind"])
	require.Equal(t, 0, merges)
}

func TestLandRouteUnconfigured(t *testing.T) {
	srv := &Server{store: newFakeStore(), hub: newHub(), done: make(chan struct{})}
	ts := httptest.NewServer(srv.router())
	defer ts.Close()
	code, _ := postLand(t, ts.URL, "anything")
	require.Equal(t, http.StatusForbidden, code)
}

func TestLandRouteMissingRef(t *testing.T) {
	host := &stubLandHost{merges: new(int)}
	ts, _, _ := newLandServer(t, host)
	resp, err := http.Post(ts.URL+"/api/v1/autopilot/land", "application/json", strings.NewReader(`{}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// capture records what the test server saw for one request so tests can assert
// the exact wire contract (method + path + query + body) the client emitted.
type capture struct {
	method string
	path   string
	rawQ   string
	body   map[string]any
}

// jsonServer spins up an httptest.Server that records the request into c and
// writes respJSON back. Shrink the long/default timeouts is unnecessary here:
// every handler responds immediately.
func jsonServer(t *testing.T, c *capture, status int, respJSON string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.method = r.Method
		c.path = r.URL.Path
		c.rawQ = r.URL.RawQuery
		if r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			if len(b) > 0 {
				_ = json.Unmarshal(b, &c.body)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if status != 0 {
			w.WriteHeader(status)
		}
		_, _ = io.WriteString(w, respJSON)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestGet(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, `{"id":"A-1","status":"working"}`)
	s, err := New(ts.URL).Get(context.Background(), "A-1")
	require.NoError(t, err)
	require.Equal(t, http.MethodGet, c.method)
	require.Equal(t, "/api/v1/sessions/A-1", c.path)
	require.Equal(t, "A-1", s.ID)
}

func TestGetSurfacesStatusError(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, http.StatusNotFound, `{"error":"no such session"}`)
	_, err := New(ts.URL).Get(context.Background(), "ghost")
	require.Error(t, err)
	var se *StatusError
	require.ErrorAs(t, err, &se)
	require.Equal(t, http.StatusNotFound, se.Code)
	require.Equal(t, "no such session", se.Msg)
}

func TestStatusErrorFallsBackToStatusWhenNoBody(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, http.StatusInternalServerError, ``)
	_, err := New(ts.URL).Get(context.Background(), "x")
	var se *StatusError
	require.ErrorAs(t, err, &se)
	require.Equal(t, http.StatusInternalServerError, se.Code)
	require.Contains(t, se.Error(), "500")
}

func TestSearch(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, `{"sessions":[{"id":"A-1"},{"id":"B-2"}]}`)
	got, err := New(ts.URL).Search(context.Background(), SearchParams{Query: "foo bar", Closed: true})
	require.NoError(t, err)
	require.Equal(t, "/api/v1/search", c.path)
	require.Equal(t, "foo bar", queryParam(c.rawQ, "q"))
	require.Equal(t, "true", queryParam(c.rawQ, "closed"))
	require.Len(t, got, 2)
}

func TestSearchOmitsClosedWhenFalse(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, `{"sessions":[]}`)
	_, err := New(ts.URL).Search(context.Background(), SearchParams{Query: "x"})
	require.NoError(t, err)
	require.Equal(t, "", queryParam(c.rawQ, "closed"), "closed=false must not be sent")
}

func TestHistoryForwardsFilters(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, `{"sessions":[{"id":"A-1"}]}`)
	since := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	got, err := New(ts.URL).History(context.Background(), HistoryParams{Since: since, Type: "development", Limit: 10})
	require.NoError(t, err)
	require.Equal(t, "/api/v1/history", c.path)
	require.Equal(t, "2026-01-02T03:04:05Z", queryParam(c.rawQ, "since"))
	require.Equal(t, "development", queryParam(c.rawQ, "type"))
	require.Equal(t, "10", queryParam(c.rawQ, "limit"))
	require.Len(t, got, 1)
}

func TestHistoryOmitsZeroFilters(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, `{"sessions":[]}`)
	_, err := New(ts.URL).History(context.Background(), HistoryParams{})
	require.NoError(t, err)
	require.Equal(t, "", c.rawQ, "no filters means no query string")
}

func TestImportSendsEnvelopeAndMergeFlag(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, `{"imported":["A-1"],"merged":["B-2"]}`)
	env := &store.Export{Version: 1, Sessions: []*store.Session{{ID: "A-1"}}}
	res, err := New(ts.URL).Import(context.Background(), env, true)
	require.NoError(t, err)
	require.Equal(t, http.MethodPost, c.method)
	require.Equal(t, "/api/v1/import", c.path)
	require.Equal(t, "merge=true", c.rawQ)
	require.Equal(t, []string{"A-1"}, res.Imported)
	require.Equal(t, []string{"B-2"}, res.Merged)
}

func TestGuardSendsBodyAndParsesVerdict(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, `{"decision":"deny","reason":"outside worktree"}`)
	v, err := New(ts.URL).Guard(context.Background(), "A-1", "Edit", "/etc/passwd")
	require.NoError(t, err)
	require.Equal(t, http.MethodPost, c.method)
	require.Equal(t, "/api/v1/hooks/guard", c.path)
	require.Equal(t, "A-1", c.body["session"])
	require.Equal(t, "Edit", c.body["tool"])
	require.Equal(t, "/etc/passwd", c.body["path"])
	require.Equal(t, "deny", v.Decision)
	require.Equal(t, "outside worktree", v.Reason)
}

func TestGitCommit(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, `{"committed":true,"sha":"abc123","branch":"feat","files":["a.go"]}`)
	res, err := New(ts.URL).GitCommit(context.Background(), "A-1", "/repo", "msg")
	require.NoError(t, err)
	require.Equal(t, http.MethodPost, c.method)
	require.Equal(t, "/api/v1/git/commit", c.path)
	require.Equal(t, "A-1", c.body["session"])
	require.Equal(t, "/repo", c.body["dir"])
	require.Equal(t, "msg", c.body["message"])
	require.True(t, res.Committed)
	require.Equal(t, "abc123", res.SHA)
	require.Equal(t, "feat", res.Branch)
}

func TestGitPush(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, `{"branch":"feat","remote":"origin","pushed":true}`)
	res, err := New(ts.URL).GitPush(context.Background(), "A-1", "/repo")
	require.NoError(t, err)
	require.Equal(t, "/api/v1/git/push", c.path)
	require.Equal(t, "A-1", c.body["session"])
	require.True(t, res.Pushed)
	require.Equal(t, "origin", res.Remote)
}

func TestGitSync(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, `{"branch":"feat","base":"main","updated":false,"conflicts":["a.go","b.go"]}`)
	res, err := New(ts.URL).GitSync(context.Background(), "A-1", "/repo", "main")
	require.NoError(t, err)
	require.Equal(t, "/api/v1/git/sync", c.path)
	require.Equal(t, "main", c.body["base"])
	require.False(t, res.Updated)
	require.Equal(t, []string{"a.go", "b.go"}, res.Conflicts)
}

func TestCheck(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, `{"passed":false,"checks":[{"name":"test","passed":false}]}`)
	res, err := New(ts.URL).Check(context.Background(), "A-1", "/repo", "test")
	require.NoError(t, err)
	require.Equal(t, "/api/v1/check", c.path)
	require.Equal(t, "test", c.body["name"])
	require.False(t, res.Passed)
	require.Len(t, res.Checks, 1)
}

func TestTerminate(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, ``)
	require.NoError(t, New(ts.URL).Terminate(context.Background(), "A-1"))
	require.Equal(t, http.MethodPost, c.method)
	require.Equal(t, "/api/v1/sessions/A-1/terminate", c.path)
}

func TestDeleteSendsHardFlag(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, ``)
	require.NoError(t, New(ts.URL).Delete(context.Background(), "A-1", true))
	require.Equal(t, http.MethodPost, c.method)
	require.Equal(t, "/api/v1/sessions/A-1/delete", c.path)
	require.Equal(t, true, c.body["hard"])
}

func TestInput(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, ``)
	require.NoError(t, New(ts.URL).Input(context.Background(), "A-1", "hello"))
	require.Equal(t, "/api/v1/sessions/A-1/input", c.path)
	require.Equal(t, "hello", c.body["text"])
}

func TestRestore(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, ``)
	require.NoError(t, New(ts.URL).Restore(context.Background(), "A-1"))
	require.Equal(t, http.MethodPost, c.method)
	require.Equal(t, "/api/v1/sessions/A-1/restore", c.path)
}

func TestListWorktrees(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, `{"worktrees":[{"path":".worktrees/A-1","branch":"feat","owner":"A-1","state":"clean"}]}`)
	got, err := New(ts.URL).ListWorktrees(context.Background(), "/repo")
	require.NoError(t, err)
	require.Equal(t, "/api/v1/worktrees", c.path)
	require.Equal(t, "/repo", queryParam(c.rawQ, "repo"))
	require.Len(t, got, 1)
	require.Equal(t, ".worktrees/A-1", got[0].Path)
	require.Equal(t, "clean", got[0].State)
}

func TestPrune(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, `{"results":[{"path":".worktrees/A-1","action":"removed","branch_deleted":true}]}`)
	got, err := New(ts.URL).Prune(context.Background(), PruneParams{Repo: "/repo", DryRun: true, Force: true, IncludeArchived: true})
	require.NoError(t, err)
	require.Equal(t, http.MethodPost, c.method)
	require.Equal(t, "/api/v1/prune", c.path)
	require.Equal(t, "/repo", c.body["repo"])
	require.Equal(t, true, c.body["dry_run"])
	require.Equal(t, true, c.body["force"])
	require.Equal(t, true, c.body["include_archived"])
	require.Len(t, got, 1)
	require.True(t, got[0].BranchDeleted)
}

func TestSnapshotCreate(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, `{"id":"snap-1","session":"A-1","message":"good point"}`)
	snap, err := New(ts.URL).SnapshotCreate(context.Background(), "A-1", "/repo", "good point")
	require.NoError(t, err)
	require.Equal(t, http.MethodPost, c.method)
	require.Equal(t, "/api/v1/snapshots", c.path)
	require.Equal(t, "good point", c.body["message"])
	require.Equal(t, "snap-1", snap.ID)
}

func TestSnapshotListFiltersBySession(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, `{"snapshots":[{"id":"snap-1"}]}`)
	got, err := New(ts.URL).SnapshotList(context.Background(), "A-1")
	require.NoError(t, err)
	require.Equal(t, "/api/v1/snapshots", c.path)
	require.Equal(t, "A-1", queryParam(c.rawQ, "session"))
	require.Len(t, got, 1)
}

func TestSnapshotListNoFilter(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, `{"snapshots":[]}`)
	_, err := New(ts.URL).SnapshotList(context.Background(), "")
	require.NoError(t, err)
	require.Equal(t, "", c.rawQ)
}

func TestSnapshotRestore(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, `{"restored":true}`)
	_, err := New(ts.URL).SnapshotRestore(context.Background(), "snap-1", true)
	require.NoError(t, err)
	require.Equal(t, http.MethodPost, c.method)
	require.Equal(t, "/api/v1/snapshots/snap-1/restore", c.path)
	require.Equal(t, true, c.body["force"])
}

func TestCreatePR(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, `{"branch":"feat","base":"main","url":"http://gh/pr/1","created":true}`)
	res, err := New(ts.URL).CreatePR(context.Background(), "A-1", "main")
	require.NoError(t, err)
	require.Equal(t, http.MethodPost, c.method)
	require.Equal(t, "/api/v1/sessions/A-1/create-pr", c.path)
	require.Equal(t, "main", c.body["base"])
	require.True(t, res.Created)
	require.Equal(t, "http://gh/pr/1", res.URL)
}

func TestOutput(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, `{"output":"line1\nline2"}`)
	out, err := New(ts.URL).Output(context.Background(), "A-1", 50)
	require.NoError(t, err)
	require.Equal(t, http.MethodGet, c.method)
	require.Equal(t, "/api/v1/sessions/A-1/output", c.path)
	require.Equal(t, "lines=50", c.rawQ)
	require.Equal(t, "line1\nline2", out)
}

func TestCtxCAS(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, `{"key":"global.k","value":"new","updated_by":"A"}`)
	e, err := New(ts.URL).CtxCAS(context.Background(), "global.k", "old", "new", "A")
	require.NoError(t, err)
	require.Equal(t, http.MethodPost, c.method)
	require.Equal(t, "/api/v1/context/global.k/cas", c.path)
	require.Equal(t, "old", c.body["expected"])
	require.Equal(t, "new", c.body["value"])
	require.Equal(t, "A", c.body["by"])
	require.Equal(t, "new", e.Value)
}

func TestCtxCASConflictMapsToSentinel(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, http.StatusConflict, `{"error":"value changed"}`)
	_, err := New(ts.URL).CtxCAS(context.Background(), "global.k", "old", "new", "A")
	require.ErrorIs(t, err, ErrCASConflict)
}

func TestCtxAppend(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, `{"key":"log","value":"a,b","updated_by":"A"}`)
	e, err := New(ts.URL).CtxAppend(context.Background(), "log", "b", ",", "A")
	require.NoError(t, err)
	require.Equal(t, http.MethodPost, c.method)
	require.Equal(t, "/api/v1/context/log/append", c.path)
	require.Equal(t, "b", c.body["value"])
	require.Equal(t, ",", c.body["sep"])
	require.Equal(t, "a,b", e.Value)
}

func TestCtxGet(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, `{"key":"k","value":"v","updated_by":"A"}`)
	e, err := New(ts.URL).CtxGet(context.Background(), "k")
	require.NoError(t, err)
	require.Equal(t, http.MethodGet, c.method)
	require.Equal(t, "/api/v1/context/k", c.path)
	require.Equal(t, "v", e.Value)
}

func TestCtxGetRejectsInvalidKeyBeforeCall(t *testing.T) {
	called := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer ts.Close()
	_, err := New(ts.URL).CtxGet(context.Background(), "bad/key")
	require.Error(t, err)
	_, err = New(ts.URL).CtxAppend(context.Background(), "bad/key", "v", ",", "A")
	require.Error(t, err)
	_, err = New(ts.URL).CtxCAS(context.Background(), "bad/key", "", "v", "A")
	require.Error(t, err)
	require.False(t, called, "invalid keys must be rejected before any daemon call")
}

func TestCollabConflicts(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, `{"conflicts":[{"file":"a.go","agents":[{"id":"A-1"},{"id":"B-2"}]}]}`)
	got, err := New(ts.URL).CollabConflicts(context.Background())
	require.NoError(t, err)
	require.Equal(t, "/api/v1/collab/conflicts", c.path)
	require.Len(t, got, 1)
	require.Equal(t, "a.go", got[0].File)
	require.Len(t, got[0].Agents, 2)
}

func TestBranchStatuses(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, `{"branches":[{"agent_id":"A-1","branch":"feat","ci":{"state":"success"},"ahead":2,"behind":1,"merged":false}]}`)
	got, err := New(ts.URL).BranchStatuses(context.Background())
	require.NoError(t, err)
	require.Equal(t, "/api/v1/collab/branches", c.path)
	require.Len(t, got, 1)
	require.Equal(t, "A-1", got[0].AgentID)
	require.Equal(t, "success", got[0].CI.State)
	require.Equal(t, 2, got[0].Ahead)
}

func TestPipelineList(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, `{"pipelines":[{"id":"p1","name":"demo"}]}`)
	got, err := New(ts.URL).PipelineList(context.Background())
	require.NoError(t, err)
	require.Equal(t, http.MethodGet, c.method)
	require.Equal(t, "/api/v1/pipelines", c.path)
	require.Len(t, got, 1)
	require.Equal(t, "p1", got[0].ID)
}

func TestPipelineGet(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, `{"id":"p1","name":"demo"}`)
	got, err := New(ts.URL).PipelineGet(context.Background(), "p1")
	require.NoError(t, err)
	require.Equal(t, "/api/v1/pipelines/p1", c.path)
	require.Equal(t, "p1", got.ID)
}

func TestPipelineLifecycleActions(t *testing.T) {
	cases := []struct {
		name       string
		call       func(c *Client) error
		wantMethod string
		wantPath   string
	}{
		{"start", func(c *Client) error { return c.PipelineStart(context.Background(), "p1") }, http.MethodPost, "/api/v1/pipelines/p1/start"},
		{"pause", func(c *Client) error { return c.PipelinePause(context.Background(), "p1") }, http.MethodPost, "/api/v1/pipelines/p1/pause"},
		{"resume", func(c *Client) error { return c.PipelineResume(context.Background(), "p1") }, http.MethodPost, "/api/v1/pipelines/p1/resume"},
		{"cancel", func(c *Client) error { return c.PipelineCancel(context.Background(), "p1") }, http.MethodPost, "/api/v1/pipelines/p1/cancel"},
		{"delete", func(c *Client) error { return c.PipelineDelete(context.Background(), "p1") }, http.MethodDelete, "/api/v1/pipelines/p1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var c capture
			ts := jsonServer(t, &c, 0, ``)
			require.NoError(t, tc.call(New(ts.URL)))
			require.Equal(t, tc.wantMethod, c.method)
			require.Equal(t, tc.wantPath, c.path)
		})
	}
}

func TestScheduleCreate(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, `{"id":"nightly","name":"nightly","kind":"cron","mode":"agent","enabled":true}`)
	sc, err := New(ts.URL).ScheduleCreate(context.Background(), ScheduleCreateRequest{
		Name: "nightly", Cron: "0 0 * * *", Type: "development", Repo: "/repo", Prompt: "go",
	})
	require.NoError(t, err)
	require.Equal(t, http.MethodPost, c.method)
	require.Equal(t, "/api/v1/schedules", c.path)
	require.Equal(t, "nightly", c.body["name"])
	require.Equal(t, "0 0 * * *", c.body["cron"])
	require.Equal(t, "nightly", sc.ID)
}

func TestScheduleList(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, `{"schedules":[{"id":"nightly","name":"nightly"}]}`)
	got, err := New(ts.URL).ScheduleList(context.Background())
	require.NoError(t, err)
	require.Equal(t, "/api/v1/schedules", c.path)
	require.Len(t, got, 1)
	require.Equal(t, "nightly", got[0].ID)
}

func TestScheduleDelete(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, ``)
	require.NoError(t, New(ts.URL).ScheduleDelete(context.Background(), "nightly"))
	require.Equal(t, http.MethodDelete, c.method)
	require.Equal(t, "/api/v1/schedules/nightly", c.path)
}

func TestGetAgentHistory(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, `{"summaries":[{"id":"A-1","status":"working","samples":3}]}`)
	since := "2026-01-01T00:00:00Z"
	got, err := New(ts.URL).GetAgentHistory(context.Background(), since, "A-1")
	require.NoError(t, err)
	require.Equal(t, "/api/v1/metrics/history", c.path)
	require.Equal(t, "true", queryParam(c.rawQ, "summary"))
	require.Equal(t, since, queryParam(c.rawQ, "since"))
	require.Equal(t, "A-1", queryParam(c.rawQ, "agent"))
	require.Len(t, got, 1)
	require.Equal(t, "A-1", got[0].ID)
	require.Equal(t, 3, got[0].Samples)
}

func TestSetAutoApprove(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, ``)
	require.NoError(t, New(ts.URL).SetAutoApprove(context.Background(), "A-1", true))
	require.Equal(t, http.MethodPatch, c.method)
	require.Equal(t, "/api/v1/sessions/A-1/auto-approve", c.path)
	require.Equal(t, true, c.body["enabled"])
}

func TestSetForceCompact(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, ``)
	require.NoError(t, New(ts.URL).SetForceCompact(context.Background(), "A-1", "on"))
	require.Equal(t, http.MethodPatch, c.method)
	require.Equal(t, "/api/v1/sessions/A-1/force-compact", c.path)
	require.Equal(t, "on", c.body["state"])
}

func TestSetName(t *testing.T) {
	var c capture
	ts := jsonServer(t, &c, 0, ``)
	require.NoError(t, New(ts.URL).SetName(context.Background(), "A-1", "scout"))
	require.Equal(t, http.MethodPatch, c.method)
	require.Equal(t, "/api/v1/sessions/A-1/name", c.path)
	require.Equal(t, "scout", c.body["name"])
}

func TestInsightsAggregatesReadEndpoints(t *testing.T) {
	// Insights composes List + History + GetAgentHistory (+ best-effort Digest).
	// Assert it hits each endpoint and folds the results into one report without
	// a Digest fetch when there are no active sessions to scan.
	var hits []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/sessions":
			_, _ = io.WriteString(w, `{"sessions":[]}`)
		case r.URL.Path == "/api/v1/history":
			_, _ = io.WriteString(w, `{"sessions":[{"id":"A-1","status":"done"}]}`)
		case r.URL.Path == "/api/v1/metrics/history":
			_, _ = io.WriteString(w, `{"summaries":[{"id":"A-1"}]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer ts.Close()

	rep, err := New(ts.URL).Insights(context.Background(), InsightsParams{})
	require.NoError(t, err)
	require.NotNil(t, rep)
	require.Contains(t, hits, "/api/v1/sessions")
	require.Contains(t, hits, "/api/v1/history")
	require.Contains(t, hits, "/api/v1/metrics/history")
}

func TestInsightsPropagatesListError(t *testing.T) {
	ts := jsonServer(t, &capture{}, http.StatusInternalServerError, `{"error":"boom"}`)
	_, err := New(ts.URL).Insights(context.Background(), InsightsParams{})
	require.Error(t, err)
}

// queryParam extracts one key from a raw query string for compact assertions.
func queryParam(raw, key string) string {
	v, err := url.ParseQuery(raw)
	if err != nil {
		return ""
	}
	return v.Get(key)
}

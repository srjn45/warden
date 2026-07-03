package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMethodsSurfaceStatusErrors asserts a representative spread of methods turn
// a daemon 5xx into a StatusError (the error path most of them share), so a
// failing daemon never looks like a silent success to the CLI/MCP layers.
func TestMethodsSurfaceStatusErrors(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"boom"}`)
	}))
	defer ts.Close()
	c := New(ts.URL)
	ctx := context.Background()

	cases := map[string]func() error{
		"Search":          func() error { _, e := c.Search(ctx, SearchParams{Query: "x"}); return e },
		"History":         func() error { _, e := c.History(ctx, HistoryParams{}); return e },
		"GitCommit":       func() error { _, e := c.GitCommit(ctx, "", "/d", "m"); return e },
		"GitPush":         func() error { _, e := c.GitPush(ctx, "", "/d", false); return e },
		"Check":           func() error { _, e := c.Check(ctx, "", "/d", ""); return e },
		"ListWorktrees":   func() error { _, e := c.ListWorktrees(ctx, "/r"); return e },
		"Prune":           func() error { _, e := c.Prune(ctx, PruneParams{Repo: "/r"}); return e },
		"CreatePR":        func() error { _, e := c.CreatePR(ctx, "A-1", ""); return e },
		"Output":          func() error { _, e := c.Output(ctx, "A-1", 10); return e },
		"CollabConflicts": func() error { _, e := c.CollabConflicts(ctx); return e },
		"BranchStatuses":  func() error { _, e := c.BranchStatuses(ctx); return e },
		"PipelineList":    func() error { _, e := c.PipelineList(ctx); return e },
		"PipelineGet":     func() error { _, e := c.PipelineGet(ctx, "p"); return e },
		"ScheduleList":    func() error { _, e := c.ScheduleList(ctx); return e },
		"GetMetrics":      func() error { _, e := c.GetMetrics(ctx); return e },
		"SnapshotList":    func() error { _, e := c.SnapshotList(ctx, ""); return e },
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			err := call()
			require.Error(t, err)
			var se *StatusError
			require.ErrorAs(t, err, &se, "%s must surface a StatusError", name)
			require.Equal(t, http.StatusInternalServerError, se.Code)
		})
	}
}

func TestGetMetricsHistoryForwardsFilters(t *testing.T) {
	var rawQ string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/metrics/history", r.URL.Path)
		rawQ = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"samples":[{}]}`)
	}))
	defer ts.Close()
	got, err := New(ts.URL).GetMetricsHistory(context.Background(), "2026-01-01T00:00:00Z", 5)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Contains(t, rawQ, "since=")
	require.Contains(t, rawQ, "limit=5")
}

// TestInsightsRecoversFilesFromDigest drives Insights with one active session so
// the best-effort digest file-set recovery path (bestEffortFiles) runs.
func TestInsightsRecoversFilesFromDigest(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/sessions":
			_, _ = io.WriteString(w, `{"sessions":[{"id":"A-1","status":"working","repo":"/r"}]}`)
		case "/api/v1/history":
			_, _ = io.WriteString(w, `{"sessions":[]}`)
		case "/api/v1/metrics/history":
			_, _ = io.WriteString(w, `{"summaries":[]}`)
		case "/api/v1/sessions/A-1/digest":
			_, _ = io.WriteString(w, `{"files":[{"path":"a.go"},{"path":"b.go"}]}`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	defer ts.Close()
	rep, err := New(ts.URL).Insights(context.Background(), InsightsParams{MaxFileScans: 5})
	require.NoError(t, err)
	require.NotNil(t, rep)
}

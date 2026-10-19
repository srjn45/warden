package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/srjn45/warden/internal/ptyhost"
	"github.com/srjn45/warden/internal/ptyobs"
)

func getBody(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestSessionRuntimeAndDecisionsEndpoints(t *testing.T) {
	srv, _, ts, fs := ptyServer(t, ptyhost.ModeShadow)
	ctx := context.Background()
	_ = fs.Insert(ctx, &agentstore.Agent{ID: "A-1", TmuxSession: "A-1", PermissionPosture: ptyevent.PostureDangerouslyExecuteAll})
	_ = fs.Insert(ctx, &agentstore.Agent{ID: "A-2", TmuxSession: "A-2"})

	// Runtime: posture/danger are reported even for an unsupervised session.
	code, body := getBody(t, ts.URL+"/api/v1/sessions/A-1/runtime")
	require.Equal(t, http.StatusOK, code, body)
	var rv ptyobs.RuntimeView
	require.NoError(t, json.Unmarshal([]byte(body), &rv))
	require.Equal(t, "dangerously-execute-all", rv.Posture)
	require.True(t, rv.Danger)
	require.Contains(t, rv.DangerBanner, "DANGER")
	require.False(t, rv.Supervised)

	code, body = getBody(t, ts.URL+"/api/v1/sessions/A-2/runtime")
	require.Equal(t, http.StatusOK, code)
	require.NoError(t, json.Unmarshal([]byte(body), &rv))
	require.Equal(t, "allowlisted", rv.Posture, "unset posture reports the safe default")
	require.False(t, rv.Danger)

	code, _ = getBody(t, ts.URL+"/api/v1/sessions/nope/runtime")
	require.Equal(t, http.StatusNotFound, code)

	// Decisions: empty before any, then newest-first with outcome filter.
	code, body = getBody(t, ts.URL+"/api/v1/sessions/A-1/decisions")
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, body, `"records":[]`)

	tr := srv.openDecisionTrace()
	require.NotNil(t, tr)
	now := time.Now().UTC()
	require.NoError(t, tr.Append(ptyobs.TraceRecord{ID: "d1", Time: now.Add(-time.Minute), SessionID: "A-1", Outcome: ptyobs.OutcomeApproved, Route: ptyobs.RoutePosture}))
	require.NoError(t, tr.Append(ptyobs.TraceRecord{ID: "d2", Time: now, SessionID: "A-1", Outcome: ptyobs.OutcomeEscalated, Route: ptyobs.RouteHuman, Reason: "unrecognized"}))

	var list struct {
		SessionID string
		Records   []ptyobs.TraceRecord
	}
	_, body = getBody(t, ts.URL+"/api/v1/sessions/A-1/decisions")
	require.NoError(t, json.Unmarshal([]byte(body), &list))
	require.Len(t, list.Records, 2)
	require.Equal(t, "d2", list.Records[0].ID)

	_, body = getBody(t, ts.URL+"/api/v1/sessions/A-1/decisions?outcome=approved")
	require.NoError(t, json.Unmarshal([]byte(body), &list))
	require.Len(t, list.Records, 1)

	code, _ = getBody(t, ts.URL+"/api/v1/sessions/A-1/decisions?outcome=bogus")
	require.GreaterOrEqual(t, code, 400)

	// The latest decision's route surfaces in the runtime view.
	_, body = getBody(t, ts.URL+"/api/v1/sessions/A-1/runtime")
	require.NoError(t, json.Unmarshal([]byte(body), &rv))
	require.Equal(t, "human", rv.Route)
	require.Equal(t, "escalated_human", rv.LastOutcome)
}

func TestPTYMetricsEndpoints(t *testing.T) {
	srv, _, ts, _ := ptyServer(t, ptyhost.ModeShadow)
	srv.countPTYEvent(ptyevent.Event{Kind: ptyevent.KindMarker, Marker: &ptyevent.Marker{Type: ptyevent.MarkerGap}})
	srv.countPTYEvent(ptyevent.Event{Kind: ptyevent.KindLifecycle, Lifecycle: &ptyevent.Lifecycle{Phase: ptyevent.PhaseRecovered}})
	srv.countPTYEvent(ptyevent.Event{Kind: ptyevent.KindOutput, Redactions: []ptyevent.Redaction{{Kind: ptyevent.SecretJWT}, {Kind: ptyevent.SecretJWT}}})

	code, body := getBody(t, ts.URL+"/api/v1/pty/metrics")
	require.Equal(t, http.StatusOK, code, body)
	var snap ptyobs.Snapshot
	require.NoError(t, json.Unmarshal([]byte(body), &snap))
	require.EqualValues(t, 1, snap.Total(ptyobs.MBackpressure, "kind", "gap"))
	require.EqualValues(t, 1, snap.Total(ptyobs.MReconnects, "result", "resumed"))
	require.EqualValues(t, 2, snap.Total(ptyobs.MRedactions, "kind", "jwt"))
	require.EqualValues(t, 3, snap.Total(ptyobs.MEvents))

	code, body = getBody(t, ts.URL+"/api/v1/pty/metrics.prom")
	require.Equal(t, http.StatusOK, code)
	require.True(t, strings.Contains(body, "# TYPE warden_pty_events_total counter"), body)
	require.Contains(t, body, `warden_pty_redactions_total{kind="jwt"} 2`)
}

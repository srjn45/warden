package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/ptyobs"
)

// TestInspectPTYScriptedFixture drives `inspect pty runtime|decisions|metrics`
// against canned daemon documents: the danger banner, route, redacted summary
// and bounded metrics must survive to the terminal, and --json round-trips.
func TestInspectPTYScriptedFixture(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/runtime"):
			_, _ = w.Write([]byte(`{"session_id":"A-1","mode":"on","requested_mode":"on","actual_kind":"tmux","process_owner":"tmux","evidence_source":"live","evidence_freshness":"fresh","unsupported_reason":"shadow mirror is observe-only","supervised":true,"authoritative":false,"state":"waiting","reason":"permission","evidence":["menu:4 options"],"input_owner":"warden","posture":"dangerously-execute-all","danger":true,"danger_banner":"` + ptyobs.DangerBanner + `","route":"posture","last_outcome":"approved","prompt":{"class":"permission","text":"Run a command: git status"}}`))
		case strings.HasSuffix(r.URL.Path, "/decisions"):
			require.Equal(t, "escalated_human", r.URL.Query().Get("outcome"))
			_, _ = w.Write([]byte(`{"session_id":"A-1","records":[{"id":"d2","time":"2026-10-19T10:00:00Z","session_id":"A-1","outcome":"escalated_human","route":"human","reason":"unrecognized","posture":"allowlisted","policy":{"act":true},"permission":{"class":"permission","text":"x"},"latency_ms":3}]}`))
		case strings.HasSuffix(r.URL.Path, "/pty/metrics"):
			_, _ = w.Write([]byte(`{"families":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")

	out, err := runGit(t, addr, "inspect", "pty", "runtime", "A-1")
	require.NoError(t, err)
	for _, want := range []string{"DANGER", "waiting", "permission", "warden", "posture", "approved", "requested=on", "actual=tmux", "freshness=fresh", "observe-only"} {
		require.Contains(t, out, want)
	}

	out, err = runGit(t, addr, "inspect", "pty", "runtime", "A-1", "--json")
	require.NoError(t, err)
	require.Contains(t, out, `"danger": true`)

	out, err = runGit(t, addr, "inspect", "pty", "decisions", "A-1", "--outcome", "escalated_human")
	require.NoError(t, err)
	require.Contains(t, out, "escalated_human")
	require.Contains(t, out, "unrecognized")

	_, err = runGit(t, addr, "inspect", "pty", "metrics")
	require.NoError(t, err)
}

package notify

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWebhookNotifierPostsJSONPayload(t *testing.T) {
	var (
		mu        sync.Mutex
		gotBody   webhookPayload
		gotMethod string
		gotCType  string
		hitCount  int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		hitCount++
		gotMethod = r.Method
		gotCType = r.Header.Get("Content-Type")
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	NewWebhook(srv.URL).Notify("warden — needs input", "agent-a1b2: review auth")

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, hitCount)
	require.Equal(t, http.MethodPost, gotMethod)
	require.Equal(t, "application/json", gotCType)
	require.Equal(t, "warden — needs input", gotBody.Title)
	require.Equal(t, "agent-a1b2: review auth", gotBody.Body)
	// Slack renders the "text" field; it must carry both title and body.
	require.Contains(t, gotBody.Text, "warden — needs input")
	require.Contains(t, gotBody.Text, "agent-a1b2: review auth")
}

func TestWebhookNotifierNon2xxIsBestEffort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(prev)

	NewWebhook(srv.URL).Notify("title", "body") // must not panic or propagate
	require.Contains(t, logBuf.String(), "non-2xx")
}

func TestWebhookNotifierTransportErrorIsBestEffort(t *testing.T) {
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(prev)

	// Nothing listening on this address → connection refused, logged not propagated.
	NewWebhook("http://127.0.0.1:0/hook").Notify("title", "body")
	require.Contains(t, logBuf.String(), "webhook post failed")
}

func TestMultiNotifierFansOutAndSkipsNil(t *testing.T) {
	a := &countingNotifier{}
	b := &countingNotifier{}
	n := Multi(a, nil, b)
	n.Notify("t", "x")
	require.Equal(t, 1, a.count)
	require.Equal(t, 1, b.count)
}

func TestMultiNotifierSingleReturnsUnderlying(t *testing.T) {
	a := &countingNotifier{}
	require.Same(t, a, Multi(a, nil))
}

type countingNotifier struct{ count int }

func (c *countingNotifier) Notify(string, string) { c.count++ }

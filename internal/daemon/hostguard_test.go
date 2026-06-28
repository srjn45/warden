package daemon

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// serve runs req through s.hostGuard wrapping a 200 handler and reports whether
// the inner handler was reached and the response code.
func serveHostGuard(s *Server, host string) (reached bool, code int) {
	h := s.hostGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
	req.Host = host
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return reached, rec.Code
}

func TestHostGuardNoAuthRejectsNonLoopbackHost(t *testing.T) {
	// The default no-auth mode: only a loopback Host is allowed. A rebound
	// attacker-domain Host (…→127.0.0.1) is rejected before the handler runs.
	s := &Server{} // authToken == "" ⇒ auth disabled
	cases := []struct {
		host string
		want bool // reached
	}{
		{"127.0.0.1:8765", true},
		{"localhost:8765", true},
		{"[::1]:8765", true},
		{"localhost", true},
		{"attacker.example.com:8765", false},
		{"warden.evil.test", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.host, func(t *testing.T) {
			reached, code := serveHostGuard(s, tc.host)
			require.Equal(t, tc.want, reached, "host %q reached=%v", tc.host, reached)
			if !tc.want {
				require.Equal(t, http.StatusForbidden, code)
			}
		})
	}
}

func TestHostGuardWithTokenAllowsAnyHost(t *testing.T) {
	// When a token is configured it is the trust boundary, so a reverse proxy /
	// tunnel forwarding the public domain as Host must pass the guard (auth still
	// runs separately).
	s := &Server{authToken: "secret"}
	reached, code := serveHostGuard(s, "warden.example.com")
	require.True(t, reached, "token mode must not host-gate proxied requests")
	require.Equal(t, http.StatusOK, code)
}

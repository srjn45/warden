package daemon

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAuthLimiter(t *testing.T) {
	now := time.Unix(0, 0)
	l := newAuthLimiter(3, time.Minute)

	// Under the cap: not blocked.
	for i := 0; i < 3; i++ {
		require.False(t, l.blocked("1.2.3.4", now), "blocked before exceeding cap")
		l.recordFailure("1.2.3.4", now)
	}
	// Cap reached → blocked.
	require.True(t, l.blocked("1.2.3.4", now))
	// A different IP is independent.
	require.False(t, l.blocked("5.6.7.8", now))

	// The window rolls off.
	require.False(t, l.blocked("1.2.3.4", now.Add(time.Minute+time.Second)))

	// A success clears the budget.
	l = newAuthLimiter(1, time.Minute)
	l.recordFailure("1.2.3.4", now)
	require.True(t, l.blocked("1.2.3.4", now))
	l.clear("1.2.3.4")
	require.False(t, l.blocked("1.2.3.4", now))
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.RemoteAddr = "203.0.113.7:54321"
	require.Equal(t, "203.0.113.7", clientIP(r))

	r.RemoteAddr = "no-port" // malformed → returned as-is
	require.Equal(t, "no-port", clientIP(r))
}

func TestAuthMiddlewareThrottles(t *testing.T) {
	s := &Server{authToken: "secret", authLimiter: newAuthLimiter(2, time.Minute)}
	h := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	do := func(token string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
		req.RemoteAddr = "9.9.9.9:1111"
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	require.Equal(t, http.StatusUnauthorized, do("wrong")) // fail 1
	require.Equal(t, http.StatusUnauthorized, do("wrong")) // fail 2 (hits cap)
	require.Equal(t, http.StatusTooManyRequests, do("wrong"), "over cap → 429")

	// A valid token bypasses the throttle entirely and clears the budget.
	require.Equal(t, http.StatusOK, do("secret"))
	require.Equal(t, http.StatusUnauthorized, do("wrong"), "budget reset after success")
}

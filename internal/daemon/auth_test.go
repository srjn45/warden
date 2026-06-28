package daemon

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAuthorizeNoTokenAllowsAll(t *testing.T) {
	s := &Server{} // authToken == "" ⇒ auth disabled
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
	if ok, scope := s.authorize(req); !ok || scope != scopeFull {
		t.Fatalf("auth-disabled must allow; got ok=%v scope=%v", ok, scope)
	}
}

func TestAuthorizeWithToken(t *testing.T) {
	s := &Server{authToken: "secret"}

	cases := []struct {
		name string
		set  func(r *http.Request)
		want bool
	}{
		{"valid header", func(r *http.Request) { r.Header.Set("Authorization", "Bearer secret") }, true},
		{"valid query param", func(r *http.Request) { r.URL.RawQuery = "token=secret" }, true},
		{"missing", func(r *http.Request) {}, false},
		{"wrong token", func(r *http.Request) { r.Header.Set("Authorization", "Bearer nope") }, false},
		{"wrong query token", func(r *http.Request) { r.URL.RawQuery = "token=nope" }, false},
		{"header without Bearer prefix", func(r *http.Request) { r.Header.Set("Authorization", "secret") }, false},
		{"empty bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer ") }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
			tc.set(req)
			if ok, _ := s.authorize(req); ok != tc.want {
				t.Fatalf("authorize() ok=%v, want %v", ok, tc.want)
			}
		})
	}
}

func TestAuthorizeReadonlyToken(t *testing.T) {
	s := &Server{authToken: "full-secret", readonlyToken: "ro-secret"}

	bearer := func(tok string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		return r
	}

	ok, scope := s.authorize(bearer("full-secret"))
	require.True(t, ok)
	require.Equal(t, scopeFull, scope)

	ok, scope = s.authorize(bearer("ro-secret"))
	require.True(t, ok)
	require.Equal(t, scopeReadonly, scope)

	ok, scope = s.authorize(bearer("nope"))
	require.False(t, ok)
	require.Equal(t, scopeNone, scope)

	// An empty readonly token must never match an absent/empty presented token.
	s2 := &Server{authToken: "full-secret"} // readonlyToken == ""
	ok, scope = s2.authorize(bearer(""))
	require.False(t, ok)
	require.Equal(t, scopeNone, scope)
}

func TestAuthMiddlewareReadonlyScope(t *testing.T) {
	s := &Server{authToken: "full-secret", readonlyToken: "ro-secret"}
	var reached bool
	h := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	do := func(method, path, tok string) int {
		reached = false
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	// Read-only token: GETs pass (including the pure-read SSE event stream).
	require.Equal(t, http.StatusOK, do(http.MethodGet, "/api/v1/sessions", "ro-secret"))
	require.True(t, reached)
	require.Equal(t, http.StatusOK, do(http.MethodGet, "/api/v1/events/stream", "ro-secret"))
	require.True(t, reached)

	// Read-only token: writes are forbidden (403), handler not reached.
	require.Equal(t, http.StatusForbidden, do(http.MethodPost, "/api/v1/spawn", "ro-secret"))
	require.False(t, reached)
	require.Equal(t, http.StatusForbidden, do(http.MethodDelete, "/api/v1/pipelines/p1", "ro-secret"))
	require.False(t, reached)
	// The interactive PTY attach is a GET but grants write power → forbidden.
	require.Equal(t, http.StatusForbidden, do(http.MethodGet, "/api/v1/sessions/s1/attach", "ro-secret"))
	require.False(t, reached)
	require.Equal(t, http.StatusForbidden, do(http.MethodGet, "/api/v1/cockpit/attach", "ro-secret"))
	require.False(t, reached)

	// Full token: a write reaches the handler.
	require.Equal(t, http.StatusOK, do(http.MethodPost, "/api/v1/spawn", "full-secret"))
	require.True(t, reached)
}

func TestAuthorizeLoopbackStillRequiresToken(t *testing.T) {
	// No loopback exemption: a localhost request with no token is denied when a
	// token is configured (a same-host reverse proxy must not bypass auth).
	s := &Server{authToken: "secret"}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	if ok, _ := s.authorize(req); ok {
		t.Fatal("loopback request without token must be denied when auth is on")
	}
}

func TestAuthMiddleware(t *testing.T) {
	s := &Server{authToken: "secret"}
	var reached bool
	h := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	// Denied: no token → 401, handler not reached.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: code=%d, want 401", rec.Code)
	}
	if reached {
		t.Fatal("handler must not run for an unauthorized request")
	}

	// Allowed: valid token → handler runs.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
	req.Header.Set("Authorization", "Bearer secret")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !reached {
		t.Fatalf("valid token: code=%d reached=%v, want 200/true", rec.Code, reached)
	}
}

// TestRouterAuthSplit pins the public/gated boundary: with a token configured,
// the static UI and the liveness probe stay reachable (so a remote browser can
// load the app and prompt for a token), while data routes demand the token.
func TestRouterAuthSplit(t *testing.T) {
	srv := &Server{store: newFakeStore(), authToken: "secret"}
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	get := func(path, token string) int {
		req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		require.NoError(t, err)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		return resp.StatusCode
	}

	// Public even without a token.
	require.Equal(t, http.StatusOK, get("/healthz", ""))
	require.NotEqual(t, http.StatusUnauthorized, get("/", ""), "static UI must load so the token modal can render")

	// Gated.
	require.Equal(t, http.StatusUnauthorized, get("/api/v1/sessions", ""))
	require.Equal(t, http.StatusOK, get("/api/v1/sessions", "secret"))
}

func TestBearerToken(t *testing.T) {
	cases := []struct {
		name string
		set  func(r *http.Request)
		want string
	}{
		{"header", func(r *http.Request) { r.Header.Set("Authorization", "Bearer abc") }, "abc"},
		{"header trims space", func(r *http.Request) { r.Header.Set("Authorization", "Bearer  abc ") }, "abc"},
		{"query", func(r *http.Request) { r.URL.RawQuery = "token=xyz" }, "xyz"},
		{"none", func(r *http.Request) {}, ""},
		{"non-bearer header", func(r *http.Request) { r.Header.Set("Authorization", "Basic abc") }, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/x", nil)
			tc.set(req)
			if got := bearerToken(req); got != tc.want {
				t.Fatalf("bearerToken()=%q, want %q", got, tc.want)
			}
		})
	}
}

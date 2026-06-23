package daemon

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAuthorizeNoTokenAllowsAll(t *testing.T) {
	s := &Server{} // authToken == "" ⇒ auth disabled
	req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
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
			req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
			tc.set(req)
			if ok, _ := s.authorize(req); ok != tc.want {
				t.Fatalf("authorize() ok=%v, want %v", ok, tc.want)
			}
		})
	}
}

func TestAuthorizeLoopbackStillRequiresToken(t *testing.T) {
	// No loopback exemption: a localhost request with no token is denied when a
	// token is configured (a same-host reverse proxy must not bypass auth).
	s := &Server{authToken: "secret"}
	req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
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
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: code=%d, want 401", rec.Code)
	}
	if reached {
		t.Fatal("handler must not run for an unauthorized request")
	}

	// Allowed: valid token → handler runs.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	req.Header.Set("Authorization", "Bearer secret")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !reached {
		t.Fatalf("valid token: code=%d reached=%v, want 200/true", rec.Code, reached)
	}
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
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			tc.set(req)
			if got := bearerToken(req); got != tc.want {
				t.Fatalf("bearerToken()=%q, want %q", got, tc.want)
			}
		})
	}
}

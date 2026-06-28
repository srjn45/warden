package daemon

import (
	"crypto/subtle"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/config"
)

// authScope describes what an authenticated request is permitted to do. Today a
// valid token grants full access; the scope return value is the seam for future
// per-client / read-only tokens — callers branch on the scope, never on the raw
// token — so adding scoped tokens later is an additive change to authorize, not
// a rewrite of the middleware or routes.
type authScope int

const (
	scopeNone authScope = iota // request is not authorized
	scopeFull                  // full access
)

// authMiddleware rejects requests that authorize denies. It guards the
// authenticated route group — the data/action API, the SSE stream, and the WS
// attach. The static UI and /healthz are intentionally served outside this
// group so a remote browser can load the app shell and then prompt for a token
// (the compiled SPA carries no secrets; the data routes behind it do).
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Authorize first: a valid token always passes, so a successful client
		// is never throttled (and a fixed typo clears the IP's failure count).
		if ok, _ := s.authorize(r); ok {
			if s.authLimiter != nil {
				s.authLimiter.clear(clientIP(r))
			}
			next.ServeHTTP(w, r)
			return
		}
		// Failed auth: throttle repeated wrong-token attempts per source IP.
		if s.authLimiter != nil {
			ip, now := clientIP(r), time.Now()
			if s.authLimiter.blocked(ip, now) {
				w.Header().Set("Retry-After", strconv.Itoa(int(s.authLimiter.window.Seconds())))
				writeErr(w, http.StatusTooManyRequests, "too many failed authentication attempts; slow down")
				return
			}
			s.authLimiter.recordFailure(ip, now)
		}
		writeErr(w, http.StatusUnauthorized, "unauthorized: missing or invalid bearer token")
	})
}

// hostGuard defends the no-auth loopback default against DNS rebinding. When no
// token is configured, the daemon's only trust boundary is its network position
// (it binds loopback), so a request must also present a loopback Host header — a
// browser whose attacker-controlled domain has been rebound to 127.0.0.1 sends
// the attacker's domain as Host, which is rejected. When a token IS set, the
// token is the trust boundary and arbitrary Host values are allowed: a reverse
// proxy or tunnel (e.g. Cloudflare Tunnel) legitimately forwards the public
// domain as Host, and those deployments already require a token to bind a
// non-loopback address. This guards only the data/action API group; the static
// SPA shell and /healthz carry no secrets and stay reachable for any Host.
func (s *Server) hostGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.authToken == "" && !config.IsLoopbackHost(r.Host) {
			writeErr(w, http.StatusForbidden,
				"forbidden: request Host is not loopback; set a bearer token (run `warden token generate`) to allow proxied hosts")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authorize is the single decision point for request authentication. When no
// token is configured (the default loopback-only mode) auth is disabled and
// every request is allowed — preserving today's behavior. When a token is set,
// every request must present it, including loopback: the source address cannot
// distinguish the local CLI from a same-host reverse proxy (e.g. Cloudflare
// Tunnel) forwarding a remote client, so there is no loopback exemption.
func (s *Server) authorize(r *http.Request) (bool, authScope) {
	if s.authToken == "" {
		return true, scopeFull
	}
	got := bearerToken(r)
	if got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(s.authToken)) == 1 {
		return true, scopeFull
	}
	return false, scopeNone
}

// bearerToken extracts the presented token from the Authorization header
// ("Bearer <t>"), falling back to a ?token=<t> query param. The query-param
// form exists for SSE (EventSource) and the WS upgrade, which cannot set custom
// request headers.
func bearerToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if t, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(t)
		}
	}
	return strings.TrimSpace(r.URL.Query().Get("token"))
}

const (
	// maxBodyBytes caps a request body (JSON POSTs); GETs and the WS upgrade
	// read no body, so this is a no-op for them.
	maxBodyBytes int64 = 1 << 20
	// writeTimeoutDur bounds non-streaming handler execution. Streaming routes
	// (SSE, WS attach, message long-poll) are exempt — see isStreamingPath.
	writeTimeoutDur = 30 * time.Second
)

// isStreamingPath reports whether a request targets a long-lived endpoint that
// must NOT be wrapped in http.TimeoutHandler (it buffers the response and breaks
// Flush/Hijack): the SSE stream, the WS tmux attach, and the message long-poll.
func isStreamingPath(r *http.Request) bool {
	p := r.URL.Path
	return strings.HasSuffix(p, "/events/stream") ||
		strings.HasSuffix(p, "/attach") ||
		strings.HasSuffix(p, "/messages/wait")
}

// maxBytes returns middleware that caps each request body at n bytes.
func maxBytes(n int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, n)
			next.ServeHTTP(w, r)
		})
	}
}

// writeTimeout returns middleware that bounds handler execution at d, except for
// streaming paths (which would break under http.TimeoutHandler's buffering).
func writeTimeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		timed := http.TimeoutHandler(next, d, `{"error":"request timed out"}`)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isStreamingPath(r) {
				next.ServeHTTP(w, r)
				return
			}
			timed.ServeHTTP(w, r)
		})
	}
}

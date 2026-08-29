package daemon

import (
	"crypto/subtle"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/auth"
	"github.com/srjn45/warden/internal/config"
)

// authScope describes what an authenticated request is permitted to do. Callers
// branch on the scope, never on the raw token, so adding scopes is an additive
// change to authorize, not a rewrite of the middleware or routes. Today there
// are two grant levels: scopeFull (the primary WARDEN_TOKEN) and scopeReadonly
// (the optional WARDEN_READONLY_TOKEN), enforced in authMiddleware.
type authScope int

const (
	scopeNone     authScope = iota // request is not authorized
	scopeReadonly                  // reads only (GETs + SSE); writes/attach denied
	scopeFull                      // full access
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
		if ok, scope := s.authorize(r); ok {
			if s.authLimiter != nil {
				s.authLimiter.clear(clientIP(r))
			}
			// A read-only token authenticates fine but may not mutate state or
			// open the interactive PTY attach. This is enforced here, after the
			// limiter clear, so a valid read-only token is never throttled — its
			// write attempts are an authorization (403) failure, not an auth one.
			if scope == scopeReadonly && isWriteRequest(r) {
				writeErr(w, http.StatusForbidden,
					"forbidden: this is a read-only token ("+auth.ReadonlyTokenEnv+"); use the full token for this action")
				return
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
	if got == "" {
		return false, scopeNone
	}
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.authToken)) == 1 {
		return true, scopeFull
	}
	// The read-only token is optional; only consider it when configured so an
	// empty s.readonlyToken can never match an empty/absent presented token.
	if s.readonlyToken != "" && subtle.ConstantTimeCompare([]byte(got), []byte(s.readonlyToken)) == 1 {
		return true, scopeReadonly
	}
	return false, scopeNone
}

// isWriteRequest reports whether a request needs full (write) scope. Every
// non-GET method mutates state or triggers an action (verified against the
// OpenAPI spec — there is no read implemented as POST). The one read-method
// exception is the interactive PTY attach (paths ending "/attach": the session
// attach and the cockpit attach), an HTTP GET that upgrades to a WebSocket the
// client can type *into* — a write capability — so a read-only token is denied
// it too. The SSE event stream (".../events/stream") stays a pure read.
func isWriteRequest(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return strings.HasSuffix(r.URL.Path, "/attach")
	default:
		return true
	}
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
	// writeTimeoutDur bounds fast (data/action) handler execution. Streaming
	// routes (SSE, WS attach, message long-poll) are exempt — see
	// isStreamingPath — and slow lifecycle routes get slowWriteTimeoutDur.
	// Both budgets are defaults only: `http_timeout_fast` / `http_timeout_slow`
	// in config override them (Server.SetWriteTimeouts).
	writeTimeoutDur = 30 * time.Second
	// slowWriteTimeoutDur bounds the handful of lifecycle/action routes that do
	// real work: git worktree checkout on spawn (materializes a whole tree — for
	// a large monorepo this alone can run for minutes), push/sync, running the
	// project's tests, pipeline reconciliation. These legitimately exceed the 30s
	// fast-path budget — the CLI client already allots them a 5-minute deadline
	// (client.longTimeout) — so the daemon must not cut them at 30s. Before this
	// existed, the global 30s guard returned a 503 ("request timed out") and, for
	// spawn, SIGKILLed git mid-checkout (cancelled request context), leaking a
	// half-built worktree. Set ABOVE the client deadline so the client's own
	// timeout is the effective cap and this is only a backstop against a
	// genuinely wedged handler.
	slowWriteTimeoutDur = 10 * time.Minute
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

// isSlowPath reports whether a request targets a lifecycle/action route that
// does real, possibly-minutes-long work (git, network, running tests, pipeline
// reconciliation) and so must run under slowWriteTimeoutDur rather than the 30s
// fast-path budget. This set mirrors the routes the CLI client budgets
// longTimeout (5min) for (internal/client.longTimeout call sites) — keep the two
// in sync. Matching is by path suffix and deliberately generous: over-matching a
// route that is actually fast is harmless (it finishes well inside either
// budget), whereas under-matching a genuinely slow route reintroduces the 503
// regression. Streaming paths are handled first in writeTimeout, so the
// "/events/stream" SSE feed never reaches here (only the fast "/events" hook
// ingestion, which is intentionally NOT slow).
func isSlowPath(r *http.Request) bool {
	p := r.URL.Path
	switch {
	case strings.HasSuffix(p, "/spawn"),
		strings.HasSuffix(p, "/adopt"),
		strings.HasSuffix(p, "/check"),
		strings.HasSuffix(p, "/prune"),
		// commit runs the repo's own pre-commit hooks (vet/lint/tests), which in
		// a large monorepo routinely exceed the fast budget.
		strings.HasSuffix(p, "/git/commit"),
		strings.HasSuffix(p, "/git/push"),
		strings.HasSuffix(p, "/git/sync"),
		// clone shells `git clone` over the network, same class as the git/* routes.
		strings.HasSuffix(p, "/fs/clone"),
		// the remote/new project flows shell git clone / init+commit, same class.
		strings.HasSuffix(p, "/projects/remote"),
		strings.HasSuffix(p, "/projects/new"),
		strings.HasSuffix(p, "/remove-worktree"),
		strings.HasSuffix(p, "/create-pr"),
		strings.HasSuffix(p, "/digest"),
		strings.HasSuffix(p, "/snapshots"),
		strings.HasSuffix(p, "/restore"),
		strings.HasSuffix(p, "/start"),
		strings.HasSuffix(p, "/resume"),
		strings.HasSuffix(p, "/emit"),
		strings.HasSuffix(p, "/retry"):
		return true
	}
	return false
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

// writeTimeout returns middleware that bounds handler execution, using the fast
// budget for ordinary data/action routes and the slow budget for lifecycle
// routes that do real work (isSlowPath). Streaming paths are exempt entirely —
// http.TimeoutHandler buffers the response, breaking Flush/Hijack.
func writeTimeout(fast, slow time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		fastH := http.TimeoutHandler(next, fast, `{"error":"request timed out"}`)
		slowH := http.TimeoutHandler(next, slow, `{"error":"request timed out"}`)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case isStreamingPath(r):
				next.ServeHTTP(w, r)
			case isSlowPath(r):
				slowH.ServeHTTP(w, r)
			default:
				fastH.ServeHTTP(w, r)
			}
		})
	}
}

package daemon

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// Auth-failure throttling. A 256-bit random token is already infeasible to
// brute-force, so this is defense-in-depth (and stops a wrong-token client from
// spamming the daemon / logs): after authFailMax failed attempts from one
// source IP within authFailWindow, further failures get 429 until the window
// rolls off. A request bearing a VALID token is never throttled — authorize is
// checked first — so legitimate users are never locked out, even when many
// clients share one source IP behind a reverse proxy (e.g. Cloudflare Tunnel).
const (
	authFailMax    = 10
	authFailWindow = time.Minute
	// authLimiterPrune bounds memory: once the IP map grows past this many
	// entries, expired ones are swept on the next recorded failure.
	authLimiterPrune = 1024
)

type failWindow struct {
	count   int
	expires time.Time
}

type authLimiter struct {
	mu     sync.Mutex
	fails  map[string]*failWindow
	max    int
	window time.Duration
}

func newAuthLimiter(max int, window time.Duration) *authLimiter {
	return &authLimiter{fails: make(map[string]*failWindow), max: max, window: window}
}

// blocked reports whether ip has exhausted its failure budget for the current
// window. It does not mutate the budget.
func (l *authLimiter) blocked(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.fails[ip]
	return w != nil && now.Before(w.expires) && w.count >= l.max
}

// recordFailure counts one failed attempt against ip, opening a fresh window if
// none is active.
func (l *authLimiter) recordFailure(ip string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.fails) >= authLimiterPrune {
		for k, w := range l.fails {
			if !now.Before(w.expires) {
				delete(l.fails, k)
			}
		}
	}
	w := l.fails[ip]
	if w == nil || !now.Before(w.expires) {
		l.fails[ip] = &failWindow{count: 1, expires: now.Add(l.window)}
		return
	}
	w.count++
}

// clear forgets ip's failures — called after a successful auth so a user who
// fat-fingered the token once isn't throttled after they fix it.
func (l *authLimiter) clear(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, ip)
}

// clientIP is the throttling key: the source host without its port. Behind a
// reverse proxy this is the proxy's address (we deliberately do not trust a
// spoofable X-Forwarded-For); see the package comment for why that's safe here.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

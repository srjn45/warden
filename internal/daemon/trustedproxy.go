package daemon

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

// ParseTrustedProxies turns a config list of IPs/CIDRs into nets. A bare IP
// becomes a single-address net (/32 or /128); a CIDR is parsed as-is. An invalid
// entry is an error so the daemon fails fast at startup rather than silently
// trusting nothing. An empty/nil input yields a nil slice (the feature is off).
func ParseTrustedProxies(entries []string) ([]*net.IPNet, error) {
	var nets []*net.IPNet
	for _, raw := range entries {
		e := strings.TrimSpace(raw)
		if e == "" {
			continue
		}
		if !strings.Contains(e, "/") {
			ip := net.ParseIP(e)
			if ip == nil {
				return nil, fmt.Errorf("invalid trusted_proxies entry %q: not an IP or CIDR", raw)
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			nets = append(nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		_, n, err := net.ParseCIDR(e)
		if err != nil {
			return nil, fmt.Errorf("invalid trusted_proxies entry %q: %w", raw, err)
		}
		nets = append(nets, n)
	}
	return nets, nil
}

// ipInNets reports whether ip (a host string, no port) falls inside any net.
func ipInNets(ip string, nets []*net.IPNet) bool {
	parsed := net.ParseIP(strings.TrimSpace(ip))
	if parsed == nil {
		return false
	}
	for _, n := range nets {
		if n.Contains(parsed) {
			return true
		}
	}
	return false
}

// auditActor returns the identity to stamp on an audit event. By default it is
// the immediate peer's address (clientIP / RemoteAddr host). When that peer is a
// configured trusted proxy AND the request carries X-Forwarded-For, it instead
// returns the right-most XFF entry that is not itself a trusted proxy — the real
// client behind the proxy chain. If the peer is not trusted, XFF is ignored
// entirely (it is client-spoofable), so an attacker cannot forge an actor. This
// affects only the audit trail; the auth-failure throttle keeps the peer key.
func (s *Server) auditActor(r *http.Request) string {
	peer := clientIP(r)
	if len(s.auditTrustedProxies) == 0 || !ipInNets(peer, s.auditTrustedProxies) {
		return peer
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return peer
	}
	parts := strings.Split(xff, ",")
	// Walk right-to-left, skipping trusted hops, to the first untrusted address —
	// the closest-to-origin client we can attribute without trusting spoofable data.
	for i := len(parts) - 1; i >= 0; i-- {
		hop := strings.TrimSpace(parts[i])
		if hop == "" {
			continue
		}
		if !ipInNets(hop, s.auditTrustedProxies) {
			return hop
		}
	}
	// Every hop is a trusted proxy (no distinct client to name): keep the peer.
	return peer
}

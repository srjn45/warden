package daemon

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseTrustedProxies(t *testing.T) {
	nets, err := ParseTrustedProxies([]string{"127.0.0.1", "10.0.0.0/8", "::1", "fd00::/8", "  ", ""})
	require.NoError(t, err)
	require.Len(t, nets, 4, "blank entries are skipped")

	// Bare IPs become single-address nets; CIDRs match their range.
	require.True(t, ipInNets("127.0.0.1", nets))
	require.False(t, ipInNets("127.0.0.2", nets), "bare IP is /32, not a range")
	require.True(t, ipInNets("10.1.2.3", nets))
	require.True(t, ipInNets("::1", nets))
	require.True(t, ipInNets("fd00::abcd", nets))
	require.False(t, ipInNets("8.8.8.8", nets))
	require.False(t, ipInNets("not-an-ip", nets))

	_, err = ParseTrustedProxies([]string{"nonsense"})
	require.Error(t, err, "an unparseable entry is a hard error")
	_, err = ParseTrustedProxies([]string{"10.0.0.0/99"})
	require.Error(t, err, "a bad CIDR is a hard error")
}

func TestAuditActor(t *testing.T) {
	req := func(remoteAddr, xff string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/spawn", nil)
		r.RemoteAddr = remoteAddr
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}

	// No trusted proxies configured: actor is always the peer host, XFF ignored.
	plain := &Server{}
	require.Equal(t, "203.0.113.5", plain.auditActor(req("203.0.113.5:443", "1.2.3.4")))

	nets, err := ParseTrustedProxies([]string{"127.0.0.1", "10.0.0.0/8"})
	require.NoError(t, err)
	s := &Server{auditTrustedProxies: nets}

	// Trusted peer + XFF → the real client from XFF.
	require.Equal(t, "203.0.113.9", s.auditActor(req("127.0.0.1:5000", "203.0.113.9")))

	// Untrusted peer + XFF → XFF ignored (anti-spoof), actor is the peer.
	require.Equal(t, "198.51.100.7", s.auditActor(req("198.51.100.7:5000", "203.0.113.9")))

	// Chain: closest hop (10.0.0.2) is trusted, peer is trusted → right-most
	// untrusted entry is the real client.
	require.Equal(t, "203.0.113.9", s.auditActor(req("127.0.0.1:5000", "203.0.113.9, 10.0.0.2")))

	// Trusted peer but no XFF → fall back to the peer host.
	require.Equal(t, "127.0.0.1", s.auditActor(req("127.0.0.1:5000", "")))

	// Trusted peer, every XFF hop trusted → no distinct client; keep the peer.
	require.Equal(t, "127.0.0.1", s.auditActor(req("127.0.0.1:5000", "10.0.0.2, 10.0.0.3")))
}

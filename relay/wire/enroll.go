package wire

import "time"

// --- Leg 0: enrollment (plain REST, not yamux) ---
//
// Enrollment provisions a daemon's identity BEFORE it can dial the relay. It is
// a daemon-holds-key flow: the daemon generates its own keypair and sends only a
// PKCS#10 CSR; the private key never transits the hub. The hub mints the
// daemon_id, has its CA sign the CSR with CN == daemon_id, and returns the cert
// plus CA chain — never a private key.

// EnrollmentTokenRequest is the hub-authenticated operator call that mints a
// one-time enrollment token to hand to a daemon.
type EnrollmentTokenRequest struct {
	Label      string `json:"label,omitempty"`       // human note, e.g. "home-server"
	TTLSeconds int    `json:"ttl_seconds,omitempty"` // 0 => hub default
}

// EnrollmentTokenResponse carries the minted one-time token and its expiry.
type EnrollmentTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// EnrollRequest is the daemon's enrollment call. It presents the one-time token
// and a PEM-encoded PKCS#10 CSR built from a keypair it generated locally.
type EnrollRequest struct {
	Token    string   `json:"token"`
	CSRPEM   string   `json:"csr_pem"`
	Hostname string   `json:"hostname,omitempty"`
	Caps     []string `json:"caps,omitempty"`
}

// EnrollResponse returns the minted identity: the hub-assigned daemon_id, the
// signed client cert (CN == daemon_id), and the CA cert chain to trust. There is
// deliberately no private-key field — the daemon already holds its key.
type EnrollResponse struct {
	DaemonID  string `json:"daemon_id"`
	CertPEM   string `json:"cert_pem"`
	CACertPEM string `json:"ca_cert_pem"`
}

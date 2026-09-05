package wire

// --- Device authorization (`warden login`) ---
//
// Device authorization is a SIBLING of Leg-0 enrollment, not a fourth relay leg:
// it is a second plain-REST, daemon-holds-key CSR provisioning path that mints a
// daemon identity BEFORE the daemon can dial the relay. Where enrollment consumes
// a one-time operator token, the device flow is the interactive `warden login`
// path — the daemon starts an authorization, the human approves it in a browser
// against a short user code, and the daemon polls for its signed cert. As with
// enrollment the daemon generates its keypair locally and sends only a PKCS#10
// CSR; the private key never transits the hub, and no response carries a key.
//
// Endpoints are hub-owned and daemon-consumed (see warden-hub
// hub-node-contract.md §3.9): POST /api/v1/login/device/start then a poll loop on
// POST /api/v1/login/device/token. On approval the daemon stores its credentials
// and proceeds to dial Leg 1 (/relay) with the issued client certificate.

// DeviceStartRequest initiates device authorization (POST /api/v1/login/device/start).
type DeviceStartRequest struct {
	Hostname string `json:"hostname,omitempty"`
}

// DeviceStartResponse returns verification parameters for the user and daemon.
type DeviceStartResponse struct {
	DeviceCode      string `json:"device_code"`      // high-entropy secret polled by daemon
	UserCode        string `json:"user_code"`        // short code (e.g. "WDN-4821") for user match
	VerificationURI string `json:"verification_uri"` // e.g. "http://localhost:9876/login/device"
	ExpiresIn       int    `json:"expires_in"`       // TTL in seconds (e.g. 900)
	Interval        int    `json:"interval"`         // recommended polling interval (e.g. 5)
}

// DeviceTokenRequest polls authorization status (POST /api/v1/login/device/token).
// It carries the daemon's locally generated PKCS#10 CSR; there is deliberately no
// private-key field — the daemon already holds its key.
type DeviceTokenRequest struct {
	DeviceCode string   `json:"device_code"`
	CSRPEM     string   `json:"csr_pem"`            // PKCS#10 CSR generated locally by daemon
	Hostname   string   `json:"hostname,omitempty"` // node display label
	Caps       []string `json:"caps,omitempty"`     // advertised capabilities
}

// DeviceTokenResponse returns issued identity credentials upon user approval.
// Like EnrollResponse it returns the signed cert and CA chain but never a private
// key — the daemon holds its own key.
type DeviceTokenResponse struct {
	DaemonID  string `json:"daemon_id"`   // minted UUIDv4
	CertPEM   string `json:"cert_pem"`    // signed leaf client cert
	CACertPEM string `json:"ca_cert_pem"` // per-user intermediate CA certificate
}

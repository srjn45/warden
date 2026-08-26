package wire

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
)

// DomainSepAuth is prepended to the challenge nonce before hashing so a signature
// produced for relay daemon-auth can never be replayed as a signature in another
// protocol that reuses the same enrolled key (the key also anchors the daemon's
// TLS identity inside NativeE2E streams). Both sides MUST use these exact bytes.
var DomainSepAuth = []byte("warden-hub/relay/auth/v1\n")

// NonceLen is the required length of an auth challenge nonce. The hub generates
// NonceLen cryptographically-random bytes, single-use, with a short TTL (<=30s);
// single-use and TTL are hub policy, not part of the wire encoding.
const NonceLen = 32

// AuthChallenge is sent by the hub right after the WSS upgrade. The daemon proves
// possession of its enrolled private key by signing it (see AuthResponse).
type AuthChallenge struct {
	Nonce []byte `json:"nonce"`
}

// AuthResponse is the daemon's reply: its hub-minted id plus an
// ECDSA-P256/SHA-256, DER-encoded signature over AuthDigest(nonce). The hub
// verifies Signature against the public key in the daemon's enrolled cert
// (pinned by DaemonID). No secret ever transits the wire.
type AuthResponse struct {
	DaemonID  string `json:"daemon_id"`
	Signature []byte `json:"signature"`
}

// AuthDigest returns the 32-byte digest both sides sign and verify:
// SHA-256(DomainSepAuth || nonce).
func AuthDigest(nonce []byte) []byte {
	h := sha256.New()
	h.Write(DomainSepAuth)
	h.Write(nonce)
	return h.Sum(nil)
}

// NewNonce returns a fresh NonceLen-byte random challenge nonce.
func NewNonce() ([]byte, error) {
	n := make([]byte, NonceLen)
	if _, err := rand.Read(n); err != nil {
		return nil, err
	}
	return n, nil
}

// SignChallenge signs AuthDigest(nonce) with the daemon's enrolled private key,
// returning a DER-encoded ECDSA signature for AuthResponse.Signature.
func SignChallenge(priv *ecdsa.PrivateKey, nonce []byte) ([]byte, error) {
	return ecdsa.SignASN1(rand.Reader, priv, AuthDigest(nonce))
}

// VerifyChallenge reports whether sig is a valid signature over AuthDigest(nonce)
// under pub (the public key pinned from the daemon's enrolled cert).
func VerifyChallenge(pub *ecdsa.PublicKey, nonce, sig []byte) bool {
	return ecdsa.VerifyASN1(pub, AuthDigest(nonce), sig)
}

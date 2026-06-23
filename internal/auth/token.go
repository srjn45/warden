// Package auth holds warden's remote-access authentication primitives: bearer
// token generation and resolution. The actual request-time enforcement
// (middleware) is layered on top of this in the daemon; keeping the token
// plumbing here lets the CLI and the daemon share one definition of where the
// secret comes from.
package auth

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
)

// TokenEnv is the environment variable that holds the daemon's bearer token.
// The token is a shared secret, not per-client; see the remote-access design.
const TokenEnv = "WARDEN_TOKEN"

// tokenBytes is the entropy size of a generated token (32 bytes → 64 hex chars).
// 256 bits of randomness makes the token infeasible to brute-force, so a single
// shared secret is sufficient for the personal-tool threat model.
const tokenBytes = 32

// GenerateToken returns a cryptographically random bearer token, hex-encoded.
// warden never persists the secret: the caller is responsible for exporting it
// (typically as WARDEN_TOKEN) for the daemon to pick up.
func GenerateToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// TokenFromEnv returns the configured bearer token, or "" if unset. Surrounding
// whitespace is trimmed so a trailing newline in an exported value (a common
// copy-paste/`$(...)` mishap) does not silently break authentication.
func TokenFromEnv() string {
	return strings.TrimSpace(os.Getenv(TokenEnv))
}

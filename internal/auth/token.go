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
	"path/filepath"
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

// DefaultTokenFile is where a managed remote install persists the bearer token
// (chmod 600), in the same WARDEN_TOKEN=<hex> form an EnvironmentFile uses.
// Returns "" if the home directory cannot be resolved.
func DefaultTokenFile() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(h, ".warden", "token.env")
}

// tokenFromFile reads WARDEN_TOKEN=<value> from a token.env-style file, or
// returns "" if the file is missing/unreadable or has no such line.
func tokenFromFile(path string) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, TokenEnv+"="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// ResolveToken returns the bearer token a local CLI/TUI should present: the
// WARDEN_TOKEN env var if set, otherwise the token persisted by a managed
// remote install in DefaultTokenFile(). This lets local clients authenticate
// against an auth-enabled daemon without every shell exporting the secret — the
// env var stays the override, the file is the fallback.
func ResolveToken() string {
	if t := TokenFromEnv(); t != "" {
		return t
	}
	return tokenFromFile(DefaultTokenFile())
}

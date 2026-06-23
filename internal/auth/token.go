// Package auth holds warden's remote-access authentication primitives: bearer
// token generation and resolution. The actual request-time enforcement
// (middleware) is layered on top of this in the daemon; keeping the token
// plumbing here lets the CLI and the daemon share one definition of where the
// secret comes from.
package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
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

// WriteTokenFile persists token to a token.env-style file (WARDEN_TOKEN=<hex>)
// with 0600 permissions, creating the parent directory (0700) if needed. The
// write is atomic — a sibling temp file is created, chmod'd, then renamed into
// place — so a reader (the daemon's EnvironmentFile, a concurrent ResolveToken)
// never observes a half-written or world-readable secret. This is the durable
// half of `warden token rotate`.
func WriteTokenFile(path, token string) error {
	if path == "" {
		return errors.New("empty token file path")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".token-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := fmt.Fprintf(tmp, "%s=%s\n", TokenEnv, token); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
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

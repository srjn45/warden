package store

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// NewSessionID returns a random RFC-4122 v4 UUID string, suitable for a
// `claude --session-id`. It returns an error only if the OS random source
// fails; callers propagate it (failing the spawn) rather than crash the daemon.
func NewSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("store: read random bytes for session id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	s := hex.EncodeToString(b[:])
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32], nil
}

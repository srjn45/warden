package store

import (
	"crypto/rand"
	"encoding/hex"
)

// NewSessionID returns a random RFC-4122 v4 UUID string, suitable for a
// `claude --session-id`. It panics only if the OS random source fails (which
// would make the daemon unable to function anyway).
func NewSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("store: cannot read random bytes for session id: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	s := hex.EncodeToString(b[:])
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

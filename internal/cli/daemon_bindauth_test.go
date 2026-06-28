package cli

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRequireTokenForNonLoopback(t *testing.T) {
	// Loopback binds never need a token (hostGuard defends them).
	for _, addr := range []string{"127.0.0.1:8765", "localhost:8765", "[::1]:8765"} {
		require.NoError(t, requireTokenForNonLoopback(addr, ""), "loopback %q should not require a token", addr)
		require.NoError(t, requireTokenForNonLoopback(addr, "tok"), "loopback %q with token is fine", addr)
	}

	// A non-loopback bind requires a token — always (audit #7: allow_nonloopback
	// no longer relaxes this, so there is no tokenless escape hatch left).
	for _, addr := range []string{"0.0.0.0:8765", "192.168.1.10:8765", ":8765"} {
		require.Error(t, requireTokenForNonLoopback(addr, ""), "non-loopback %q without a token must be refused", addr)
		require.NoError(t, requireTokenForNonLoopback(addr, "tok"), "non-loopback %q with a token is allowed", addr)
	}
}

func TestRequireReadonlyHasPrimary(t *testing.T) {
	// A read-only token with no primary token is refused (auth would be off, so
	// the read-only token would silently grant full access).
	require.Error(t, requireReadonlyHasPrimary("", "ro-tok"))

	// Every safe combination is allowed.
	require.NoError(t, requireReadonlyHasPrimary("full-tok", "ro-tok"), "both set is fine")
	require.NoError(t, requireReadonlyHasPrimary("full-tok", ""), "primary only is fine")
	require.NoError(t, requireReadonlyHasPrimary("", ""), "neither set (auth disabled) is fine")
}

package auth

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGenerateToken(t *testing.T) {
	tok, err := GenerateToken()
	require.NoError(t, err)

	// 32 bytes hex-encoded == 64 chars that decode back to 32 bytes.
	require.Len(t, tok, 2*tokenBytes)
	raw, err := hex.DecodeString(tok)
	require.NoError(t, err, "token must be valid hex")
	require.Len(t, raw, tokenBytes)
}

func TestGenerateTokenIsRandom(t *testing.T) {
	a, err := GenerateToken()
	require.NoError(t, err)
	b, err := GenerateToken()
	require.NoError(t, err)
	require.NotEqual(t, a, b, "two generated tokens must differ")
}

func TestTokenFromEnv(t *testing.T) {
	t.Setenv(TokenEnv, "")
	require.Empty(t, TokenFromEnv(), "unset/empty env yields empty token")

	// Surrounding whitespace (e.g. a trailing newline from `$(...)`) is trimmed.
	t.Setenv(TokenEnv, "  secret-value\n")
	require.Equal(t, "secret-value", TokenFromEnv())
}

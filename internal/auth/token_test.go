package auth

import (
	"encoding/hex"
	"os"
	"path/filepath"
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

func TestTokenFromFile(t *testing.T) {
	dir := t.TempDir()

	// Missing file yields empty, never an error.
	require.Empty(t, tokenFromFile(filepath.Join(dir, "nope.env")))
	require.Empty(t, tokenFromFile(""))

	// EnvironmentFile-style content: WARDEN_TOKEN=<value> among other lines.
	p := filepath.Join(dir, "token.env")
	require.NoError(t, os.WriteFile(p, []byte("# comment\nWARDEN_TOKEN=file-secret\n"), 0o600))
	require.Equal(t, "file-secret", tokenFromFile(p))
}

func TestResolveToken(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "token.env")
	require.NoError(t, os.WriteFile(p, []byte("WARDEN_TOKEN=from-file\n"), 0o600))
	// Point DefaultTokenFile() at the temp dir via HOME so the fallback resolves.
	t.Setenv("HOME", dir)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".warden"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".warden", "token.env"), []byte("WARDEN_TOKEN=from-home\n"), 0o600))

	// Env var wins when set.
	t.Setenv(TokenEnv, "from-env")
	require.Equal(t, "from-env", ResolveToken())

	// Falls back to the home token file when the env var is empty.
	t.Setenv(TokenEnv, "")
	require.Equal(t, "from-home", ResolveToken())
}

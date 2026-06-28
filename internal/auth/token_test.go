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

func TestWriteTokenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "token.env")

	require.NoError(t, WriteTokenFile(path, "rotated-secret"))

	// Parent dir created; file is chmod 600 and round-trips through the reader.
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "token file must not be group/world readable")
	require.Equal(t, "rotated-secret", tokenFromFile(path))

	// Overwriting replaces the value (atomic rename, no stale temp left behind).
	require.NoError(t, WriteTokenFile(path, "newer-secret"))
	require.Equal(t, "newer-secret", tokenFromFile(path))
	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	require.Len(t, entries, 1, "no leftover temp files")

	require.Error(t, WriteTokenFile("", "x"), "empty path is rejected")
}

func TestReadonlyTokenFromEnv(t *testing.T) {
	t.Setenv(ReadonlyTokenEnv, "")
	require.Empty(t, ReadonlyTokenFromEnv(), "unset/empty env yields empty token")

	t.Setenv(ReadonlyTokenEnv, "  ro-value\n")
	require.Equal(t, "ro-value", ReadonlyTokenFromEnv())
}

func TestTokenFromFileKeyBothTokens(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "token.env")
	// A single token.env may carry both the primary and the read-only token.
	require.NoError(t, os.WriteFile(p, []byte("WARDEN_TOKEN=full-secret\nWARDEN_READONLY_TOKEN=ro-secret\n"), 0o600))

	require.Equal(t, "full-secret", tokenFromFileKey(p, TokenEnv))
	require.Equal(t, "ro-secret", tokenFromFileKey(p, ReadonlyTokenEnv))
	require.Empty(t, tokenFromFileKey(p, "WARDEN_NOPE"))
}

func TestResolveReadonlyToken(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".warden"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".warden", "token.env"),
		[]byte("WARDEN_TOKEN=full-home\nWARDEN_READONLY_TOKEN=ro-home\n"), 0o600))

	// Env var wins when set.
	t.Setenv(ReadonlyTokenEnv, "ro-env")
	require.Equal(t, "ro-env", ResolveReadonlyToken())

	// Falls back to the home token file's read-only line when the env var is empty.
	t.Setenv(ReadonlyTokenEnv, "")
	require.Equal(t, "ro-home", ResolveReadonlyToken())
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

package cli

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/auth"
)

func TestTokenGenerateCommand(t *testing.T) {
	cmd := newTokenGenerateCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(nil)

	require.NoError(t, cmd.Execute())

	tok := strings.TrimSpace(out.String())
	require.Len(t, tok, 64, "generate must print a 32-byte hex token")
	_, err := hex.DecodeString(tok)
	require.NoError(t, err, "printed token must be valid hex")
}

// runToken executes a `token` subcommand, returning stdout and stderr separately
// (stdout carries the secret for piping; stderr carries human-facing notes).
func runToken(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	root := newRootCmd()
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs(append([]string{"token"}, args...))
	err := root.Execute()
	return out.String(), errBuf.String(), err
}

func TestTokenShowFromEnv(t *testing.T) {
	t.Setenv(auth.TokenEnv, "env-secret")
	out, errOut, err := runToken(t, "show")
	require.NoError(t, err)
	require.Equal(t, "env-secret\n", out)
	require.Contains(t, errOut, "source: $"+auth.TokenEnv)
}

func TestTokenShowFromFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(auth.TokenEnv, "")
	require.NoError(t, auth.WriteTokenFile(auth.DefaultTokenFile(), "file-secret"))

	out, errOut, err := runToken(t, "show")
	require.NoError(t, err)
	require.Equal(t, "file-secret\n", out)
	require.Contains(t, errOut, auth.DefaultTokenFile())
}

func TestTokenShowMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(auth.TokenEnv, "")
	_, _, err := runToken(t, "show")
	require.Error(t, err, "no token configured is an error")
}

func TestTokenShowReadonly(t *testing.T) {
	t.Setenv(auth.ReadonlyTokenEnv, "ro-env-secret")
	out, errOut, err := runToken(t, "show", "--readonly")
	require.NoError(t, err)
	require.Equal(t, "ro-env-secret\n", out)
	require.Contains(t, errOut, "source: $"+auth.ReadonlyTokenEnv)
}

func TestTokenShowReadonlyMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(auth.ReadonlyTokenEnv, "")
	_, _, err := runToken(t, "show", "--readonly")
	require.Error(t, err, "no read-only token configured is an error")
}

func TestTokenRotateNoRestart(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(auth.TokenEnv, "")
	require.NoError(t, auth.WriteTokenFile(auth.DefaultTokenFile(), "old-secret"))

	out, errOut, err := runToken(t, "rotate", "--no-restart")
	require.NoError(t, err)

	newTok := strings.TrimSpace(out)
	require.Len(t, newTok, 64, "rotated token is 64 hex chars")
	require.NotEqual(t, "old-secret", newTok)
	// The new secret was persisted and now resolves.
	require.Equal(t, newTok, auth.ResolveToken())
	require.Contains(t, errOut, "skipped restart")
}

func TestRewritePlistToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "warden.plist")
	plist := `<dict>
    <key>WARDEN_TOKEN</key><string>old-value</string>
    <key>RunAtLoad</key><true/>
</dict>`
	require.NoError(t, os.WriteFile(path, []byte(plist), 0o600))

	require.NoError(t, rewritePlistToken(path, "fresh-value"))
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(b), "<string>fresh-value</string>")
	require.NotContains(t, string(b), "old-value")
	require.Contains(t, string(b), "RunAtLoad", "rest of the plist is untouched")

	// Missing file and missing entry both error rather than silently succeeding.
	require.Error(t, rewritePlistToken(filepath.Join(dir, "nope.plist"), "x"))
	noTokenPath := filepath.Join(dir, "notoken.plist")
	require.NoError(t, os.WriteFile(noTokenPath, []byte("<dict></dict>"), 0o600))
	require.Error(t, rewritePlistToken(noTokenPath, "x"))
}

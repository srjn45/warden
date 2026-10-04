package updater

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVerifyChecksumOK(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	payload := []byte("warden-release-bytes")
	archive := filepath.Join(dir, "warden_1.2.3_linux_amd64.tar.gz")
	require.NoError(t, os.WriteFile(archive, payload, 0o644))

	sum := sha256.Sum256(payload)
	sums := filepath.Join(dir, "checksums.txt")
	require.NoError(t, os.WriteFile(sums, []byte(hex.EncodeToString(sum[:])+"  warden_1.2.3_linux_amd64.tar.gz\n"), 0o644))

	require.NoError(t, VerifyChecksum(archive, sums))
}

func TestVerifyChecksumMismatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	archive := filepath.Join(dir, "warden_1.2.3_linux_amd64.tar.gz")
	require.NoError(t, os.WriteFile(archive, []byte("real"), 0o644))
	sums := filepath.Join(dir, "checksums.txt")
	require.NoError(t, os.WriteFile(sums, []byte("deadbeef  warden_1.2.3_linux_amd64.tar.gz\n"), 0o644))

	err := VerifyChecksum(archive, sums)
	require.Error(t, err)
	require.Contains(t, err.Error(), "SHA256 mismatch")
}

func TestVerifyChecksumMissingEntry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	archive := filepath.Join(dir, "warden_1.2.3_linux_amd64.tar.gz")
	require.NoError(t, os.WriteFile(archive, []byte("x"), 0o644))
	sums := filepath.Join(dir, "checksums.txt")
	require.NoError(t, os.WriteFile(sums, []byte("abcd  other.tar.gz\n"), 0o644))

	err := VerifyChecksum(archive, sums)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no entry")
}

package updater

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// VerifyChecksum checks that file's SHA256 matches the entry for its basename
// in a goreleaser-style checksums.txt (hex digest, two spaces, filename).
func VerifyChecksum(file, checksumsPath string) error {
	base := filepath.Base(file)
	expected, err := lookupChecksum(checksumsPath, base)
	if err != nil {
		return err
	}
	actual, err := fileSHA256(file)
	if err != nil {
		return err
	}
	if !strings.EqualFold(expected, actual) {
		return fmt.Errorf("SHA256 mismatch for %s (expected %s, got %s)", base, expected, actual)
	}
	return nil
}

func lookupChecksum(path, base string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open checksums: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := fields[len(fields)-1]
		// goreleaser may prefix with "./"
		name = strings.TrimPrefix(name, "./")
		if name == base {
			return fields[0], nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("read checksums: %w", err)
	}
	return "", fmt.Errorf("checksums.txt has no entry for %s", base)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

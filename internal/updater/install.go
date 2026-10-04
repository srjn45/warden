package updater

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// stagedRelease is a downloaded+extracted release sitting under StagingDir.
type stagedRelease struct {
	Dir    string
	Binary string
}

func downloadAndVerify(opts Options, rel Release) (stagedRelease, error) {
	if err := os.MkdirAll(opts.StagingDir, 0o755); err != nil {
		return stagedRelease{}, fmt.Errorf("create staging dir: %w", err)
	}
	dir, err := os.MkdirTemp(opts.StagingDir, "warden-update-*")
	if err != nil {
		return stagedRelease{}, fmt.Errorf("create staging temp: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(dir)
		}
	}()

	archive := archiveName(rel, opts.GOOS, opts.GOARCH)
	archivePath := filepath.Join(dir, archive)
	sumsPath := filepath.Join(dir, "checksums.txt")

	if err := downloadFile(opts, assetURL(opts, rel, archive), archivePath); err != nil {
		return stagedRelease{}, fmt.Errorf("download archive: %w", err)
	}
	if err := downloadFile(opts, assetURL(opts, rel, "checksums.txt"), sumsPath); err != nil {
		return stagedRelease{}, fmt.Errorf("download checksums: %w", err)
	}
	if err := VerifyChecksum(archivePath, sumsPath); err != nil {
		return stagedRelease{}, err
	}

	bin, err := extractWardenBinary(archivePath, dir)
	if err != nil {
		return stagedRelease{}, err
	}

	cleanup = false
	return stagedRelease{Dir: dir, Binary: bin}, nil
}

func downloadFile(opts Options, url, dest string) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "warden-updater")
	resp, err := opts.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("%s returned %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return err
	}
	return nil
}

func extractWardenBinary(archivePath, destDir string) (string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("gunzip archive: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var found string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("read archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		base := filepath.Base(hdr.Name)
		if base != "warden" {
			continue
		}
		outPath := filepath.Join(destDir, "warden")
		out, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			return "", err
		}
		if err := out.Close(); err != nil {
			return "", err
		}
		found = outPath
		break
	}
	if found == "" {
		return "", fmt.Errorf("archive %s did not contain a warden binary", filepath.Base(archivePath))
	}
	return found, nil
}

// swapBinary replaces installBin with newBin atomically, returning the backup
// path of the previous binary (empty when there was none).
func swapBinary(installBin, newBin string) (backup string, err error) {
	if err := os.MkdirAll(filepath.Dir(installBin), 0o755); err != nil {
		return "", fmt.Errorf("create install dir: %w", err)
	}

	if _, err := os.Stat(installBin); err == nil {
		backup = installBin + ".bak"
		if err := copyFile(installBin, backup, 0o755); err != nil {
			return "", fmt.Errorf("backup current binary: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}

	tmp := installBin + ".tmp"
	if err := copyFile(newBin, tmp, 0o755); err != nil {
		return backup, fmt.Errorf("stage new binary: %w", err)
	}
	if err := os.Rename(tmp, installBin); err != nil {
		_ = os.Remove(tmp)
		return backup, fmt.Errorf("atomic install: %w", err)
	}

	// Keep the wd → warden symlink fresh (idempotent).
	alias := filepath.Join(filepath.Dir(installBin), "wd")
	_ = os.Remove(alias)
	_ = os.Symlink("warden", alias)

	return backup, nil
}

func restoreBackup(installBin, backup string) error {
	if backup == "" {
		return fmt.Errorf("no backup to restore")
	}
	tmp := installBin + ".tmp"
	if err := copyFile(backup, tmp, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmp, installBin); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Chmod(mode)
}

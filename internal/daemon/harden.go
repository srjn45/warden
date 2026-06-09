package daemon

import (
	"os"
	"path/filepath"
)

// hardenedSubdirs are warden's known data subdirectories under the data root.
// HardenDataDir tightens each that exists; new dirs are created 0o700 at their
// own creation sites (the stores; the prompts dir via `mkdir -m 700`).
var hardenedSubdirs = []string{"sessions", "closed", "context", "inbox", "pipelines", "prompts", "metrics"}

// HardenDataDir chmods the data root and each known subdirectory that already
// exists to 0o700 (owner-only), so pre-existing installs created at 0o755 are
// tightened. Missing dirs are skipped; the first real error is returned.
func HardenDataDir(dataDir string) error {
	if err := chmodDirIfExists(dataDir); err != nil {
		return err
	}
	for _, sub := range hardenedSubdirs {
		if err := chmodDirIfExists(filepath.Join(dataDir, sub)); err != nil {
			return err
		}
	}
	return nil
}

// chmodDirIfExists sets p to 0o700 when it exists and is a directory; a missing
// path is a no-op (not an error).
func chmodDirIfExists(p string) error {
	info, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return nil
	}
	return os.Chmod(p, 0o700)
}

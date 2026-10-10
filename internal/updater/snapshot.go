package updater

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/schema"
)

var (
	// ErrNoSnapshot is returned when no snapshot is found in backups/.
	ErrNoSnapshot = errors.New("no snapshot found")
)

// SnapshotManifest records the files and checksums captured in a snapshot.
type SnapshotManifest struct {
	CreatedAt     time.Time         `json:"created_at"`
	PreVersion    string            `json:"pre_version"`
	SchemaVersion int               `json:"schema_version"`
	Files         map[string]string `json:"files"` // relPath -> sha256 checksum
}

// SnapshotStores snapshots the stores in dataDir (excluding transcripts and backups)
// to <dataDir>/backups/pre-<ver>-<ts>/ using hard links / copy with checksum verification.
// It also copies installBin (if present) to <snapshotDir>/bin/warden.
func SnapshotStores(dataDir, preVersion, installBin string) (string, error) {
	if strings.TrimSpace(dataDir) == "" {
		return "", errors.New("snapshot: dataDir is empty")
	}

	preVer := stripV(preVersion)
	if preVer == "" {
		preVer = "unknown"
	}
	ts := time.Now().UTC().Format("20060102T150405Z")
	snapName := fmt.Sprintf("pre-v%s-%s", preVer, ts)
	snapDir := filepath.Join(dataDir, "backups", snapName)

	if err := os.MkdirAll(snapDir, 0o700); err != nil {
		return "", fmt.Errorf("snapshot: create backup dir: %w", err)
	}

	manifest := SnapshotManifest{
		CreatedAt:  time.Now().UTC(),
		PreVersion: preVer,
		Files:      make(map[string]string),
	}

	if l, err := schema.Load(dataDir); err == nil {
		manifest.SchemaVersion = l.SchemaVersion
	}

	// Walk dataDir and copy stores
	err := filepath.WalkDir(dataDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dataDir, path)
		if err != nil || rel == "." {
			return nil
		}

		// Normalize slash for path checks
		slashRel := filepath.ToSlash(rel)
		firstPart := strings.Split(slashRel, "/")[0]

		// Exclude backups, tmp, and transcripts
		if firstPart == "backups" || firstPart == "tmp" || firstPart == "transcripts" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Exclude flat transcript files under snapshots/ (snapshots-db/ is a store and is kept!)
		if firstPart == "snapshots" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Exclude locks and temporary files
		name := d.Name()
		if name == ".owner.lock" || name == "LOCK" || strings.HasSuffix(name, ".lock") ||
			strings.HasSuffix(name, ".tmp") || strings.Contains(name, ".restore.tmp") ||
			strings.HasSuffix(name, ".sock") {
			return nil
		}

		targetPath := filepath.Join(snapDir, rel)
		if d.IsDir() {
			return os.MkdirAll(targetPath, 0o700)
		}

		if !d.Type().IsRegular() {
			return nil
		}

		// Ensure parent dir exists
		if err := os.MkdirAll(filepath.Dir(targetPath), 0o700); err != nil {
			return err
		}

		sum, err := copyWithChecksum(path, targetPath)
		if err != nil {
			return fmt.Errorf("snapshot %s: %w", rel, err)
		}
		manifest.Files[slashRel] = sum
		return nil
	})
	if err != nil {
		_ = os.RemoveAll(snapDir)
		return "", err
	}

	// Also backup the installed binary if provided and exists
	if installBin != "" {
		if fi, err := os.Stat(installBin); err == nil && fi.Mode().IsRegular() {
			binBackupDir := filepath.Join(snapDir, "bin")
			_ = os.MkdirAll(binBackupDir, 0o755)
			targetBin := filepath.Join(binBackupDir, filepath.Base(installBin))
			binSum, err := copyWithChecksum(installBin, targetBin)
			if err == nil {
				manifest.Files["bin/"+filepath.Base(installBin)] = binSum
				_ = os.Chmod(targetBin, 0o755)
			}
		}
	}

	// Write manifest.json
	manifestRaw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		_ = os.RemoveAll(snapDir)
		return "", fmt.Errorf("snapshot: encode manifest: %w", err)
	}
	manifestPath := filepath.Join(snapDir, "manifest.json")
	if err := os.WriteFile(manifestPath, append(manifestRaw, '\n'), 0o600); err != nil {
		_ = os.RemoveAll(snapDir)
		return "", fmt.Errorf("snapshot: write manifest: %w", err)
	}

	return snapDir, nil
}

// RestoreSnapshot restores stores from snapDir into dataDir.
func RestoreSnapshot(snapDir, dataDir string) error {
	if fi, err := os.Stat(snapDir); err != nil || !fi.IsDir() {
		return fmt.Errorf("restore snapshot: %s does not exist or is not a directory", snapDir)
	}

	err := filepath.WalkDir(snapDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(snapDir, path)
		if err != nil || rel == "." {
			return nil
		}

		slashRel := filepath.ToSlash(rel)
		firstPart := strings.Split(slashRel, "/")[0]

		// Skip manifest and backed up binary
		if slashRel == "manifest.json" || firstPart == "bin" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		targetPath := filepath.Join(dataDir, rel)
		if d.IsDir() {
			return os.MkdirAll(targetPath, 0o700)
		}

		if !d.Type().IsRegular() {
			return nil
		}

		if err := os.MkdirAll(filepath.Dir(targetPath), 0o700); err != nil {
			return err
		}

		tmpTarget := targetPath + ".restore.tmp"
		if _, err := copyWithChecksum(path, tmpTarget); err != nil {
			_ = os.Remove(tmpTarget)
			return err
		}
		if err := os.Rename(tmpTarget, targetPath); err != nil {
			_ = os.Remove(tmpTarget)
			return err
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("restore snapshot: %w", err)
	}

	// Ensure restored schema ledger has InProgress cleared so restored daemon boots cleanly
	if l, err := schema.Load(dataDir); err == nil && l.InProgress != nil {
		l.InProgress = nil
		_ = schema.Save(dataDir, l)
	}

	return nil
}

// PruneSnapshots removes old snapshots according to retention policy:
// keep at least keepMin newest snapshots, OR any snapshot within retention duration,
// whichever retains more.
func PruneSnapshots(dataDir string, clock Clock, keepMin int, retention time.Duration) ([]string, error) {
	if keepMin <= 0 {
		keepMin = 2
	}
	if retention <= 0 {
		retention = 14 * 24 * time.Hour
	}
	if clock == nil {
		clock = realClock{}
	}

	backupsDir := filepath.Join(dataDir, "backups")
	entries, err := os.ReadDir(backupsDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("prune snapshots: read backups dir: %w", err)
	}

	type snapInfo struct {
		path string
		t    time.Time
	}
	var snaps []snapInfo
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "pre-") {
			continue
		}
		p := filepath.Join(backupsDir, e.Name())
		t := parseSnapshotTime(e.Name())
		if t.IsZero() {
			if fi, err := e.Info(); err == nil {
				t = fi.ModTime().UTC()
			}
		}
		snaps = append(snaps, snapInfo{path: p, t: t})
	}

	// Sort newest first
	sort.Slice(snaps, func(i, j int) bool {
		return snaps[i].t.After(snaps[j].t)
	})

	now := clock.Now().UTC()
	var pruned []string
	for i, s := range snaps {
		// Keep if among the newest keepMin
		if i < keepMin {
			continue
		}
		// Keep if within retention window
		if !s.t.IsZero() && now.Sub(s.t) <= retention {
			continue
		}
		// Otherwise, prune
		if err := os.RemoveAll(s.path); err == nil {
			pruned = append(pruned, s.path)
		}
	}

	return pruned, nil
}

// LatestSnapshot finds the newest pre-* snapshot in <dataDir>/backups.
func LatestSnapshot(dataDir string) (string, error) {
	backupsDir := filepath.Join(dataDir, "backups")
	entries, err := os.ReadDir(backupsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", ErrNoSnapshot
		}
		return "", err
	}

	type snapInfo struct {
		path string
		t    time.Time
	}
	var snaps []snapInfo
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "pre-") {
			continue
		}
		p := filepath.Join(backupsDir, e.Name())
		t := parseSnapshotTime(e.Name())
		if t.IsZero() {
			if fi, err := e.Info(); err == nil {
				t = fi.ModTime().UTC()
			}
		}
		snaps = append(snaps, snapInfo{path: p, t: t})
	}

	if len(snaps) == 0 {
		return "", ErrNoSnapshot
	}

	sort.Slice(snaps, func(i, j int) bool {
		return snaps[i].t.After(snaps[j].t)
	})

	return snaps[0].path, nil
}

func parseSnapshotTime(name string) time.Time {
	// Format: pre-v<ver>-<ts> or pre-<ver>-<ts> where ts is 20060102T150405Z
	parts := strings.Split(name, "-")
	if len(parts) >= 3 {
		last := parts[len(parts)-1]
		if t, err := time.Parse("20060102T150405Z", last); err == nil {
			return t
		}
	}
	return time.Time{}
}

// copyWithChecksum copies src to dst with checksum verification.
func copyWithChecksum(src, dst string) (string, error) {
	_ = os.Remove(dst)

	srcF, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer srcF.Close()

	fi, err := srcF.Stat()
	if err != nil {
		return "", err
	}

	dstF, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fi.Mode().Perm())
	if err != nil {
		return "", err
	}
	defer dstF.Close()

	hasher := sha256.New()
	writer := io.MultiWriter(dstF, hasher)
	if _, err := io.Copy(writer, srcF); err != nil {
		return "", err
	}
	if err := dstF.Sync(); err != nil {
		return "", err
	}

	return hex.EncodeToString(hasher.Sum(nil)), nil
}

package migrate

// A read-only preflight of the schema-1 -> schema-2 mapping.
//
// The strict store preflight proves each legacy store is intact; it cannot see
// whether the stores *map* (a pipeline reference with no pipeline, a legacy
// file that disagrees with its database record). Those were only found by the
// staging export, after the updater had already stopped the daemon. This runs
// the same mapping on a private point-in-time copy of the migrated sources, so
// nothing in the data dir is opened, locked or written, and so it is safe while
// a daemon owns the data.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// PreflightStagingMapping returns the findings the staging export would
// produce for dataDir, without writing to dataDir. Blocking findings mean the
// cutover would refuse. The error is non-nil only when the preview itself could
// not be run (copy or read failure), never because the data is blocked.
func PreflightStagingMapping(ctx context.Context, dataDir string) ([]Finding, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	tmp, err := os.MkdirTemp("", "warden-mapping-preflight-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	if err := copyMigratedSources(ctx, dataDir, tmp); err != nil {
		return nil, fmt.Errorf("copy sources for mapping preflight: %w", err)
	}
	set, err := buildStagingSet(ctx, tmp)
	if err != nil {
		return nil, err
	}
	// The preview data dir is a private temp path; never leak it into messages.
	out := make([]Finding, 0, len(set.findings))
	for _, f := range set.findings {
		f.Message = strings.ReplaceAll(f.Message, tmp, "<data-dir>")
		f.Details = strings.ReplaceAll(f.Details, tmp, "<data-dir>")
		out = append(out, f)
	}
	return out, nil
}

// copyMigratedSources copies every migrated source (writable, regular files
// only) into dst so the legacy stores can be opened without touching the
// originals. Anything that is not a regular file is skipped: the strict
// preflight already reports symlinks and specials.
func copyMigratedSources(ctx context.Context, dataDir, dst string) error {
	for _, s := range schema2Sources {
		if s.Disposition != DispositionMigrated {
			continue
		}
		root := filepath.Join(dataDir, filepath.FromSlash(s.Path))
		fi, err := os.Lstat(root)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			if fi.Mode().IsRegular() {
				if err := copyPlain(root, filepath.Join(dst, filepath.FromSlash(s.Path))); err != nil {
					return err
				}
			}
			continue
		}
		err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if d.IsDir() || !d.Type().IsRegular() || (s.SkipSuffix != "" && strings.HasSuffix(d.Name(), s.SkipSuffix)) {
				return nil
			}
			rel, err := filepath.Rel(dataDir, p)
			if err != nil {
				return err
			}
			return copyPlain(p, filepath.Join(dst, rel))
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func copyPlain(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

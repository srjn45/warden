package factoryreset

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// backupTree copies dataDir to dst. dst must not already exist and must not be
// inside dataDir. Mirrors the repair command's backup discipline.
func backupTree(src, dst string) error {
	absSrc, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	absDst, err := filepath.Abs(dst)
	if err != nil {
		return err
	}
	if rel, err := filepath.Rel(absSrc, absDst); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("backup destination must be outside the data directory")
	}
	if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("backup destination already exists")
	}
	type dirTime struct {
		path string
		when time.Time
	}
	var dirTimes []dirTime
	err = filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if err := os.Mkdir(target, info.Mode().Perm()); err != nil {
				return err
			}
			if err := os.Chmod(target, info.Mode().Perm()); err != nil {
				return err
			}
			dirTimes = append(dirTimes, dirTime{target, info.ModTime()})
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported backup file type at %s (%s)", path, info.Mode().Type())
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			_ = in.Close()
			return err
		}
		_, cpErr := io.Copy(out, in)
		inCloseErr := in.Close()
		syncErr := out.Sync()
		outCloseErr := out.Close()
		if cpErr != nil {
			return cpErr
		}
		if inCloseErr != nil {
			return inCloseErr
		}
		if syncErr != nil {
			return syncErr
		}
		if outCloseErr != nil {
			return outCloseErr
		}
		if err := os.Chmod(target, info.Mode().Perm()); err != nil {
			return err
		}
		if err := os.Chtimes(target, info.ModTime(), info.ModTime()); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, dt := range dirTimes {
		if err := os.Chtimes(dt.path, dt.when, dt.when); err != nil {
			return err
		}
	}
	return nil
}

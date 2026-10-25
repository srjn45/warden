package ptyholder

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

var idRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ValidID reports whether id is safe to use as a file name under the holder
// root.
func ValidID(id string) bool { return idRe.MatchString(id) }

func metaPath(root, id string) string  { return filepath.Join(root, id+".json") }
func credPath(root, id string) string  { return filepath.Join(root, id+".cred") }
func specPath(root, id string) string  { return filepath.Join(root, id+".spec") }
func auditPath(root, id string) string { return filepath.Join(root, id+".audit") }

// maxSocket is below sun_path (104 on macOS, 108 on Linux) with margin.
const maxSocket = 96

// SocketPath returns the socket for id: under root when the path fits a Unix
// socket address, else under a short per-user directory in the OS temp dir.
func SocketPath(root, id string) string {
	p := filepath.Join(root, id+".sock")
	if len(p) <= maxSocket {
		return p
	}
	sum := sha1.Sum([]byte(root + "\x00" + id))
	return filepath.Join(os.TempDir(), fmt.Sprintf("wph-%d", os.Getuid()), hex.EncodeToString(sum[:8])+".sock")
}

func writeMeta(root string, m Meta) error {
	return writeFileAtomic(metaPath(root, m.ID), mustJSON(m))
}

// Audit returns the refusal log of id (one line per refused connection).
func Audit(root, id string) string {
	b, _ := os.ReadFile(auditPath(root, id))
	return string(b)
}

func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// KillAll releases only authenticated holders recorded in root. It never sweeps
// tmux, cgroups or PIDs from unverified metadata. Failed records are retained
// for diagnosis/retry. The daemon never calls this on its restart path.
func KillAll(root string) error {
	ents, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var failures []error
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, e := range ents {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		id := e.Name()[:len(e.Name())-len(".json")]
		h, err := Open(root, id)
		if errors.Is(err, ErrGone) {
			continue // record forgotten concurrently (e.g. by its own release)
		}
		if err == nil {
			err = h.Release(ctx)
		}
		if errors.Is(err, ErrGone) && h != nil && recordedGone(ctx, h) {
			err = nil
		}
		if err != nil {
			cleanupFailures.Add(1)
			failures = append(failures, fmt.Errorf("ptyholder: cleanup %s: %w", id, err))
			continue
		}
		Forget(root, id)
	}
	return errors.Join(failures...)
}

// recordedGone reports, after a bounded wait, whether neither recorded
// incarnation of an unreachable holder remains. Only then may its record be
// forgotten; it never signals a replacement PID or treats an authentication
// failure as permission. A record without metadata names no process at all.
func recordedGone(ctx context.Context, h *Handle) bool {
	m, ok := h.Meta()
	if !ok {
		return true
	}
	for end := time.Now().Add(time.Second); time.Now().Before(end) && ctx.Err() == nil; {
		hs, he := StartTime(m.HolderPID)
		cs, ce := StartTime(m.ChildPID)
		if (he != nil || m.HolderStart != 0 && hs != m.HolderStart) && (ce != nil || m.ChildStart != 0 && cs != m.ChildStart) {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

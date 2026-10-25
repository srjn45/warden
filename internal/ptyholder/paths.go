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
		if errors.Is(err, ErrGone) && h != nil {
			if gone, gerr := recordedGone(ctx, h); gerr != nil {
				err = fmt.Errorf("%w; ownership unresolved: %w", err, gerr)
			} else if gone {
				err = nil
			}
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

// startTimeOf is StartTime, swappable so tests can inject inspection errors.
var startTimeOf = StartTime

// incarnationGone reports whether the recorded (pid, start) incarnation no
// longer exists. Only os.ErrNotExist or a verified start-time mismatch counts
// as gone; any other inspection error (EACCES, a transient or malformed /proc
// read) is unresolved and returned, so ownership evidence is kept.
func incarnationGone(pid int, start uint64) (bool, error) {
	if pid <= 0 {
		return true, nil // no process was recorded
	}
	st, err := startTimeOf(pid)
	switch {
	case err == nil:
		return start != 0 && st != start, nil
	case errors.Is(err, os.ErrNotExist):
		return true, nil
	default:
		return false, fmt.Errorf("ptyholder: inspect pid %d: %w", pid, err)
	}
}

// recordedGone reports, after a bounded wait, whether neither recorded
// incarnation of an unreachable holder remains. Only then may its record be
// forgotten; it never signals a replacement PID or treats an authentication
// failure as permission. A record without metadata names no process at all.
// An unresolved inspection error is returned so the caller reports a cleanup
// failure and retains the record.
func recordedGone(ctx context.Context, h *Handle) (bool, error) {
	m, ok := h.Meta()
	if !ok {
		return true, nil
	}
	for end := time.Now().Add(time.Second); ; {
		hg, herr := incarnationGone(m.HolderPID, m.HolderStart)
		cg, cerr := incarnationGone(m.ChildPID, m.ChildStart)
		if err := errors.Join(herr, cerr); err != nil {
			return false, err
		}
		if hg && cg {
			return true, nil
		}
		if !time.Now().Before(end) || ctx.Err() != nil {
			return false, nil
		}
		time.Sleep(5 * time.Millisecond)
	}
}

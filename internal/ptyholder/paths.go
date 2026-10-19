package ptyholder

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
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

// KillAll force-ends every holder recorded under root and the process group it
// hosts, then forgets their state. It is the teardown for tests and for
// uninstall/factory-reset tooling — the daemon never calls it on a restart
// path (a restart must leave holders alone).
func KillAll(root string) {
	ents, _ := os.ReadDir(root)
	for _, e := range ents {
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		var m Meta
		if b, err := os.ReadFile(filepath.Join(root, e.Name())); err == nil && json.Unmarshal(b, &m) == nil {
			// Only a process that is still the recorded incarnation (same start
			// time) is signalled: a recycled pid is somebody else's.
			if st, err := StartTime(m.ChildPID); m.ChildPID > 0 && err == nil && st == m.ChildStart && m.ChildPID != os.Getpid() {
				_ = syscall.Kill(-m.ChildPID, syscall.SIGKILL)
				_ = syscall.Kill(m.ChildPID, syscall.SIGKILL)
			}
			if st, err := StartTime(m.HolderPID); m.HolderPID > 0 && err == nil && st == m.HolderStart && m.HolderPID != os.Getpid() {
				_ = syscall.Kill(m.HolderPID, syscall.SIGKILL)
			}
			Forget(root, m.ID)
		}
	}
}

// Package ownerlock is the single process-level ownership lock for a warden
// data directory (#841). One flock on <data>/.warden-owner.lock spans every
// store under the directory: the daemon holds it for its whole life, a direct
// CLI open holds it for the command, and offline repair refuses while a daemon
// holds it. The kernel drops the flock on process death, so a crash never
// leaves a stuck lock; the JSON metadata in the file is advisory and is only
// trusted while the flock is actually held by someone else.
package ownerlock

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FileName is the lock file inside the data directory.
const FileName = ".warden-owner.lock"

// Kinds of owner.
const (
	KindDaemon = "daemon"
	KindCLI    = "cli"
)

// Launch modes (best effort).
const (
	LaunchSystemd = "systemd"
	LaunchManual  = "manual"
)

// ErrOwned is wrapped by every *OwnedError.
var ErrOwned = errors.New("warden data directory is owned by another process")

// Owner is the advisory metadata the holder writes into the lock file.
type Owner struct {
	PID     int       `json:"pid"`
	Started time.Time `json:"started"`
	Version string    `json:"version,omitempty"`
	Kind    string    `json:"kind"`
	Launch  string    `json:"launch,omitempty"`
	Addr    string    `json:"addr,omitempty"`
	Command string    `json:"command,omitempty"`
}

// OwnedError reports a live holder with guidance.
type OwnedError struct {
	Dir   string
	Owner *Owner // nil when the metadata is unreadable
}

func (e *OwnedError) Unwrap() error { return ErrOwned }

func (e *OwnedError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "warden data directory %s is already owned", e.Dir)
	if o := e.Owner; o != nil {
		fmt.Fprintf(&b, " by %s pid %d", o.Kind, o.PID)
		if o.Launch != "" {
			fmt.Fprintf(&b, " (launched: %s)", o.Launch)
		}
		if o.Version != "" {
			fmt.Fprintf(&b, ", version %s", o.Version)
		}
		if !o.Started.IsZero() {
			fmt.Fprintf(&b, ", started %s", o.Started.Format(time.RFC3339))
		}
		if o.Addr != "" {
			fmt.Fprintf(&b, ", address %s", o.Addr)
		}
	}
	b.WriteString("\nnext step: " + e.NextStep())
	return b.String()
}

// NextStep is the safe operator action; it never suggests deleting the lock file.
func (e *OwnedError) NextStep() string {
	o := e.Owner
	switch {
	case o != nil && o.Kind == KindCLI:
		return "a warden CLI command (pid " + fmt.Sprint(o.PID) + ") is using the data directory; wait for it to finish and retry"
	case o != nil && o.Launch == LaunchSystemd:
		return "the systemd service owns this data directory: check it with `systemctl --user status warden`; stop it with `systemctl --user stop warden` only if you intend to replace it (do not run a second `warden daemon`, and do not delete the lock file)"
	case o != nil && o.Launch == LaunchManual:
		return fmt.Sprintf("a manually launched `warden daemon` (pid %d) owns this data directory; stop that process, or use it instead of starting another (do not delete the lock file). If you meant to use the systemd service, stop the manual daemon first and run `systemctl --user status warden`", o.PID)
	}
	return "stop the running warden daemon (`systemctl --user status warden`, or the manual `warden daemon` process) that uses this data directory, then retry; do not delete the lock file"
}

// Lock is a held ownership lock.
type Lock struct {
	f    *file
	path string
}

// Info describes the options for Acquire.
type Info struct {
	Kind, Version, Addr, Command string
}

// LaunchMode detects how this process was launched: systemd sets INVOCATION_ID
// for every unit it starts.
func LaunchMode() string {
	if os.Getenv("INVOCATION_ID") != "" || os.Getenv("JOURNAL_STREAM") != "" {
		return LaunchSystemd
	}
	return LaunchManual
}

// Acquire takes the exclusive lock for dataDir without blocking and records
// owner metadata. A live holder yields *OwnedError.
func Acquire(dataDir string, info Info) (*Lock, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	canon := dataDir
	if abs, err := filepath.Abs(dataDir); err == nil {
		if r, err := filepath.EvalSymlinks(abs); err == nil {
			canon = r
		}
	}
	path := filepath.Join(canon, FileName)
	f, err := openLocked(path)
	if err != nil {
		if errors.Is(err, errHeld) {
			return nil, &OwnedError{Dir: canon, Owner: readOwner(path)}
		}
		return nil, err
	}
	o := Owner{PID: os.Getpid(), Started: time.Now().UTC(), Version: info.Version, Kind: info.Kind,
		Launch: LaunchMode(), Addr: info.Addr, Command: info.Command}
	b, _ := json.Marshal(o)
	if err := f.writeAll(b); err != nil {
		_ = f.close()
		return nil, err
	}
	return &Lock{f: f, path: path}, nil
}

// SetAddr updates the recorded listen address once it is known.
func (l *Lock) SetAddr(addr string) {
	if l == nil {
		return
	}
	o := readOwner(l.path)
	if o == nil || o.PID != os.Getpid() {
		return
	}
	o.Addr = addr
	b, _ := json.Marshal(o)
	_ = l.f.writeAll(b)
}

// Release drops the lock (also dropped by the kernel on process death).
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := l.f.close()
	l.f = nil
	return err
}

// Probe reports the current owner of dataDir, or nil when it is unowned. It
// never blocks and never leaves a lock behind. A lock held by this very
// process is reported as unowned (self).
func Probe(dataDir string) (*OwnedError, error) {
	path := filepath.Join(dataDir, FileName)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	f, err := openLocked(path)
	if err == nil {
		_ = f.close()
		return nil, nil
	}
	if !errors.Is(err, errHeld) {
		return nil, err
	}
	o := readOwner(path)
	if o != nil && o.PID == os.Getpid() {
		return nil, nil
	}
	return &OwnedError{Dir: dataDir, Owner: o}, nil
}

func readOwner(path string) *Owner {
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return nil
	}
	var o Owner
	if json.Unmarshal(b, &o) != nil || o.PID == 0 {
		return nil
	}
	return &o
}

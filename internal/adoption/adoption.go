// Package adoption is the append-only evidence log for the schema-2 cutover
// and the legacy-store retirement gate (docs/specs/2026-10-18-legacy-store-retirement-evidence.md).
//
// Events are local facts about one data dir: nothing is sent anywhere. Recording
// is best-effort and never changes migration behaviour; a failure to write
// evidence is reported to the caller but must not fail a migration.
package adoption

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// FileName is the evidence log inside the data dir. The leading dot keeps it
// an "external" top-level entry for the schema-2 source inventory.
const FileName = ".adoption-evidence.jsonl"

// Kind names an evidence event.
type Kind string

const (
	// MigrationApplied: a migration committed (ledger advanced).
	MigrationApplied Kind = "migration_applied"
	// MigrationFailed: a migration run, resume or verify returned an error after
	// it had started changing state. A refusal before any live change is NOT
	// logged: it must leave the data dir byte-identical.
	MigrationFailed Kind = "migration_failed"
	// RecoveryRestored: an interrupted migration was restored from its backup.
	RecoveryRestored Kind = "recovery_restored"
	// RecoveryFailed: a restore was attempted and failed.
	RecoveryFailed Kind = "recovery_failed"
	// RecoveryResumed: an interrupted migration was resumed and finished.
	RecoveryResumed Kind = "recovery_resumed"
	// UpdaterRollback: wd update rolled the data dir back to a pre-update snapshot.
	UpdaterRollback Kind = "updater_rollback"
	// LegacyOpenRefused: something tried to open the legacy stores of a
	// schema-2 data dir and was refused.
	LegacyOpenRefused Kind = "legacy_open_refused"
	// RuntimeBoot: the daemon booted on warden-db (adoption heartbeat).
	RuntimeBoot Kind = "runtime_boot"
)

// Event is one evidence record. Details never carry record values, only ids,
// counts and bounded error text.
type Event struct {
	At            time.Time `json:"at"`
	Kind          Kind      `json:"kind"`
	Migration     string    `json:"migration,omitempty"`
	BinaryVersion string    `json:"binary_version,omitempty"`
	FromSchema    int       `json:"from_schema,omitempty"`
	ToSchema      int       `json:"to_schema,omitempty"`
	Backup        string    `json:"backup,omitempty"`
	Step          string    `json:"step,omitempty"`
	Error         string    `json:"error,omitempty"`
}

const maxErrorLen = 300

var mu sync.Mutex

// Path is the evidence log's path.
func Path(dataDir string) string { return filepath.Join(dataDir, FileName) }

// Record appends one event. A zero At is stamped with the current time.
func Record(dataDir string, e Event) error {
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	if len(e.Error) > maxErrorLen {
		e.Error = e.Error[:maxErrorLen]
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()
	f, err := os.OpenFile(Path(dataDir), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("adoption: open evidence log: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("adoption: append evidence: %w", err)
	}
	return f.Sync()
}

// Log is a decoded evidence log. Malformed lines are counted, never fatal.
type Log struct {
	Events    []Event
	Malformed int
}

// Read decodes the evidence log. A missing log is an empty one.
func Read(dataDir string) (*Log, error) {
	f, err := os.Open(Path(dataDir))
	if errors.Is(err, fs.ErrNotExist) {
		return &Log{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := &Log{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil || e.Kind == "" {
			out.Malformed++
			continue
		}
		out.Events = append(out.Events, e)
	}
	return out, sc.Err()
}

// Count returns how many events of kind k occurred at or after since.
func (l *Log) Count(k Kind, since time.Time) int {
	n := 0
	for _, e := range l.Events {
		if e.Kind == k && !e.At.Before(since) {
			n++
		}
	}
	return n
}

// Last returns the most recent event of kind k.
func (l *Log) Last(k Kind) (Event, bool) {
	var best Event
	found := false
	for _, e := range l.Events {
		if e.Kind == k && (!found || !e.At.Before(best.At)) {
			best, found = e, true
		}
	}
	return best, found
}

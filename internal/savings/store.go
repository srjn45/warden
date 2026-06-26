package savings

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Store is the append-only savings ledger: one JSON object per line in
// <dir>/ledger.jsonl. Append-only fits the access pattern — every emit is a
// single write, every report is a full scan over a window — and keeps the
// daemon the single writer without the read-modify-write a JSON-array file would
// need. A mutex serializes the daemon's concurrent emit callers; a corrupt line
// (partial write after a crash) is skipped on read rather than failing the whole
// report. Mirrors the audit writer's discipline (internal/audit).
type Store struct {
	mu   sync.Mutex
	path string
}

// NewStore creates the savings dir (0700) and returns a ledger writer/reader
// rooted at <dir>/ledger.jsonl.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{path: filepath.Join(dir, "ledger.jsonl")}, nil
}

// Record appends one event. It is the daemon-side sink the lifecycle emit points
// call through; the caller has already converted bytes→tokens via EstimateTokens.
// A zero-saving event is still recorded (it documents that the feature ran), but
// the caller may skip emitting one if it prefers. Errors are returned so the
// caller can log-and-continue — recording must never break the action it measures.
func (s *Store) Record(ev Event) error {
	if ev.TS.IsZero() {
		ev.TS = time.Now().UTC()
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	return nil
}

// Events returns every recorded event with TS >= since (since.IsZero ⇒ all),
// oldest-first. A missing ledger is not an error — it yields an empty slice, so
// `wd savings` on a fresh install reads as "nothing saved yet" rather than failing.
// Malformed lines are skipped (best-effort, mirrors the gauge's transcript scan).
func (s *Store) Events(since time.Time) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.Open(s.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []Event
	sc := bufio.NewScanner(f)
	// Events are tiny, but a line cap guards against a wedged write; match the
	// transcript scanner's generous buffer rather than the 64 KiB default.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			continue // skip a partial/corrupt line rather than abort the report
		}
		if !since.IsZero() && ev.TS.Before(since) {
			continue
		}
		out = append(out, ev)
	}
	return out, sc.Err()
}

// Summary reads the events since `since` and aggregates them. Convenience wrapper
// the daemon's GET /savings handler uses so the HTTP layer stays a thin shell.
func (s *Store) Summary(since time.Time) (Summary, error) {
	evs, err := s.Events(since)
	if err != nil {
		return Summary{}, err
	}
	return Summarize(evs, since), nil
}

package spend

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// Store persists each agent's cumulative billed spend (input+output tokens) in a
// single JSON object, <dir>/spend.json, keyed by session id. Unlike the savings
// ledger (append-only — every emit is a distinct historical event), spend is a
// running gauge per session: each update REPLACES the session's figure with the
// latest cumulative reading parsed from its transcript, so the file stays one
// line per live agent rather than growing per tick. A mutex serializes the
// daemon's concurrent updaters; reads tolerate a missing/corrupt file by
// degrading to an empty map (fail-open), so spend is best-effort and never breaks
// the report it feeds.
type Store struct {
	mu   sync.Mutex
	path string
}

// NewStore creates the spend dir (0700) and returns a tracker rooted at
// <dir>/spend.json.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{path: filepath.Join(dir, "spend.json")}, nil
}

// Record sets the cumulative spend for one session to tokens, but only ever
// raises it (max with the stored value). The monotone guard makes Record robust
// to a transcript that was rotated or partially read on a given tick yielding a
// momentarily smaller cumulative figure — spend only grows, so a lower reading is
// treated as a transient under-count, not a correction downward. A non-positive
// tokens (e.g. an unmeasurable read the caller passed through) and an unknown
// session that already holds a higher figure are both no-ops.
func (s *Store) Record(sessionID string, tokens int) error {
	if sessionID == "" || tokens <= 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return err
	}
	if tokens <= m[sessionID] {
		return nil // never lower an established figure (rotation/partial-read guard)
	}
	m[sessionID] = tokens
	return s.save(m)
}

// Total returns the summed cumulative spend across all known sessions — the
// measured-spend denominator. A missing file is not an error (0, nil): a fresh
// install reads as "no spend observed yet", which the report renders as a
// graceful fallback rather than a failure.
func (s *Store) Total() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return 0, err
	}
	var total int
	for _, v := range m {
		total += v
	}
	return total, nil
}

// load reads the spend map. A missing file yields an empty map (not an error); a
// corrupt file likewise degrades to empty rather than failing every future
// update — spend is a best-effort gauge, so a single bad write must not wedge it.
func (s *Store) load() (map[string]int, error) {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]int{}, nil
		}
		return nil, err
	}
	m := map[string]int{}
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]int{}, nil // tolerate a corrupt file: start fresh
	}
	return m, nil
}

// save writes the spend map atomically (temp file + rename) at 0600 so a crash
// mid-write can't truncate the running totals to garbage.
func (s *Store) save(m map[string]int) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

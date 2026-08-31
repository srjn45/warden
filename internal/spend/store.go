package spend

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Entry is one session's running spend gauge: its cumulative billed input and
// output tokens, plus the backend/model it ran (for pricing) and repo + first-seen
// day (for the per-repo / per-day rollups). Input/Output are stored separately —
// not pre-summed — because output bills at a different rate than input, so the
// dollar figure can only be computed from the two axes kept apart.
type Entry struct {
	Input   int    `json:"input"`
	Output  int    `json:"output"`
	Backend string `json:"backend,omitempty"`
	Model   string `json:"model,omitempty"`
	Repo    string `json:"repo,omitempty"`
	Day     string `json:"day,omitempty"` // YYYY-MM-DD the session was first recorded (local)
}

// SessionSpend is one session's spend with its id attached — what Sessions()
// returns for the rollup/report layer to price and aggregate.
type SessionSpend struct {
	Session string
	Entry
}

// Store persists each agent's cumulative billed spend in a single JSON object,
// <dir>/spend.json, keyed by session id. Unlike the savings ledger (append-only),
// spend is a running gauge per session: each update RAISES the session's figure
// to the latest cumulative reading parsed from its transcript, so the file stays
// one record per live agent rather than growing per tick. A mutex serializes the
// daemon's concurrent updaters; reads tolerate a missing/corrupt file by degrading
// to an empty map (fail-open), so spend is best-effort and never breaks the report
// or budget gate it feeds.
type Store struct {
	mu   sync.Mutex
	path string
	now  func() time.Time
}

// NewStore creates the spend dir (0700) and returns a tracker rooted at
// <dir>/spend.json.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{path: filepath.Join(dir, "spend.json"), now: time.Now}, nil
}

// Record raises the cumulative spend for one session to the latest reading. Each
// of input/output is monotone (max with the stored value), so a transcript that
// was rotated or partially read on a given tick — yielding a momentarily smaller
// cumulative figure — is treated as a transient under-count, not a correction
// downward. The backend + model + repo are stamped (and refreshed if they were
// unknown), and the first-seen day is recorded once so per-day rollups have a
// stable bucket. A session with no tokens on either axis is a no-op.
func (s *Store) Record(sessionID, backend, model, repo string, inputTokens, outputTokens int) error {
	if sessionID == "" || (inputTokens <= 0 && outputTokens <= 0) {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return err
	}
	e := m[sessionID]
	changed := false
	if backend != "" && e.Backend != backend {
		e.Backend = backend
		changed = true
	}
	if inputTokens > e.Input {
		e.Input = inputTokens
		changed = true
	}
	if outputTokens > e.Output {
		e.Output = outputTokens
		changed = true
	}
	if model != "" && e.Model != model {
		e.Model = model
		changed = true
	}
	if repo != "" && e.Repo != repo {
		e.Repo = repo
		changed = true
	}
	if e.Day == "" {
		e.Day = s.now().Format("2006-01-02")
		changed = true
	}
	if !changed {
		return nil // nothing higher to persist (rotation/partial-read guard)
	}
	m[sessionID] = e
	return s.save(m)
}

// Total returns the summed cumulative input+output tokens across all sessions —
// the measured-spend denominator the savings report consumes. A missing file is
// not an error (0, nil): a fresh install reads as "no spend observed yet".
func (s *Store) Total() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return 0, err
	}
	var total int
	for _, e := range m {
		total += e.Input + e.Output
	}
	return total, nil
}

// Sessions returns every recorded session's spend, id attached, sorted by id for
// a deterministic report. A missing file yields an empty slice (fail-open).
func (s *Store) Sessions() ([]SessionSpend, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return nil, err
	}
	out := make([]SessionSpend, 0, len(m))
	for id, e := range m {
		out = append(out, SessionSpend{Session: id, Entry: e})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Session < out[j].Session })
	return out, nil
}

// load reads the spend map. A missing file yields an empty map (not an error); a
// corrupt file (or an older on-disk shape) likewise degrades to empty rather than
// failing every future update — spend is a best-effort gauge, so a single bad
// write, or a format upgrade, must not wedge it.
func (s *Store) load() (map[string]Entry, error) {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]Entry{}, nil
		}
		return nil, err
	}
	m := map[string]Entry{}
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]Entry{}, nil // tolerate a corrupt/legacy file: start fresh
	}
	return m, nil
}

// save writes the spend map atomically (temp file + rename) at 0600 so a crash
// mid-write can't truncate the running totals to garbage.
func (s *Store) save(m map[string]Entry) error {
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

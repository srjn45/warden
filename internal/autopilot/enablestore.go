package autopilot

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// EnableStore persists the set of repositories with autopilot switched on. The
// on/off bit is PER-REPO — `warden autopilot on` run inside a repo enables only
// that repo — while the plan/brain/merge TEMPLATE stays global in config. The set
// is persisted so previously-enabled repos come back up on daemon restart (boot
// re-Enables each; see the daemon wiring). Implementations are safe for concurrent
// use.
type EnableStore interface {
	// Enable marks repo (an absolute repo root) switched on. Idempotent; a blank
	// repo is a no-op.
	Enable(repo string) error
	// Disable clears repo's switch. Idempotent — disabling an unknown repo is a
	// no-op, not an error.
	Disable(repo string) error
	// IsEnabled reports whether repo is currently switched on.
	IsEnabled(repo string) bool
	// List returns every enabled repo root, sorted.
	List() []string
}

// enableMarker is the stable marker-file name for a repo root: a short hash of the
// absolute path, mirroring the RunID hashing style (sha256 → hex prefix) so the two
// read alike. The repo root itself is stored as the file's content, so List can
// return real paths rather than un-hash them.
func enableMarker(repo string) string {
	sum := sha256.Sum256([]byte(repo))
	return hex.EncodeToString(sum[:])[:16]
}

// newEnableStore builds the EnableStore for a controller: a filesystem store under
// dataDir when set (production — persisted across restarts), else an in-memory
// fallback so a controller built without a data dir (the DataDir-less unit tests)
// keeps working.
func newEnableStore(dataDir string) EnableStore {
	if strings.TrimSpace(dataDir) == "" {
		return newMemEnableStore()
	}
	return newFSEnableStore(filepath.Join(dataDir, "autopilot", "enabled"))
}

// fsEnableStore persists the enabled set as marker files under
// <data_dir>/autopilot/enabled/<hash>, each file holding its repo root. This is the
// production store; it survives daemon restarts.
type fsEnableStore struct {
	mu  sync.Mutex
	dir string
}

func newFSEnableStore(dir string) *fsEnableStore { return &fsEnableStore{dir: dir} }

func (s *fsEnableStore) Enable(repo string) error {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.dir, enableMarker(canonicalPath(repo))), []byte(repo), 0o644)
}

func (s *fsEnableStore) Disable(repo string) error {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := canonicalPath(repo)
	if err := os.Remove(filepath.Join(s.dir, enableMarker(key))); err != nil && !os.IsNotExist(err) {
		return err
	}
	entries, _ := os.ReadDir(s.dir)
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err == nil && canonicalPath(string(data)) == key {
			_ = os.Remove(filepath.Join(s.dir, e.Name()))
		}
	}
	return nil
}

func (s *fsEnableStore) IsEnabled(repo string) bool {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := canonicalPath(repo)
	if _, err := os.Stat(filepath.Join(s.dir, enableMarker(key))); err == nil {
		return true
	}
	entries, _ := os.ReadDir(s.dir)
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err == nil && canonicalPath(string(data)) == key {
			return true
		}
	}
	return false
}

func (s *fsEnableStore) List() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil // no dir yet (nothing ever enabled) ⇒ empty set
	}
	seen := make(map[string]bool)
	var repos []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		if repo := strings.TrimSpace(string(data)); repo != "" && !seen[canonicalPath(repo)] {
			seen[canonicalPath(repo)] = true
			repos = append(repos, repo)
		}
	}
	sort.Strings(repos)
	return repos
}

// memEnableStore is the in-memory EnableStore for tests and the DataDir-less
// fallback (nothing is persisted to disk). Safe for concurrent use.
type memEnableStore struct {
	mu    sync.Mutex
	repos map[string]string // canonical identity -> first caller-facing spelling
}

func newMemEnableStore() *memEnableStore { return &memEnableStore{repos: map[string]string{}} }

func (s *memEnableStore) Enable(repo string) error {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := canonicalPath(repo)
	if _, exists := s.repos[key]; !exists {
		s.repos[key] = repo
	}
	return nil
}

func (s *memEnableStore) Disable(repo string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.repos, canonicalPath(repo))
	return nil
}

func (s *memEnableStore) IsEnabled(repo string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.repos[canonicalPath(repo)]
	return ok
}

func (s *memEnableStore) List() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.repos))
	for _, r := range s.repos {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

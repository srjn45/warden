// Package preset stores named spawn configurations. A preset bundles the
// reusable `warden start` defaults (type, model, permission mode, …) under a
// name so repeated spawn configs can be saved once and replayed with
// `warden start --preset <name>`. Presets live in a single YAML file (default
// ~/.warden/presets.yaml), sitting beside the main config file.
package preset

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// Preset is one saved spawn configuration. Every field maps to a `warden start`
// flag; an empty/false field means "unset" and contributes no default, leaving
// warden's own defaults (or an explicit CLI flag) in charge. Per-invocation
// inputs (ticket, branch, pr, dir) are intentionally not stored — a preset
// captures the reusable shape of a spawn, not its one-off target.
type Preset struct {
	Type           string `yaml:"type,omitempty"`
	Model          string `yaml:"model,omitempty"`
	PermissionMode string `yaml:"permission_mode,omitempty"`
	AutoRestart    bool   `yaml:"auto_restart,omitempty"`
	Worktree       bool   `yaml:"worktree,omitempty"`
	InRepo         bool   `yaml:"in_repo,omitempty"`
}

// Store is the on-disk view of presets.yaml: a name→Preset mapping.
type Store struct {
	Presets map[string]Preset `yaml:"presets"`
}

// DefaultPath resolves the presets file location (~/.warden/presets.yaml),
// mirroring config.DefaultPath so both files share the ~/.warden home.
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".warden", "presets.yaml")
	}
	return filepath.Join(home, ".warden", "presets.yaml")
}

// Load reads the presets file at path. A missing file yields an empty (but
// usable) Store, so callers can Get/Set without a prior existence check. A
// present-but-unparseable file is a real error.
func Load(path string) (*Store, error) {
	s := &Store{Presets: map[string]Preset{}}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("preset: parse %s: %w", path, err)
	}
	if s.Presets == nil {
		s.Presets = map[string]Preset{}
	}
	return s, nil
}

// Get returns the named preset and whether it exists.
func (s *Store) Get(name string) (Preset, bool) {
	p, ok := s.Presets[name]
	return p, ok
}

// Set adds or replaces a preset by name (save semantics: last write wins).
func (s *Store) Set(name string, p Preset) {
	if s.Presets == nil {
		s.Presets = map[string]Preset{}
	}
	s.Presets[name] = p
}

// Names returns the preset names sorted alphabetically, for stable listing.
func (s *Store) Names() []string {
	names := make([]string, 0, len(s.Presets))
	for n := range s.Presets {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Save writes the store to path as YAML, creating ~/.warden if needed.
func (s *Store) Save(path string) error {
	out, err := yaml.Marshal(s)
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, out, 0o644)
}

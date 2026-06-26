// Package prompttemplate stores named, variabled prompt templates. Where a
// spawn preset (see internal/preset) captures reusable `warden start` *flags*,
// a prompt template captures a reusable *prompt body* with `{{VAR}}`
// placeholders that are filled in at spawn time. Templates live in a single
// YAML file (default ~/.warden/prompt-templates.yaml), sitting beside the
// presets file and the main config so all three share the ~/.warden home.
//
// Resolve with `warden start --prompt-template <name> --set VAR=value …`; the
// substituted body becomes the spawn prompt (an explicit --prompt/positional
// prompt still wins).
package prompttemplate

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// placeholderRE matches a `{{VAR}}` placeholder. Variable names are
// alphanumeric plus underscore (a typical SHELL_STYLE token), optionally
// padded with spaces inside the braces (`{{ VAR }}`).
var placeholderRE = regexp.MustCompile(`{{\s*([A-Za-z_][A-Za-z0-9_]*)\s*}}`)

// Template is one saved prompt template: a prompt body with `{{VAR}}`
// placeholders and the list of variables it declares. Vars is the authoritative
// declaration used to validate `--set` inputs at resolve time; it is kept in
// sync with the placeholders found in Prompt when a template is saved.
type Template struct {
	Prompt string   `yaml:"prompt"`
	Vars   []string `yaml:"vars,omitempty"`
}

// Store is the on-disk view of prompt-templates.yaml: a name→Template mapping.
type Store struct {
	Templates map[string]Template `yaml:"templates"`
}

// DefaultPath resolves the prompt-templates file location
// (~/.warden/prompt-templates.yaml), mirroring preset.DefaultPath so both files
// share the ~/.warden home.
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".warden", "prompt-templates.yaml")
	}
	return filepath.Join(home, ".warden", "prompt-templates.yaml")
}

// Load reads the prompt-templates file at path. A missing file yields an empty
// (but usable) Store, so callers can Get/Set without a prior existence check. A
// present-but-unparseable file is a real error.
func Load(path string) (*Store, error) {
	s := &Store{Templates: map[string]Template{}}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("prompttemplate: parse %s: %w", path, err)
	}
	if s.Templates == nil {
		s.Templates = map[string]Template{}
	}
	return s, nil
}

// Get returns the named template and whether it exists.
func (s *Store) Get(name string) (Template, bool) {
	t, ok := s.Templates[name]
	return t, ok
}

// Set adds or replaces a template by name (save semantics: last write wins).
func (s *Store) Set(name string, t Template) {
	if s.Templates == nil {
		s.Templates = map[string]Template{}
	}
	s.Templates[name] = t
}

// Names returns the template names sorted alphabetically, for stable listing.
func (s *Store) Names() []string {
	names := make([]string, 0, len(s.Templates))
	for n := range s.Templates {
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

// Placeholders returns the distinct `{{VAR}}` variable names referenced in a
// prompt body, in first-seen order. Used to auto-derive a template's declared
// variables when saving so callers need not list them by hand.
func Placeholders(prompt string) []string {
	seen := map[string]bool{}
	var vars []string
	for _, m := range placeholderRE.FindAllStringSubmatch(prompt, -1) {
		name := m[1]
		if !seen[name] {
			seen[name] = true
			vars = append(vars, name)
		}
	}
	return vars
}

// Resolve substitutes the template's `{{VAR}}` placeholders with values from
// vars, returning the finished prompt. Every declared variable must be supplied
// (a missing one is an error so a half-filled prompt never reaches an agent),
// and an unknown variable in vars is also an error (so a typo'd --set surfaces
// instead of being silently ignored). Substitution is literal — values are
// inserted verbatim, never re-scanned for further placeholders.
func (t Template) Resolve(vars map[string]string) (string, error) {
	declared := t.Vars
	if declared == nil {
		declared = Placeholders(t.Prompt)
	}
	declaredSet := map[string]bool{}
	var missing []string
	for _, name := range declared {
		declaredSet[name] = true
		if _, ok := vars[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return "", fmt.Errorf("missing value for %s — supply with --set %s=…",
			strings.Join(missing, ", "), missing[0])
	}
	var unknown []string
	for name := range vars {
		if !declaredSet[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return "", fmt.Errorf("unknown variable(s) %s — template declares: %s",
			strings.Join(unknown, ", "), strings.Join(declared, ", "))
	}
	out := placeholderRE.ReplaceAllStringFunc(t.Prompt, func(match string) string {
		name := placeholderRE.FindStringSubmatch(match)[1]
		return vars[name]
	})
	return out, nil
}

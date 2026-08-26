// Package role is the registry of warden's built-in agent roles. A role is a
// named, persistent system-prompt persona attached to an agent, plus an optional
// set of default spawn flags. Every agent has exactly one role; the empty role is
// the built-in "general" role (no persona, behaves as agents did before roles).
//
// The role set is a FIXED built-in set — there are no user-defined roles. Each
// role is one embedded YAML file under roles/ (mirroring
// internal/pipeline/templates), loaded once at package init. A malformed embedded
// file panics at init: it is a build-time asset, never user input.
package role

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed roles/*.yaml
var rolesFS embed.FS

// Default is the built-in role name used when a session carries no role. It
// injects no persona and applies no default flags, so a general-role agent
// behaves exactly as agents did before roles existed.
const Default = "general"

// Defaults are a role's optional default spawn flags. Each is applied at spawn
// ONLY when the caller left the corresponding request field unset (precedence:
// explicit request value > role default > global default); tags are unioned onto
// the request's tags rather than replacing them. A zero value means "no default"
// for that field.
type Defaults struct {
	Type           string   `yaml:"type"`
	Model          string   `yaml:"model"`
	PermissionMode string   `yaml:"permission_mode"`
	AutoApprove    bool     `yaml:"auto_approve"`
	Tags           []string `yaml:"tags"`
}

// Role is one built-in role: its name, a one-line description, the persona
// (multi-line system-prompt text injected as an addendum; empty ⇒ inject
// nothing), and its optional default spawn flags.
type Role struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Persona     string   `yaml:"persona"`
	Defaults    Defaults `yaml:"defaults"`
}

// registry holds the loaded roles keyed by name, plus the sorted name order
// (Default first, then the rest alphabetically) for stable Names()/All() output.
var (
	registry map[string]Role
	order    []string
)

func init() {
	registry = make(map[string]Role)
	entries, err := fs.ReadDir(rolesFS, "roles")
	if err != nil {
		panic(fmt.Sprintf("role: read embedded roles dir: %v", err))
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		data, err := rolesFS.ReadFile("roles/" + e.Name())
		if err != nil {
			panic(fmt.Sprintf("role: read embedded role %s: %v", e.Name(), err))
		}
		var r Role
		if err := yaml.Unmarshal(data, &r); err != nil {
			panic(fmt.Sprintf("role: parse embedded role %s: %v", e.Name(), err))
		}
		if r.Name == "" {
			panic(fmt.Sprintf("role: embedded role %s has no name", e.Name()))
		}
		if _, dup := registry[r.Name]; dup {
			panic(fmt.Sprintf("role: duplicate embedded role name %q", r.Name))
		}
		registry[r.Name] = r
		names = append(names, r.Name)
	}
	if _, ok := registry[Default]; !ok {
		panic(fmt.Sprintf("role: missing built-in default role %q", Default))
	}
	// Stable order: Default first, then the rest alphabetically.
	sort.Strings(names)
	order = make([]string, 0, len(names))
	order = append(order, Default)
	for _, n := range names {
		if n != Default {
			order = append(order, n)
		}
	}
}

// Get returns the role named name and whether it exists. The empty name and
// "general" both resolve to the built-in default role. Legacy roles ("reviewer",
// "implementer", "auto-merger") are mapped to "worker" for backward compatibility.
// An unknown name returns (zero, false); it is the call site's job to turn that
// into an error.
func Get(name string) (Role, bool) {
	if name == "" {
		name = Default
	}
	switch name {
	case "reviewer", "implementer", "auto-merger":
		name = "worker"
	}
	r, ok := registry[name]
	return r, ok
}

// Names returns every built-in role name, Default first then the rest sorted.
func Names() []string {
	out := make([]string, len(order))
	copy(out, order)
	return out
}

// All returns every built-in role in Names() order.
func All() []Role {
	out := make([]Role, 0, len(order))
	for _, n := range order {
		out = append(out, registry[n])
	}
	return out
}

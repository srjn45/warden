package plugin

import (
	"fmt"
	"strings"

	"github.com/srjn45/warden/internal/store"
)

// TaskTypeSpec is the config-facing declaration of one custom agent task type a
// plugin provides. Name is the new store.Type label (must not collide with a
// built-in); Worktree is the type's isolation policy — true means spawning it
// creates a git worktree by default, exactly like the built-in write-agent types.
type TaskTypeSpec struct {
	Name     string `yaml:"name"`
	Worktree bool   `yaml:"worktree"`
}

// Spec is the config-facing description of one registered plugin (config key
// `plugin_registry`). It is plain data so internal/config can embed it without a
// dependency cycle (config imports plugin; plugin never imports config). Path is
// the external executable warden shells out to over the JSON-over-stdio protocol.
type Spec struct {
	Name      string         `yaml:"name"`
	Path      string         `yaml:"path"`
	Events    []string       `yaml:"events"`
	TaskTypes []TaskTypeSpec `yaml:"task_types"`
}

// Plugin is a validated, in-memory plugin descriptor (a Spec that survived Load).
// Events are normalized to HookEvent and TaskTypes to store-ready policies.
type Plugin struct {
	Name      string
	Path      string
	Events    []HookEvent
	TaskTypes []TaskTypeSpec
}

// Subscribes reports whether the plugin is registered for event e.
func (p Plugin) Subscribes(e HookEvent) bool {
	for _, x := range p.Events {
		if x == e {
			return true
		}
	}
	return false
}

// Registry is the loaded set of plugins plus the custom-type index store consults.
// It is instance-based (not global) so it is trivially testable; the daemon wires
// the active registry into store once at startup via store.SetCustomTypeLookup.
type Registry struct {
	plugins     []Plugin
	customTypes map[string]store.CustomTypePolicy
}

// Load validates specs into a Registry. It is strict on the things that would
// silently misbehave (blank/duplicate names, blank paths, unknown events,
// custom-type names that collide with a built-in or another plugin) and returns
// an error listing every problem, so a typo in config surfaces loudly rather than
// producing a half-wired plugin. An empty spec list yields an empty (valid)
// registry. Load does NOT stat the executable — a plugin binary may legitimately
// not exist yet, and dispatch fails open on a missing binary anyway.
func Load(specs []Spec) (*Registry, error) {
	r := &Registry{customTypes: map[string]store.CustomTypePolicy{}}
	var errs []string
	seenName := map[string]bool{}
	for i, s := range specs {
		name := strings.TrimSpace(s.Name)
		if name == "" {
			errs = append(errs, fmt.Sprintf("plugin[%d]: empty name", i))
			continue
		}
		if seenName[name] {
			errs = append(errs, fmt.Sprintf("plugin %q: duplicate name", name))
			continue
		}
		seenName[name] = true
		if strings.TrimSpace(s.Path) == "" {
			errs = append(errs, fmt.Sprintf("plugin %q: empty path", name))
		}
		var events []HookEvent
		for _, e := range s.Events {
			ev := HookEvent(strings.TrimSpace(e))
			if !ValidEvent(ev) {
				errs = append(errs, fmt.Sprintf("plugin %q: unknown event %q", name, e))
				continue
			}
			events = append(events, ev)
		}
		var types []TaskTypeSpec
		for _, tt := range s.TaskTypes {
			tn := strings.TrimSpace(tt.Name)
			if tn == "" {
				errs = append(errs, fmt.Sprintf("plugin %q: empty task-type name", name))
				continue
			}
			if store.Type(tn).Builtin() {
				errs = append(errs, fmt.Sprintf("plugin %q: task type %q collides with a built-in type", name, tn))
				continue
			}
			if _, dup := r.customTypes[tn]; dup {
				errs = append(errs, fmt.Sprintf("plugin %q: task type %q already registered by another plugin", name, tn))
				continue
			}
			r.customTypes[tn] = store.CustomTypePolicy{Worktree: tt.Worktree}
			types = append(types, TaskTypeSpec{Name: tn, Worktree: tt.Worktree})
		}
		r.plugins = append(r.plugins, Plugin{Name: name, Path: strings.TrimSpace(s.Path), Events: events, TaskTypes: types})
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("plugin config: %s", strings.Join(errs, "; "))
	}
	return r, nil
}

// Plugins returns the loaded plugin descriptors (read-only view).
func (r *Registry) Plugins() []Plugin {
	if r == nil {
		return nil
	}
	return r.plugins
}

// Lookup resolves a custom task type's isolation policy. It is the function the
// store closed-enum logic consults (via store.SetCustomTypeLookup) for any name
// that is not a built-in type. Safe on a nil registry (reports not-found).
func (r *Registry) Lookup(name string) (store.CustomTypePolicy, bool) {
	if r == nil {
		return store.CustomTypePolicy{}, false
	}
	p, ok := r.customTypes[name]
	return p, ok
}

// subscribers returns the plugins registered for event e (in registry order).
func (r *Registry) subscribers(e HookEvent) []Plugin {
	if r == nil {
		return nil
	}
	var out []Plugin
	for _, p := range r.plugins {
		if p.Subscribes(e) {
			out = append(out, p)
		}
	}
	return out
}

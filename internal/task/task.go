package task

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed tasks/*.yaml
var tasksFS embed.FS

// Task represents a built-in task definition loaded from YAML.
type Task struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Tier        int      `yaml:"tier"`
	Roles       []string `yaml:"roles"`
}

var (
	registry map[string]Task
	order    []string
)

func init() {
	registry = make(map[string]Task)
	entries, err := fs.ReadDir(tasksFS, "tasks")
	if err != nil {
		panic(fmt.Sprintf("task: read embedded tasks dir: %v", err))
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		data, err := tasksFS.ReadFile("tasks/" + e.Name())
		if err != nil {
			panic(fmt.Sprintf("task: read embedded task %s: %v", e.Name(), err))
		}
		var t Task
		if err := yaml.Unmarshal(data, &t); err != nil {
			panic(fmt.Sprintf("task: parse embedded task %s: %v", e.Name(), err))
		}
		if t.Name == "" {
			panic(fmt.Sprintf("task: embedded task %s has no name", e.Name()))
		}
		if _, dup := registry[t.Name]; dup {
			panic(fmt.Sprintf("task: duplicate embedded task name %q", t.Name))
		}
		if t.Tier != 1 && t.Tier != 2 && t.Tier != 3 {
			panic(fmt.Sprintf("task: task %q has invalid tier %d (must be 1, 2, or 3)", t.Name, t.Tier))
		}
		if len(t.Roles) == 0 {
			panic(fmt.Sprintf("task: task %q has no roles", t.Name))
		}
		registry[t.Name] = t
		names = append(names, t.Name)
	}
	sort.Strings(names)
	order = names
}

// Get returns the task named name and whether it exists.
func Get(name string) (Task, bool) {
	t, ok := registry[name]
	return t, ok
}

// Names returns all task names in alphabetical order.
func Names() []string {
	out := make([]string, len(order))
	copy(out, order)
	return out
}

// All returns all tasks in alphabetical order.
func All() []Task {
	out := make([]Task, 0, len(order))
	for _, n := range order {
		out = append(out, registry[n])
	}
	return out
}

// ForRole returns tasks valid for a given role in alphabetical order.
func ForRole(roleName string) []Task {
	var out []Task
	for _, n := range order {
		t := registry[n]
		for _, r := range t.Roles {
			if r == roleName {
				out = append(out, t)
				break
			}
		}
	}
	return out
}

// TierFor returns the tier for a task name.
func TierFor(name string) (int, bool) {
	t, ok := registry[name]
	if !ok {
		return 0, false
	}
	return t.Tier, true
}

// ValidForRole checks if a task is allowed for a role.
func ValidForRole(taskName, roleName string) bool {
	t, ok := registry[taskName]
	if !ok {
		return false
	}
	for _, r := range t.Roles {
		if r == roleName {
			return true
		}
	}
	return false
}

package planstore

import (
	"fmt"
	"strings"
)

// ApplyDefaultTaskDAG ensures multi-task plans are real DAGs. When two or more
// tasks are present and none declare an after edge, tasks are chained in
// declaration order (task[i] after task[i-1]). Explicit after edges are left
// unchanged (including intentional parallel roots that feed a join).
//
// Single-task plans need no edges. Callers should run ValidateTaskDAG after.
func ApplyDefaultTaskDAG(tasks []TaskSpec) []TaskSpec {
	if len(tasks) < 2 {
		return tasks
	}
	hasEdge := false
	for _, t := range tasks {
		for _, dep := range t.After {
			if strings.TrimSpace(dep) != "" {
				hasEdge = true
				break
			}
		}
		if hasEdge {
			break
		}
	}
	if hasEdge {
		return tasks
	}
	out := make([]TaskSpec, len(tasks))
	copy(out, tasks)
	for i := 1; i < len(out); i++ {
		prev := strings.TrimSpace(out[i-1].ID)
		if prev == "" {
			continue
		}
		out[i].After = []string{prev}
	}
	return out
}

// ValidateTaskDAG checks task ids, prompts, after-refs, and acyclicity.
// It does not mutate tasks; run ApplyDefaultTaskDAG first when authoring.
func ValidateTaskDAG(tasks []TaskSpec) error {
	if len(tasks) == 0 {
		return &ValidationError{Field: "tasks", Msg: "at least one task is required"}
	}
	ids := make(map[string]struct{}, len(tasks))
	order := make([]string, 0, len(tasks))
	for i, t := range tasks {
		id := strings.TrimSpace(t.ID)
		prompt := strings.TrimSpace(t.Prompt)
		if id == "" {
			return &ValidationError{Field: "tasks", Msg: fmt.Sprintf("task[%d] is missing id", i)}
		}
		if prompt == "" {
			return &ValidationError{Field: "tasks", Msg: fmt.Sprintf("task %q is missing prompt", id)}
		}
		if _, dup := ids[id]; dup {
			return &ValidationError{Field: "tasks", Msg: fmt.Sprintf("duplicate task id %q", id)}
		}
		ids[id] = struct{}{}
		order = append(order, id)
	}
	edges := make(map[string][]string, len(tasks))
	for _, t := range tasks {
		id := strings.TrimSpace(t.ID)
		for _, dep := range t.After {
			dep = strings.TrimSpace(dep)
			if dep == "" {
				continue
			}
			if _, ok := ids[dep]; !ok {
				return &ValidationError{Field: "tasks", Msg: fmt.Sprintf("task %q after-ref %q does not exist", id, dep)}
			}
			if dep == id {
				return &ValidationError{Field: "tasks", Msg: fmt.Sprintf("task %q cannot depend on itself", id)}
			}
			edges[id] = append(edges[id], dep)
		}
	}
	if cycle := findTaskCycle(order, edges); cycle != "" {
		return &ValidationError{Field: "tasks", Msg: "task DAG contains a cycle: " + cycle}
	}
	return nil
}

// findTaskCycle returns a readable cycle path ("a → b → a") or "" when acyclic.
// edges maps task id → after dependencies (the task waits on these ids).
func findTaskCycle(order []string, edges map[string][]string) string {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int, len(order))
	var stack []string
	var dfs func(string) string
	dfs = func(u string) string {
		color[u] = gray
		stack = append(stack, u)
		for _, v := range edges[u] {
			switch color[v] {
			case white:
				if c := dfs(v); c != "" {
					return c
				}
			case gray:
				// v is on the stack — extract cycle v … u → v
				start := 0
				for i, id := range stack {
					if id == v {
						start = i
						break
					}
				}
				cycle := append(append([]string{}, stack[start:]...), v)
				return strings.Join(cycle, " → ")
			}
		}
		stack = stack[:len(stack)-1]
		color[u] = black
		return ""
	}
	for _, id := range order {
		if color[id] == white {
			if c := dfs(id); c != "" {
				return c
			}
		}
	}
	return ""
}

// TaskDepsSatisfied reports whether every after dependency of taskID is done
// or skipped according to progress. Unknown taskIDs return false.
func TaskDepsSatisfied(tasks []PlanTask, progress map[string]string, taskID string) bool {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return false
	}
	var after []string
	found := false
	for _, t := range tasks {
		if strings.TrimSpace(t.ID) == taskID {
			after = t.After
			found = true
			break
		}
	}
	if !found {
		return false
	}
	for _, dep := range after {
		dep = strings.TrimSpace(dep)
		if dep == "" {
			continue
		}
		st := ""
		if progress != nil {
			st = strings.ToLower(strings.TrimSpace(progress[dep]))
		}
		if st != "done" && st != "skipped" {
			return false
		}
	}
	return true
}

// ReadyTaskIDs returns pending tasks whose after dependencies are all
// done/skipped. Declaration order is preserved.
func ReadyTaskIDs(tasks []PlanTask, progress map[string]string) []string {
	var out []string
	for _, t := range tasks {
		id := strings.TrimSpace(t.ID)
		if id == "" {
			continue
		}
		st := "pending"
		if progress != nil {
			if v := strings.ToLower(strings.TrimSpace(progress[id])); v != "" {
				st = v
			}
		}
		if st != "pending" {
			continue
		}
		if TaskDepsSatisfied(tasks, progress, id) {
			out = append(out, id)
		}
	}
	return out
}

// BlockedTaskIDs returns pending tasks that still wait on unmet after deps.
func BlockedTaskIDs(tasks []PlanTask, progress map[string]string) []string {
	var out []string
	for _, t := range tasks {
		id := strings.TrimSpace(t.ID)
		if id == "" {
			continue
		}
		st := "pending"
		if progress != nil {
			if v := strings.ToLower(strings.TrimSpace(progress[id])); v != "" {
				st = v
			}
		}
		if st != "pending" {
			continue
		}
		if !TaskDepsSatisfied(tasks, progress, id) {
			out = append(out, id)
		}
	}
	return out
}

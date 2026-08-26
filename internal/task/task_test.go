package task

import (
	"reflect"
	"testing"
)

func TestGet(t *testing.T) {
	task, ok := Get("development")
	if !ok {
		t.Errorf("Get('development') should return ok=true")
	}
	if task.Name != "development" || task.Tier != 2 {
		t.Errorf("Get('development') returned wrong task data: %+v", task)
	}

	_, ok = Get("non-existent")
	if ok {
		t.Errorf("Get('non-existent') should return ok=false")
	}
}

func TestNames(t *testing.T) {
	names := Names()
	expectedLen := 13
	if len(names) != expectedLen {
		t.Errorf("Names() length = %d; want %d", len(names), expectedLen)
	}
	// Check alphabetical sorting
	for i := 0; i < len(names)-1; i++ {
		if names[i] >= names[i+1] {
			t.Errorf("Names() is not sorted: %s >= %s", names[i], names[i+1])
		}
	}
}

func TestAll(t *testing.T) {
	tasks := All()
	if len(tasks) != 13 {
		t.Errorf("All() length = %d; want 13", len(tasks))
	}
	// Check alphabetical sorting
	for i := 0; i < len(tasks)-1; i++ {
		if tasks[i].Name >= tasks[i+1].Name {
			t.Errorf("All() is not sorted: %s >= %s", tasks[i].Name, tasks[i+1].Name)
		}
	}
}

func TestForRole_Worker(t *testing.T) {
	tasks := ForRole("worker")
	if len(tasks) != 8 {
		t.Errorf("ForRole('worker') length = %d; want 8", len(tasks))
	}
	expected := []string{"code-review", "debug-ci", "development", "docs", "merge-pr", "monitor-ci", "pr-review", "release"}
	var got []string
	for _, task := range tasks {
		got = append(got, task.Name)
	}
	if !reflect.DeepEqual(got, expected) {
		t.Errorf("ForRole('worker') = %v; want %v", got, expected)
	}
}

func TestForRole_Planner(t *testing.T) {
	tasks := ForRole("planner")
	if len(tasks) != 6 {
		t.Errorf("ForRole('planner') length = %d; want 6", len(tasks))
	}
	expected := []string{"analysis", "architecture", "design", "docs", "research", "spike"}
	var got []string
	for _, task := range tasks {
		got = append(got, task.Name)
	}
	if !reflect.DeepEqual(got, expected) {
		t.Errorf("ForRole('planner') = %v; want %v", got, expected)
	}
}

func TestForRole_General(t *testing.T) {
	tasks := ForRole("general")
	if len(tasks) != 3 {
		t.Errorf("ForRole('general') length = %d; want 3", len(tasks))
	}
	expected := []string{"development", "docs", "release"}
	var got []string
	for _, task := range tasks {
		got = append(got, task.Name)
	}
	if !reflect.DeepEqual(got, expected) {
		t.Errorf("ForRole('general') = %v; want %v", got, expected)
	}
}

func TestForRole_Orchestrator(t *testing.T) {
	tasks := ForRole("orchestrator")
	if len(tasks) != 0 {
		t.Errorf("ForRole('orchestrator') length = %d; want 0", len(tasks))
	}
}

func TestTierFor(t *testing.T) {
	tier, ok := TierFor("analysis")
	if !ok || tier != 1 {
		t.Errorf("TierFor('analysis') = %d, %v; want 1, true", tier, ok)
	}
	tier, ok = TierFor("development")
	if !ok || tier != 2 {
		t.Errorf("TierFor('development') = %d, %v; want 2, true", tier, ok)
	}
	tier, ok = TierFor("debug-ci")
	if !ok || tier != 3 {
		t.Errorf("TierFor('debug-ci') = %d, %v; want 3, true", tier, ok)
	}
	tier, ok = TierFor("unknown")
	if ok {
		t.Errorf("TierFor('unknown') = %d, %v; want 0, false", tier, ok)
	}
}

func TestValidForRole(t *testing.T) {
	if !ValidForRole("development", "worker") {
		t.Errorf("ValidForRole('development', 'worker') should be true")
	}
	if ValidForRole("development", "planner") {
		t.Errorf("ValidForRole('development', 'planner') should be false")
	}
	if !ValidForRole("analysis", "planner") {
		t.Errorf("ValidForRole('analysis', 'planner') should be true")
	}
}

func TestNoDuplicateNames(t *testing.T) {
	if len(registry) != 13 {
		t.Errorf("len(registry) = %d; want 13 (no duplicates allowed)", len(registry))
	}
}

func TestAllTasksHaveValidTier(t *testing.T) {
	for name, task := range registry {
		if task.Tier != 1 && task.Tier != 2 && task.Tier != 3 {
			t.Errorf("Task %q has invalid tier %d", name, task.Tier)
		}
	}
}

func TestAllTasksHaveRoles(t *testing.T) {
	for name, task := range registry {
		if len(task.Roles) == 0 {
			t.Errorf("Task %q has no roles", name)
		}
	}
}

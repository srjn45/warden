package pipeline

import (
	"reflect"
	"sort"
	"testing"
)

func diamond(statuses map[string]JobStatus) *Pipeline {
	mk := func(id string, deps ...string) Job {
		return Job{ID: id, Prompt: "x", Worktree: "none", DependsOn: deps, Status: statuses[id]}
	}
	return &Pipeline{ID: "p", Name: "p", Repo: "/r", Jobs: []Job{
		mk("a"), mk("b", "a"), mk("c", "a"), mk("d", "b", "c"),
	}}
}

func sortedEqual(t *testing.T, got, want []string) {
	t.Helper()
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestPlanSpawnsRootsThenUnblocks(t *testing.T) {
	// nothing done yet → only the root (a) is spawnable.
	d := Plan(diamond(map[string]JobStatus{"a": JobPending, "b": JobPending, "c": JobPending, "d": JobPending}))
	sortedEqual(t, d.Spawn, []string{"a"})
	if d.Status != StatusRunning {
		t.Fatalf("status %s", d.Status)
	}

	// a done → b and c spawnable.
	d = Plan(diamond(map[string]JobStatus{"a": JobDone, "b": JobPending, "c": JobPending, "d": JobPending}))
	sortedEqual(t, d.Spawn, []string{"b", "c"})

	// b and c done → d spawnable.
	d = Plan(diamond(map[string]JobStatus{"a": JobDone, "b": JobDone, "c": JobDone, "d": JobPending}))
	sortedEqual(t, d.Spawn, []string{"d"})
}

func TestPlanRunningJobIsNotRespawned(t *testing.T) {
	d := Plan(diamond(map[string]JobStatus{"a": JobRunning, "b": JobPending, "c": JobPending, "d": JobPending}))
	if len(d.Spawn) != 0 {
		t.Fatalf("running root must not respawn or unblock: %v", d.Spawn)
	}
}

func TestPlanAllDone(t *testing.T) {
	d := Plan(diamond(map[string]JobStatus{"a": JobDone, "b": JobDone, "c": JobDone, "d": JobDone}))
	if d.Status != StatusDone || len(d.Spawn) != 0 {
		t.Fatalf("got %+v", d)
	}
}

func TestPlanFailureSkipsDescendantsAndStalls(t *testing.T) {
	// b failed → d (its descendant) is skipped; c may still run.
	d := Plan(diamond(map[string]JobStatus{"a": JobDone, "b": JobFailed, "c": JobPending, "d": JobPending}))
	sortedEqual(t, d.Spawn, []string{"c"})
	sortedEqual(t, d.Skip, []string{"d"})
	if d.Status != StatusStalled {
		t.Fatalf("status %s", d.Status)
	}
}

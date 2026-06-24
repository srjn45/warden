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

// condJob builds a two-node pipeline: a root `up` and a `down` that depends on
// it with the given run_if, at the given statuses.
func condJob(runIf string, upStatus, downStatus JobStatus) *Pipeline {
	return &Pipeline{ID: "p", Name: "p", Repo: "/r", Jobs: []Job{
		{ID: "up", Prompt: "x", Worktree: "none", Status: upStatus},
		{ID: "down", Prompt: "x", Worktree: "none", DependsOn: []string{"up"}, RunIf: runIf, Status: downStatus},
	}}
}

func TestPlanRunIfAlwaysSpawnsAfterFailure(t *testing.T) {
	// up failed; an `always` dependent still runs, and the pipeline isn't stalled
	// because the failure has a downstream handler.
	d := Plan(condJob("always", JobFailed, JobPending))
	sortedEqual(t, d.Spawn, []string{"down"})
	if len(d.Skip) != 0 {
		t.Fatalf("always job must not be skipped: %v", d.Skip)
	}
	if d.Status != StatusRunning {
		t.Fatalf("handled failure should keep pipeline running, got %s", d.Status)
	}
}

func TestPlanRunIfAlwaysSpawnsAfterSuccess(t *testing.T) {
	d := Plan(condJob("always", JobDone, JobPending))
	sortedEqual(t, d.Spawn, []string{"down"})
}

func TestPlanRunIfFailureRunsOnlyOnFailure(t *testing.T) {
	// dep failed → failure job runs.
	d := Plan(condJob("failure", JobFailed, JobPending))
	sortedEqual(t, d.Spawn, []string{"down"})
	if len(d.Skip) != 0 {
		t.Fatalf("failure job with a failed dep must not be skipped: %v", d.Skip)
	}

	// dep succeeded → failure job is skipped (nothing to recover).
	d = Plan(condJob("failure", JobDone, JobPending))
	sortedEqual(t, d.Skip, []string{"down"})
	if len(d.Spawn) != 0 {
		t.Fatalf("failure job with a successful dep must not spawn: %v", d.Spawn)
	}
	if d.Status != StatusDone {
		t.Fatalf("up done + down skipped should be done, got %s", d.Status)
	}
}

func TestPlanHandledFailureCompletesDone(t *testing.T) {
	// up failed, its `always` handler ran and emitted: the pipeline completes
	// `done`, not `stalled`, because the failure was handled.
	d := Plan(condJob("always", JobFailed, JobDone))
	if d.Status != StatusDone {
		t.Fatalf("handled+completed failure should be done, got %s", d.Status)
	}
	if len(d.Spawn) != 0 || len(d.Skip) != 0 {
		t.Fatalf("nothing left to do: %+v", d)
	}
}

func TestPlanUnhandledFailureStillStalls(t *testing.T) {
	// A plain success dependent doesn't handle the failure → stalled.
	d := Plan(condJob("success", JobFailed, JobPending))
	sortedEqual(t, d.Skip, []string{"down"})
	if d.Status != StatusStalled {
		t.Fatalf("unhandled failure should stall, got %s", d.Status)
	}
}

func TestPlanRunIfWaitsForAllDeps(t *testing.T) {
	// down (failure) depends on a (failed) and b (still running): it must not
	// spawn until b settles, even though a already failed.
	p := &Pipeline{ID: "p", Name: "p", Repo: "/r", Jobs: []Job{
		{ID: "a", Prompt: "x", Worktree: "none", Status: JobFailed},
		{ID: "b", Prompt: "x", Worktree: "none", Status: JobRunning},
		{ID: "down", Prompt: "x", Worktree: "none", DependsOn: []string{"a", "b"}, RunIf: "always", Status: JobPending},
	}}
	d := Plan(p)
	if len(d.Spawn) != 0 {
		t.Fatalf("down must wait for b to settle: %v", d.Spawn)
	}
}

func TestPlanNeedsAttentionKeepsRunningAndBlocks(t *testing.T) {
	// b is needs_attention: pipeline stays running, b does NOT unblock its
	// dependent d, and b is NOT a failure so c/d are not skipped.
	d := Plan(diamond(map[string]JobStatus{"a": JobDone, "b": JobNeedsAttention, "c": JobDone, "d": JobPending}))
	if d.Status != StatusRunning {
		t.Fatalf("needs_attention should keep pipeline running, got %s", d.Status)
	}
	for _, id := range d.Spawn {
		if id == "d" {
			t.Fatalf("d must not spawn while dep b is needs_attention")
		}
	}
	if len(d.Skip) != 0 {
		t.Fatalf("needs_attention must not skip anything, got %v", d.Skip)
	}
}

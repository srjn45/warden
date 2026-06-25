package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/lifecycle"
)

func TestBranchCell(t *testing.T) {
	if got := branchCell(lifecycle.WorktreeListing{}); got != "(detached)" {
		t.Errorf("empty branch = %q, want (detached)", got)
	}
	if got := branchCell(lifecycle.WorktreeListing{Branch: "feat-x"}); got != "feat-x" {
		t.Errorf("branch = %q, want feat-x", got)
	}
}

func TestOwnerCell(t *testing.T) {
	if got := ownerCell("", "active"); got != "orphan" {
		t.Errorf("no owner = %q, want orphan", got)
	}
	if got := ownerCell("code-1", "active"); got != "code-1 (active)" {
		t.Errorf("owner = %q, want code-1 (active)", got)
	}
}

func TestCountAction(t *testing.T) {
	results := []lifecycle.PruneResult{
		{Action: lifecycle.PruneRemove},
		{Action: lifecycle.PruneRemove},
		{Action: lifecycle.PruneSkip},
		{Action: lifecycle.PruneKeep},
	}
	if n := countAction(results, lifecycle.PruneRemove); n != 2 {
		t.Errorf("remove count = %d, want 2", n)
	}
	if n := countAction(results, lifecycle.PruneSkip); n != 1 {
		t.Errorf("skip count = %d, want 1", n)
	}
}

func TestPruneActionCell(t *testing.T) {
	cases := []struct {
		action lifecycle.PruneAction
		dryRun bool
		want   string
	}{
		{lifecycle.PruneRemove, true, "would remove"},
		{lifecycle.PruneRemove, false, "removed"},
		{lifecycle.PruneSkip, true, "SKIP"},
		{lifecycle.PruneSkip, false, "SKIPPED"},
		{lifecycle.PruneKeep, false, "keep"},
	}
	for _, c := range cases {
		if got := pruneActionCell(c.action, c.dryRun); got != c.want {
			t.Errorf("pruneActionCell(%v,%v)=%q want %q", c.action, c.dryRun, got, c.want)
		}
	}
}

func TestPruneDetail(t *testing.T) {
	cases := []struct {
		r    lifecycle.PruneResult
		want string
	}{
		{lifecycle.PruneResult{Action: lifecycle.PruneRemove, BranchDeleted: true}, "+ branch"},
		{lifecycle.PruneResult{Action: lifecycle.PruneRemove, BranchDeleted: true, Reason: "orphan"}, "orphan; + branch"},
		{lifecycle.PruneResult{Action: lifecycle.PruneSkip, Reason: "dirty"}, "dirty"},
		{lifecycle.PruneResult{Action: lifecycle.PruneRemove}, ""},
	}
	for _, c := range cases {
		if got := pruneDetail(c.r); got != c.want {
			t.Errorf("pruneDetail(%+v)=%q want %q", c.r, got, c.want)
		}
	}
}

// TestWorktreeLsJSON drives `worktree ls --json` against a stub daemon.
func TestWorktreeLsJSON(t *testing.T) {
	addr := stubDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"worktrees": []lifecycle.WorktreeListing{
				{Path: "/r/.worktrees/a", Branch: "feat", Owner: "code-1", Lifecycle: "active", State: "live"},
			},
		})
	})
	out, err := runCLI(t, addr, "worktree", "ls", "--repo", "/r", "--json")
	if err != nil {
		t.Fatalf("worktree ls --json: %v", err)
	}
	var got []lifecycle.WorktreeListing
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, out)
	}
	if len(got) != 1 || got[0].Owner != "code-1" {
		t.Fatalf("unexpected payload: %s", out)
	}
}

// TestWorktreeLsTable renders the human worktree table.
func TestWorktreeLsTable(t *testing.T) {
	addr := stubDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"worktrees": []lifecycle.WorktreeListing{
				{Path: "/r/.worktrees/a", Branch: "feat", Owner: "code-1", Lifecycle: "active", State: "live"},
			},
		})
	})
	out, err := runCLI(t, addr, "worktree", "ls", "--repo", "/r")
	if err != nil {
		t.Fatalf("worktree ls: %v", err)
	}
	for _, want := range []string{"PATH", "BRANCH", "OWNER", "feat", "code-1 (active)"} {
		if !strings.Contains(out, want) {
			t.Errorf("worktree ls missing %q:\n%s", want, out)
		}
	}
}

// TestWorktreeLsEmpty covers the "no warden worktrees" branch.
func TestWorktreeLsEmpty(t *testing.T) {
	addr := stubDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"worktrees": []lifecycle.WorktreeListing{}})
	})
	out, err := runCLI(t, addr, "worktree", "ls", "--repo", "/r")
	if err != nil {
		t.Fatalf("worktree ls: %v", err)
	}
	if !strings.Contains(out, "no warden worktrees") {
		t.Errorf("empty ls missing notice:\n%s", out)
	}
}

// TestPruneDryRun covers the prune command's --dry-run path (no prompt) and the
// printPrune summary rendering.
func TestPruneDryRun(t *testing.T) {
	addr := stubDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []lifecycle.PruneResult{
				{Action: lifecycle.PruneRemove, Path: "/r/.worktrees/a", Branch: "feat", State: "orphan"},
				{Action: lifecycle.PruneSkip, Path: "/r/.worktrees/b", Reason: "dirty", State: "live"},
			},
		})
	})
	out, err := runCLI(t, addr, "prune", "--repo", "/r", "--dry-run")
	if err != nil {
		t.Fatalf("prune --dry-run: %v", err)
	}
	for _, want := range []string{"ACTION", "would remove", "SKIP", "Summary:", "1 removable", "1 blocked"} {
		if !strings.Contains(out, want) {
			t.Errorf("prune --dry-run missing %q:\n%s", want, out)
		}
	}
}

// TestPruneYesApplies covers the non-dry-run path with --yes (skips the prompt).
func TestPruneYesApplies(t *testing.T) {
	addr := stubDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []lifecycle.PruneResult{
				{Action: lifecycle.PruneRemove, Path: "/r/.worktrees/a", Branch: "feat", BranchDeleted: true, State: "orphan"},
			},
		})
	})
	out, err := runCLI(t, addr, "prune", "--repo", "/r", "--yes")
	if err != nil {
		t.Fatalf("prune --yes: %v", err)
	}
	if !strings.Contains(out, "Removed 1 worktree") || !strings.Contains(out, "reclaimed 1 branch") {
		t.Errorf("prune --yes summary unexpected:\n%s", out)
	}
}

// TestPruneJSON covers the --json output for prune.
func TestPruneJSON(t *testing.T) {
	addr := stubDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []lifecycle.PruneResult{{Action: lifecycle.PruneRemove, Path: "/r/.worktrees/a"}},
		})
	})
	out, err := runCLI(t, addr, "prune", "--repo", "/r", "--dry-run", "--json")
	if err != nil {
		t.Fatalf("prune --json: %v", err)
	}
	var got []lifecycle.PruneResult
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, out)
	}
}

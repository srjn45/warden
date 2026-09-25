package tree

import (
	"path/filepath"
	"testing"

	"github.com/srjn45/warden/internal/store"
)

// Baseline (Phase 0) characterization of TODAY's path-based grouping / membership
// derivation. Project→member and agent→children relationships are NOT stored today;
// they are re-derived at render time from each session's Repo/Workdir path and
// parent_id (see internal/tree/membership.go, internal/tui/tree_adapter.go).
//
// PHASE1+ (spec §0 + D2/D3, plan Phase2_EdgeMaintenance): the entity-hierarchy
// redesign makes membership EXPLICIT and STORED — Project.agents[]/pipelines[]/
// terminals[] and Agent.child_agents[]/child_pipelines[]. Once those lists are the
// membership of record, grouping no longer needs to be reconstructed from paths and
// parent walks. These tests pin the current derivation so a Phase 1+ change to it is
// intentional and diffable, not a silent regression of the render.

// canonicalDir strips a "/.worktrees/<name>" suffix so an agent in a worktree groups
// under its parent repo, and makes the path absolute. This path-normalization is how
// a project "contains" its worktree agents today.
func TestBaseline_CanonicalDir_StripsWorktreeSuffix(t *testing.T) {
	repo := filepath.FromSlash("/home/u/dev/warden")
	wt := filepath.Join(repo, ".worktrees", "feature-x")

	if got := canonicalDir(wt); got != repo {
		t.Fatalf("worktree path must canonicalize to its parent repo: got %q want %q", got, repo)
	}
	if got := canonicalDir(repo); got != repo {
		t.Fatalf("a plain repo path must be unchanged: got %q want %q", got, repo)
	}
	// PHASE1+ (D2): an empty location falls into the synthetic "No project" bucket
	// today; with stored membership an agent's project is read from Project.agents[],
	// not inferred from an empty path.
	if got := canonicalDir(""); got != "" {
		t.Fatalf("empty dir must stay empty (No-project bucket): got %q", got)
	}
}

// sessionDir prefers Repo (set for typed/worktree agents) over Workdir (the caller
// cwd). This preference is the sole basis for which project a session groups under
// today — a back-ref that becomes an explicit Project.agents[] entry in Phase 1+.
func TestBaseline_SessionDir_PrefersRepoOverWorkdir(t *testing.T) {
	repo := filepath.FromSlash("/home/u/dev/warden")
	other := filepath.FromSlash("/home/u/dev/other")

	// PHASE1+ (D2): grouping is derived from the path here; later it is read from the
	// project's stored membership list.
	if got := sessionDir(&store.Session{Repo: repo, Workdir: other}); got != repo {
		t.Fatalf("Repo must win over Workdir: got %q want %q", got, repo)
	}
	if got := sessionDir(&store.Session{Workdir: other}); got != other {
		t.Fatalf("Workdir is the fallback when Repo is empty: got %q want %q", got, other)
	}
	if got := sessionDir(&store.Session{}); got != "" {
		t.Fatalf("no location ⇒ empty grouping key: got %q", got)
	}
}

// resolveGroupKey maps a (projectID, dir) to the group it renders under: a matching
// project id (open OR closed) wins, else the bare dir is a loose group, else "" is
// the synthetic No-project bucket. Path/back-ref matching is the derivation Phase 1+
// replaces with stored membership.
func TestBaseline_ResolveGroupKey_PathAndBackRefMatching(t *testing.T) {
	repo := filepath.FromSlash("/home/u/dev/warden")
	open := map[string]string{repo: repo} // indexed by id AND path → project id
	closed := map[string]string{"pid-c": "pid-c"}

	// projectID back-ref wins.
	if got := resolveGroupKey(repo, "", open, closed); got != repo {
		t.Fatalf("projectID back-ref must resolve to the open project: got %q", got)
	}
	// PHASE1+ (D2): a closed project still claims its member (kept, not folded into
	// Ungrouped) — this stays true, but the MATCH is by back-ref/path today vs. by
	// stored Project.agents[] membership after Phase 1+.
	if got := resolveGroupKey("pid-c", "", open, closed); got != "pid-c" {
		t.Fatalf("a closed project keeps its member: got %q", got)
	}
	// No project id, but the dir matches a project path.
	if got := resolveGroupKey("", repo, open, closed); got != repo {
		t.Fatalf("dir matching a project path must group under it: got %q", got)
	}
	// Unknown dir ⇒ a loose dir group keyed by the dir itself.
	loose := filepath.FromSlash("/tmp/scratch")
	if got := resolveGroupKey("", loose, open, closed); got != loose {
		t.Fatalf("unknown dir ⇒ loose dir group: got %q", got)
	}
	// Nothing at all ⇒ the No-project bucket.
	if got := resolveGroupKey("", "", open, closed); got != "" {
		t.Fatalf("no id + no dir ⇒ No-project bucket: got %q", got)
	}
}

// agentForest reconstructs the agent hierarchy from parent_id edges at render time:
// a child nests under its parent only when they share a project (same canonical dir);
// a cross-project child is promoted to a root under its own project; an orphan whose
// parent is absent is promoted so it never vanishes.
//
// PHASE1+ (D3): parent_id gains a stored forward edge Agent.child_agents[]. The
// forest is then read from that stored list rather than walked from parent_id, and
// the "same project only" nesting rule is enforced where child_agents[] is populated.
func TestBaseline_AgentForest_NestsSameProjectChildrenOnly(t *testing.T) {
	repo := filepath.FromSlash("/home/u/dev/warden")
	other := filepath.FromSlash("/home/u/dev/other")

	parent := &store.Session{ID: "p", Repo: repo, Kind: store.KindAgent}
	sameProjChild := &store.Session{ID: "c1", ParentID: "p", Repo: repo, Kind: store.KindAgent}
	crossProjChild := &store.Session{ID: "c2", ParentID: "p", Repo: other, Kind: store.KindAgent}
	orphan := &store.Session{ID: "o", ParentID: "missing", Repo: repo, Kind: store.KindAgent}

	roots, childrenByParent := agentForest([]*store.Session{parent, sameProjChild, crossProjChild, orphan})

	// c1 nests under p; c2 and the orphan are roots.
	if kids := childrenByParent["p"]; len(kids) != 1 || kids[0].ID != "c1" {
		t.Fatalf("only the same-project child nests under its parent: got %+v", kids)
	}
	rootIDs := map[string]bool{}
	for _, r := range roots {
		rootIDs[r.ID] = true
	}
	if !rootIDs["p"] {
		t.Fatalf("the parent is a root: roots=%v", rootIDs)
	}
	if !rootIDs["c2"] {
		t.Fatalf("a cross-project child is promoted to a root under its own project: roots=%v", rootIDs)
	}
	if !rootIDs["o"] {
		t.Fatalf("an orphan whose parent is absent is promoted to a root: roots=%v", rootIDs)
	}
	if rootIDs["c1"] {
		t.Fatalf("the nested same-project child must not also be a root: roots=%v", rootIDs)
	}
}

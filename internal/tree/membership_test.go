package tree

import (
	"path/filepath"
	"testing"

	"github.com/srjn45/warden/internal/projectstore"
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

// agentForest reconstructs the agent hierarchy from the STORED parent/child edges
// (spec D3), preferring them over path: a child nests under its parent whenever a
// stored edge connects them — regardless of whether they share a canonical dir — so
// a worktree or cross-project child of a parent nests correctly. An orphan whose
// parent is absent is promoted so it never vanishes.
//
// This removes the PR455 directory-equality override: explicit authoritative edges
// win even when canonical dirs differ.
func TestAgentForest_NestsByStoredEdgesRegardlessOfPath(t *testing.T) {
	repo := filepath.FromSlash("/home/u/dev/warden")
	other := filepath.FromSlash("/home/u/dev/other")

	parent := &store.Session{ID: "p", Repo: repo, Kind: store.KindAgent}
	sameProjChild := &store.Session{ID: "c1", ParentID: "p", Repo: repo, Kind: store.KindAgent}
	crossProjChild := &store.Session{ID: "c2", ParentID: "p", Repo: other, Kind: store.KindAgent}
	orphan := &store.Session{ID: "o", ParentID: "missing", Repo: repo, Kind: store.KindAgent}

	roots, childrenByParent := agentForest([]*store.Session{parent, sameProjChild, crossProjChild, orphan})

	// Both children nest under p by the parent_id edge — path is not consulted.
	kids := childrenByParent["p"]
	kidIDs := map[string]bool{}
	for _, k := range kids {
		kidIDs[k.ID] = true
	}
	if len(kids) != 2 || !kidIDs["c1"] || !kidIDs["c2"] {
		t.Fatalf("both edge-linked children must nest under their parent regardless of path: got %+v", kids)
	}
	rootIDs := map[string]bool{}
	for _, r := range roots {
		rootIDs[r.ID] = true
	}
	if !rootIDs["p"] {
		t.Fatalf("the parent is a root: roots=%v", rootIDs)
	}
	if !rootIDs["o"] {
		t.Fatalf("an orphan whose parent is absent is promoted to a root: roots=%v", rootIDs)
	}
	if rootIDs["c1"] || rootIDs["c2"] {
		t.Fatalf("a nested child must not also be a root: roots=%v", rootIDs)
	}
}

// Forward edge: a parent's child_agents[] nests a child whose own parent_id is empty
// or dangling (spec §6.1/§6.3, dangling ids tolerated).
func TestAgentForest_ForwardChildAgentsEdge(t *testing.T) {
	repo := filepath.FromSlash("/home/u/dev/warden")

	// c1 has no parent_id but is listed in p.child_agents; c2 has a stale parent_id
	// AND is listed forward — the forward edge still resolves it under p.
	parent := &store.Session{ID: "p", Repo: repo, Kind: store.KindAgent, ChildAgents: []string{"c1", "c2", "ghost"}}
	c1 := &store.Session{ID: "c1", Repo: repo, Kind: store.KindAgent}
	c2 := &store.Session{ID: "c2", ParentID: "vanished", Repo: repo, Kind: store.KindAgent}

	roots, childrenByParent := agentForest([]*store.Session{parent, c1, c2})

	kids := childrenByParent["p"]
	if len(kids) != 2 {
		t.Fatalf("forward child_agents[] must nest both listed present children: got %+v", kids)
	}
	rootIDs := map[string]bool{}
	for _, r := range roots {
		rootIDs[r.ID] = true
	}
	if !rootIDs["p"] || rootIDs["c1"] || rootIDs["c2"] {
		t.Fatalf("only the parent is a root: roots=%v", rootIDs)
	}
}

// When two parents claim the same child via forward edges only, the lex-smallest
// parent id wins (deterministic conflict tolerance). A valid backward edge still
// beats every forward claim.
func TestAgentForest_ForwardConflictPicksLexSmallestParent(t *testing.T) {
	child := &store.Session{ID: "c", Kind: store.KindAgent}
	// Intentionally unsorted input order: larger id first.
	pZ := &store.Session{ID: "z-parent", Kind: store.KindAgent, ChildAgents: []string{"c"}}
	pA := &store.Session{ID: "a-parent", Kind: store.KindAgent, ChildAgents: []string{"c"}}

	_, childrenByParent := agentForest([]*store.Session{pZ, child, pA})
	if kids := childrenByParent["a-parent"]; len(kids) != 1 || kids[0].ID != "c" {
		t.Fatalf("lex-smallest forward parent must win: got %+v", childrenByParent)
	}
	if kids := childrenByParent["z-parent"]; len(kids) != 0 {
		t.Fatalf("larger forward parent must not nest the child: got %+v", kids)
	}

	// Backward edge beats both forward claims.
	child.ParentID = "z-parent"
	_, childrenByParent = agentForest([]*store.Session{pZ, child, pA})
	if kids := childrenByParent["z-parent"]; len(kids) != 1 || kids[0].ID != "c" {
		t.Fatalf("backward parent_id must beat forward claims: got %+v", childrenByParent)
	}
}

// A parent_id/child_agents cycle is broken: members render as roots (never vanish,
// never recurse forever).
func TestAgentForest_CycleBrokenToRoots(t *testing.T) {
	a := &store.Session{ID: "a", ParentID: "b", Kind: store.KindAgent}
	b := &store.Session{ID: "b", ParentID: "a", Kind: store.KindAgent}

	roots, childrenByParent := agentForest([]*store.Session{a, b})

	if len(childrenByParent) != 0 {
		t.Fatalf("a 2-cycle must nest nobody: got %+v", childrenByParent)
	}
	rootIDs := map[string]bool{}
	for _, r := range roots {
		rootIDs[r.ID] = true
	}
	if !rootIDs["a"] || !rootIDs["b"] {
		t.Fatalf("both cycle members must be promoted to roots: roots=%v", rootIDs)
	}
}

// Project.agents[]/pipelines[]/terminals[] are the membership of record: they win
// over a contradictory ProjectID or path. An entity listed nowhere falls back to
// the legacy ProjectID/path resolver.
func TestResolveMembershipKey_AuthoritativeOverProjectIDAndPath(t *testing.T) {
	projA := filepath.FromSlash("/home/u/dev/a")
	projB := filepath.FromSlash("/home/u/dev/b")
	projects := []projectstore.Project{
		{ID: projA, Path: projA, Agents: []string{"agent-1"}, Pipelines: []string{"pipe-1"}, Terminals: []string{"term-1"}},
		{ID: projB, Path: projB, Agents: []string{"agent-1"}}, // conflict: both list agent-1
	}
	open := map[string]string{projA: projA, projB: projB}
	closed := map[string]string{}

	// Listed in both → lex-smallest project id (projA < projB by path).
	if got := resolveMembershipKey("agent-1", projB, projB, membershipAgents, projects, open, closed); got != projA {
		t.Fatalf("membership list must win + lex-smallest on conflict: got %q want %q", got, projA)
	}
	// Contradictory ProjectID + path pointing at B, but listed only on A.
	projects[1].Agents = nil
	if got := resolveMembershipKey("agent-1", projB, projB, membershipAgents, projects, open, closed); got != projA {
		t.Fatalf("Agents[] must beat ProjectID/path: got %q want %q", got, projA)
	}
	// Not listed anywhere → legacy ProjectID wins.
	if got := resolveMembershipKey("agent-2", projB, projA, membershipAgents, projects, open, closed); got != projB {
		t.Fatalf("unlisted entity falls back to ProjectID: got %q want %q", got, projB)
	}
	// Pipelines / terminals membership.
	if got := resolveMembershipKey("pipe-1", projB, projB, membershipPipelines, projects, open, closed); got != projA {
		t.Fatalf("Pipelines[] must beat ProjectID: got %q", got)
	}
	if got := resolveMembershipKey("term-1", projB, projB, membershipTerminals, projects, open, closed); got != projA {
		t.Fatalf("Terminals[] must beat ProjectID: got %q", got)
	}
}

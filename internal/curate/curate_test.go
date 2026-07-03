package curate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/memory"
)

func day(s string) time.Time {
	t, _ := time.Parse("2006-01-02", s)
	return t
}

// TestMergeProposesUnverifiedNeverTrusted: a fresh candidate is appended as
// `unverified` — never silently trusted (verify-before-trust). Even a caller that
// hands a Trusted candidate is downgraded.
func TestMergeProposesUnverifiedNeverTrusted(t *testing.T) {
	m := &memory.Memory{}
	cands := []memory.Entry{
		{Text: "the daemon API is spec-first", Provenance: "agent a1"},
		{Trust: memory.TrustTrusted, Text: "tests run behind `wd check`", Provenance: "agent a1"},
	}
	r := Merge(m, cands, day("2026-07-03"))
	if r.Added != 2 {
		t.Fatalf("Added = %d, want 2", r.Added)
	}
	for _, e := range m.Entries {
		if e.Trust != memory.TrustUnverified {
			t.Errorf("entry %q trust = %q, want unverified (never auto-trusted)", e.Text, e.Trust)
		}
	}
}

// TestMergeSupersedesContradiction: a later fact about the SAME topic with a
// different value strikes the older one (tombstone) and appends the new proposal.
func TestMergeSupersedesContradiction(t *testing.T) {
	m := &memory.Memory{Entries: []memory.Entry{
		{Trust: memory.TrustTrusted, Timestamp: "2026-06-01", Text: "the parser lives in foo/parse.go", Provenance: "agent a1", Raw: "- old"},
	}}
	r := Merge(m, []memory.Entry{
		{Text: "the parser lives in bar/parse.go", Provenance: "agent c3"},
	}, day("2026-07-03"))
	if r.Superseded != 1 || r.Added != 1 {
		t.Fatalf("Superseded=%d Added=%d, want 1/1", r.Superseded, r.Added)
	}
	if !m.Entries[0].Tombstone {
		t.Errorf("old entry not tombstoned: %+v", m.Entries[0])
	}
	if !strings.Contains(m.Entries[0].Note, "superseded 2026-07-03") || !strings.Contains(m.Entries[0].Note, "agent c3") {
		t.Errorf("tombstone note = %q", m.Entries[0].Note)
	}
	// The struck entry must not project; the new one must.
	proj := m.RenderDefault()
	if strings.Contains(proj, "foo/parse.go") {
		t.Errorf("superseded fact leaked into projection:\n%s", proj)
	}
	if !strings.Contains(proj, "bar/parse.go") {
		t.Errorf("new fact missing from projection:\n%s", proj)
	}
}

// TestMergeCorroborationPromotes: the SAME fact re-observed by a DIFFERENT agent
// promotes an unverified entry to trusted; a re-assertion by the SAME agent does not.
func TestMergeCorroborationPromotes(t *testing.T) {
	m := &memory.Memory{Entries: []memory.Entry{
		{Trust: memory.TrustUnverified, Timestamp: "2026-07-01", Text: "state lives in the daemon store", Provenance: "agent a1"},
	}}
	// Same agent re-asserts — no promotion.
	Merge(m, []memory.Entry{{Text: "state lives in the daemon store", Provenance: "agent a1"}}, day("2026-07-03"))
	if m.Entries[0].Trust != memory.TrustUnverified {
		t.Fatalf("same-agent re-assertion promoted trust: %q", m.Entries[0].Trust)
	}
	// Different agent corroborates — promote.
	r := Merge(m, []memory.Entry{{Text: "state lives in the daemon store", Provenance: "agent c3"}}, day("2026-07-03"))
	if r.Promoted != 1 || m.Entries[0].Trust != memory.TrustTrusted {
		t.Fatalf("cross-agent corroboration did not promote: promoted=%d trust=%q", r.Promoted, m.Entries[0].Trust)
	}
	if len(m.Entries) != 1 {
		t.Errorf("corroboration should dedup, not append: %d entries", len(m.Entries))
	}
}

// TestAgeOutTombstonesStaleUnverified: an unverified entry older than the TTL ages
// out (tombstone); a trusted or recent entry does not.
func TestAgeOutTombstonesStaleUnverified(t *testing.T) {
	m := &memory.Memory{Entries: []memory.Entry{
		{Trust: memory.TrustUnverified, Timestamp: "2026-01-01", Text: "old unverified hint"},
		{Trust: memory.TrustTrusted, Timestamp: "2026-01-01", Text: "old trusted fact"},
		{Trust: memory.TrustUnverified, Timestamp: "2026-07-02", Text: "fresh hint"},
	}}
	var r Result
	r.AgeOut(m, 45*24*time.Hour, day("2026-07-03"))
	if r.AgedOut != 1 {
		t.Fatalf("AgedOut = %d, want 1", r.AgedOut)
	}
	if !m.Entries[0].Tombstone {
		t.Errorf("old unverified not aged out")
	}
	if m.Entries[1].Tombstone {
		t.Errorf("trusted entry aged out (should not)")
	}
	if m.Entries[2].Tombstone {
		t.Errorf("fresh entry aged out (should not)")
	}
}

// TestCheckStaleFlagsMissingPath: a fact naming a path that no longer exists is
// flagged stale; a fact whose path still exists, or that names no repo path, is not.
func TestCheckStaleFlagsMissingPath(t *testing.T) {
	exists := func(abs string) bool { return strings.HasSuffix(abs, "internal/here.go") }
	m := &memory.Memory{Entries: []memory.Entry{
		{Text: "the helper lives in internal/gone.go"},
		{Text: "the helper lives in internal/here.go"},
		{Text: "run tests via `wd check`"},
	}}
	n := CheckStale(m, "/repo", exists, day("2026-07-03"))
	if n != 1 {
		t.Fatalf("flagged %d, want 1", n)
	}
	if !m.Entries[0].Stale || !strings.Contains(m.Entries[0].Note, "internal/gone.go") {
		t.Errorf("missing-path entry not flagged: %+v", m.Entries[0])
	}
	if m.Entries[1].Stale {
		t.Errorf("existing-path entry wrongly flagged")
	}
	if m.Entries[2].Stale {
		t.Errorf("non-path entry wrongly flagged")
	}
}

// fakeStopper is a no-op timer for the debounce test's injected clock.
type fakeStopper struct{}

func (fakeStopper) Stop() bool { return true }

// countingProposer records how many times Propose runs and the batch size it saw.
type countingProposer struct {
	calls      int
	lastSignal int
	give       []memory.Entry
}

func (c *countingProposer) Propose(_ context.Context, in ProposeInput) ([]memory.Entry, error) {
	c.calls++
	c.lastSignal = len(in.Signals)
	return c.give, nil
}

// TestDebounceCoalescesBurst: a burst of completions in the same repo re-arms the
// debounce timer each time, so exactly ONE pass runs and it sees the whole batch.
func TestDebounceCoalescesBurst(t *testing.T) {
	root := t.TempDir()
	store := &memory.Store{RepoRoot: func(_ context.Context, _ string) (string, error) { return root, nil }}
	prop := &countingProposer{give: []memory.Entry{{Text: "a durable fact"}}}
	cur := New(store, prop)

	var scheduled func()
	cur.After = func(_ time.Duration, f func()) stopper { scheduled = f; return fakeStopper{} }

	for i := 0; i < 3; i++ {
		cur.Enqueue(context.Background(), root, Signal{Agent: "a" + string(rune('0'+i))})
	}
	if scheduled == nil {
		t.Fatal("no pass scheduled")
	}
	scheduled() // fire the coalesced timer
	cur.Wait()

	if prop.calls != 1 {
		t.Errorf("proposer ran %d times, want 1 (burst must coalesce)", prop.calls)
	}
	if prop.lastSignal != 3 {
		t.Errorf("pass saw %d signals, want 3 (whole batch)", prop.lastSignal)
	}
}

// TestPassWritesWorkingTreeNeverCommits is the never-auto-commit invariant PROOF: a
// pass over a REAL git repo writes .warden/memory.md into the working tree and leaves
// it UNCOMMITTED — no new commit, no clean tree. The committed diff stays the human
// gate.
func TestPassWritesWorkingTreeNeverCommits(t *testing.T) {
	root := initGitRepo(t)
	store := &memory.Store{RepoRoot: func(_ context.Context, _ string) (string, error) { return root, nil }}
	prop := &countingProposer{give: []memory.Entry{{Text: "the daemon API is spec-first: edit openapi.yaml"}}}
	cur := New(store, prop)
	cur.Now = func() time.Time { return day("2026-07-03") }

	headBefore := gitHead(t, root)
	cur.runPass(context.Background(), root, []Signal{{Agent: "a1", Commit: "deadbeefcafe"}})

	// The proposal landed in the working tree...
	data, err := os.ReadFile(filepath.Join(root, ".warden", "memory.md"))
	if err != nil {
		t.Fatalf("memory.md not written: %v", err)
	}
	if !strings.Contains(string(data), "the daemon API is spec-first") {
		t.Fatalf("proposal not in file:\n%s", data)
	}
	if !strings.Contains(string(data), "[unverified") {
		t.Errorf("proposal not marked unverified:\n%s", data)
	}
	// ...but was NOT committed: HEAD unchanged and the tree is dirty.
	if got := gitHead(t, root); got != headBefore {
		t.Errorf("HEAD advanced (%s → %s): curation must never commit", headBefore, got)
	}
	// git collapses a fully-untracked dir to "?? .warden/"; either form proves the
	// proposal is uncommitted.
	if st := gitStatus(t, root); !strings.Contains(st, ".warden") {
		t.Errorf("expected uncommitted .warden change in `git status`, got:\n%s", st)
	}
}

func initGitRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@t"},
		{"config", "user.name", "t"},
		{"commit", "--allow-empty", "-q", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return root
}

func gitHead(t *testing.T, root string) string {
	t.Helper()
	return gitOut(t, root, "rev-parse", "HEAD")
}

func gitStatus(t *testing.T, root string) string {
	t.Helper()
	return gitOut(t, root, "status", "--porcelain")
}

func gitOut(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

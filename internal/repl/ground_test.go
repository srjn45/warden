package repl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/srjn45/warden/internal/memory"
	"github.com/stretchr/testify/require"
)

// recordingCompleter is a scripted local llm.Completer that records how it was
// called, so a test can prove grounding used the LOCAL model (and ONLY it) — the
// $0 guarantee, the same way monitor_test.go stubs its condenser.
type recordingCompleter struct {
	out    string
	err    error
	calls  int
	prompt string
}

func (r *recordingCompleter) Complete(_ context.Context, prompt string) (string, error) {
	r.calls++
	r.prompt = prompt
	return r.out, r.err
}

// groundStore returns a memory.Store rooted at a fresh temp repo carrying the
// given .warden/memory.md body — the hermetic stand-in for the git-shelling
// default, so tests need no real repo.
func groundStore(t *testing.T, body string) (*memory.Store, string) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".warden"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".warden", "memory.md"), []byte(body), 0o644))
	return &memory.Store{RepoRoot: func(context.Context, string) (string, error) { return dir, nil }}, dir
}

// groundStoreAbsent roots at a temp dir with NO memory.md — Locate succeeds, the
// read misses, and (crucially) nothing is auto-created.
func groundStoreAbsent(t *testing.T) (*memory.Store, string) {
	t.Helper()
	dir := t.TempDir()
	return &memory.Store{RepoRoot: func(context.Context, string) (string, error) { return dir, nil }}, dir
}

const sampleMemory = `<!-- warden project memory -->

- [trusted · 2026-06-30 · agent a1b2 · sha 04e2aed] The daemon API is spec-first: edit openapi.yaml then ` + "`make generate`" + `; never hand-write handlers.
- [unverified · 2026-07-01 · agent c3d4] Tests run behind ` + "`wd check`" + `.
- A plain human-authored bullet about the release process.
`

// TestGrounding_WhereXLives answers a "where does X live?" query from the matching
// entry, through the LOCAL model only, and surfaces its trust + provenance.
func TestGrounding_WhereXLives(t *testing.T) {
	store, _ := groundStore(t, sampleMemory)
	comp := &recordingCompleter{out: "The daemon API is spec-first — edit openapi.yaml then run `make generate`."}
	g := NewGrounder("/repo/x", store, comp)

	out, err := g.Answer(context.Background(), "where does the daemon API live?")
	require.NoError(t, err)

	// The local model was consulted exactly once — the $0 path — and it saw the
	// relevant fact grounded into its prompt.
	require.Equal(t, 1, comp.calls, "grounding must use the local model exactly once")
	require.Contains(t, comp.prompt, "spec-first", "the matching fact must be grounded into the prompt")

	// The answer carries the model's prose AND a citation surfacing trust/provenance.
	require.Contains(t, out, "spec-first")
	require.Contains(t, out, "grounded in .warden/memory.md")
	require.Contains(t, out, "trusted")
	require.Contains(t, out, "agent a1b2")
}

// TestGrounding_SurfacesUnverifiedTrust proves an unverified hint is visibly a
// hint in the citations.
func TestGrounding_SurfacesUnverifiedTrust(t *testing.T) {
	store, _ := groundStore(t, sampleMemory)
	comp := &recordingCompleter{out: "Run the tests with `wd check`."}
	g := NewGrounder("/repo/x", store, comp)

	out, err := g.Answer(context.Background(), "how do I run the tests?")
	require.NoError(t, err)
	require.Contains(t, out, "unverified", "an unverified entry must be flagged as such")
	require.Contains(t, out, "wd check")
}

// TestGrounding_AbsentMemoryGraceful returns a plain "no project memory" answer,
// never crashes, and NEVER auto-creates the file (PR-2 owns writes; PR-3 is
// read-only).
func TestGrounding_AbsentMemoryGraceful(t *testing.T) {
	store, dir := groundStoreAbsent(t)
	comp := &recordingCompleter{out: "should not be called"}
	g := NewGrounder("/repo/x", store, comp)

	out, err := g.Answer(context.Background(), "where does X live?")
	require.NoError(t, err)
	require.Contains(t, out, "not in project memory")

	// Read-only discipline: the file must NOT have sprung into being.
	_, statErr := os.Stat(filepath.Join(dir, ".warden", "memory.md"))
	require.True(t, os.IsNotExist(statErr), "grounding must never auto-create memory.md")
	require.Zero(t, comp.calls, "no memory ⇒ no model call")
}

// TestGrounding_NoLocalModelDegrades returns the matching entries verbatim (still
// $0) when no local model is wired — it must NOT escalate to a paid model.
func TestGrounding_NoLocalModelDegrades(t *testing.T) {
	store, _ := groundStore(t, sampleMemory)
	g := NewGrounder("/repo/x", store, nil) // nil ⇒ no local model

	out, err := g.Answer(context.Background(), "where does the daemon API live?")
	require.NoError(t, err)
	require.Contains(t, out, "no local model wired")
	require.Contains(t, out, "spec-first", "the matching entry is returned verbatim")
	require.Contains(t, out, "trusted", "trust is surfaced in the degraded answer")
	require.Contains(t, out, "agent a1b2", "provenance is surfaced in the degraded answer")
}

// TestGrounding_ModelErrorFallsBackLocally proves a local-model failure degrades
// to the verbatim entries — never a cloud round-trip. The Grounder holds no
// Escalator, so escalation is also structurally impossible.
func TestGrounding_ModelErrorFallsBackLocally(t *testing.T) {
	store, _ := groundStore(t, sampleMemory)
	comp := &recordingCompleter{err: errors.New("local model down")}
	g := NewGrounder("/repo/x", store, comp)

	out, err := g.Answer(context.Background(), "where does the daemon API live?")
	require.NoError(t, err)
	require.Equal(t, 1, comp.calls, "it tries the local model")
	require.Contains(t, out, "spec-first", "then falls back to the verbatim entries, $0")
}

// TestGrounding_ExcludesTombstoneAndStale never surfaces bookkeeping entries: a
// superseded (tombstoned) or stale-flagged bullet stays in the committed file for
// the diff reviewer but must not reach an answer.
func TestGrounding_ExcludesTombstoneAndStale(t *testing.T) {
	body := "- [trusted · 2026-06-30] The build uses go 1.23.\n" +
		"- ~~The daemon lived in cmd/oldd~~ <!-- superseded 2026-07-01 by agent c3 -->\n" +
		"- [unverified · 2026-07-01] The parser is in internal/parse <!-- stale: internal/parse missing -->\n"
	store, _ := groundStore(t, body)
	g := NewGrounder("/repo/x", store, nil)

	out, err := g.Answer(context.Background(), "where is the parser?")
	require.NoError(t, err)
	require.NotContains(t, out, "cmd/oldd", "tombstoned entry must never be surfaced")
	require.NotContains(t, out, "internal/parse", "stale entry must never be surfaced")
	require.Contains(t, out, "go 1.23", "live entries still answer")
}

// TestGrounding_EmptyMemoryGraceful treats a header-only (no live entries) file
// the same as absent.
func TestGrounding_EmptyMemoryGraceful(t *testing.T) {
	store, _ := groundStore(t, "<!-- warden project memory -->\n")
	g := NewGrounder("/repo/x", store, &recordingCompleter{})

	out, err := g.Answer(context.Background(), "where does X live?")
	require.NoError(t, err)
	require.Contains(t, out, "not in project memory")
}

// TestSelectRelevant_PicksKeywordMatchesFirst pins the retrieval: an entry whose
// text overlaps the question's keywords ranks ahead of unrelated ones.
func TestSelectRelevant_PicksKeywordMatchesFirst(t *testing.T) {
	entries := []memory.Entry{
		{Text: "The release process uses goreleaser."},
		{Text: "The daemon API is spec-first: edit openapi.yaml."},
		{Text: "Tests run behind wd check."},
	}
	got := selectRelevant("where does the daemon api live?", entries)
	require.NotEmpty(t, got)
	require.Contains(t, got[0].Text, "daemon API", "the keyword match must rank first")
}

// TestAddGrounding_RegistersReadOnlyTool confirms the verb is registered as a
// read-only (auto-execute, $0) tool and dispatches to the grounder.
func TestAddGrounding_RegistersReadOnlyTool(t *testing.T) {
	store, _ := groundStore(t, sampleMemory)
	reg := NewRegistry()
	reg.AddGrounding(NewGrounder("/repo/x", store, nil))

	tl, ok := reg.Lookup("project_memory")
	require.True(t, ok, "project_memory must be registered")
	require.False(t, tl.Mutating, "grounding is read-only (auto-execute, no gate)")

	out, err := reg.Dispatch(context.Background(), &fakeDaemon{},
		ToolCall{Name: "project_memory", Args: map[string]any{"question": "where does the daemon API live?"}})
	require.NoError(t, err)
	require.Contains(t, out, "spec-first")
}

// TestEnableGrounding_NilIsNoOp guards the config-off path: a nil grounder leaves
// the registry unchanged (project_memory absent).
func TestEnableGrounding_NilIsNoOp(t *testing.T) {
	s := &Session{reg: NewRegistry()}
	s.EnableGrounding(nil)
	_, ok := s.reg.Lookup("project_memory")
	require.False(t, ok, "a nil grounder must register nothing")
}

// TestGroundingQueryRoutesLocal proves a grounding-style project question stays on
// the LOCAL tier via the EXISTING tier-classify machinery — it classifies T0, so
// even a model wired for escalation plans it locally (never a paid cloud plan).
func TestGroundingQueryRoutesLocal(t *testing.T) {
	tier, err := heuristicClassifier{}.NeededTier(context.Background(), "where does the spawn gate live?")
	require.NoError(t, err)
	require.Equal(t, T0, tier, "a plain project question needs only the local tier")

	// A T0 model with escalation ON still plans locally for a T0 request — no
	// escalation, no cloud spend.
	r := NewRouter(T0, true, heuristicClassifier{}, nil)
	route := r.Route(context.Background(), "where does the spawn gate live?")
	require.Equal(t, PlanLocal, route.Mode, "grounding-style queries route to the local plan")
}

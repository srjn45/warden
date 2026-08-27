package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// ---- summaryFromFiles unit tests ----

func TestSummaryFromFilesCLAUDEMDSection(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "CLAUDE.md", "# Title\n\n## Summary\nThis is the project.\n\n## More\nOther stuff.\n")
	require.Equal(t, "This is the project.", summaryFromFiles(dir))
}

func TestSummaryFromFilesCLAUDEMDProjectSummaryVariant(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "CLAUDE.md", "## Project Summary\nBuilds widgets at scale.\n")
	require.Equal(t, "Builds widgets at scale.", summaryFromFiles(dir))
}

func TestSummaryFromFilesCLAUDEMDAboutVariant(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "CLAUDE.md", "## About\nA widget factory.\n")
	require.Equal(t, "A widget factory.", summaryFromFiles(dir))
}

func TestSummaryFromFilesREADMEFallback(t *testing.T) {
	dir := t.TempDir()
	// No CLAUDE.md; README has ## Summary.
	writeFile(t, dir, "README.md", "# Repo\n\n## Summary\nReadme summary.\n")
	require.Equal(t, "Readme summary.", summaryFromFiles(dir))
}

func TestSummaryFromFilesCLAUDEBeatREADMESection(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "CLAUDE.md", "## Summary\nCLAUDE wins.\n")
	writeFile(t, dir, "README.md", "## Summary\nREADME loses.\n")
	require.Equal(t, "CLAUDE wins.", summaryFromFiles(dir))
}

func TestSummaryFromFilesFirstParagraphFallback(t *testing.T) {
	dir := t.TempDir()
	// CLAUDE.md has no ## Summary; fall back to first content line.
	writeFile(t, dir, "CLAUDE.md", "# Title\n\nFirst paragraph line.\n\nSecond paragraph.\n")
	require.Equal(t, "First paragraph line.", summaryFromFiles(dir))
}

func TestSummaryFromFilesREADMEFirstParagraphFallback(t *testing.T) {
	dir := t.TempDir()
	// No CLAUDE.md; README has no ## Summary either.
	writeFile(t, dir, "README.md", "# Title\n\nReadme paragraph.\n")
	require.Equal(t, "Readme paragraph.", summaryFromFiles(dir))
}

func TestSummaryFromFilesEmpty(t *testing.T) {
	dir := t.TempDir()
	require.Equal(t, "", summaryFromFiles(dir), "empty dir yields empty summary")
}

func TestSummaryFromFilesSkipsBlanksBetweenHeadingAndContent(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "CLAUDE.md", "## Summary\n\n\nContent after blanks.\n")
	require.Equal(t, "Content after blanks.", summaryFromFiles(dir))
}

// ---- Integration tests: join picks up the declared summary ----

// TestJoinGroupDeclaredSummaryInMember checks that a joining agent whose
// project dir has a CLAUDE.md `## Summary` gets that summary on its roster seat
// and in the intro messages delivered to its peers.
func TestJoinGroupDeclaredSummaryInMember(t *testing.T) {
	srv, fs, _, mb := newGroupServerMbox(t)
	dir := t.TempDir()
	writeFile(t, dir, "CLAUDE.md", "## Summary\nFrontend orchestrator.\n")
	seedNamedAgent(t, fs, "a1", "alpha", dir, store.StatusWorking)
	seedNamedAgent(t, fs, "a2", "beta", t.TempDir(), store.StatusWorking)

	_, err := srv.JoinGroup(context.Background(), joinReq("team", "a1"))
	require.NoError(t, err)
	resp, err := srv.JoinGroup(context.Background(), joinReq("team", "a2"))
	require.NoError(t, err)

	// a1's seat should carry the declared summary.
	ok := resp.(oapi.JoinGroup200JSONResponse)
	var a1Summary string
	for _, m := range ok.Group.Members {
		if m.AgentId == "a1" {
			a1Summary = m.Summary
		}
	}
	require.Equal(t, "Frontend orchestrator.", a1Summary)

	// At least one intro delivered to a2 must contain the declared summary.
	// (a2 also gets a summary ask via pane — not in inbox — so inbox is intros only.)
	a2inbox := introBodies(t, mb, "a2")
	require.GreaterOrEqual(t, len(a2inbox), 1, "a2 must receive at least the intro about a1")
	found := false
	for _, body := range a2inbox {
		if strings.Contains(body, "Frontend orchestrator.") {
			found = true
		}
	}
	require.True(t, found, "intro to a2 must carry a1's declared summary")
}

// TestJoinGroupDeclaredSummaryBeatsAsk verifies that when a declared summary
// exists, no pane question is typed to the agent — declared beats generated.
func TestJoinGroupDeclaredSummaryBeatsAsk(t *testing.T) {
	srv, fs, fl, _ := newGroupServerMbox(t)
	dir := t.TempDir()
	writeFile(t, dir, "CLAUDE.md", "## Summary\nDeclared blurb.\n")
	// Seed as idle so the ask would fire if triggered (parked status).
	seedNamedAgent(t, fs, "a1", "alpha", dir, store.StatusIdle)

	_, err := srv.JoinGroup(context.Background(), joinReq("team", "a1"))
	require.NoError(t, err)

	// The pane input must NOT be a summary ask — declared summary suppresses it.
	// (fl.lastInput may be empty or unrelated; never the ask prompt phrase.)
	require.NotContains(t, fl.lastInput, "one-time; cached forever after",
		"declared summary must suppress the pane ask: got %q", fl.lastInput)
}

// TestJoinGroupNoSummaryTriggersAsk verifies that when no declared summary
// exists and no cache entry exists, a pane-level summary ask is typed into the
// joining agent's active session (one cheap, one-time user-turn per §4.2).
func TestJoinGroupNoSummaryTriggersAsk(t *testing.T) {
	srv, fs, fl, _ := newGroupServerMbox(t)
	seedNamedAgent(t, fs, "a1", "alpha", t.TempDir(), store.StatusWorking)

	_, err := srv.JoinGroup(context.Background(), joinReq("team", "a1"))
	require.NoError(t, err)

	// The last pane input must be the summary question (no peers, so no intro
	// nudge overwrites it). Check for a unique phrase from summaryAskPromptFmt.
	require.Contains(t, fl.lastInput, "one-time; cached forever after",
		"missing pane ask; got %q", fl.lastInput)
}

// TestJoinGroupCacheHitOnRejoin is the B5 acceptance test for cache hits:
// a declared summary from a first join is cached in the group's SummaryCache,
// survives the member leaving, and is reused on the next join for the same
// project key — without reading the file again (we remove it to prove this).
func TestJoinGroupCacheHitOnRejoin(t *testing.T) {
	srv, fs, _, _ := newGroupServerMbox(t)
	dir := t.TempDir()
	writeFile(t, dir, "CLAUDE.md", "## Summary\nCached project.\n")
	seedNamedAgent(t, fs, "a1", "alpha", dir, store.StatusWorking)

	// First join: summary is read from file and cached.
	resp1, err := srv.JoinGroup(context.Background(), joinReq("team", "a1"))
	require.NoError(t, err)
	ok1 := resp1.(oapi.JoinGroup200JSONResponse)
	require.Equal(t, "Cached project.", ok1.Group.Members[0].Summary)

	// Leave — removes the member seat; the cache must survive.
	_, err = srv.LeaveGroup(context.Background(), leaveReq("team", "a1"))
	require.NoError(t, err)

	// Remove the file so only the cache can provide the summary.
	require.NoError(t, os.Remove(filepath.Join(dir, "CLAUDE.md")))

	// Rejoin with a fresh agent that shares the same project dir/key.
	seedNamedAgent(t, fs, "a1b", "alpha-new", dir, store.StatusWorking)
	resp2, err := srv.JoinGroup(context.Background(), joinReq("team", "a1b"))
	require.NoError(t, err)
	ok2 := resp2.(oapi.JoinGroup200JSONResponse)
	require.Equal(t, "Cached project.", ok2.Group.Members[0].Summary,
		"summary cache must survive leave/rejoin without re-reading the file")
}

// TestJoinGroupIdempotentRejoinPreservesExistingSummary checks that a re-join
// by the same agent (idempotent seat refresh) does not overwrite an already-set
// summary on the member record.
func TestJoinGroupIdempotentRejoinPreservesExistingSummary(t *testing.T) {
	srv, fs, _, _ := newGroupServerMbox(t)
	dir := t.TempDir()
	writeFile(t, dir, "CLAUDE.md", "## Summary\nOriginal summary.\n")
	seedNamedAgent(t, fs, "a1", "alpha", dir, store.StatusWorking)

	_, err := srv.JoinGroup(context.Background(), joinReq("team", "a1"))
	require.NoError(t, err)

	// Remove the file before the idempotent re-join.
	require.NoError(t, os.Remove(filepath.Join(dir, "CLAUDE.md")))

	resp2, err := srv.JoinGroup(context.Background(), joinReq("team", "a1"))
	require.NoError(t, err)
	ok2 := resp2.(oapi.JoinGroup200JSONResponse)
	require.Equal(t, "Original summary.", ok2.Group.Members[0].Summary,
		"idempotent re-join must preserve the existing summary on the member record")
}

// writeFile is a test helper that creates file under dir with the given content.
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("writeFile %s: %v", name, err)
	}
}

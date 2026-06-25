package snapshot

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/lifecycle"
)

// calledArgs flattens a FakeRunner's recorded calls to argv slices for assertions.
func calledArgs(fr *lifecycle.FakeRunner) [][]string {
	out := make([][]string, 0, len(fr.Calls))
	for _, c := range fr.Calls {
		out = append(out, c.Argv)
	}
	return out
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := NewStore(filepath.Join(t.TempDir(), "snapshots"))
	require.NoError(t, err)
	return st
}

func TestParsePorcelainPaths(t *testing.T) {
	in := " M a.go\n?? b.go\nR  old.go -> new.go\n\n"
	require.Equal(t, []string{"a.go", "b.go", "new.go"}, parsePorcelainPaths(in))
}

func TestCountLines(t *testing.T) {
	require.Equal(t, 0, countLines(""))
	require.Equal(t, 1, countLines("one line"))
	require.Equal(t, 2, countLines("a\nb"))
	require.Equal(t, 2, countLines("a\nb\n")) // trailing newline doesn't add a line
}

func TestNewIDUniqueAndPrefixed(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id, err := newID()
		require.NoError(t, err)
		require.Contains(t, id, "snap-")
		require.False(t, seen[id], "ids must be unique")
		seen[id] = true
		require.NoError(t, safeID(id), "generated id must be filesystem-safe")
	}
}

func TestSafeIDRejectsTraversal(t *testing.T) {
	for _, bad := range []string{"", "../etc", "a/b", `a\b`, "a:b", ".."} {
		require.Error(t, safeID(bad), "must reject %q", bad)
	}
	require.NoError(t, safeID("snap-abc12345"))
}

// --- Capture (FakeRunner) ---

func TestCaptureHappyPathPersistsMetadataAndTranscript(t *testing.T) {
	fr := &lifecycle.FakeRunner{Responses: map[string]lifecycle.FakeResp{
		"git rev-parse --abbrev-ref HEAD":      {Out: "feature-x\n"},
		"git rev-parse HEAD":                   {Out: "headsha1\n"},
		"git stash create warden snapshot":     {Out: "stashsha1\n"},
		"git status --porcelain":               {Out: " M internal/foo.go\n?? new.go\n"},
		"tmux capture-pane -p -S - -t agent-1": {Out: "line one\nline two\n"},
	}}
	st := newTestStore(t)
	snap, err := New(fr, st).Capture(context.Background(), CaptureRequest{
		SessionID: "agent-1", Workdir: "/wt", TmuxSession: "agent-1", Message: "before refactor",
	})
	require.NoError(t, err)
	require.Equal(t, "agent-1", snap.SessionID)
	require.Equal(t, "feature-x", snap.Branch)
	require.Equal(t, "headsha1", snap.HeadSHA)
	require.Equal(t, "stashsha1", snap.StashSHA)
	require.Equal(t, "before refactor", snap.Message)
	require.ElementsMatch(t, []string{"internal/foo.go", "new.go"}, snap.DirtyFiles)
	require.Equal(t, 2, snap.TranscriptLines)
	require.NotEmpty(t, snap.TranscriptPath)

	// git stash create is the non-destructive primitive — no `git stash push/save`.
	require.Contains(t, calledArgs(fr), []string{"git", "stash", "create", "warden snapshot"})
	require.NotContains(t, calledArgs(fr), []string{"git", "stash", "push"})

	// Persisted + transcript readable back.
	got, err := st.Get(snap.ID)
	require.NoError(t, err)
	require.Equal(t, snap.ID, got.ID)
	blob, err := os.ReadFile(got.TranscriptPath)
	require.NoError(t, err)
	require.Contains(t, string(blob), "line two")
}

func TestCaptureCleanTreeHasNoStashOrTranscript(t *testing.T) {
	fr := &lifecycle.FakeRunner{Responses: map[string]lifecycle.FakeResp{
		"git rev-parse --abbrev-ref HEAD": {Out: "feature-x\n"},
		"git rev-parse HEAD":              {Out: "headsha1\n"},
		// stash create + status unmatched → "" (clean tree, empty stash)
	}}
	snap, err := New(fr, newTestStore(t)).Capture(context.Background(), CaptureRequest{
		SessionID: "agent-1", Workdir: "/wt", // no tmux → no transcript
	})
	require.NoError(t, err)
	require.Empty(t, snap.StashSHA, "clean tree yields no stash commit")
	require.Empty(t, snap.DirtyFiles)
	require.Empty(t, snap.TranscriptPath)
	require.Equal(t, 0, snap.TranscriptLines)
}

func TestCaptureRefusesNonRepo(t *testing.T) {
	fr := &lifecycle.FakeRunner{} // rev-parse --abbrev-ref unmatched → "" → not a repo
	_, err := New(fr, newTestStore(t)).Capture(context.Background(), CaptureRequest{Workdir: "/notrepo"})
	require.Error(t, err)
	require.NotContains(t, calledArgs(fr), []string{"git", "stash", "create", "warden snapshot"})
}

func TestCaptureRequiresWorkdir(t *testing.T) {
	_, err := New(&lifecycle.FakeRunner{}, newTestStore(t)).Capture(context.Background(), CaptureRequest{})
	require.Error(t, err)
}

func TestCaptureTranscriptFailureIsBestEffort(t *testing.T) {
	fr := &lifecycle.FakeRunner{Responses: map[string]lifecycle.FakeResp{
		"git rev-parse --abbrev-ref HEAD":      {Out: "feature-x\n"},
		"git rev-parse HEAD":                   {Out: "headsha1\n"},
		"git stash create warden snapshot":     {Out: "stashsha1\n"},
		"git status --porcelain":               {Out: " M foo.go\n"},
		"tmux capture-pane -p -S - -t agent-1": {Err: errStub("no such session")},
	}}
	snap, err := New(fr, newTestStore(t)).Capture(context.Background(), CaptureRequest{
		SessionID: "agent-1", Workdir: "/wt", TmuxSession: "agent-1",
	})
	require.NoError(t, err, "a missing tmux pane must not fail the snapshot")
	require.Empty(t, snap.TranscriptPath)
	require.Equal(t, "stashsha1", snap.StashSHA, "git state is still captured")
}

// --- List ordering ---

func TestListNewestFirstAndSessionFilter(t *testing.T) {
	st := newTestStore(t)
	base := time.Now().UTC()
	put := func(id, session string, ageSec int) {
		require.NoError(t, st.Put(&Snapshot{
			ID: id, SessionID: session, CreatedAt: base.Add(-time.Duration(ageSec) * time.Second),
		}, ""))
	}
	put("snap-old", "agent-1", 30)
	put("snap-new", "agent-1", 5)
	put("snap-mid", "agent-2", 15)

	all, err := st.List("")
	require.NoError(t, err)
	require.Equal(t, []string{"snap-new", "snap-mid", "snap-old"}, idsOf(all), "newest first across all sessions")

	mine, err := st.List("agent-1")
	require.NoError(t, err)
	require.Equal(t, []string{"snap-new", "snap-old"}, idsOf(mine), "filtered to the session, newest first")
}

func TestGetMissingSnapshot(t *testing.T) {
	_, err := newTestStore(t).Get("snap-doesnotexist")
	require.ErrorIs(t, err, ErrNotFound)
}

// --- Restore (FakeRunner) ---

func TestRestoreRefusesDirtyTree(t *testing.T) {
	st := newTestStore(t)
	require.NoError(t, st.Put(&Snapshot{ID: "snap-1", Workdir: "/wt", HeadSHA: "h1", StashSHA: "s1"}, ""))
	fr := &lifecycle.FakeRunner{Responses: map[string]lifecycle.FakeResp{
		"git rev-parse --abbrev-ref HEAD": {Out: "feature-x\n"},
		"git status --porcelain":          {Out: " M live.go\n"}, // dirty
	}}
	_, err := New(fr, st).Restore(context.Background(), "snap-1", false)
	require.ErrorIs(t, err, ErrDirtyWorktree)
	require.NotContains(t, calledArgs(fr), []string{"git", "stash", "apply", "s1"}, "no apply over a dirty tree")
}

func TestRestoreForceOverridesDirtyTree(t *testing.T) {
	st := newTestStore(t)
	require.NoError(t, st.Put(&Snapshot{ID: "snap-1", Workdir: "/wt", HeadSHA: "h1", StashSHA: "s1"}, ""))
	fr := &lifecycle.FakeRunner{Responses: map[string]lifecycle.FakeResp{
		"git rev-parse --abbrev-ref HEAD": {Out: "feature-x\n"},
		"git status --porcelain":          {Out: " M live.go\n"},
		"git rev-parse HEAD":              {Out: "h1\n"},
	}}
	res, err := New(fr, st).Restore(context.Background(), "snap-1", true)
	require.NoError(t, err)
	require.True(t, res.Applied)
	require.True(t, res.HeadMatch)
	require.Contains(t, calledArgs(fr), []string{"git", "stash", "apply", "s1"})
}

func TestRestoreRefusesProtectedBranch(t *testing.T) {
	st := newTestStore(t)
	require.NoError(t, st.Put(&Snapshot{ID: "snap-1", Workdir: "/wt", StashSHA: "s1"}, ""))
	for _, b := range []string{"main", "master"} {
		fr := &lifecycle.FakeRunner{Responses: map[string]lifecycle.FakeResp{
			"git rev-parse --abbrev-ref HEAD": {Out: b + "\n"},
		}}
		_, err := New(fr, st).Restore(context.Background(), "snap-1", false)
		require.Error(t, err, "must refuse restore onto %s", b)
		require.NotContains(t, calledArgs(fr), []string{"git", "stash", "apply", "s1"})
	}
}

func TestRestoreMissingSnapshot(t *testing.T) {
	_, err := New(&lifecycle.FakeRunner{}, newTestStore(t)).Restore(context.Background(), "snap-nope", false)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestRestoreCleanCaptureAppliesNoPatch(t *testing.T) {
	st := newTestStore(t)
	require.NoError(t, st.Put(&Snapshot{ID: "snap-1", Workdir: "/wt", HeadSHA: "h1"}, "")) // no StashSHA
	fr := &lifecycle.FakeRunner{Responses: map[string]lifecycle.FakeResp{
		"git rev-parse --abbrev-ref HEAD": {Out: "feature-x\n"},
		"git status --porcelain":          {Out: ""},
		"git rev-parse HEAD":              {Out: "h2\n"},
	}}
	res, err := New(fr, st).Restore(context.Background(), "snap-1", false)
	require.NoError(t, err)
	require.False(t, res.Applied, "a clean capture has no patch to re-apply")
	require.False(t, res.HeadMatch, "HEAD moved since capture")
	require.NotContains(t, calledArgs(fr), []string{"git", "stash", "apply"})
}

func TestRestoreSurfacesConflicts(t *testing.T) {
	st := newTestStore(t)
	require.NoError(t, st.Put(&Snapshot{ID: "snap-1", Workdir: "/wt", HeadSHA: "h1", StashSHA: "s1"}, ""))
	fr := &lifecycle.FakeRunner{Responses: map[string]lifecycle.FakeResp{
		"git rev-parse --abbrev-ref HEAD":      {Out: "feature-x\n"},
		"git status --porcelain":               {Out: ""},
		"git rev-parse HEAD":                   {Out: "h1\n"},
		"git stash apply s1":                   {Out: "CONFLICT\n", Err: errStub("apply exit 1")},
		"git diff --name-only --diff-filter=U": {Out: "foo.go\nbar.go\n"},
	}}
	res, err := New(fr, st).Restore(context.Background(), "snap-1", false)
	require.NoError(t, err, "conflicts are a result, not a transport error")
	require.False(t, res.Applied)
	require.ElementsMatch(t, []string{"foo.go", "bar.go"}, res.Conflicts)
}

// --- End-to-end against a real git repo (exercises ExecRunner + git) ---

func TestCaptureAndRestoreRealRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	runGit("init", "-b", "feature-x")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("base\n"), 0o644))
	runGit("add", "-A")
	runGit("commit", "-m", "base")

	// Make a dirty change and snapshot it.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("WIP change\n"), 0o644))
	m := New(lifecycle.ExecRunner{}, newTestStore(t))
	snap, err := m.Capture(context.Background(), CaptureRequest{SessionID: "agent-1", Workdir: dir})
	require.NoError(t, err)
	require.NotEmpty(t, snap.StashSHA, "dirty change should produce a stash commit")
	require.Equal(t, "feature-x", snap.Branch)
	require.ElementsMatch(t, []string{"a.txt"}, snap.DirtyFiles)

	// git stash create is non-destructive: the working change is still present.
	cur, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	require.NoError(t, err)
	require.Equal(t, "WIP change\n", string(cur))

	// Discard the change, then restore the snapshot to bring it back.
	runGit("checkout", "--", "a.txt")
	after, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	require.Equal(t, "base\n", string(after))

	res, err := m.Restore(context.Background(), snap.ID, false)
	require.NoError(t, err)
	require.True(t, res.Applied)
	require.True(t, res.HeadMatch)
	restored, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	require.NoError(t, err)
	require.Equal(t, "WIP change\n", string(restored), "restore re-applies the captured working state")
}

func idsOf(snaps []*Snapshot) []string {
	out := make([]string, len(snaps))
	for i, s := range snaps {
		out[i] = s.ID
	}
	return out
}

type errStub string

func (e errStub) Error() string { return string(e) }

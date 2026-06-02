package lifecycle

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/srajanpathak/agentctl/internal/store"
	"github.com/stretchr/testify/require"
)

func TestParseSummaryCapsByRune(t *testing.T) {
	got := parseSummary(strings.Repeat("é", 100)) // 2 bytes/rune
	require.Equal(t, 80, utf8.RuneCountInString(got), "capped to 80 runes")
	require.True(t, utf8.ValidString(got), "must not slice a rune in half")
}

func TestSummaryArgDropsLeadingPartialRune(t *testing.T) {
	// 0xA9 is a UTF-8 continuation byte: a byte-sliced tail can start on one.
	got := summaryArg(string([]byte{0xA9, 'h', 'i'}))
	require.Equal(t, summaryInstruction+"hi", got)
	require.True(t, utf8.ValidString(strings.TrimPrefix(got, summaryInstruction)))
}

func TestResolveIDAutoFormat(t *testing.T) {
	id, err := resolveID(SpawnRequest{Type: store.TypeDevelopment})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(id, "development-"), "got %q", id)
	require.Len(t, strings.TrimPrefix(id, "development-"), 8, "4 random bytes → 8 hex chars")

	ticketed, err := resolveID(SpawnRequest{Ticket: "JIRA-1", Type: store.TypeDevelopment})
	require.NoError(t, err)
	require.Equal(t, "JIRA-1", ticketed, "an explicit ticket is used verbatim")
}

func TestFirstWords(t *testing.T) {
	require.Equal(t, "review the auth module", firstWords("review the auth module", 10))
	require.Equal(t, "one two three…", firstWords("one two three four five", 3))
	require.Equal(t, "", firstWords("", 5))
}

func TestParseSummary(t *testing.T) {
	require.Equal(t, "review auth module for security", parseSummary("review auth module for security\n"))
	require.Equal(t, "tracing a flaky test", parseSummary(`"tracing a flaky test"`))
	require.Equal(t, "first line only", parseSummary("first line only\nsecond ignored"))
}

func TestClaudeProjectDir(t *testing.T) {
	got := claudeProjectDir("/root/projects", "/Users/srajan.pathak/agentctl-agents/agent-a1b2")
	require.Equal(t, "/root/projects/-Users-srajan-pathak-agentctl-agents-agent-a1b2", got)
	require.Equal(t, "", claudeProjectDir("", "/anything")) // empty root → no transcript lookup
}

const noOtherWorktrees = "worktree /repo\nHEAD abc\nbranch refs/heads/main\n"

func TestSpawnDevelopmentCreatesWorktreeTmuxAndDoc(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git worktree list --porcelain": {Out: noOtherWorktrees},
	}}
	lc := New(fr)
	s, err := lc.Spawn(context.Background(), SpawnRequest{
		Type: store.TypeDevelopment, Ticket: "PROJ-350", Repo: "/repo",
	})
	require.NoError(t, err)
	require.Equal(t, "PROJ-350", s.ID)
	require.Equal(t, store.TypeDevelopment, s.Type)
	require.Equal(t, store.StatusSpawning, s.Status)
	require.Equal(t, ".worktrees/PROJ-350", s.Worktree)
	require.Equal(t, "PROJ-350", s.Branch)

	// Worktree on a new branch.
	require.Contains(t, fr.calledArgs(), []string{"git", "worktree", "add", ".worktrees/PROJ-350", "-b", "PROJ-350"})
	// Detached tmux session in the worktree.
	require.Contains(t, fr.calledArgs(), []string{"tmux", "new-session", "-d", "-s", "PROJ-350", "-c", "/repo/.worktrees/PROJ-350"})
	// Launch claude UNATTENDED, with a pinned session id and display name.
	require.NotEmpty(t, s.ClaudeSessionID)
	require.Contains(t, fr.calledArgs(), []string{"tmux", "send-keys", "-t", "PROJ-350", claudeLaunch(s.ClaudeSessionID, "PROJ-350"), "Enter"})
}

func TestSpawnAdoptsExistingWorktree(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git worktree list --porcelain": {Out: noOtherWorktrees + "\nworktree /repo/.worktrees/PROJ-350\nHEAD def\nbranch refs/heads/PROJ-350\n"},
	}}
	lc := New(fr)
	_, err := lc.Spawn(context.Background(), SpawnRequest{
		Type: store.TypeDevelopment, Ticket: "PROJ-350", Repo: "/repo",
	})
	require.NoError(t, err)
	// Adopt: must NOT call `git worktree add` again.
	require.NotContains(t, fr.calledArgs(), []string{"git", "worktree", "add", ".worktrees/PROJ-350", "-b", "PROJ-350"})
	require.Contains(t, fr.calledArgs(), []string{"tmux", "new-session", "-d", "-s", "PROJ-350", "-c", "/repo/.worktrees/PROJ-350"})
}

func TestSpawnNoWorktreeTypeRunsInRepoWithAutoID(t *testing.T) {
	fr := &FakeRunner{}
	lc := New(fr)
	s, err := lc.Spawn(context.Background(), SpawnRequest{Type: store.TypeBuildkiteDebug, Repo: "/repo"})
	require.NoError(t, err)
	require.Empty(t, s.Worktree)
	require.Empty(t, s.Branch)
	require.Empty(t, s.Ticket)
	require.True(t, strings.HasPrefix(s.ID, "buildkitedebug-"), "auto id for no-ticket session, got %q", s.ID)
	// No git calls for a no-worktree type.
	for _, argv := range fr.calledArgs() {
		require.NotEqual(t, "git", argv[0], "no-worktree type must not call git")
	}
	require.Contains(t, fr.calledArgs(), []string{"tmux", "new-session", "-d", "-s", s.ID, "-c", "/repo"})
	require.NotEmpty(t, s.ClaudeSessionID)
	require.Contains(t, fr.calledArgs(), []string{"tmux", "send-keys", "-t", s.ID, claudeLaunch(s.ClaudeSessionID, s.ID), "Enter"})
}

func TestSpawnPRReviewChecksOutPR(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git worktree list --porcelain": {Out: noOtherWorktrees},
	}}
	lc := New(fr)
	s, err := lc.Spawn(context.Background(), SpawnRequest{Type: store.TypePRReview, Repo: "/repo", PR: "12345"})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(s.ID, "prreview-"), "got %q", s.ID)
	require.Equal(t, ".worktrees/"+s.ID, s.Worktree)
	require.Equal(t, "12345", s.PR)
	// Detached worktree, then `gh pr checkout` inside it.
	require.Contains(t, fr.calledArgs(), []string{"git", "worktree", "add", "--detach", s.Worktree})
	require.Contains(t, fr.calledArgs(), []string{"gh", "pr", "checkout", "12345"})
}

func TestSpawnSpikeWorktreeIsOptIn(t *testing.T) {
	// Default: no worktree.
	fr := &FakeRunner{}
	s1, err := New(fr).Spawn(context.Background(), SpawnRequest{Type: store.TypeSpike, Repo: "/repo"})
	require.NoError(t, err)
	require.Empty(t, s1.Worktree)

	// --worktree: new-branch worktree like development.
	fr2 := &FakeRunner{Responses: map[string]FakeResp{"git worktree list --porcelain": {Out: noOtherWorktrees}}}
	s2, err := New(fr2).Spawn(context.Background(), SpawnRequest{Type: store.TypeSpike, Repo: "/repo", Worktree: true})
	require.NoError(t, err)
	require.Equal(t, ".worktrees/"+s2.ID, s2.Worktree)
	require.Contains(t, fr2.calledArgs(), []string{"git", "worktree", "add", s2.Worktree, "-b", s2.ID})
}

// calledArgs is a test helper.
func (f *FakeRunner) calledArgs() [][]string {
	out := make([][]string, 0, len(f.Calls))
	for _, c := range f.Calls {
		out = append(out, c.Argv)
	}
	return out
}

func TestCleanupGuardAbortsOnUncommitted(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git -C /repo/.worktrees/A-1 status --porcelain": {Out: " M file.go\n"},
	}}
	lc := New(fr)
	err := lc.Cleanup(context.Background(), cleanupInput("A-1"), false)
	require.ErrorIs(t, err, ErrDirtyWorktree)
	// Guard must run BEFORE worktree removal.
	require.NotContains(t, fr.calledArgs(), []string{"git", "-C", "/repo", "worktree", "remove", ".worktrees/A-1"})
}

func TestCleanupGuardAbortsOnUnpushed(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git -C /repo/.worktrees/A-1 status --porcelain":   {Out: ""},
		"git -C /repo/.worktrees/A-1 log @{u}.. --oneline": {Out: "abc123 wip\n"},
	}}
	lc := New(fr)
	err := lc.Cleanup(context.Background(), cleanupInput("A-1"), false)
	require.ErrorIs(t, err, ErrUnpushedCommits)
}

func TestCleanupForceProceedsAndKillsTmuxFirst(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git -C /repo/.worktrees/A-1 status --porcelain": {Out: " M dirty\n"},
	}}
	lc := New(fr)
	err := lc.Cleanup(context.Background(), cleanupInput("A-1"), true)
	require.NoError(t, err)
	require.Contains(t, fr.calledArgs(), []string{"tmux", "kill-session", "-t", "A-1"})
	require.Contains(t, fr.calledArgs(), []string{"git", "-C", "/repo", "worktree", "remove", "--force", ".worktrees/A-1"})
	require.Contains(t, fr.calledArgs(), []string{"git", "-C", "/repo", "branch", "-D", "A-1"})
}

func TestCleanupCleanProceeds(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git -C /repo/.worktrees/A-1 status --porcelain":   {Out: ""},
		"git -C /repo/.worktrees/A-1 log @{u}.. --oneline": {Out: ""},
	}}
	lc := New(fr)
	require.NoError(t, lc.Cleanup(context.Background(), cleanupInput("A-1"), false))
	require.Contains(t, fr.calledArgs(), []string{"git", "-C", "/repo", "worktree", "remove", ".worktrees/A-1"})
}

func TestCleanupNoWorktreeOnlyKillsTmux(t *testing.T) {
	fr := &FakeRunner{}
	lc := New(fr)
	// A no-worktree session (e.g. buildkite-debug): empty Worktree/Branch.
	tgt := CleanupTarget{ID: "buildkitedebug-a1b2", Repo: "/repo", TmuxSession: "buildkitedebug-a1b2"}
	require.NoError(t, lc.Cleanup(context.Background(), tgt, false))
	require.Contains(t, fr.calledArgs(), []string{"tmux", "kill-session", "-t", "buildkitedebug-a1b2"})
	// No git guard or prune for a session without a worktree.
	for _, argv := range fr.calledArgs() {
		require.NotEqual(t, "git", argv[0], "no-worktree cleanup must not touch git")
	}
}

func cleanupInput(id string) CleanupTarget {
	return CleanupTarget{ID: id, Repo: "/repo", Worktree: ".worktrees/" + id, Branch: id, TmuxSession: id}
}

func TestCleanupGuardAbortsWhenNoUpstream(t *testing.T) {
	// `git log @{u}..` errors when no upstream is configured → treat as unpushed.
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git -C /repo/.worktrees/A-1 status --porcelain":   {Out: ""},
		"git -C /repo/.worktrees/A-1 log @{u}.. --oneline": {Err: errStub("no upstream configured")},
	}}
	err := New(fr).Cleanup(context.Background(), cleanupInput("A-1"), false)
	require.ErrorIs(t, err, ErrUnpushedCommits)
}

func TestSpawnPRReviewWithExplicitBranch(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git worktree list --porcelain": {Out: noOtherWorktrees},
	}}
	s, err := New(fr).Spawn(context.Background(), SpawnRequest{
		Type: store.TypePRReview, Repo: "/repo", PR: "12345", Branch: "feature-x",
	})
	require.NoError(t, err)
	require.Equal(t, "feature-x", s.Branch)
	// Given a branch, pr-review checks it out directly (no detached + gh).
	require.Contains(t, fr.calledArgs(), []string{"git", "worktree", "add", s.Worktree, "feature-x"})
	require.NotContains(t, fr.calledArgs(), []string{"gh", "pr", "checkout", "12345"})
}

func TestInputBracketPastesThenSubmits(t *testing.T) {
	inputSubmitDelay = 0 // no real wait in tests
	fr := &FakeRunner{}
	lc := New(fr)
	require.NoError(t, lc.Input(context.Background(), "A-1", "what is your status?"))
	args := fr.calledArgs()
	// Text is loaded into a per-session buffer and bracketed-pasted (so it is
	// treated as content), then Enter is a SEPARATE keystroke that submits.
	require.Contains(t, args, []string{"tmux", "set-buffer", "-b", "agentctl-input-A-1", "--", "what is your status?"})
	require.Contains(t, args, []string{"tmux", "paste-buffer", "-t", "A-1", "-b", "agentctl-input-A-1", "-p", "-d"})
	require.Contains(t, args, []string{"tmux", "send-keys", "-t", "A-1", "Enter"})

	// The submit Enter must come AFTER the paste.
	paste, enter := -1, -1
	for i, a := range args {
		if len(a) > 1 && a[1] == "paste-buffer" {
			paste = i
		}
		if len(a) > 1 && a[1] == "send-keys" && a[len(a)-1] == "Enter" {
			enter = i
		}
	}
	require.Greater(t, enter, paste, "Enter is sent after the paste, not fused with it")
}

func TestInputMultilineIsPastedAsContentNotEnters(t *testing.T) {
	inputSubmitDelay = 0
	fr := &FakeRunner{}
	require.NoError(t, New(fr).Input(context.Background(), "A-1", "line one\nline two"))
	// The whole multi-line message is one buffer (newlines preserved as content).
	require.Contains(t, fr.calledArgs(), []string{"tmux", "set-buffer", "-b", "agentctl-input-A-1", "--", "line one\nline two"})
	// Exactly one Enter keystroke — the submit — never one per line.
	enters := 0
	for _, a := range fr.calledArgs() {
		if len(a) > 1 && a[1] == "send-keys" && a[len(a)-1] == "Enter" {
			enters++
		}
	}
	require.Equal(t, 1, enters, "multi-line text submits with a single Enter, not one per line")
}

func TestOutputCapturesPane(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"tmux capture-pane -p -t A-1 -S -200": {Out: "line1\nline2\n"},
	}}
	lc := New(fr)
	out, err := lc.Output(context.Background(), "A-1", 200)
	require.NoError(t, err)
	require.Equal(t, "line1\nline2\n", out)
}

func TestShellQuoteArg(t *testing.T) {
	require.Equal(t, `'hi there'`, shellQuoteArg("hi there"))
	require.Equal(t, `'a'\''b'`, shellQuoteArg("a'b"))
	require.Equal(t, "'line1\nline2'", shellQuoteArg("line1\nline2"))
}

func TestParseType(t *testing.T) {
	require.Equal(t, store.TypeDevelopment, parseType("development"))
	require.Equal(t, store.TypePRReview, parseType("pr-review\n"))
	require.Equal(t, store.TypeAnalysis, parseType("This is an analysis task."))
	require.Equal(t, store.TypeBuildkiteDebug, parseType("Label: buildkite-debug"))
	require.Equal(t, store.TypeOther, parseType("I am not sure"))
	require.Equal(t, store.TypeOther, parseType(""))
}

func TestClassifyCallsClaudeP(t *testing.T) {
	prompt := "build a REST API for orders"
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"claude -p " + classifyArg(prompt): {Out: "development\n"},
	}}
	got, err := New(fr).Classify(context.Background(), prompt)
	require.NoError(t, err)
	require.Equal(t, store.TypeDevelopment, got)
	require.Contains(t, fr.calledArgs(), []string{"claude", "-p", classifyArg(prompt)})
}

func TestClassifyDefaultsToOtherOnError(t *testing.T) {
	prompt := "whatever"
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"claude -p " + classifyArg(prompt): {Err: errStub("claude not found")},
	}}
	got, err := New(fr).Classify(context.Background(), prompt)
	require.Error(t, err)
	require.Equal(t, store.TypeOther, got)
}

func TestSpawnPromptModePerAgentWorkdir(t *testing.T) {
	fr := &FakeRunner{}
	prompt := "research SSE reconnection semantics"
	s, err := New(fr).Spawn(context.Background(), SpawnRequest{Prompt: prompt, Workdir: "/home/me/agentctl-agents"})
	require.NoError(t, err)
	expDir := "/home/me/agentctl-agents/" + s.ID
	require.Equal(t, expDir, s.Workdir, "per-agent subdir")
	require.Equal(t, prompt, firstWordsExpand(s.Subject, prompt), "subject seeded from prompt")
	// Creates the dir and starts tmux there.
	require.Contains(t, fr.calledArgs(), []string{"mkdir", "-p", expDir})
	require.Contains(t, fr.calledArgs(), []string{"tmux", "new-session", "-d", "-s", s.ID, "-c", expDir})
}

// firstWordsExpand is a test helper: the seeded subject is firstWords(prompt,10);
// for a <=10-word prompt that equals the prompt, so this just returns prompt when they match.
func firstWordsExpand(subject, prompt string) string {
	if subject == firstWords(prompt, 10) {
		return prompt
	}
	return subject
}

func TestSpawnTypedModeRecordsWorkdir(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git worktree list --porcelain": {Out: noOtherWorktrees},
	}}
	s, err := New(fr).Spawn(context.Background(), SpawnRequest{Type: store.TypeDevelopment, Ticket: "A-1", Repo: "/repo"})
	require.NoError(t, err)
	require.Equal(t, "/repo/.worktrees/A-1", s.Workdir, "typed worktree dir recorded")
}

func TestSpawnNoWorktreeTypeRecordsRepoWorkdir(t *testing.T) {
	fr := &FakeRunner{}
	s, err := New(fr).Spawn(context.Background(), SpawnRequest{Type: store.TypeBuildkiteDebug, Repo: "/repo"})
	require.NoError(t, err)
	require.Equal(t, "/repo", s.Workdir, "no-worktree type runs in repo")
}

func TestSpawnPromptModeNoWorktree(t *testing.T) {
	fr := &FakeRunner{}
	prompt := "research how SSE reconnection works"
	s, err := New(fr).Spawn(context.Background(), SpawnRequest{
		Prompt: prompt, Workdir: "/home/me/agentctl-agents",
	})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(s.ID, "agent-"), "got %q", s.ID)
	require.Equal(t, store.Type(""), s.Type, "type empty until classified")
	require.Empty(t, s.Worktree)
	require.Empty(t, s.Repo)
	require.Equal(t, prompt, s.Prompt)
	// No git at all for a prompt-spawned agent.
	for _, argv := range fr.calledArgs() {
		require.NotEqual(t, "git", argv[0], "prompt mode must not touch git")
	}
	// Per-agent subdir created and used for tmux.
	expDir := "/home/me/agentctl-agents/" + s.ID
	require.Equal(t, expDir, s.Workdir, "per-agent subdir recorded")
	require.Contains(t, fr.calledArgs(), []string{"mkdir", "-p", expDir})
	require.Contains(t, fr.calledArgs(), []string{"tmux", "new-session", "-d", "-s", s.ID, "-c", expDir})
	// The prompt is written to a file, then claude is launched reading it back.
	promptFile := expDir + "/" + promptFileName
	require.Contains(t, fr.calledArgs(), []string{"sh", "-c", `printf '%s' "$1" > "$2"`, "sh", prompt, promptFile})
	launch := claudeLaunch(s.ClaudeSessionID, s.ID) + ` "$(cat ` + shellQuoteArg(promptFile) + `)"`
	require.Contains(t, fr.calledArgs(), []string{"tmux", "send-keys", "-t", s.ID, launch, "Enter"})
}

func TestReadFileTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.txt")

	// Smaller than maxBytes → whole file.
	require.NoError(t, os.WriteFile(path, []byte("hello world"), 0o644))
	require.Equal(t, "hello world", readFileTail(path, 4000))

	// Larger than maxBytes → only the trailing maxBytes.
	require.NoError(t, os.WriteFile(path, append(bytes.Repeat([]byte("A"), 100), []byte("TAIL")...), 0o644))
	got := readFileTail(path, 4)
	require.Equal(t, "TAIL", got)

	// Missing file → "".
	require.Equal(t, "", readFileTail(filepath.Join(dir, "nope"), 4000))
}

func TestNewestTranscriptTailPicksNewestAndTails(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "old.jsonl"), []byte("OLD"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ignore.txt"), []byte("ZZZ"), 0o644)) // non-jsonl ignored
	newf := filepath.Join(dir, "new.jsonl")
	require.NoError(t, os.WriteFile(newf, []byte("XXXXXXXXNEWEST"), 0o644))
	future := time.Now().Add(time.Minute)
	require.NoError(t, os.Chtimes(newf, future, future)) // make it strictly newest

	require.Equal(t, "NEWEST", newestTranscriptTail(dir, 6), "tail of the newest .jsonl")
	require.Equal(t, "", newestTranscriptTail(t.TempDir(), 100), "empty dir → no transcript")
}

func TestSpawnPromptModeMultilinePromptIsFileBacked(t *testing.T) {
	fr := &FakeRunner{}
	prompt := "line one\nline two with a ' quote\nline three"
	s, err := New(fr).Spawn(context.Background(), SpawnRequest{Prompt: prompt, Workdir: "/w"})
	require.NoError(t, err)

	promptFile := "/w/" + s.ID + "/" + promptFileName
	// Prompt written verbatim via an exec arg — no shell interpolation/escaping.
	require.Contains(t, fr.calledArgs(), []string{"sh", "-c", `printf '%s' "$1" > "$2"`, "sh", prompt, promptFile})

	// The launch line is a single physical line; the multi-line prompt is read
	// back via $(cat …) so no embedded newline is ever typed into the pane.
	launch := claudeLaunch(s.ClaudeSessionID, s.ID) + ` "$(cat ` + shellQuoteArg(promptFile) + `)"`
	require.Contains(t, fr.calledArgs(), []string{"tmux", "send-keys", "-t", s.ID, launch, "Enter"})
	require.NotContains(t, launch, "\n", "the typed launch command must never contain a raw newline")
}

func TestSummarizeUsesTranscriptThenClaudeP(t *testing.T) {
	root := t.TempDir()
	workdir := "/Users/me/agentctl-agents/agent-zz99"
	projDir := claudeProjectDir(root, workdir)
	require.NoError(t, os.MkdirAll(projDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(projDir, "sess.jsonl"),
		[]byte(`{"role":"user","text":"look into the auth bug"}`+"\n"), 0o644))

	fr := &FakeRunner{Responses: map[string]FakeResp{}}
	lc := New(fr)
	lc.ProjectsDir = root
	// The claude -p call is keyed by the transcript-derived text.
	sess := &store.Session{ID: "agent-zz99", TmuxSession: "agent-zz99", Workdir: workdir}
	// Stub claude -p for whatever arg is built from the transcript text:
	fr.Responses["claude -p "+summaryArg(`{"role":"user","text":"look into the auth bug"}`+"\n")] = FakeResp{Out: "looking into the auth bug\n"}

	got, err := lc.Summarize(context.Background(), sess)
	require.NoError(t, err)
	require.Equal(t, "looking into the auth bug", got)
}

func TestSummarizeFallsBackToPane(t *testing.T) {
	root := t.TempDir() // empty → no transcript
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"tmux capture-pane -p -t agent-aa11 -S -40":              {Out: "building the REST handler\n"},
		"claude -p " + summaryArg("building the REST handler\n"): {Out: "building a REST handler"},
	}}
	lc := New(fr)
	lc.ProjectsDir = root
	sess := &store.Session{ID: "agent-aa11", TmuxSession: "agent-aa11", Workdir: "/Users/me/agentctl-agents/agent-aa11"}
	got, err := lc.Summarize(context.Background(), sess)
	require.NoError(t, err)
	require.Equal(t, "building a REST handler", got)
}

func TestTranscriptPathBySessionIDBeatsNewest(t *testing.T) {
	root := t.TempDir()
	workdir := "/Users/me/agentctl-agents/agent-zz99"
	dir := claudeProjectDir(root, workdir)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	sid := "33333333-3333-4333-8333-333333333333"
	want := filepath.Join(dir, sid+".jsonl")
	require.NoError(t, os.WriteFile(want, []byte("HELLO"), 0o644))
	// A decoy with a newer mtime that the legacy heuristic would wrongly pick.
	decoy := filepath.Join(dir, "decoy.jsonl")
	require.NoError(t, os.WriteFile(decoy, []byte("DECOY"), 0o644))
	future := time.Now().Add(time.Minute)
	require.NoError(t, os.Chtimes(decoy, future, future))

	lc := New(&FakeRunner{})
	lc.ProjectsDir = root
	sess := &store.Session{ID: "agent-zz99", Workdir: workdir, ClaudeSessionID: sid}
	require.Equal(t, want, lc.transcriptPath(sess), "pinned id beats newest-mtime decoy")
}

func TestTranscriptPathGlobFallback(t *testing.T) {
	root := t.TempDir()
	sid := "44444444-4444-4444-8444-444444444444"
	// The transcript lives under a project dir that does NOT match the workdir
	// encoding (simulates the /tmp -> /private/tmp path-resolution mismatch).
	other := filepath.Join(root, "-some-other-encoded-dir")
	require.NoError(t, os.MkdirAll(other, 0o755))
	want := filepath.Join(other, sid+".jsonl")
	require.NoError(t, os.WriteFile(want, []byte("X"), 0o644))

	lc := New(&FakeRunner{})
	lc.ProjectsDir = root
	sess := &store.Session{ID: "agent-x", Workdir: "/mismatch/dir", ClaudeSessionID: sid}
	require.Equal(t, want, lc.transcriptPath(sess), "unique glob finds it despite dir mismatch")
}

func TestTranscriptPathLegacyFallsBackToNewest(t *testing.T) {
	root := t.TempDir()
	workdir := "/Users/me/agentctl-agents/agent-leg"
	dir := claudeProjectDir(root, workdir)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "old.jsonl"), []byte("OLD"), 0o644))
	newf := filepath.Join(dir, "new.jsonl")
	require.NoError(t, os.WriteFile(newf, []byte("NEW"), 0o644))
	future := time.Now().Add(time.Minute)
	require.NoError(t, os.Chtimes(newf, future, future))

	lc := New(&FakeRunner{})
	lc.ProjectsDir = root
	sess := &store.Session{ID: "agent-leg", Workdir: workdir} // no ClaudeSessionID
	require.Equal(t, newf, lc.transcriptPath(sess), "empty id -> newest .jsonl (legacy)")
}

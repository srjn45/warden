package lifecycle

import (
	"bytes"
	"context"
	"errors"
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

func TestSpawnRemovesCreatedWorktreeWhenTmuxFails(t *testing.T) {
	// new-session fails AFTER we created the worktree → the worktree we created
	// must be force-removed so a failed spawn doesn't leak it.
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git worktree list --porcelain": {Out: noOtherWorktrees},
		"tmux new-session -d -s PROJ-350 -c /repo/.worktrees/PROJ-350": {Err: errStub("tmux boom")},
	}}
	_, err := New(fr).Spawn(context.Background(), SpawnRequest{
		Type: store.TypeDevelopment, Ticket: "PROJ-350", Repo: "/repo",
	})
	require.Error(t, err)
	require.Contains(t, fr.calledArgs(),
		[]string{"git", "-C", "/repo", "worktree", "remove", "--force", ".worktrees/PROJ-350"},
		"a worktree we created must be rolled back on spawn failure")
}

func TestSpawnDoesNotRemoveAdoptedWorktreeOnFailure(t *testing.T) {
	// The worktree already existed (adopted, not created). A spawn failure must
	// NOT delete the user's pre-existing worktree.
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git worktree list --porcelain": {Out: noOtherWorktrees + "\nworktree /repo/.worktrees/PROJ-350\nHEAD def\nbranch refs/heads/PROJ-350\n"},
		"tmux new-session -d -s PROJ-350 -c /repo/.worktrees/PROJ-350": {Err: errStub("tmux boom")},
	}}
	_, err := New(fr).Spawn(context.Background(), SpawnRequest{
		Type: store.TypeDevelopment, Ticket: "PROJ-350", Repo: "/repo",
	})
	require.Error(t, err)
	require.NotContains(t, fr.calledArgs(),
		[]string{"git", "-C", "/repo", "worktree", "remove", "--force", ".worktrees/PROJ-350"},
		"an adopted worktree must not be removed on spawn failure")
}

func TestSpawnKillsSessionWhenSendKeysFails(t *testing.T) {
	// send-keys fails AFTER the tmux session was created → the orphaned session
	// must be killed so it doesn't linger beyond reach of the store.
	fr := &FakeRunner{FailIf: func(argv []string) error {
		if len(argv) >= 2 && argv[0] == "tmux" && argv[1] == "send-keys" {
			return errStub("send-keys boom")
		}
		return nil
	}}
	s, err := New(fr).Spawn(context.Background(), SpawnRequest{Type: store.TypeBuildkiteDebug, Repo: "/repo"})
	_ = s
	require.Error(t, err)
	killed := false
	for _, argv := range fr.calledArgs() {
		if len(argv) >= 2 && argv[0] == "tmux" && argv[1] == "kill-session" {
			killed = true
		}
	}
	require.True(t, killed, "send-keys failure must kill the orphaned tmux session")
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

// callIndex returns the index of the first recorded call whose argv joins to
// key (space-separated), or -1 if absent. Used for ordering assertions.
func (f *FakeRunner) callIndex(key string) int {
	for i, c := range f.Calls {
		if strings.Join(c.Argv, " ") == key {
			return i
		}
	}
	return -1
}

func cleanupInput(id string) CleanupTarget {
	return CleanupTarget{ID: id, Repo: "/repo", Worktree: ".worktrees/" + id, Branch: id, TmuxSession: id}
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
	// treated as content), then Enter is a SEPARATE keystroke that submits. The
	// -r flag stops paste-buffer translating LF→CR: -p (bracketed paste) only
	// protects newlines when the app requested bracketed-paste mode, so without
	// -r an embedded newline submits early at a non-composer prompt.
	require.Contains(t, args, []string{"tmux", "set-buffer", "-b", "agentctl-input-A-1", "--", "what is your status?"})
	require.Contains(t, args, []string{"tmux", "paste-buffer", "-t", "A-1", "-b", "agentctl-input-A-1", "-p", "-r", "-d"})
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

func TestSpawnPromptModeRequiresCwd(t *testing.T) {
	fr := &FakeRunner{}
	l := New(fr)
	l.PromptsDir = "/state/prompts"
	// A prompt-mode spawn must be given the caller's launch dir; it never
	// silently invents a per-agent directory to run in.
	_, err := l.Spawn(context.Background(), SpawnRequest{Prompt: "do a thing"})
	require.Error(t, err)
	for _, argv := range fr.calledArgs() {
		require.NotEqual(t, "tmux", argv[0], "no tmux session created on validation failure")
	}
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
	l := New(fr)
	l.PromptsDir = "/state/prompts"
	s, err := l.Spawn(context.Background(), SpawnRequest{Prompt: prompt, Cwd: "/work/project"})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(s.ID, "agent-"), "got %q", s.ID)
	require.Equal(t, store.Type(""), s.Type, "type empty until classified")
	require.Empty(t, s.Worktree)
	require.Empty(t, s.Repo)
	require.Equal(t, prompt, s.Prompt)
	require.Equal(t, prompt, firstWordsExpand(s.Subject, prompt), "subject seeded from prompt")
	// No git at all for a prompt-spawned agent.
	for _, argv := range fr.calledArgs() {
		require.NotEqual(t, "git", argv[0], "prompt mode must not touch git")
	}
	// Launches in the caller's cwd; no per-agent directory is ever created.
	require.Equal(t, "/work/project", s.Workdir, "launches in caller cwd")
	require.Contains(t, fr.calledArgs(), []string{"tmux", "new-session", "-d", "-s", s.ID, "-c", "/work/project"})
	for _, argv := range fr.calledArgs() {
		if argv[0] == "mkdir" {
			require.Equal(t, []string{"mkdir", "-p", "/state/prompts"}, argv, "only the shared prompts dir is created")
		}
	}
}

func TestSpawnPromptModeLaunchesFromCwd(t *testing.T) {
	fr := &FakeRunner{}
	prompt := "fix the auth bug"
	l := New(fr)
	l.PromptsDir = "/state/prompts"
	s, err := l.Spawn(context.Background(), SpawnRequest{Prompt: prompt, Cwd: "/work/project"})
	require.NoError(t, err)

	// Claude launches from the caller's cwd.
	require.Equal(t, "/work/project", s.Workdir, "sess.Workdir is the caller cwd")
	require.Contains(t, fr.calledArgs(), []string{"tmux", "new-session", "-d", "-s", s.ID, "-c", "/work/project"})
	// The prompt file lives in the shared state dir, keyed by agent id — never
	// in the caller's project and never in a per-agent directory.
	promptFile := "/state/prompts/" + s.ID
	require.Contains(t, fr.calledArgs(), []string{"mkdir", "-p", "/state/prompts"})
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
	l := New(fr)
	l.PromptsDir = "/state/prompts"
	s, err := l.Spawn(context.Background(), SpawnRequest{Prompt: prompt, Cwd: "/work/project"})
	require.NoError(t, err)

	promptFile := "/state/prompts/" + s.ID
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

func TestRestoreRecreatesAndResumes(t *testing.T) {
	root := t.TempDir()
	workdir := t.TempDir()
	sid := "66666666-6666-4666-8666-666666666666"
	pdir := claudeProjectDir(root, workdir)
	require.NoError(t, os.MkdirAll(pdir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(pdir, sid+".jsonl"), []byte("{}"), 0o644))

	fr := &FakeRunner{Responses: map[string]FakeResp{
		"tmux has-session -t agent-r1": {Err: errStub("no session")}, // dead
	}}
	lc := New(fr)
	lc.ProjectsDir = root
	sess := &store.Session{ID: "agent-r1", TmuxSession: "agent-r1", Workdir: workdir, ClaudeSessionID: sid}

	require.NoError(t, lc.Restore(context.Background(), sess))
	require.Contains(t, fr.calledArgs(), []string{"tmux", "new-session", "-d", "-s", "agent-r1", "-c", workdir})
	require.Contains(t, fr.calledArgs(), []string{"tmux", "send-keys", "-t", "agent-r1", claudeResume(sid, "agent-r1"), "Enter"})
}

func TestRestorePreconditionErrors(t *testing.T) {
	sid := "66666666-6666-4666-8666-666666666666"
	dead := func() *FakeRunner {
		return &FakeRunner{Responses: map[string]FakeResp{"tmux has-session -t a": {Err: errStub("dead")}}}
	}

	// no pinned session id (checked before any tmux call)
	require.ErrorIs(t, New(&FakeRunner{}).Restore(context.Background(),
		&store.Session{ID: "a", TmuxSession: "a", Workdir: t.TempDir()}), ErrNoSessionID)

	// already running: has-session succeeds (FakeRunner default = success = alive)
	require.ErrorIs(t, New(&FakeRunner{}).Restore(context.Background(),
		&store.Session{ID: "a", TmuxSession: "a", Workdir: t.TempDir(), ClaudeSessionID: sid}), ErrAlreadyRunning)

	// workdir gone
	require.ErrorIs(t, New(dead()).Restore(context.Background(),
		&store.Session{ID: "a", TmuxSession: "a", Workdir: "/no/such/dir", ClaudeSessionID: sid}), ErrWorkdirMissing)

	// no transcript: dead, workdir exists, empty ProjectsDir
	lc := New(dead())
	lc.ProjectsDir = t.TempDir()
	require.ErrorIs(t, lc.Restore(context.Background(),
		&store.Session{ID: "a", TmuxSession: "a", Workdir: t.TempDir(), ClaudeSessionID: sid}), ErrNoTranscript)
}

func TestTerminateKillsTmuxOnly(t *testing.T) {
	fr := &FakeRunner{}
	require.NoError(t, New(fr).Terminate(context.Background(), "A-1"))
	require.Contains(t, fr.calledArgs(), []string{"tmux", "kill-session", "-t", "A-1"})
	for _, a := range fr.calledArgs() {
		require.NotEqual(t, "git", a[0], "terminate touches no git")
	}
}

func TestRemoveWorktreeRefusesIfAlive(t *testing.T) {
	// has-session succeeds (FakeRunner default) → agent alive → refuse.
	fr := &FakeRunner{}
	err := New(fr).RemoveWorktree(context.Background(), cleanupInput("A-1"), false)
	require.ErrorIs(t, err, ErrWorktreeAgentAlive)
	for _, a := range fr.calledArgs() {
		require.NotEqual(t, "git", a[0], "must not touch git while the agent is alive")
	}
}

func TestRemoveWorktreeGuardsDirty(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"tmux has-session -t A-1":                        {Err: errStub("dead")},
		"git -C /repo/.worktrees/A-1 status --porcelain": {Out: " M f.go\n"},
	}}
	require.ErrorIs(t, New(fr).RemoveWorktree(context.Background(), cleanupInput("A-1"), false), ErrDirtyWorktree)
}

func TestRemoveWorktreeForceProceeds(t *testing.T) {
	fr := &FakeRunner{} // has-session would say alive, but force skips the checks
	require.NoError(t, New(fr).RemoveWorktree(context.Background(), cleanupInput("A-1"), true))
	require.Contains(t, fr.calledArgs(), []string{"git", "-C", "/repo", "worktree", "remove", "--force", ".worktrees/A-1"})
	require.Contains(t, fr.calledArgs(), []string{"git", "-C", "/repo", "branch", "-D", "A-1"})
}

func TestRemoveWorktreeCleanProceeds(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"tmux has-session -t A-1":                          {Err: errStub("dead")},
		"git -C /repo/.worktrees/A-1 status --porcelain":   {Out: ""},
		"git -C /repo/.worktrees/A-1 log @{u}.. --oneline": {Out: ""},
	}}
	require.NoError(t, New(fr).RemoveWorktree(context.Background(), cleanupInput("A-1"), false))
	require.Contains(t, fr.calledArgs(), []string{"git", "-C", "/repo", "worktree", "remove", ".worktrees/A-1"})
}

func TestRemoveWorktreeNoWorktreeErrors(t *testing.T) {
	tgt := CleanupTarget{ID: "x", TmuxSession: "x"} // no Worktree
	require.ErrorIs(t, New(&FakeRunner{}).RemoveWorktree(context.Background(), tgt, false), ErrNoWorktree)
}

func TestNewestClaudeSession(t *testing.T) {
	root := t.TempDir()
	workdir := t.TempDir()
	pdir := claudeProjectDir(root, workdir)
	require.NoError(t, os.MkdirAll(pdir, 0o755))
	older := "11111111-1111-4111-8111-111111111111"
	newer := "22222222-2222-4222-8222-222222222222"
	require.NoError(t, os.WriteFile(filepath.Join(pdir, older+".jsonl"), []byte("{}"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(pdir, newer+".jsonl"), []byte("{}"), 0o644))
	// Make `newer` the most recently modified.
	future := time.Now().Add(time.Minute)
	require.NoError(t, os.Chtimes(filepath.Join(pdir, newer+".jsonl"), future, future))

	lc := New(&FakeRunner{})
	lc.ProjectsDir = root
	got, err := lc.NewestClaudeSession(workdir)
	require.NoError(t, err)
	require.Equal(t, newer, got)
}

func TestNewestClaudeSessionNone(t *testing.T) {
	lc := New(&FakeRunner{})
	lc.ProjectsDir = t.TempDir() // exists but empty
	_, err := lc.NewestClaudeSession(t.TempDir())
	require.ErrorIs(t, err, ErrNoTranscript)

	lc2 := New(&FakeRunner{}) // ProjectsDir empty → disabled
	_, err = lc2.NewestClaudeSession(t.TempDir())
	require.ErrorIs(t, err, ErrNoTranscript)
}

func TestAdoptResumeMode(t *testing.T) {
	workdir := t.TempDir()
	sid := "33333333-3333-4333-8333-333333333333"
	fr := &FakeRunner{}
	lc := New(fr)
	sess, err := lc.Adopt(context.Background(), AdoptRequest{
		ID: "agent-a1", Cwd: workdir, ClaudeSessionID: sid, TmuxSession: "",
	})
	require.NoError(t, err)
	require.Equal(t, "agent-a1", sess.ID)
	require.Equal(t, "agent-a1", sess.TmuxSession)
	require.Equal(t, sid, sess.ClaudeSessionID)
	require.Equal(t, store.TypeOther, sess.Type)
	require.Equal(t, store.StatusSpawning, sess.Status)
	require.Equal(t, workdir, sess.Workdir)
	require.Contains(t, fr.calledArgs(), []string{"tmux", "new-session", "-d", "-s", "agent-a1", "-c", workdir})
	require.Contains(t, fr.calledArgs(), []string{"tmux", "send-keys", "-t", "agent-a1", claudeResume(sid, "agent-a1"), "Enter"})
}

func TestAdoptResumeGeneratesID(t *testing.T) {
	sess, err := New(&FakeRunner{}).Adopt(context.Background(), AdoptRequest{
		ID: "", Cwd: t.TempDir(), ClaudeSessionID: "x", TmuxSession: "",
	})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(sess.ID, "agent-"), "generated id, got %q", sess.ID)
	require.Equal(t, sess.ID, sess.TmuxSession)
}

func TestAdoptResumeNoClaudeID(t *testing.T) {
	_, err := New(&FakeRunner{}).Adopt(context.Background(), AdoptRequest{
		ID: "agent-a1", Cwd: t.TempDir(), ClaudeSessionID: "", TmuxSession: "",
	})
	require.ErrorIs(t, err, ErrNoSessionID)
}

func TestAdoptResumeWorkdirMissing(t *testing.T) {
	_, err := New(&FakeRunner{}).Adopt(context.Background(), AdoptRequest{
		ID: "agent-a1", Cwd: "/no/such/dir", ClaudeSessionID: "x", TmuxSession: "",
	})
	require.ErrorIs(t, err, ErrWorkdirMissing)
}

func TestAdoptLiveKeepsName(t *testing.T) {
	// FakeRunner default = success → has-session succeeds → tmux session alive.
	fr := &FakeRunner{}
	sess, err := New(fr).Adopt(context.Background(), AdoptRequest{
		ID: "work", Cwd: t.TempDir(), ClaudeSessionID: "x", TmuxSession: "work",
	})
	require.NoError(t, err)
	require.Equal(t, "work", sess.ID)
	require.Equal(t, "work", sess.TmuxSession)
	require.Equal(t, store.StatusWorking, sess.Status)
	require.Contains(t, fr.calledArgs(), []string{"tmux", "has-session", "-t", "work"})
	for _, a := range fr.calledArgs() {
		require.NotEqual(t, "rename-session", argAt(a, 1), "no rename when id == tmux name")
		require.NotEqual(t, "new-session", argAt(a, 1), "live adopt never relaunches")
	}
}

func TestAdoptLiveRenamesWhenIDDiffers(t *testing.T) {
	fr := &FakeRunner{}
	sess, err := New(fr).Adopt(context.Background(), AdoptRequest{
		ID: "agent-b2", Cwd: t.TempDir(), ClaudeSessionID: "x", TmuxSession: "0",
	})
	require.NoError(t, err)
	require.Equal(t, "agent-b2", sess.ID)
	require.Equal(t, "agent-b2", sess.TmuxSession)
	require.Contains(t, fr.calledArgs(), []string{"tmux", "rename-session", "-t", "0", "agent-b2"})
}

func TestAdoptLiveTmuxGone(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"tmux has-session -t ghost": {Err: errStub("no session")},
	}}
	_, err := New(fr).Adopt(context.Background(), AdoptRequest{
		ID: "ghost", Cwd: t.TempDir(), ClaudeSessionID: "x", TmuxSession: "ghost",
	})
	require.ErrorIs(t, err, ErrTmuxGone)
}

// argAt returns a[i] or "" when out of range — for asserting tmux subcommands.
func argAt(a []string, i int) string {
	if i < len(a) {
		return a[i]
	}
	return ""
}

func TestSpawnSetsMouseOnAgentSession(t *testing.T) {
	fr := &FakeRunner{}
	s, err := New(fr).Spawn(context.Background(), SpawnRequest{Type: store.TypeBuildkiteDebug, Repo: "/repo"})
	require.NoError(t, err)
	require.Contains(t, fr.calledArgs(), []string{"tmux", "set-option", "-t", s.ID, "mouse", "on"})
	require.Greater(t,
		fr.callIndex("tmux set-option -t "+s.ID+" mouse on"),
		fr.callIndex("tmux new-session -d -s "+s.ID+" -c /repo"),
		"mouse on must be set after new-session")
}

func TestSpawnPromptModeSetsMouseOn(t *testing.T) {
	fr := &FakeRunner{}
	l := New(fr)
	l.PromptsDir = "/state/prompts"
	s, err := l.Spawn(context.Background(), SpawnRequest{Prompt: "do a thing", Cwd: "/work/project"})
	require.NoError(t, err)
	require.Contains(t, fr.calledArgs(), []string{"tmux", "set-option", "-t", s.ID, "mouse", "on"})
	require.Greater(t,
		fr.callIndex("tmux set-option -t "+s.ID+" mouse on"),
		fr.callIndex("tmux new-session -d -s "+s.ID+" -c /work/project"),
		"mouse on must follow new-session")
}

func TestResumeInTmuxSetsMouseOn(t *testing.T) {
	fr := &FakeRunner{}
	err := New(fr).resumeInTmux(context.Background(), "ag1", "/cwd", "claude-id")
	require.NoError(t, err)
	require.Contains(t, fr.calledArgs(), []string{"tmux", "set-option", "-t", "ag1", "mouse", "on"})
	require.Greater(t,
		fr.callIndex("tmux set-option -t ag1 mouse on"),
		fr.callIndex("tmux new-session -d -s ag1 -c /cwd"),
		"mouse on must follow new-session")
}

func TestSpawnRaisesHistoryLimitBeforeNewSession(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"tmux show-options -g -v history-limit": {Out: "2000"},
	}}
	s, err := New(fr).Spawn(context.Background(), SpawnRequest{Type: store.TypeBuildkiteDebug, Repo: "/repo"})
	require.NoError(t, err)
	setIdx := fr.callIndex("tmux set-option -g history-limit 50000")
	newIdx := fr.callIndex("tmux new-session -d -s " + s.ID + " -c /repo")
	require.NotEqual(t, -1, setIdx, "history-limit must be raised when current is lower")
	require.Less(t, setIdx, newIdx, "history-limit must be raised BEFORE new-session")
}

func TestSpawnRaisesHistoryLimitWhenCurrentUnreadable(t *testing.T) {
	// show-options failing (e.g. no value set / older tmux) must fall through to
	// raising the limit — a safe default, not a silent skip.
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"tmux show-options -g -v history-limit": {Err: errors.New("no server")},
	}}
	_, err := New(fr).Spawn(context.Background(), SpawnRequest{Type: store.TypeBuildkiteDebug, Repo: "/repo"})
	require.NoError(t, err)
	require.Contains(t, fr.calledArgs(), []string{"tmux", "set-option", "-g", "history-limit", "50000"},
		"unreadable current limit must fall through to raising it")
}

func TestSpawnDoesNotLowerHistoryLimit(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"tmux show-options -g -v history-limit": {Out: "100000"},
	}}
	_, err := New(fr).Spawn(context.Background(), SpawnRequest{Type: store.TypeBuildkiteDebug, Repo: "/repo"})
	require.NoError(t, err)
	require.NotContains(t, fr.calledArgs(), []string{"tmux", "set-option", "-g", "history-limit", "50000"},
		"a larger user-configured history-limit must be left untouched")
}

func TestSpawnSucceedsWhenHistoryLimitSetFails(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"tmux set-option -g history-limit 50000": {Err: errors.New("boom")},
	}}
	_, err := New(fr).Spawn(context.Background(), SpawnRequest{Type: store.TypeBuildkiteDebug, Repo: "/repo"})
	require.NoError(t, err, "history-limit failure must not fail the spawn")
}

func TestResumeSucceedsWhenMouseSetFails(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"tmux set-option -t ag1 mouse on": {Err: errors.New("boom")},
	}}
	err := New(fr).resumeInTmux(context.Background(), "ag1", "/cwd", "cid")
	require.NoError(t, err, "mouse-on failure must not fail the resume")
}

func TestResumeFailsWhenNewSessionFails(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"tmux new-session -d -s ag1 -c /cwd": {Err: errors.New("boom")},
	}}
	err := New(fr).resumeInTmux(context.Background(), "ag1", "/cwd", "cid")
	require.Error(t, err, "new-session failure stays fatal")
}

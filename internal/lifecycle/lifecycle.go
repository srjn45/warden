package lifecycle

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/srajanpathak/agentctl/internal/store"
)

// claudeCallTimeout bounds every headless `claude -p` invocation (classify /
// summarize). Without it a stuck CLI would block its caller indefinitely — in
// particular the poller, which runs Summarize inline on its lifetime context.
const claudeCallTimeout = 30 * time.Second

// runClaudeP runs `claude -p <arg>` with a bounded timeout derived from ctx.
func (l *Lifecycle) runClaudeP(ctx context.Context, arg string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, claudeCallTimeout)
	defer cancel()
	return l.run.Run(cctx, "", "claude", "-p", arg)
}

// claudeCmd is launched in every spawned session. Agents run unattended
// (design §4): permission prompts are skipped; the Notification hook still
// records when one *would* have prompted.
const claudeCmd = "claude --dangerously-skip-permissions"

// claudeLaunch builds the claude invocation for a spawned agent: the base
// command plus a pinned --session-id (deterministic transcript + future
// --resume) and a --name display label equal to the agent id, so the agent id,
// tmux session, and claude session all read the same. sessionID is a generated
// UUID (safe charset); name is the agent id (may be a ticket key) so it is quoted.
func claudeLaunch(sessionID, name string) string {
	return claudeCmd + " --session-id " + sessionID + " --name " + shellQuoteArg(name)
}

// claudeResume builds the invocation that resumes an existing agent conversation
// by its pinned session id (continues the same transcript). --name re-applies the
// display label so the resumed session still reads as the agent id.
func claudeResume(sessionID, name string) string {
	return claudeCmd + " --resume " + sessionID + " --name " + shellQuoteArg(name)
}

// promptFileName is where a prompt-spawned agent's initial prompt is written
// inside its workdir, so the launch line can read it back via "$(cat …)".
const promptFileName = ".agentctl-prompt"

// classifyInstruction is prepended to the task prompt for headless classification.
const classifyInstruction = "You are a classifier. Classify the following agent task into exactly one of these labels: development, analysis, spike, pr-review, buildkite-debug, test-run, env-test, other. Reply with ONLY the label, nothing else.\n\nTask: "

// classifyArg builds the single argument passed to `claude -p`.
func classifyArg(prompt string) string { return classifyInstruction + prompt }

const summaryInstruction = "In 8 words or fewer, summarize what this agent is currently working on. Reply with ONLY the phrase — no quotes, no preamble.\n\nRecent activity:\n"

// firstWords returns the first n whitespace-separated words of s, appending an
// ellipsis when truncated. Used to seed a subject from the prompt (no LLM call).
func firstWords(s string, n int) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	if len(fields) <= n {
		return strings.Join(fields, " ")
	}
	return strings.Join(fields[:n], " ") + "…"
}

// parseSummary cleans an LLM summary reply into a single short line.
func parseSummary(out string) string {
	line := strings.TrimSpace(out)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(line)
	line = strings.Trim(line, "\"'`")
	line = strings.Join(strings.Fields(line), " ")
	// Cap by runes, not bytes, so a multi-byte rune is never sliced in half.
	if r := []rune(line); len(r) > 80 {
		line = strings.TrimSpace(string(r[:80]))
	}
	return line
}

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]`)

// claudeProjectDir maps an absolute workdir to its Claude Code transcript
// project directory under root (Claude replaces non-alphanumerics with '-').
// Returns "" when root is empty (transcript lookup disabled).
func claudeProjectDir(root, workdir string) string {
	if root == "" || workdir == "" {
		return ""
	}
	return filepath.Join(root, nonAlnum.ReplaceAllString(workdir, "-"))
}

func summaryArg(text string) string {
	const max = 4000
	if len(text) > max {
		text = text[len(text)-max:]
	}
	// text may be a byte-sliced tail (here or from readFileTail) that begins
	// mid-rune; drop the leading partial rune so the model gets valid UTF-8.
	for len(text) > 0 && !utf8.RuneStart(text[0]) {
		text = text[1:]
	}
	return summaryInstruction + text
}

// parseType extracts the first known type label from a model's free-form reply.
func parseType(out string) store.Type {
	for _, raw := range strings.Fields(strings.ToLower(out)) {
		tok := strings.Trim(raw, ".,:;'\"`*()[]")
		if t := store.NormalizeType(tok); t != store.TypeOther || tok == "other" {
			return t
		}
	}
	return store.TypeOther
}

// shellQuoteArg single-quotes s for safe inclusion in a shell command line
// typed into a tmux pane (preserves spaces, quotes, and newlines).
func shellQuoteArg(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

type Lifecycle struct {
	run Runner
	// ProjectsDir is the Claude Code transcript root (default empty → transcript
	// lookup disabled; the daemon sets it from config). Overridable in tests.
	ProjectsDir string
}

func New(r Runner) *Lifecycle { return &Lifecycle{run: r} }

// SpawnRequest is the type-aware input to Spawn (design §2 / §6).
type SpawnRequest struct {
	Type     store.Type
	Ticket   string // optional; becomes the id when present
	Repo     string
	Branch   string // optional; development branch / pr-review checkout target
	PR       string // optional; pr-review
	Worktree bool   // analysis/spike opt-in
	Prompt   string // prompt-mode: the agent's initial prompt (no repo/worktree)
	Workdir  string // prompt-mode: working directory for the tmux session
}

func worktreeRel(id string) string { return filepath.Join(".worktrees", id) }

// shortID returns 8 hex chars (4 random bytes) for auto-generated session ids.
// The wider space keeps collisions negligible across many sessions; a failed
// RNG read is surfaced rather than silently yielding an all-zero id.
func shortID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// resolveID uses the ticket when given, else "<typeslug>-<shortid>".
func resolveID(req SpawnRequest) (string, error) {
	if req.Ticket != "" {
		return req.Ticket, nil
	}
	slug := strings.ReplaceAll(string(req.Type), "-", "")
	if slug == "" {
		slug = "agent"
	}
	sid, err := shortID()
	if err != nil {
		return "", err
	}
	return slug + "-" + sid, nil
}

// wantWorktree applies the per-type policy (design §2).
func wantWorktree(req SpawnRequest) bool {
	if req.Type.DefaultWorktree() {
		return true
	}
	return req.Worktree && (req.Type == store.TypeAnalysis || req.Type == store.TypeSpike)
}

// worktreeExists checks `git worktree list --porcelain` for an absolute path.
func (l *Lifecycle) worktreeExists(ctx context.Context, repo, rel string) (bool, error) {
	out, err := l.run.Run(ctx, repo, "git", "worktree", "list", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("git worktree list: %w", err)
	}
	want := filepath.Join(repo, rel)
	for _, line := range strings.Split(out, "\n") {
		if line == "worktree "+want {
			return true, nil
		}
	}
	return false, nil
}

// ensureWorktree creates (or adopts) the worktree and returns the branch name
// recorded on the doc (empty for a detached pr-review checkout).
func (l *Lifecycle) ensureWorktree(ctx context.Context, req SpawnRequest, id, rel string) (string, error) {
	exists, err := l.worktreeExists(ctx, req.Repo, rel)
	if err != nil {
		return "", err
	}
	if exists { // adopt
		if req.Branch != "" {
			return req.Branch, nil
		}
		return id, nil
	}
	if req.Type == store.TypePRReview && req.Branch == "" {
		// Detached worktree, then let gh resolve + fetch the PR branch.
		if out, err := l.run.Run(ctx, req.Repo, "git", "worktree", "add", "--detach", rel); err != nil {
			return "", fmt.Errorf("git worktree add --detach: %w: %s", err, out)
		}
		abs := filepath.Join(req.Repo, rel)
		if out, err := l.run.Run(ctx, abs, "gh", "pr", "checkout", req.PR); err != nil {
			return "", fmt.Errorf("gh pr checkout: %w: %s", err, out)
		}
		return "", nil
	}
	if req.Type == store.TypePRReview { // checkout the given existing branch
		if out, err := l.run.Run(ctx, req.Repo, "git", "worktree", "add", rel, req.Branch); err != nil {
			return "", fmt.Errorf("git worktree add: %w: %s", err, out)
		}
		return req.Branch, nil
	}
	// development / opt-in analysis|spike → new branch (branch = req.Branch or id).
	branch := req.Branch
	if branch == "" {
		branch = id
	}
	if out, err := l.run.Run(ctx, req.Repo, "git", "worktree", "add", rel, "-b", branch); err != nil {
		return "", fmt.Errorf("git worktree add: %w: %s", err, out)
	}
	return branch, nil
}

// Classify asks the same Claude (headless) to label a task prompt. On any error
// it returns TypeOther alongside the error so callers can fall back gracefully.
func (l *Lifecycle) Classify(ctx context.Context, prompt string) (store.Type, error) {
	out, err := l.runClaudeP(ctx, classifyArg(prompt))
	if err != nil {
		return store.TypeOther, fmt.Errorf("claude -p: %w: %s", err, out)
	}
	return parseType(out), nil
}

// Summarize produces a one-line subject for an agent: it reads recent activity
// (transcript, else pane) and asks claude -p for an <=8-word phrase.
func (l *Lifecycle) Summarize(ctx context.Context, sess *store.Session) (string, error) {
	text := l.recentActivity(ctx, sess)
	if strings.TrimSpace(text) == "" {
		text = sess.Prompt // last resort: the original prompt
	}
	if strings.TrimSpace(text) == "" {
		return "", nil
	}
	out, err := l.runClaudeP(ctx, summaryArg(text))
	if err != nil {
		return "", fmt.Errorf("claude -p: %w: %s", err, out)
	}
	return parseSummary(out), nil
}

// transcriptPath resolves the agent's claude transcript file. With a pinned
// ClaudeSessionID the file is exactly <id>.jsonl: look under the encoded project
// dir first, then an unambiguous glob across all project dirs (the UUID is
// globally unique, so this is robust to cwd path-encoding quirks). With no
// pinned id (legacy sessions) it falls back to the newest .jsonl in the dir.
func (l *Lifecycle) transcriptPath(sess *store.Session) string {
	if sess.ClaudeSessionID != "" {
		if dir := claudeProjectDir(l.ProjectsDir, sess.Workdir); dir != "" {
			p := filepath.Join(dir, sess.ClaudeSessionID+".jsonl")
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
		if l.ProjectsDir != "" {
			if m, _ := filepath.Glob(filepath.Join(l.ProjectsDir, "*", sess.ClaudeSessionID+".jsonl")); len(m) == 1 {
				return m[0]
			}
		}
		return "" // pinned but not written yet -> caller falls back to the pane
	}
	if dir := claudeProjectDir(l.ProjectsDir, sess.Workdir); dir != "" {
		return newestTranscriptPath(dir)
	}
	return ""
}

// recentActivity returns recent conversation text: the tail of the agent's
// transcript file (by pinned session id or newest .jsonl), else the tmux pane.
func (l *Lifecycle) recentActivity(ctx context.Context, sess *store.Session) string {
	if p := l.transcriptPath(sess); p != "" {
		if txt := readFileTail(p, 4000); txt != "" {
			return txt
		}
	}
	out, err := l.run.Run(ctx, "", "tmux", "capture-pane", "-p", "-t", sess.TmuxSession, "-S", "-40")
	if err != nil {
		return ""
	}
	return out
}

// newestTranscriptPath returns the path of the most recently modified *.jsonl in
// dir, or "" if none.
func newestTranscriptPath(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	type f struct {
		path string
		mod  int64
	}
	var files []f
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, f{filepath.Join(dir, e.Name()), info.ModTime().UnixNano()})
	}
	if len(files) == 0 {
		return ""
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod > files[j].mod })
	return files[0].path
}

// newestTranscriptTail returns up to maxBytes from the end of the most recently
// modified *.jsonl file in dir, or "" if none. (readFileTail("") is a safe "".)
func newestTranscriptTail(dir string, maxBytes int64) string {
	return readFileTail(newestTranscriptPath(dir), maxBytes)
}

// readFileTail returns up to maxBytes from the end of the file at path, or ""
// on error. It seeks to the tail rather than reading the whole file: transcripts
// grow to many megabytes and the summarizer only needs the last few KB, so this
// bounds memory to ~maxBytes regardless of file size.
func readFileTail(path string, maxBytes int64) string {
	fh, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer fh.Close()
	info, err := fh.Stat()
	if err != nil {
		return ""
	}
	if start := info.Size() - maxBytes; start > 0 {
		if _, err := fh.Seek(start, io.SeekStart); err != nil {
			return ""
		}
	}
	data, err := io.ReadAll(fh) // from the seek position to EOF (≈ last maxBytes)
	if err != nil {
		return ""
	}
	return string(data)
}

// Spawn creates an agent session. Prompt mode (Prompt set, no Type) runs a plain
// claude in Workdir with NO git worktree, seeded with the prompt. Typed mode is
// the existing per-type worktree flow (unchanged).
func (l *Lifecycle) Spawn(ctx context.Context, req SpawnRequest) (*store.Session, error) {
	promptMode := req.Prompt != "" && req.Type == ""
	if !promptMode {
		req.Type = store.NormalizeType(string(req.Type))
	}
	id, err := resolveID(req)
	if err != nil {
		return nil, err
	}

	sess := &store.Session{
		ID:          id,
		Type:        req.Type,
		Ticket:      req.Ticket,
		TmuxSession: id,
		Repo:        req.Repo,
		PR:          req.PR,
		Prompt:      req.Prompt,
		Subject:     firstWords(req.Prompt, 10),
		Status:      store.StatusSpawning,
	}
	sess.ClaudeSessionID = store.NewSessionID()

	if promptMode {
		dir := filepath.Join(req.Workdir, id)
		if out, err := l.run.Run(ctx, "", "mkdir", "-p", dir); err != nil {
			return nil, fmt.Errorf("mkdir workdir: %w: %s", err, out)
		}
		sess.Workdir = dir
		// Persist the prompt to a file, then launch claude with the prompt read
		// back via "$(cat …)". This keeps the command typed into the pane to a
		// single physical line: a multi-line prompt typed directly would have its
		// embedded newlines register as Enter, submitting the half-typed command.
		// The prompt is passed to the writer as an exec argument (never through a
		// shell), so quotes and newlines in it need no escaping.
		promptFile := filepath.Join(dir, promptFileName)
		if out, err := l.run.Run(ctx, "", "sh", "-c", `printf '%s' "$1" > "$2"`, "sh", req.Prompt, promptFile); err != nil {
			return nil, fmt.Errorf("write prompt file: %w: %s", err, out)
		}
		if out, err := l.run.Run(ctx, "", "tmux", "new-session", "-d", "-s", id, "-c", dir); err != nil {
			return nil, fmt.Errorf("tmux new-session: %w: %s", err, out)
		}
		launch := claudeLaunch(sess.ClaudeSessionID, id) + ` "$(cat ` + shellQuoteArg(promptFile) + `)"`
		if out, err := l.run.Run(ctx, "", "tmux", "send-keys", "-t", id, launch, "Enter"); err != nil {
			return nil, fmt.Errorf("tmux send-keys claude: %w: %s", err, out)
		}
		return sess, nil
	}

	// Typed/managed path (unchanged).
	workdir := req.Repo
	if wantWorktree(req) {
		rel := worktreeRel(id)
		branch, err := l.ensureWorktree(ctx, req, id, rel)
		if err != nil {
			return nil, err
		}
		sess.Worktree = rel
		sess.Branch = branch
		workdir = filepath.Join(req.Repo, rel)
	}
	sess.Workdir = workdir
	if out, err := l.run.Run(ctx, req.Repo, "tmux", "new-session", "-d", "-s", id, "-c", workdir); err != nil {
		return nil, fmt.Errorf("tmux new-session: %w: %s", err, out)
	}
	if out, err := l.run.Run(ctx, req.Repo, "tmux", "send-keys", "-t", id, claudeLaunch(sess.ClaudeSessionID, id), "Enter"); err != nil {
		return nil, fmt.Errorf("tmux send-keys claude: %w: %s", err, out)
	}
	return sess, nil
}

// Restore recreates a lost agent's tmux session in its original workdir and
// resumes the same claude conversation (claude --resume). It is resume-only: it
// validates that the session is actually gone, has a pinned id, its workdir
// still exists, and its transcript is present — returning a specific sentinel
// otherwise — and never silently starts a fresh conversation.
func (l *Lifecycle) Restore(ctx context.Context, sess *store.Session) error {
	if sess.ClaudeSessionID == "" {
		return ErrNoSessionID
	}
	// Refuse if the tmux session is still alive (avoid a double-launch).
	if _, err := l.run.Run(ctx, "", "tmux", "has-session", "-t", sess.TmuxSession); err == nil {
		return ErrAlreadyRunning
	}
	if fi, err := os.Stat(sess.Workdir); err != nil || !fi.IsDir() {
		return ErrWorkdirMissing
	}
	if l.transcriptPath(sess) == "" {
		return ErrNoTranscript
	}
	if out, err := l.run.Run(ctx, "", "tmux", "new-session", "-d", "-s", sess.ID, "-c", sess.Workdir); err != nil {
		return fmt.Errorf("tmux new-session: %w: %s", err, out)
	}
	if out, err := l.run.Run(ctx, "", "tmux", "send-keys", "-t", sess.ID, claudeResume(sess.ClaudeSessionID, sess.ID), "Enter"); err != nil {
		return fmt.Errorf("tmux send-keys resume: %w: %s", err, out)
	}
	return nil
}

var (
	ErrDirtyWorktree      = errors.New("worktree has uncommitted changes (use --force)")
	ErrUnpushedCommits    = errors.New("worktree has unpushed commits (use --force)")
	ErrAlreadyRunning     = errors.New("agent is already running (use send/attach)")
	ErrNoSessionID        = errors.New("no pinned claude session id; re-spawn instead")
	ErrWorkdirMissing     = errors.New("agent workdir is gone; re-spawn instead")
	ErrNoTranscript       = errors.New("no transcript to resume")
	ErrNoWorktree         = errors.New("session has no worktree")
	ErrWorktreeAgentAlive = errors.New("agent is still running; terminate it before removing its worktree")
)

// CleanupTarget carries the fields Cleanup needs (filled from the store doc).
type CleanupTarget struct {
	ID          string
	Repo        string
	Worktree    string // relative, e.g. .worktrees/A-1
	Branch      string
	TmuxSession string
}

func (l *Lifecycle) worktreeAbs(t CleanupTarget) string {
	return filepath.Join(t.Repo, t.Worktree)
}

// guard returns an error if the worktree has uncommitted or unpushed work.
func (l *Lifecycle) guard(ctx context.Context, t CleanupTarget) error {
	abs := l.worktreeAbs(t)
	dirty, err := l.run.Run(ctx, "", "git", "-C", abs, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("git status: %w: %s", err, dirty)
	}
	if strings.TrimSpace(dirty) != "" {
		return ErrDirtyWorktree
	}
	unpushed, err := l.run.Run(ctx, "", "git", "-C", abs, "log", "@{u}..", "--oneline")
	if err != nil {
		// No upstream configured → treat as "has unpushed work" to be safe.
		return ErrUnpushedCommits
	}
	if strings.TrimSpace(unpushed) != "" {
		return ErrUnpushedCommits
	}
	return nil
}

// Terminate kills the agent's tmux session (which kills the claude process
// inside it). It is idempotent: killing an already-gone session is not an error.
// It touches no git and leaves the record and any worktree intact.
func (l *Lifecycle) Terminate(ctx context.Context, tmuxSession string) error {
	// tmux kill-session errors if the session is already gone; that is the
	// desired end state, so the error is ignored.
	_, _ = l.run.Run(ctx, "", "tmux", "kill-session", "-t", tmuxSession)
	return nil
}

// RemoveWorktree removes the session's git worktree and branch. It is always an
// explicit, separate step. Unless force is set, it refuses when the agent's tmux
// session is still alive (terminate first) and when the worktree has uncommitted
// or unpushed work (the guard). Sessions with no worktree return ErrNoWorktree.
func (l *Lifecycle) RemoveWorktree(ctx context.Context, t CleanupTarget, force bool) error {
	if t.Worktree == "" {
		return ErrNoWorktree
	}
	if !force {
		if _, err := l.run.Run(ctx, "", "tmux", "has-session", "-t", t.TmuxSession); err == nil {
			return ErrWorktreeAgentAlive
		}
		if err := l.guard(ctx, t); err != nil {
			return err
		}
	}
	removeArgs := []string{"-C", t.Repo, "worktree", "remove", t.Worktree}
	if force {
		removeArgs = []string{"-C", t.Repo, "worktree", "remove", "--force", t.Worktree}
	}
	if out, err := l.run.Run(ctx, "", "git", removeArgs...); err != nil {
		return fmt.Errorf("git worktree remove: %w: %s", err, out)
	}
	if t.Branch != "" {
		if out, err := l.run.Run(ctx, "", "git", "-C", t.Repo, "branch", "-D", t.Branch); err != nil {
			return fmt.Errorf("git branch -D: %w: %s", err, out)
		}
	}
	return nil
}

// inputSubmitDelay is the pause between pasting the message and pressing Enter,
// so the TUI finishes ingesting a (possibly multi-line) bracketed paste before
// the submit keystroke arrives. Overridable in tests.
var inputSubmitDelay = 150 * time.Millisecond

// Input sends text to the agent's tmux pane and submits it.
//
// It does NOT type the text followed by Enter in one send-keys call: in the
// Claude Code TUI that fuses two problems — a long/multi-line paste can still be
// settling when the Enter arrives (so the submit is dropped, leaving the text
// stranded in the composer), and raw embedded newlines register as Enter
// keypresses (so multi-line / plan-mode input submits a fragment or never
// submits). Instead it bracketed-pastes the text via a per-session tmux buffer
// (newlines stay content; per-session name avoids clobbering across concurrent
// sends) and then presses Enter as a SEPARATE keystroke after a short settle.
func (l *Lifecycle) Input(ctx context.Context, tmuxSession, text string) error {
	buf := "agentctl-input-" + tmuxSession
	if out, err := l.run.Run(ctx, "", "tmux", "set-buffer", "-b", buf, "--", text); err != nil {
		return fmt.Errorf("tmux set-buffer: %w: %s", err, out)
	}
	if out, err := l.run.Run(ctx, "", "tmux", "paste-buffer", "-t", tmuxSession, "-b", buf, "-p", "-d"); err != nil {
		return fmt.Errorf("tmux paste-buffer: %w: %s", err, out)
	}
	if inputSubmitDelay > 0 {
		select {
		case <-time.After(inputSubmitDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if out, err := l.run.Run(ctx, "", "tmux", "send-keys", "-t", tmuxSession, "Enter"); err != nil {
		return fmt.Errorf("tmux send-keys Enter: %w: %s", err, out)
	}
	return nil
}

// Output returns the last `lines` rows of the agent's tmux pane.
func (l *Lifecycle) Output(ctx context.Context, tmuxSession string, lines int) (string, error) {
	if lines <= 0 {
		lines = 200
	}
	out, err := l.run.Run(ctx, "", "tmux", "capture-pane", "-p", "-t", tmuxSession, "-S", "-"+strconv.Itoa(lines))
	if err != nil {
		return "", fmt.Errorf("tmux capture-pane: %w: %s", err, out)
	}
	return out, nil
}

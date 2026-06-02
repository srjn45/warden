package lifecycle

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/srajanpathak/agentctl/internal/store"
)

// claudeCmd is launched in every spawned session. Agents run unattended
// (design §4): permission prompts are skipped; the Notification hook still
// records when one *would* have prompted.
const claudeCmd = "claude --dangerously-skip-permissions"

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
	if len(line) > 80 {
		line = strings.TrimSpace(line[:80])
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
	if len(text) > 4000 {
		text = text[len(text)-4000:]
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

// shortID returns 4 hex chars for auto-generated session ids.
func shortID() string {
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// resolveID uses the ticket when given, else "<typeslug>-<shortid>".
func resolveID(req SpawnRequest) string {
	if req.Ticket != "" {
		return req.Ticket
	}
	slug := strings.ReplaceAll(string(req.Type), "-", "")
	if slug == "" {
		slug = "agent"
	}
	return slug + "-" + shortID()
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
	out, err := l.run.Run(ctx, "", "claude", "-p", classifyArg(prompt))
	if err != nil {
		return store.TypeOther, fmt.Errorf("claude -p: %w: %s", err, out)
	}
	return parseType(out), nil
}

// Spawn creates an agent session. Prompt mode (Prompt set, no Type) runs a plain
// claude in Workdir with NO git worktree, seeded with the prompt. Typed mode is
// the existing per-type worktree flow (unchanged).
func (l *Lifecycle) Spawn(ctx context.Context, req SpawnRequest) (*store.Session, error) {
	promptMode := req.Prompt != "" && req.Type == ""
	if !promptMode {
		req.Type = store.NormalizeType(string(req.Type))
	}
	id := resolveID(req)

	sess := &store.Session{
		ID:          id,
		Type:        req.Type,
		Ticket:      req.Ticket,
		TmuxSession: id,
		Repo:        req.Repo,
		PR:          req.PR,
		Prompt:      req.Prompt,
		Status:      store.StatusSpawning,
	}

	if promptMode {
		if out, err := l.run.Run(ctx, "", "tmux", "new-session", "-d", "-s", id, "-c", req.Workdir); err != nil {
			return nil, fmt.Errorf("tmux new-session: %w: %s", err, out)
		}
		launch := claudeCmd + " " + shellQuoteArg(req.Prompt)
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
	if out, err := l.run.Run(ctx, req.Repo, "tmux", "new-session", "-d", "-s", id, "-c", workdir); err != nil {
		return nil, fmt.Errorf("tmux new-session: %w: %s", err, out)
	}
	if out, err := l.run.Run(ctx, req.Repo, "tmux", "send-keys", "-t", id, claudeCmd, "Enter"); err != nil {
		return nil, fmt.Errorf("tmux send-keys claude: %w: %s", err, out)
	}
	return sess, nil
}

var (
	ErrDirtyWorktree   = errors.New("worktree has uncommitted changes (use --force)")
	ErrUnpushedCommits = errors.New("worktree has unpushed commits (use --force)")
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

// Cleanup removes a session's tmux session and, for sessions that have a
// worktree, the worktree and branch as well.
//
// When the session has a worktree and force is false, guard is run first. If
// guard returns an error, Cleanup returns immediately without touching tmux,
// the worktree, or the branch — this is intentional so the user can push and
// retry without ending up with an orphaned worktree.
//
// Once past the guard (or when force is true), tmux is killed and then the
// worktree + branch are removed. No-worktree sessions skip the git steps
// entirely and only kill tmux.
func (l *Lifecycle) Cleanup(ctx context.Context, t CleanupTarget, force bool) error {
	hasWorktree := t.Worktree != ""
	if hasWorktree && !force {
		if err := l.guard(ctx, t); err != nil {
			return err
		}
	}
	// Guard passed (or was skipped); kill tmux, then remove worktree+branch.
	_, _ = l.run.Run(ctx, "", "tmux", "kill-session", "-t", t.TmuxSession)

	if !hasWorktree {
		return nil // nothing to prune
	}

	removeArgs := []string{"-C", t.Repo, "worktree", "remove", t.Worktree}
	if force {
		removeArgs = []string{"-C", t.Repo, "worktree", "remove", "--force", t.Worktree}
	}
	if out, err := l.run.Run(ctx, "", "git", removeArgs...); err != nil {
		return fmt.Errorf("git worktree remove: %w: %s", err, out)
	}
	// Branch may be empty (e.g. a detached pr-review checkout) — skip if so.
	if t.Branch != "" {
		if out, err := l.run.Run(ctx, "", "git", "-C", t.Repo, "branch", "-D", t.Branch); err != nil {
			return fmt.Errorf("git branch -D: %w: %s", err, out)
		}
	}
	return nil
}

// Input types text into the agent's tmux pane and presses Enter.
// `--` prevents text starting with `-` being read as a flag.
func (l *Lifecycle) Input(ctx context.Context, tmuxSession, text string) error {
	out, err := l.run.Run(ctx, "", "tmux", "send-keys", "-t", tmuxSession, "--", text, "Enter")
	if err != nil {
		return fmt.Errorf("tmux send-keys: %w: %s", err, out)
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

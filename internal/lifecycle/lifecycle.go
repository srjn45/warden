package lifecycle

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/srjn45/warden/internal/pressure"
	"github.com/srjn45/warden/internal/store"
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

// PermissionModes is the canonical set of accepted claude permission modes.
// It is the single source of truth for both the spawn-time validation gate and
// the live PATCH /permission-mode handler.
var PermissionModes = []string{"acceptEdits", "auto", "bypassPermissions", "default", "dontAsk", "plan"}

// ValidPermissionMode reports whether mode is one of PermissionModes. An empty
// mode is valid: callers treat it as "use the configured default".
func ValidPermissionMode(mode string) bool {
	if mode == "" {
		return true
	}
	for _, m := range PermissionModes {
		if mode == m {
			return true
		}
	}
	return false
}

// permissionFlag selects the claude permission mode flag for a spawned agent.
// mode is one of PermissionModes. The value is shell-quoted because claudeBase's
// result is typed into a tmux pane and run by a shell — an unquoted mode that
// slipped past validation must never be able to inject shell syntax.
func permissionFlag(mode string) string {
	return "--permission-mode " + shellQuoteArg(mode)
}

// claudeBase is the claude command + model + permission flag every agent session starts from.
// Uses the provided model, or the default (DefaultModel) when model is empty.
// modelID is shell-quoted: it may be an arbitrary caller-supplied full model ID
// (ResolveModel passes unknown values through), and claudeBase's result is typed
// into a tmux pane and executed by a shell, so an unquoted model would be a
// command-injection vector.
func (l *Lifecycle) claudeBase(model string, mode string) string {
	modelID := l.modelOrDefault(model)
	return "claude --model " + shellQuoteArg(modelID) + " " + permissionFlag(mode)
}

// claudeLaunch builds the claude invocation for a spawned agent: the base
// command plus a pinned --session-id (deterministic transcript + future
// --resume) and a --name display label equal to the agent id, so the agent id,
// tmux session, and claude session all read the same. sessionID is a generated
// UUID (safe charset); name is the agent id (may be a ticket key) so it is quoted.
func (l *Lifecycle) claudeLaunch(sessionID, name string, model string, mode string) string {
	return l.claudeBase(model, mode) + " --session-id " + sessionID + " --name " + shellQuoteArg(name)
}

// pipelineHintGuidance is appended to a freshly spawned plain agent's system
// prompt so that, handed a large/multi-phase task, the agent recommends
// decomposing it into an warden pipeline of short-lived stages (which keeps
// each agent's context bounded and returns its memory to the OS on teardown)
// before proceeding. Worded conditionally so small tasks trigger no advisory.
// No apostrophes — keeps the single-quoted shell form (shellQuoteArg) clean.
const pipelineHintGuidance = "You were launched as a standalone warden agent. " +
	"If this task is large or spans multiple distinct phases (for example analyze, " +
	"implement, test, review) such that you would accumulate a very large context, " +
	"briefly recommend up front that it be split into an warden pipeline of smaller " +
	"stages (each a short-lived agent with a fresh, bounded context), then proceed with " +
	"the task as a single agent unless told otherwise."

// pipelineHint returns the claude flag fragment that injects pipelineHintGuidance
// as a system-prompt addendum, or "" when the pipeline_hint config setting is
// disabled. The leading space lets callers concatenate it directly onto a
// claudeLaunch string. Applied only by Spawn (plain agents); SpawnJob (pipeline
// jobs, already decomposed) and resume omit it.
func (l *Lifecycle) pipelineHint() string {
	if !l.cfg.GetPipelineHint() {
		return ""
	}
	return " --append-system-prompt " + shellQuoteArg(pipelineHintGuidance)
}

// claudeResume builds the invocation that resumes an existing agent conversation
// by its pinned session id (continues the same transcript). --name re-applies the
// display label so the resumed session still reads as the agent id.
func (l *Lifecycle) claudeResume(sessionID, name string, model string, mode string) string {
	return l.claudeBase(model, mode) + " --resume " + sessionID + " --name " + shellQuoteArg(name)
}

// classifyInstruction is prepended to the task prompt for headless classification.
const classifyInstruction = "You are a classifier. Classify the following agent task into exactly one of these labels: development, analysis, spike, pr-review, code, docs, website, debug-ci, tests, other. Reply with ONLY the label, nothing else.\n\nTask: "

// classifyArg builds the single argument passed to `claude -p`.
func classifyArg(prompt string) string { return classifyInstruction + prompt }

const summaryInstruction = "In 8 words or fewer, summarize what this agent is currently working on. Reply with ONLY the phrase — no quotes, no preamble.\n\nRecent activity:\n"

// spawnSubject is the short list-view label for a spawned agent: the first
// words of its prompt, or "interactive" when there is no prompt (the agent was
// opened to wait for instructions typed into Claude directly).
func spawnSubject(prompt string) string {
	if prompt == "" {
		return "interactive"
	}
	return firstWords(prompt, 10)
}

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
	cfg ConfigProvider
	// ProjectsDir is the Claude Code transcript root (default empty → transcript
	// lookup disabled; the daemon sets it from config). Overridable in tests.
	ProjectsDir string
	// PromptsDir is a single shared directory (the daemon sets it from config,
	// e.g. ~/.warden/prompts) where free-form agents with a prompt drop their
	// initial prompt file, keyed by agent id. It is NOT per-agent and is never the
	// dir the agent runs in — agents launch in the caller's cwd. Interactive
	// (empty-prompt) agents write no file. Overridable in tests.
	PromptsDir string
	// ExitsDir is a shared dir (the daemon sets it, e.g. ~/.warden/exits) where
	// each agent's shell records claude's exit status, keyed by agent id. Empty
	// (tests) disables exit capture — agents then fall back to orphaned-only
	// classification. Never the dir the agent runs in.
	ExitsDir string
}

// ConfigProvider is the subset of config.Config that lifecycle needs.
// Extracted to avoid a circular dependency and to allow test doubles.
type ConfigProvider interface {
	GetDefaultPermissionMode() string
	GetModelDefault() string
	GetPipelineHint() bool
}

func New(r Runner, cfg ConfigProvider) *Lifecycle { return &Lifecycle{run: r, cfg: cfg} }

// SpawnRequest is the type-aware input to Spawn (design §2 / §6).
type SpawnRequest struct {
	Type           store.Type
	Ticket         string // optional; becomes the id when present
	Name           string // optional; human-readable name for the agent
	Repo           string
	Branch         string // optional; development branch / pr-review checkout target
	PR             string // optional; pr-review
	Worktree       bool   // analysis/spike opt-in
	Prompt         string // free-form: the agent's initial prompt (no repo/worktree); empty = interactive
	Cwd            string // free-form: dir to launch claude from (the caller's "master shell"); required
	PermissionMode string // explicit mode override; empty = use global default
	AutoRestart    bool   // opt-in: auto-resume this agent when it errors (capped)
	Model          string // claude model (opus/sonnet/haiku or full ID); empty = default
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

// wrapWorktreeError detects common git worktree failure patterns and adds recovery hints.
func wrapWorktreeError(err error, output, path string) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(output)

	// Already exists
	if strings.Contains(msg, "already exists") || strings.Contains(msg, "already checked out") {
		return fmt.Errorf("%w: %s\nRecovery: git worktree remove %s --force", err, output, path)
	}

	// Locked worktree
	if strings.Contains(msg, "locked") {
		return fmt.Errorf("%w: %s\nRecovery: git worktree unlock %s", err, output, path)
	}

	// Dirty/uncommitted changes
	if strings.Contains(msg, "contains modified or untracked files") ||
		strings.Contains(msg, "uncommitted changes") {
		return fmt.Errorf("%w: %s\nRecovery: commit or stash changes in %s", err, output, path)
	}

	// Return original error with output
	return fmt.Errorf("%w: %s", err, output)
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
// recorded on the doc (empty for a detached pr-review checkout) plus whether it
// CREATED the worktree (vs. adopted a pre-existing one). The created flag lets a
// failed spawn roll back only worktrees it made, never the user's existing ones.
func (l *Lifecycle) ensureWorktree(ctx context.Context, req SpawnRequest, id, rel string) (branch string, created bool, err error) {
	exists, err := l.worktreeExists(ctx, req.Repo, rel)
	if err != nil {
		return "", false, err
	}
	if exists { // adopt — we did not create it, so it is never ours to roll back
		if req.Branch != "" {
			return req.Branch, false, nil
		}
		return id, false, nil
	}
	if req.Type == store.TypePRReview && req.Branch == "" {
		// Detached worktree, then let gh resolve + fetch the PR branch.
		if out, err := l.run.Run(ctx, req.Repo, "git", "worktree", "add", "--detach", rel); err != nil {
			return "", false, wrapWorktreeError(fmt.Errorf("git worktree add --detach: %w", err), out, rel)
		}
		abs := filepath.Join(req.Repo, rel)
		if out, err := l.run.Run(ctx, abs, "gh", "pr", "checkout", req.PR); err != nil {
			// Wrap gh command-not-found errors with install hint
			if strings.Contains(strings.ToLower(out), "command not found") ||
				strings.Contains(strings.ToLower(out), "not found") {
				hint := commandInstallHint("gh")
				return "", true, fmt.Errorf("gh pr checkout: %w: %s\n%s", err, out, hint)
			}
			return "", true, fmt.Errorf("gh pr checkout: %w: %s", err, out)
		}
		return "", true, nil
	}
	if req.Type == store.TypePRReview { // checkout the given existing branch
		if out, err := l.run.Run(ctx, req.Repo, "git", "worktree", "add", rel, req.Branch); err != nil {
			return "", false, wrapWorktreeError(fmt.Errorf("git worktree add: %w", err), out, rel)
		}
		return req.Branch, true, nil
	}
	// development / opt-in analysis|spike → new branch (branch = req.Branch or id).
	branch = req.Branch
	if branch == "" {
		branch = id
	}
	if out, err := l.run.Run(ctx, req.Repo, "git", "worktree", "add", rel, "-b", branch); err != nil {
		return "", false, wrapWorktreeError(fmt.Errorf("git worktree add: %w", err), out, rel)
	}
	return branch, true, nil
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

// TranscriptPath is the exported accessor the daemon uses to resolve an agent's
// transcript file (see transcriptPath). Returns "" when unresolved/disabled.
func (l *Lifecycle) TranscriptPath(sess *store.Session) string {
	return l.transcriptPath(sess)
}

// GitBranch returns the current branch name for dir, or "" on any error /
// non-repo. Best-effort: used only to annotate a digest.
func (l *Lifecycle) GitBranch(ctx context.Context, dir string) string {
	out, err := l.run.Run(ctx, dir, "git", "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// GitNumstat returns raw `git diff --numstat` output for dir, or "" on error.
func (l *Lifecycle) GitNumstat(ctx context.Context, dir string) string {
	out, err := l.run.Run(ctx, dir, "git", "diff", "--numstat")
	if err != nil {
		return ""
	}
	return out
}

// CommitWorktree stages and commits every change in dir on its current branch.
// It returns committed=false (no error) when the tree is already clean — either
// the agent committed its own work, or it produced nothing. The pipeline calls
// this when a job emits, so a job's work always lands on its branch before any
// downstream from:<job> job forks it (otherwise an agent that finished without
// committing would silently hand its dependents an empty base).
func (l *Lifecycle) CommitWorktree(ctx context.Context, dir, message string) (bool, error) {
	out, err := l.run.Run(ctx, dir, "git", "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("git status: %w: %s", err, out)
	}
	if strings.TrimSpace(out) == "" {
		return false, nil // clean tree — nothing to commit
	}
	if out, err := l.run.Run(ctx, dir, "git", "add", "-A"); err != nil {
		return false, fmt.Errorf("git add: %w: %s", err, out)
	}
	if out, err := l.run.Run(ctx, dir, "git", "commit", "-m", message); err != nil {
		return false, fmt.Errorf("git commit: %w: %s", err, out)
	}
	return true, nil
}

// MemoryPressure reads the macOS memory-pressure level via sysctl. Best-effort:
// on any error (sysctl missing on non-macOS, unparseable output) it degrades to
// pressure.Normal with no error, so the spawn gate falls back to count-only.
func (l *Lifecycle) MemoryPressure(ctx context.Context) (pressure.Level, error) {
	out, err := l.run.Run(ctx, "", "sysctl", "-n", "kern.memorystatus_vm_pressure_level")
	if err != nil {
		return pressure.Normal, nil
	}
	lvl, perr := pressure.ParseSysctl(out)
	if perr != nil {
		return pressure.Normal, nil
	}
	return lvl, nil
}

// RunClaudeP exposes the bounded headless `claude -p` runner (the same plumbing
// Classify/Summarize use) so the digest Narrator can reuse it.
func (l *Lifecycle) RunClaudeP(ctx context.Context, arg string) (string, error) {
	return l.runClaudeP(ctx, arg)
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

// NewestClaudeSession returns the claude session id (uuid) of the most recently
// modified transcript for workdir, or ErrNoTranscript when there is none (or
// transcript lookup is disabled). Pure filesystem inspection — no subprocess.
func (l *Lifecycle) NewestClaudeSession(workdir string) (string, error) {
	dir := claudeProjectDir(l.ProjectsDir, workdir)
	if dir == "" {
		return "", ErrNoTranscript
	}
	p := newestTranscriptPath(dir)
	if p == "" {
		return "", ErrNoTranscript
	}
	return strings.TrimSuffix(filepath.Base(p), ".jsonl"), nil
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
// the existing per-type worktree flow. Spawn resolves the id + claude session id
// shared by both, then dispatches to spawnFreeForm or spawnTyped.
func (l *Lifecycle) Spawn(ctx context.Context, req SpawnRequest) (*store.Session, error) {
	freeMode := req.Type == ""
	if !freeMode {
		req.Type = store.NormalizeType(string(req.Type))
	}
	id, err := resolveID(req)
	if err != nil {
		return nil, err
	}

	sess := &store.Session{
		ID:             id,
		Name:           req.Name,
		Type:           req.Type,
		Ticket:         req.Ticket,
		TmuxSession:    id,
		Repo:           req.Repo,
		PR:             req.PR,
		Prompt:         req.Prompt,
		Subject:        spawnSubject(req.Prompt),
		Status:         store.StatusSpawning,
		PermissionMode: req.PermissionMode,
		AutoRestart:    req.AutoRestart,
		Model:          req.Model,
	}
	sess.ClaudeSessionID, err = store.NewSessionID()
	if err != nil {
		return nil, err
	}

	if freeMode {
		return l.spawnFreeForm(ctx, req, sess)
	}
	return l.spawnTyped(ctx, req, sess)
}

// spawnFreeForm launches a plain claude agent in the caller's cwd with NO git
// worktree, seeded with req.Prompt (empty prompt = interactive). The agent runs
// in the caller's directory (the "master shell"), which is already trusted by
// Claude Code — we never create a fresh per-agent dir, which would trigger
// Claude's per-directory trust/onboarding prompts on every spawn. cwd is
// required: there is no directory to fall back to.
func (l *Lifecycle) spawnFreeForm(ctx context.Context, req SpawnRequest, sess *store.Session) (*store.Session, error) {
	if req.Cwd == "" {
		return nil, fmt.Errorf("free-form spawn requires a launch dir (cwd)")
	}
	sess.Workdir = req.Cwd

	// launchPrompt is the trailing claude argument. Empty for an interactive
	// agent (open claude and wait); for an autonomous agent it reads the prompt
	// back from a file via "$(cat …)". Persisting the prompt to a file (keyed by
	// id, in a shared state dir outside the caller's project) keeps the command
	// typed into the pane to a single physical line: a multi-line prompt typed
	// directly would have its embedded newlines register as Enter and submit a
	// half-typed command. The prompt is passed to the writer as an exec argument
	// (never through a shell), so quotes and newlines in it need no escaping.
	launchPrompt := ""
	if req.Prompt != "" {
		if l.PromptsDir == "" {
			return nil, fmt.Errorf("prompt spawn requires a prompts dir")
		}
		if out, err := l.run.Run(ctx, "", "mkdir", "-m", "700", "-p", l.PromptsDir); err != nil {
			return nil, fmt.Errorf("mkdir prompts dir: %w: %s", err, out)
		}
		promptFile := filepath.Join(l.PromptsDir, sess.ID)
		if out, err := l.run.Run(ctx, "", "sh", "-c", `printf '%s' "$1" > "$2"`, "sh", req.Prompt, promptFile); err != nil {
			return nil, fmt.Errorf("write prompt file: %w: %s", err, out)
		}
		launchPrompt = ` "$(cat ` + shellQuoteArg(promptFile) + `)"`
	}

	if err := l.newAgentSession(ctx, "", sess.ID, req.Cwd); err != nil {
		return nil, err
	}
	mode := req.PermissionMode
	if mode == "" {
		mode = l.cfg.GetDefaultPermissionMode()
	}
	launch := l.claudeLaunch(sess.ClaudeSessionID, sess.ID, req.Model, mode) + l.pipelineHint() + launchPrompt + l.exitSuffix(sess.ID)
	if out, err := l.run.Run(ctx, "", "tmux", "send-keys", "-t", sess.ID, launch, "Enter"); err != nil {
		// The session exists but launch failed — don't orphan it. No worktree here.
		l.cleanupFailedSpawn(sess, true, false)
		return nil, fmt.Errorf("tmux send-keys claude: %w: %s", err, out)
	}
	return sess, nil
}

// spawnTyped runs the per-type managed flow: optionally create a git worktree,
// start a tmux session in it, and auto-launch claude. On any post-resource
// failure it rolls back via cleanupFailedSpawn (a worktree only when WE made it).
func (l *Lifecycle) spawnTyped(ctx context.Context, req SpawnRequest, sess *store.Session) (*store.Session, error) {
	workdir := req.Repo
	worktreeCreated := false
	if wantWorktree(req) {
		rel := worktreeRel(sess.ID)
		branch, created, err := l.ensureWorktree(ctx, req, sess.ID, rel)
		if err != nil {
			return nil, err
		}
		sess.Worktree = rel
		sess.Branch = branch
		worktreeCreated = created
		workdir = filepath.Join(req.Repo, rel)
	}
	sess.Workdir = workdir
	if err := l.newAgentSession(ctx, req.Repo, sess.ID, workdir); err != nil {
		// new-session failed, so no tmux session exists; only undo a worktree we made.
		l.cleanupFailedSpawn(sess, false, worktreeCreated)
		return nil, err
	}
	mode := req.PermissionMode
	if mode == "" {
		mode = l.cfg.GetDefaultPermissionMode()
	}
	launch := l.claudeLaunch(sess.ClaudeSessionID, sess.ID, req.Model, mode) + l.pipelineHint() + l.exitSuffix(sess.ID)
	if out, err := l.run.Run(ctx, req.Repo, "tmux", "send-keys", "-t", sess.ID, launch, "Enter"); err != nil {
		l.cleanupFailedSpawn(sess, true, worktreeCreated)
		return nil, fmt.Errorf("tmux send-keys claude: %w: %s", err, out)
	}
	return sess, nil
}

// cleanupFailedSpawn best-effort reverses the partial side effects of a spawn
// that failed after creating resources: it kills the tmux session (when one was
// actually created — killTmux=false when new-session itself failed) and
// force-removes the worktree (only when worktreeCreated, i.e. THIS spawn made
// it — never an adopted, pre-existing one). Both steps are non-fatal and log on
// failure (via killSession/rollbackWorktree) so a leak is visible. This is the
// single shared cleanup used by spawnFreeForm, spawnTyped, and SpawnJob, in
// every case preserving the spawn-before-reap ordering (cleanup runs only after
// the failing step returns).
func (l *Lifecycle) cleanupFailedSpawn(sess *store.Session, killTmux, worktreeCreated bool) {
	if killTmux {
		l.killSession(sess.ID)
	}
	if worktreeCreated {
		l.rollbackWorktree(sess)
	}
}

// killSession best-effort kills a tmux session created during a spawn that then
// failed, so it does not orphan beyond the reach of the store. A detached
// context is used so cleanup still runs when the spawn failed via ctx cancellation.
// A failure is logged (not returned) so a leaked session is visible.
func (l *Lifecycle) killSession(id string) {
	if out, err := l.run.Run(context.Background(), "", "tmux", "kill-session", "-t", id); err != nil {
		log.Printf("spawn cleanup: kill tmux session %s: %v: %s", id, err, strings.TrimSpace(out))
	}
}

// rollbackWorktree best-effort force-removes a worktree (and its branch) that
// this spawn created when a later step fails. Only ever called for worktrees we
// created — never for an adopted, pre-existing one (see ensureWorktree). A
// failure is logged (not returned) so a leaked worktree is visible.
func (l *Lifecycle) rollbackWorktree(sess *store.Session) {
	if err := l.RemoveWorktree(context.Background(), CleanupTarget{
		ID: sess.ID, Repo: sess.Repo, Worktree: sess.Worktree,
		Branch: sess.Branch, TmuxSession: sess.TmuxSession,
	}, true); err != nil {
		log.Printf("spawn cleanup: rollback worktree %s: %v", sess.Worktree, err)
	}
}

// agentHistoryLimit is the scrollback depth (lines) agent panes get, so long
// agent output can be scrolled back to in the cockpit detail pane. tmux fixes a
// pane's history at creation, so ensureScrollback raises the global option
// before new-session.
const agentHistoryLimit = 50000

// newAgentSession creates the detached tmux session for an agent in cwd and
// applies scroll-friendly options. Only new-session failing aborts the spawn;
// option-setting failures are non-fatal so a tmux quirk never blocks a launch.
func (l *Lifecycle) newAgentSession(ctx context.Context, runDir, id, cwd string, env ...string) error {
	l.ensureScrollback(ctx)        // before new-session: the new pane inherits the limit
	EnsureExtendedKeys(ctx, l.run) // so Claude sees Shift+Enter as newline, not submit
	// -e sets WARDEN_SESSION_ID (+ legacy AGENTCTL_SESSION_ID, + any extra
	// pipeline env) in the session environment so the agent's shell tools know
	// which agent they are. Both variants are set for back-compat.
	args := []string{"new-session", "-d", "-s", id,
		"-e", "WARDEN_SESSION_ID=" + id,
		"-e", "AGENTCTL_SESSION_ID=" + id}
	for _, kv := range env {
		args = append(args, "-e", kv)
	}
	args = append(args, "-c", cwd)
	if out, err := l.run.Run(ctx, runDir, "tmux", args...); err != nil {
		return fmt.Errorf("tmux new-session: %w: %s", err, out)
	}
	// mouse is a live session option: the wheel enters copy-mode, and the cockpit
	// session can forward the wheel into this nested attach. Non-fatal.
	_, _ = l.run.Run(ctx, "", "tmux", "set-option", "-t", id, "mouse", "on")
	return nil
}

// ensureScrollback raises the global tmux history-limit to agentHistoryLimit
// when it is currently lower (only-raise: a user-configured larger value is left
// untouched). Must run before new-session. All failures are ignored — deep
// scrollback is a nicety, not a precondition for spawning.
func (l *Lifecycle) ensureScrollback(ctx context.Context) {
	if out, err := l.run.Run(ctx, "", "tmux", "show-options", "-g", "-v", "history-limit"); err == nil {
		if cur, perr := strconv.Atoi(strings.TrimSpace(out)); perr == nil && cur >= agentHistoryLimit {
			return // already large enough
		}
	}
	_, _ = l.run.Run(ctx, "", "tmux", "set-option", "-g", "history-limit", strconv.Itoa(agentHistoryLimit))
}

// EnsureExtendedKeys turns on tmux's extended-keys passthrough (a server option)
// so Claude running inside an agent's tmux session receives Shift+Enter as a
// distinct CSI-u key — which Claude treats as a newline — instead of the bare CR
// that tmux collapses Shift+Enter into when extended-keys is off (which Claude
// treats as submit). terminal-features is appended only when extkeys is absent so
// repeated spawns don't accumulate duplicate entries. Best-effort: a
// keyboard-protocol quirk must never block a spawn or cockpit launch.
func EnsureExtendedKeys(ctx context.Context, run Runner) {
	_, _ = run.Run(ctx, "", "tmux", "set-option", "-s", "extended-keys", "on")
	if out, err := run.Run(ctx, "", "tmux", "show-options", "-s", "-v", "terminal-features"); err == nil && strings.Contains(out, "extkeys") {
		return // outer terminal already advertised; don't append a duplicate
	}
	_, _ = run.Run(ctx, "", "tmux", "set-option", "-sa", "terminal-features", "*:extkeys")
}

// resumeInTmux creates a detached tmux session named id in cwd and resumes the
// claude conversation claudeID inside it. Shared by Restore and Adopt.
func (l *Lifecycle) resumeInTmux(ctx context.Context, id, cwd, claudeID, model string, mode string) error {
	if err := l.newAgentSession(ctx, "", id, cwd); err != nil {
		return err
	}
	resume := l.claudeResume(claudeID, id, model, mode) + l.exitSuffix(id)
	if out, err := l.run.Run(ctx, "", "tmux", "send-keys", "-t", id, resume, "Enter"); err != nil {
		return fmt.Errorf("tmux send-keys resume: %w: %s", err, out)
	}
	return nil
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
	mode := sess.PermissionMode
	if mode == "" {
		mode = l.cfg.GetDefaultPermissionMode()
	}
	return l.resumeInTmux(ctx, sess.ID, sess.Workdir, sess.ClaudeSessionID, sess.Model, mode)
}

// AdoptRequest carries the resolved inputs for Adopt. TmuxSession == "" selects
// resume mode (create a fresh tmux session and `claude --resume`); a non-empty
// TmuxSession selects live mode (register an existing tmux session, no
// relaunch). ID == "" generates an "agent-<short>" id; in live mode an ID that
// differs from TmuxSession triggers a tmux rename so the agent id and tmux
// session name stay equal (attach/switch-client target the id).
type AdoptRequest struct {
	ID              string
	Cwd             string
	ClaudeSessionID string
	TmuxSession     string
	Model           string // claude model (opus/sonnet/haiku or full ID); empty = default
}

// Adopt registers a Claude session warden did not spawn. Resume mode resumes
// the conversation under a new tmux session; live mode adopts an existing tmux
// session as-is. It returns the (unpersisted) session record for the caller to
// store. It never relaunches a live session.
// Resume mode assumes id is fresh (caller-generated) and does not guard against
// a pre-existing tmux session of that name.
func (l *Lifecycle) Adopt(ctx context.Context, req AdoptRequest) (*store.Session, error) {
	id := req.ID
	if id == "" {
		sid, err := shortID()
		if err != nil {
			return nil, err
		}
		id = "agent-" + sid
	}
	sess := &store.Session{
		ID:              id,
		TmuxSession:     id,
		Type:            store.TypeOther,
		Workdir:         req.Cwd,
		ClaudeSessionID: req.ClaudeSessionID,
		Model:           req.Model,
	}
	if req.TmuxSession == "" { // resume mode
		if req.ClaudeSessionID == "" {
			return nil, ErrNoSessionID
		}
		if fi, err := os.Stat(req.Cwd); err != nil || !fi.IsDir() {
			return nil, ErrWorkdirMissing
		}
		sess.Status = store.StatusSpawning
		if err := l.resumeInTmux(ctx, id, req.Cwd, req.ClaudeSessionID, req.Model, l.cfg.GetDefaultPermissionMode()); err != nil {
			return nil, err
		}
		return sess, nil
	}
	// live mode: register an existing tmux session, no relaunch.
	if _, err := l.run.Run(ctx, "", "tmux", "has-session", "-t", req.TmuxSession); err != nil {
		return nil, ErrTmuxGone
	}
	if id != req.TmuxSession {
		if out, err := l.run.Run(ctx, "", "tmux", "rename-session", "-t", req.TmuxSession, id); err != nil {
			return nil, fmt.Errorf("tmux rename-session: %w: %s", err, out)
		}
	}
	sess.Status = store.StatusWorking
	return sess, nil
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
	ErrTmuxGone           = errors.New("tmux session not found")
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
	buf := "warden-input-" + tmuxSession
	if out, err := l.run.Run(ctx, "", "tmux", "set-buffer", "-b", buf, "--", text); err != nil {
		return fmt.Errorf("tmux set-buffer: %w: %s", err, out)
	}
	// -p bracketed-pastes (newlines stay content) when the app is in bracketed-
	// paste mode; -r additionally stops paste-buffer translating LF→CR so an
	// embedded newline never submits early at a non-composer prompt (permission
	// dialog/menu). -d deletes the per-session buffer afterward.
	if out, err := l.run.Run(ctx, "", "tmux", "paste-buffer", "-t", tmuxSession, "-b", buf, "-p", "-r", "-d"); err != nil {
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

// SendKeys sends a single key (e.g. a numbered menu choice) to the agent's tmux
// pane as a raw keystroke. Unlike Input it neither bracketed-pastes nor appends
// Enter: Claude Code's select prompts treat the digit itself as select-and-
// confirm, so an extra Enter could double-submit.
func (l *Lifecycle) SendKeys(ctx context.Context, tmuxSession, key string) error {
	if out, err := l.run.Run(ctx, "", "tmux", "send-keys", "-t", tmuxSession, key); err != nil {
		return fmt.Errorf("tmux send-keys %q: %w: %s", key, err, out)
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

// JobSpawnRequest spawns one pipeline job. The executor composes Prompt and
// resolves Worktree/BaseBranch before calling.
type JobSpawnRequest struct {
	PipelineID     string
	JobID          string
	Repo           string
	Prompt         string // already composed (upstream context + footer)
	Worktree       bool   // create a git worktree? false = run in repo root
	BaseBranch     string // worktree base ref ("" = off HEAD); ignored when Worktree is false
	Type           store.Type
	PermissionMode string // explicit mode override; empty = use global default
	Model          string // claude model (opus/sonnet/haiku or full ID); empty = default
}

// exitSuffix ensures ExitsDir exists, clears any stale exit-file for id (from a
// reused id), and returns the shell suffix that records claude's exit status to
// it. Returns "" (no capture) when ExitsDir is unset or the dir can't be made —
// best-effort, consistent with the other launch-time side effects.
func (l *Lifecycle) exitSuffix(id string) string {
	if l.ExitsDir == "" {
		return ""
	}
	if err := os.MkdirAll(l.ExitsDir, 0o700); err != nil {
		log.Printf("exit-capture: mkdir %s: %v", l.ExitsDir, err)
		return ""
	}
	path := filepath.Join(l.ExitsDir, id)
	_ = os.Remove(path) // clear a prior run's file so the poller can't consume it
	return " ; printf '%s' \"$?\" > " + shellQuoteArg(path)
}

// ReadExit returns the exit code recorded for id and whether one is present.
// A missing or malformed file reports (0, false) — treat as "not yet recorded".
func (l *Lifecycle) ReadExit(id string) (int, bool) {
	if l.ExitsDir == "" {
		return 0, false
	}
	b, err := os.ReadFile(filepath.Join(l.ExitsDir, id))
	if err != nil {
		return 0, false
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, false
	}
	return code, true
}

// ClearExit removes id's exit-file (best-effort) once the poller has consumed it.
func (l *Lifecycle) ClearExit(id string) {
	if l.ExitsDir == "" {
		return
	}
	_ = os.Remove(filepath.Join(l.ExitsDir, id))
}

// writePromptFile persists prompt to <PromptsDir>/<id> and returns the path, so
// a multi-line prompt is launched via "$(cat file)" as a single typed line.
func (l *Lifecycle) writePromptFile(ctx context.Context, id, prompt string) (string, error) {
	if l.PromptsDir == "" {
		return "", fmt.Errorf("prompts dir not configured")
	}
	if out, err := l.run.Run(ctx, "", "mkdir", "-m", "700", "-p", l.PromptsDir); err != nil {
		return "", fmt.Errorf("mkdir prompts dir: %w: %s", err, out)
	}
	path := filepath.Join(l.PromptsDir, id)
	if out, err := l.run.Run(ctx, "", "sh", "-c", `printf '%s' "$1" > "$2"`, "sh", prompt, path); err != nil {
		return "", fmt.Errorf("write prompt file: %w: %s", err, out)
	}
	return path, nil
}

// SpawnJob launches one pipeline-job agent: optionally creating a git worktree
// (off HEAD or off BaseBranch), starting a tmux session with the pipeline
// identity env, and auto-typing the composed prompt into claude.
func (l *Lifecycle) SpawnJob(ctx context.Context, req JobSpawnRequest) (*store.Session, error) {
	id := req.PipelineID + "-" + req.JobID
	if err := store.SafeID(id); err != nil {
		return nil, fmt.Errorf("invalid job session id %q: %w", id, err)
	}
	sess := &store.Session{
		ID: id, TmuxSession: id, Type: req.Type, Repo: req.Repo,
		Prompt: req.Prompt, Subject: firstWords(req.Prompt, 10),
		Status: store.StatusSpawning, PermissionMode: req.PermissionMode,
		PipelineID: req.PipelineID, JobID: req.JobID,
		Model: req.Model,
	}
	cid, err := store.NewSessionID()
	if err != nil {
		return nil, err
	}
	sess.ClaudeSessionID = cid

	workdir := req.Repo
	worktreeCreated := false
	if req.Worktree {
		rel := worktreeRel(id)
		add := []string{"worktree", "add", rel, "-b", id}
		if req.BaseBranch != "" {
			add = append(add, req.BaseBranch)
		}
		if out, err := l.run.Run(ctx, req.Repo, "git", add...); err != nil {
			return nil, wrapWorktreeError(fmt.Errorf("git worktree add: %w", err), out, rel)
		}
		sess.Worktree = rel
		sess.Branch = id
		worktreeCreated = true
		workdir = filepath.Join(req.Repo, rel)
	}
	sess.Workdir = workdir

	if err := l.newAgentSession(ctx, req.Repo, id, workdir,
		"WARDEN_PIPELINE_ID="+req.PipelineID, "WARDEN_JOB_ID="+req.JobID,
		"AGENTCTL_PIPELINE_ID="+req.PipelineID, "AGENTCTL_JOB_ID="+req.JobID); err != nil {
		// new-session failed, so no tmux session exists; only undo a worktree we made.
		l.cleanupFailedSpawn(sess, false, worktreeCreated)
		return nil, err
	}

	promptFile, err := l.writePromptFile(ctx, id, req.Prompt)
	if err != nil {
		l.cleanupFailedSpawn(sess, true, worktreeCreated)
		return nil, err
	}
	mode := req.PermissionMode
	if mode == "" {
		mode = l.cfg.GetDefaultPermissionMode()
	}
	launch := l.claudeLaunch(sess.ClaudeSessionID, id, req.Model, mode) + ` "$(cat ` + shellQuoteArg(promptFile) + `)"` + l.exitSuffix(id)
	if out, err := l.run.Run(ctx, req.Repo, "tmux", "send-keys", "-t", id, launch, "Enter"); err != nil {
		l.cleanupFailedSpawn(sess, true, worktreeCreated)
		return nil, fmt.Errorf("tmux send-keys claude: %w: %s", err, out)
	}
	return sess, nil
}

package lifecycle

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// protectedBranches are the long-lived integration branches an agent must never
// commit to or push directly. An agent works on its own branch; a human (or a
// reviewed PR) integrates. Keeps the wd commit / wd push rails deterministic and
// language-agnostic.
var protectedBranches = map[string]bool{"main": true, "master": true}

// IsProtectedBranch reports whether branch is one warden refuses to mutate
// directly. Exported so the daemon redirect hooks and tests share one list.
func IsProtectedBranch(branch string) bool { return protectedBranches[branch] }

// CommitResult is the compact struct `wd commit` / `mcp__warden__commit` returns
// — one value in place of the 4-6 git tool round-trips Claude would otherwise
// read (status, diff, add, commit, rev-parse, hook output).
type CommitResult struct {
	Committed  bool     `json:"committed"`             // false = clean tree, nothing to do
	SHA        string   `json:"sha,omitempty"`         // short SHA of the new commit
	Branch     string   `json:"branch"`                // branch committed onto
	Files      []string `json:"files,omitempty"`       // paths included in the commit
	HookFailed bool     `json:"hook_failed,omitempty"` // a pre-commit hook rejected the commit
	HookOutput string   `json:"hook_output,omitempty"` // captured rejection output (only on failure)
	// RawBytes is the combined output of the git commands warden ran on the
	// agent's behalf (status/add/commit/rev-parse) — the tool-result contents a
	// manual agent would have read instead of this one compact struct. Accounting
	// only (json:"-", never sent to the agent): the daemon subtracts the struct's
	// own size to record the tokens `wd commit` kept out of context. A conservative
	// floor — it excludes the per-tool-call envelope each of those round-trips adds.
	RawBytes int `json:"-"`
}

// PushResult is the compact struct `wd push` / `mcp__warden__push` returns.
type PushResult struct {
	Branch string `json:"branch"`
	Remote string `json:"remote"`
	Pushed bool   `json:"pushed"`
	Output string `json:"output,omitempty"`
	// RawBytes is the raw `git push` output warden consumed; see CommitResult.RawBytes.
	RawBytes int `json:"-"`
}

// SyncResult is the compact struct `wd sync` / `mcp__warden__sync` returns. On a
// clean rebase Updated is true and Conflicts is empty; on a conflict the rebase
// is left in progress and Conflicts names the files the agent must resolve — the
// deterministic-detect half of "conflict resolution stays Claude, handed only
// the conflicting hunks."
type SyncResult struct {
	Branch    string   `json:"branch"`
	Base      string   `json:"base"`
	Updated   bool     `json:"updated"`             // rebase completed cleanly
	Conflicts []string `json:"conflicts,omitempty"` // unresolved paths (rebase in progress)
	Output    string   `json:"output,omitempty"`
	// RawBytes is the combined fetch+rebase output warden consumed; see CommitResult.RawBytes.
	RawBytes int `json:"-"`
}

// Commit stages and commits every change in dir on its current branch, enforcing
// the protected-branch rail and surfacing a pre-commit hook rejection as a
// structured result (not an error) so the agent sees only the failure. A clean
// tree returns Committed=false with no error.
func (l *Lifecycle) Commit(ctx context.Context, dir, message string) (CommitResult, error) {
	branch := l.GitBranch(ctx, dir)
	if branch == "" {
		return CommitResult{}, fmt.Errorf("not a git repository: %s", dir)
	}
	if protectedBranches[branch] {
		return CommitResult{}, fmt.Errorf("refusing to commit on protected branch %q — agents commit on their own branch and a human integrates", branch)
	}
	status, err := l.run.Run(ctx, dir, "git", "status", "--porcelain")
	if err != nil {
		return CommitResult{}, fmt.Errorf("git status: %w: %s", err, status)
	}
	// raw accumulates the git output the agent would have read had it driven these
	// commands itself — the counterfactual `wd commit` keeps out of context.
	raw := len(status)
	files := parsePorcelainPaths(status)
	if len(files) == 0 {
		return CommitResult{Committed: false, Branch: branch, RawBytes: raw}, nil // clean tree
	}
	addOut, err := l.run.Run(ctx, dir, "git", "add", "-A")
	if err != nil {
		return CommitResult{}, fmt.Errorf("git add: %w: %s", err, addOut)
	}
	raw += len(addOut)
	// Provenance tiers: the caller's message is best (the agent wrote the change,
	// so it knows intent). With none, fill it in — local model from the staged diff,
	// else a deterministic conventional-commit floor — staging first so git diff
	// --cached sees new files too. Generation never returns empty, so we never
	// commit with a blank -m.
	if strings.TrimSpace(message) == "" {
		message = l.commitMessage(ctx, dir, files)
	}
	out, err := l.run.Run(ctx, dir, "git", "commit", "-m", message)
	raw += len(out)
	if err != nil {
		// A pre-commit hook (or other commit-time check) rejected it. Unstage so
		// the agent is back at its pre-commit state, and hand back only the output
		// it needs to fix the failure.
		_, _ = l.run.Run(ctx, dir, "git", "reset")
		return CommitResult{Branch: branch, Files: files, HookFailed: true, HookOutput: strings.TrimSpace(out), RawBytes: raw}, nil
	}
	sha, _ := l.run.Run(ctx, dir, "git", "rev-parse", "--short", "HEAD")
	raw += len(sha)
	return CommitResult{Committed: true, SHA: strings.TrimSpace(sha), Branch: branch, Files: files, RawBytes: raw}, nil
}

// commitMsgMaxDiffBytes caps how much staged diff the local model sees when
// generating a commit message. The head of a diff carries the signal; an enormous
// diff must not blow up local inference or outlast the call's timeout.
const commitMsgMaxDiffBytes = 16000

// commitMsgInstruction asks the local model for one Conventional-Commits subject
// line and nothing else. warden only reaches here when no -m was given, so the
// model fills in provenance — it does not override an author's stated intent.
const commitMsgInstruction = "Write a single-line git commit message for the staged diff below, in Conventional Commits form: type(scope): summary (type is one of feat, fix, docs, refactor, test, chore; scope optional). Use the imperative mood, no trailing period, under 72 characters. Reply with ONLY that one line — no body, no backticks, no quotes.\n\nDiff:\n"

// commitMessage produces the message for a wd commit run with no -m. It tries the
// local model first (tier b: a Conventional-Commits subject distilled from the
// staged diff) and falls back to a deterministic floor (tier c) derived from the
// changed paths — so an absent, failing, slow, or empty model never blocks the
// commit. The changes must already be staged (git add -A) so git diff --cached
// includes new files. The result is never empty.
func (l *Lifecycle) commitMessage(ctx context.Context, dir string, files []string) string {
	floor := deterministicCommitMessage(files)
	if l.LLM == nil {
		return floor
	}
	diff, err := l.run.Run(ctx, dir, "git", "diff", "--cached")
	if err != nil || strings.TrimSpace(diff) == "" {
		return floor
	}
	out, err := l.LLM.Complete(ctx, commitMsgInstruction+capDiff(diff))
	if err != nil {
		slog.Warn("commit: local LLM failed, using deterministic message", "err", err)
		return floor
	}
	if s := parseCommitMessage(out); s != "" {
		return s
	}
	return floor
}

// capDiff trims the staged diff to the head commitMsgMaxDiffBytes and drops any
// partial trailing rune so the model is fed valid UTF-8.
func capDiff(diff string) string {
	if len(diff) <= commitMsgMaxDiffBytes {
		return diff
	}
	return strings.ToValidUTF8(diff[:commitMsgMaxDiffBytes], "")
}

// parseCommitMessage reduces the model's reply to one clean subject line: the
// first non-empty, non-fence line, stripped of surrounding quotes/backticks and
// hard-capped so a runaway model can't write a paragraph into the commit subject.
func parseCommitMessage(out string) string {
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "```") {
			continue
		}
		line = strings.TrimSpace(strings.Trim(line, "`\"'"))
		if line == "" {
			continue
		}
		if len(line) > 100 {
			line = strings.TrimSpace(line[:100])
		}
		return line
	}
	return ""
}

// deterministicCommitMessage is tier (c): a Conventional-Commits subject built
// only from the changed paths, with no model. It is the floor wd commit lands on
// when no message was given and no local model is available — language-agnostic,
// stable, and never empty.
func deterministicCommitMessage(files []string) string {
	typ := commitTypeForPaths(files)
	subject := commitSubjectForPaths(files)
	if scope := commonTopDir(files); scope != "" {
		return fmt.Sprintf("%s(%s): %s", typ, scope, subject)
	}
	return fmt.Sprintf("%s: %s", typ, subject)
}

// commitTypeForPaths picks a conventional-commit type from the changed paths:
// docs when every path is documentation, test when every path is a test, else the
// honest catch-all chore (warden won't claim feat/fix it can't verify).
func commitTypeForPaths(files []string) string {
	if len(files) == 0 {
		return "chore"
	}
	allDocs, allTests := true, true
	for _, f := range files {
		if !isDocPath(f) {
			allDocs = false
		}
		if !isTestPath(f) {
			allTests = false
		}
	}
	switch {
	case allDocs:
		return "docs"
	case allTests:
		return "test"
	default:
		return "chore"
	}
}

func isDocPath(f string) bool {
	f = strings.ToLower(f)
	return strings.HasSuffix(f, ".md") || strings.HasSuffix(f, ".rst") || strings.HasSuffix(f, ".txt") ||
		strings.HasPrefix(f, "docs/") || strings.Contains(f, "/docs/")
}

func isTestPath(f string) bool {
	f = strings.ToLower(f)
	return strings.HasSuffix(f, "_test.go") || strings.Contains(f, ".test.") || strings.Contains(f, ".spec.") ||
		strings.Contains(f, "/test/") || strings.Contains(f, "/tests/")
}

// commonTopDir returns the first path segment shared by every file, used as the
// conventional-commit scope. It returns "" when the files diverge at the top level
// or any is a repo-root file (no scope rather than a misleading one).
func commonTopDir(files []string) string {
	if len(files) == 0 {
		return ""
	}
	top := topSegment(files[0])
	if top == "" {
		return ""
	}
	for _, f := range files[1:] {
		if topSegment(f) != top {
			return ""
		}
	}
	return top
}

func topSegment(f string) string {
	f = strings.TrimPrefix(f, "./")
	if i := strings.IndexByte(f, '/'); i >= 0 {
		return f[:i]
	}
	return "" // repo-root file
}

// commitSubjectForPaths names what changed: the single file's base name, or a
// plain count when several files changed.
func commitSubjectForPaths(files []string) string {
	switch len(files) {
	case 0:
		return "update working tree"
	case 1:
		return "update " + baseName(files[0])
	default:
		return fmt.Sprintf("update %d files", len(files))
	}
}

func baseName(f string) string {
	f = strings.TrimRight(f, "/")
	if i := strings.LastIndexByte(f, '/'); i >= 0 {
		return f[i+1:]
	}
	return f
}

// Push pushes dir's current branch to origin (setting upstream), enforcing the
// protected-branch rail so an agent cannot push main/master directly.
func (l *Lifecycle) Push(ctx context.Context, dir string) (PushResult, error) {
	branch := l.GitBranch(ctx, dir)
	if branch == "" {
		return PushResult{}, fmt.Errorf("not a git repository: %s", dir)
	}
	if protectedBranches[branch] {
		return PushResult{}, fmt.Errorf("refusing to push protected branch %q directly — push your agent branch and open a PR", branch)
	}
	out, err := l.run.Run(ctx, dir, "git", "push", "-u", "origin", branch)
	if err != nil {
		return PushResult{}, fmt.Errorf("git push: %w: %s", err, strings.TrimSpace(out))
	}
	return PushResult{Branch: branch, Remote: "origin", Pushed: true, Output: strings.TrimSpace(out), RawBytes: len(out)}, nil
}

// Sync fetches origin/base and rebases dir's branch onto it. It refuses a dirty
// tree (commit first) rather than silently stashing. On conflict it leaves the
// rebase in progress and returns the conflicting paths; on any other rebase
// failure it aborts to a clean state and returns the error.
func (l *Lifecycle) Sync(ctx context.Context, dir, base string) (SyncResult, error) {
	branch := l.GitBranch(ctx, dir)
	if branch == "" {
		return SyncResult{}, fmt.Errorf("not a git repository: %s", dir)
	}
	if base == "" {
		base = "main"
	}
	status, err := l.run.Run(ctx, dir, "git", "status", "--porcelain")
	if err != nil {
		return SyncResult{}, fmt.Errorf("git status: %w: %s", err, status)
	}
	if strings.TrimSpace(status) != "" {
		return SyncResult{}, fmt.Errorf("working tree has uncommitted changes — commit them first (wd commit) before syncing")
	}
	fetchOut, err := l.run.Run(ctx, dir, "git", "fetch", "origin", base)
	if err != nil {
		return SyncResult{}, fmt.Errorf("git fetch origin %s: %w: %s", base, err, strings.TrimSpace(fetchOut))
	}
	// raw accumulates the fetch+rebase output the agent would have read itself.
	raw := len(fetchOut)
	out, err := l.run.Run(ctx, dir, "git", "rebase", "origin/"+base)
	raw += len(out)
	if err != nil {
		conflicts := l.unmergedPaths(ctx, dir)
		if len(conflicts) == 0 {
			// Not a conflict (e.g. missing base ref) — abort to a known-clean tree.
			_, _ = l.run.Run(ctx, dir, "git", "rebase", "--abort")
			return SyncResult{}, fmt.Errorf("git rebase onto origin/%s: %w: %s", base, err, strings.TrimSpace(out))
		}
		return SyncResult{Branch: branch, Base: base, Conflicts: conflicts, Output: strings.TrimSpace(out), RawBytes: raw}, nil
	}
	return SyncResult{Branch: branch, Base: base, Updated: true, Output: strings.TrimSpace(out), RawBytes: raw}, nil
}

// unmergedPaths lists files with merge conflicts in dir (best-effort, nil on error).
func (l *Lifecycle) unmergedPaths(ctx context.Context, dir string) []string {
	out, err := l.run.Run(ctx, dir, "git", "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil
	}
	return nonEmptyLines(out)
}

// parsePorcelainPaths extracts the changed paths from `git status --porcelain`
// output. Each line is "XY <path>" (or "XY <orig> -> <new>" for a rename — the
// post-rename path is what landed, so that is the one kept).
func parsePorcelainPaths(porcelain string) []string {
	var paths []string
	for _, line := range nonEmptyLines(porcelain) {
		if len(line) < 4 {
			continue
		}
		p := strings.TrimSpace(line[3:])
		if idx := strings.Index(p, " -> "); idx >= 0 {
			p = p[idx+len(" -> "):]
		}
		paths = append(paths, p)
	}
	return paths
}

// nonEmptyLines splits s on newlines and drops blank lines.
func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

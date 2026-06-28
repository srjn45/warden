package backends

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// This file is the shared machinery behind every backend's
// agentbackend.ContextInjector implementation. An injecting backend has no
// launch-time --append-system-prompt flag (Caps.SystemPromptInject=false) but
// reads a rules file from its working directory on startup, so warden delivers its
// collab/git/pipeline addendum by writing that text into the file the agent reads.
// The only thing that differs per backend is the *filename* (AGENTS.md for
// Codex/OpenCode/Cursor/Antigravity, CRUSH.md for Crush, .goosehints for Goose),
// so the no-clobber/idempotent/git-exclude logic lives here once and each adapter
// calls writeRulesFile with its verified filename. Codex piloted this logic
// (originally inline); extracting it kept Codex byte-identical.

// warden's rules-file injection markers. warden's addendum is written inside a
// clearly-delimited block so a user's own rules-file content is preserved and the
// warden section can be replaced in place on every relaunch (idempotent, never
// duplicated). HTML comments are inert noise in the markdown/hints files the
// agents read, so they delimit the block without affecting the rendered guidance.
const (
	wardenBegin = "<!-- warden:begin -->"
	wardenEnd   = "<!-- warden:end -->"
)

// wardenBlockRe matches a previously-written warden block (begin…end, plus a
// trailing newline) so writeRulesFile can replace it in place.
var wardenBlockRe = regexp.MustCompile(`(?s)` + regexp.QuoteMeta(wardenBegin) + `.*?` + regexp.QuoteMeta(wardenEnd) + `\n?`)

// writeRulesFile delivers text (warden's already-assembled addendum) into the
// agent's workdir by writing the backend's rules file (filename), the shared
// implementation of agentbackend.ContextInjector. Lifecycle calls each adapter's
// InjectContext post-worktree-creation / pre-launch so the file is present when the
// agent starts.
//
// The write is deliberately careful:
//   - It does NOT clobber a user's existing rules file: warden's text goes inside a
//     `<!-- warden:begin -->` … `<!-- warden:end -->` block. A pre-existing file
//     keeps all its content; only the warden block is added or refreshed.
//   - It is idempotent: relaunch/resume re-invokes this, and the warden block is
//     replaced in place (matched by wardenBlockRe), never duplicated.
//   - The dropped file is warden-injected, not the user's code, so it is kept out of
//     git via the repo's info/exclude (excludeFromGit) — it never lands in the
//     agent's diff/PR. Best-effort: a non-git or unwritable workdir just skips the
//     exclude (the file is still written).
//
// An empty workdir or empty text is a no-op (returns nil).
func writeRulesFile(workdir, filename, text string) error {
	text = strings.TrimSpace(text)
	if workdir == "" || text == "" {
		return nil
	}
	path := filepath.Join(workdir, filename)
	block := wardenBegin + "\n" + text + "\n" + wardenEnd + "\n"

	existing, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := os.WriteFile(path, []byte(mergeRulesFile(string(existing), block)), 0o644); err != nil {
			return err
		}
	case errors.Is(err, fs.ErrNotExist):
		if err := os.WriteFile(path, []byte(block), 0o644); err != nil {
			return err
		}
	default:
		return err
	}
	excludeFromGit(workdir, filename) // best-effort; keeps it out of the diff
	return nil
}

// mergeRulesFile folds the warden block into a user's existing rules-file content.
// If a prior warden block is present it is replaced in place (idempotent); otherwise
// the block is appended below the user's content, separated by a blank line. Uses a
// literal replacement so `$` in the addendum is never treated as a capture ref.
func mergeRulesFile(existing, block string) string {
	if wardenBlockRe.MatchString(existing) {
		return wardenBlockRe.ReplaceAllLiteralString(existing, block)
	}
	if existing == "" {
		return block
	}
	if !strings.HasSuffix(existing, "\n") {
		existing += "\n"
	}
	return existing + "\n" + block
}

// excludeFromGit best-effort adds name to the repo's git info/exclude so the
// warden-injected rules file never shows up as untracked (and so the agent can't
// land it in a commit/PR). It is a no-op outside a git tree or when the exclude file
// is unwritable, and never duplicates an existing entry.
func excludeFromGit(workdir, name string) {
	excl, ok := gitInfoExcludePath(workdir)
	if !ok {
		return
	}
	data, _ := os.ReadFile(excl)
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == name {
			return // already excluded
		}
	}
	f, err := os.OpenFile(excl, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	prefix := ""
	if len(data) > 0 && !strings.HasSuffix(string(data), "\n") {
		prefix = "\n"
	}
	_, _ = f.WriteString(prefix + name + "\n")
}

// gitInfoExcludePath resolves the info/exclude file git consults for workdir,
// handling both a normal repo (.git is a directory) and a linked worktree (.git is a
// file pointing at the per-worktree gitdir, whose commondir names the shared .git).
// ok=false when workdir is not a git tree.
func gitInfoExcludePath(workdir string) (string, bool) {
	dotgit := filepath.Join(workdir, ".git")
	info, err := os.Stat(dotgit)
	if err != nil {
		return "", false
	}
	if info.IsDir() {
		return filepath.Join(dotgit, "info", "exclude"), true
	}
	// Linked worktree: .git is a file "gitdir: <per-worktree gitdir>".
	data, err := os.ReadFile(dotgit)
	if err != nil {
		return "", false
	}
	gitdir := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(data)), "gitdir:"))
	if gitdir == "" {
		return "", false
	}
	// Excludes are read from the shared commondir, named by <gitdir>/commondir
	// (relative to gitdir) when present; fall back to the per-worktree gitdir.
	if rel, err := os.ReadFile(filepath.Join(gitdir, "commondir")); err == nil {
		common := strings.TrimSpace(string(rel))
		if !filepath.IsAbs(common) {
			common = filepath.Join(gitdir, common)
		}
		return filepath.Join(filepath.Clean(common), "info", "exclude"), true
	}
	return filepath.Join(gitdir, "info", "exclude"), true
}

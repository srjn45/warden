package lifecycle

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/srjn45/warden/internal/agentbackend"
	_ "github.com/srjn45/warden/internal/agentbackend/backends" // register the Claude backend (and future adapters)
	"github.com/srjn45/warden/internal/llm"
	"github.com/srjn45/warden/internal/memory"
	"github.com/srjn45/warden/internal/pressure"
	"github.com/srjn45/warden/internal/role"
	"github.com/srjn45/warden/internal/savings"
	"github.com/srjn45/warden/internal/store"
)

// claudeCallTimeout bounds every headless `claude -p` invocation (classify /
// summarize). Without it a stuck CLI would block its caller indefinitely — in
// particular the poller, which runs Summarize inline on its lifetime context.
const claudeCallTimeout = 30 * time.Second

// runClaudeP runs the backend's headless one-shot (`claude -p <arg>` for Claude)
// with a bounded timeout derived from ctx. The argv is built by the backend so
// the literal binary/flag shape lives in one place.
func (l *Lifecycle) runClaudeP(ctx context.Context, arg string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, claudeCallTimeout)
	defer cancel()
	argv, ok := l.backend.HeadlessCmd(arg)
	if !ok {
		return "", fmt.Errorf("backend %s has no headless mode", l.backend.ID())
	}
	return l.run.Run(cctx, "", argv[0], argv[1:]...)
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

// The claude command/resume/headless builders that used to live here now belong
// to the Claude adapter (internal/agentbackend/backends/claude.go). Lifecycle
// resolves a Backend from the registry and calls backend.LaunchCmd / ResumeCmd /
// HeadlessCmd instead. The hint fragments below (pipeline/collab/git-conventions)
// are still appended by lifecycle for now.

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
func (l *Lifecycle) pipelineHint(b agentbackend.Backend) string {
	return systemPromptHint(b, l.cfg.GetPipelineHint(), pipelineHintGuidance)
}

// systemPromptHint returns the launch fragment that injects guidance as a
// system-prompt addendum for backend b when enabled, or "" otherwise. It routes
// through the backend's SystemPromptFlag so a backend that can't inject a system
// prompt (Caps.SystemPromptInject=false, e.g. Aider) silently contributes
// nothing. For Claude this reproduces the historical
// ` --append-system-prompt '<guidance>'` fragment byte-for-byte (the Phase-0
// exit gate), since Claude's SystemPromptFlag emits exactly that.
func systemPromptHint(b agentbackend.Backend, enabled bool, guidance string) string {
	if !enabled {
		return ""
	}
	frag, ok := b.SystemPromptFlag(guidance)
	if !ok {
		return ""
	}
	return frag
}

// hintSpec pairs one config-gated guidance string with whether it is enabled, so
// systemPromptHints can assemble the launch addendum from a variable set of hints.
type hintSpec struct {
	enabled bool
	text    string
}

// systemPromptHints returns the launch fragment that injects the enabled guidance
// in specs as a system-prompt addendum for backend b, file-backing the text when
// possible. The enabled guidance is concatenated (blank line between) and, when the
// backend implements SystemPromptFiler AND a HintsDir is configured, written to a
// per-agent file referenced via a single --append-system-prompt "$(cat <file>)" —
// keeping the typed launch line short so it never exceeds the tty canonical-mode
// limit (1024 bytes on macOS/BSD) that would otherwise truncate it and stop the
// agent from starting. When file-backing is unavailable (no HintsDir, a non-filer
// backend, or a write failure) it falls back to the historical inline form: one
// SystemPromptFlag fragment per enabled hint, byte-identical to the prior behavior.
// A backend that cannot inject a system prompt (SystemPromptFlag ok=false, e.g.
// Aider) contributes nothing either way.
func (l *Lifecycle) systemPromptHints(ctx context.Context, b agentbackend.Backend, id string, specs ...hintSpec) string {
	var texts []string
	for _, s := range specs {
		if s.enabled && s.text != "" {
			texts = append(texts, s.text)
		}
	}
	if len(texts) == 0 {
		return ""
	}
	if filer, ok := b.(agentbackend.SystemPromptFiler); ok && l.HintsDir != "" {
		path, err := l.writeHintsFile(ctx, id, strings.Join(texts, "\n\n"))
		if err != nil {
			slog.Warn("spawn: write hints file failed; falling back to inline addendum", "agent", id, "err", err)
		} else if frag, ok := filer.SystemPromptFileFlag(path); ok {
			return frag
		}
	}
	// Inline fallback: reproduce the pre-file-backing launch byte-for-byte (one
	// --append-system-prompt per enabled hint, in order).
	var sb strings.Builder
	for _, t := range texts {
		if frag, ok := b.SystemPromptFlag(t); ok {
			sb.WriteString(frag)
		}
	}
	return sb.String()
}

// hintGuidance returns guidance when enabled, else "" — the raw-text counterpart to
// systemPromptHint, used to assemble a ContextInjector backend's rules file. Where
// SystemPromptFlag shapes guidance into a launch-line fragment, the injection path
// needs the bare text to write into AGENTS.md.
func hintGuidance(enabled bool, guidance string) string {
	if enabled {
		return guidance
	}
	return ""
}

// injectContext delivers warden's system-prompt addendum to a backend that has no
// launch-time flag (Caps.SystemPromptInject=false) but implements the
// ContextInjector seam (Codex) — the AGENTS.md counterpart to systemPromptHint's
// --append-system-prompt fragment. It is called post-worktree-creation / pre-launch
// with the agent's workdir and the SAME config-gated guidance strings the flag path
// would have appended; empties are dropped and the rest are newline-joined into the
// rules file. A backend implementing neither SystemPromptFlag nor ContextInjector
// (and Claude, which uses the flag) skips this silently — the addendum is dropped
// exactly as today. It returns the error so callers can decide; the spawn paths log
// and continue (a failed hint-file write degrades the agent — no coordination hints
// — rather than crashing the spawn, per design §5).
func (l *Lifecycle) injectContext(b agentbackend.Backend, workdir string, guidances ...string) error {
	inj, ok := b.(agentbackend.ContextInjector)
	if !ok {
		return nil
	}
	var parts []string
	for _, g := range guidances {
		if g != "" {
			parts = append(parts, g)
		}
	}
	text := strings.Join(parts, "\n\n")
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return inj.InjectContext(workdir, text)
}

// collabHintGuidance is appended to a freshly spawned agent's system prompt so
// it cooperates with any agents running alongside it: check before editing a
// file another agent already has open, and act on the daemon conflict warnings
// that land in its inbox, instead of blindly overwriting concurrent work. The
// daemon's collab monitor only ever warns; whether a warning changes behaviour
// is up to the agent, and this hint is what makes it do so. Worded conditionally
// so a solo agent (no concurrent peers) does nothing extra. No apostrophes —
// keeps the single-quoted shell form (shellQuoteArg) clean.
const collabHintGuidance = "warden may run other agents concurrently, each in its own git worktree. " +
	"Before editing a file that peers are likely to touch, call who_is_editing_file " +
	"(or get_collaboration_status) to see whether another agent is already changing it; " +
	"if so, coordinate through send_message rather than overwriting their work. " +
	"Check read_inbox for file-conflict warnings from the daemon and reconcile before you commit. " +
	"This applies only when you share a repo with concurrent agents; for a solo task there is nothing to coordinate."

// collabHint returns the claude flag fragment that injects collabHintGuidance as
// a system-prompt addendum, or "" when the collab_hint config setting is
// disabled. The leading space lets callers concatenate it directly onto a
// claudeLaunch string. Applied to every spawn path that launches claude —
// plain agents and pipeline jobs alike, since parallel jobs are the prime
// file-conflict scenario.
func (l *Lifecycle) collabHint(b agentbackend.Backend) string {
	return systemPromptHint(b, l.cfg.GetCollabHint(), collabHintGuidance)
}

// gitConventionsGuidance steers a spawned agent toward warden's git lifecycle
// tools instead of raw git Bash: warden enforces the branch rail, runs hooks
// deterministically, and links the SHA to this agent record. This is the soft
// (Layer 1) nudge — the PreToolUse redirect hook is the hard enforcement; the
// two reinforce each other. No apostrophes — keeps the single-quoted shell form
// (shellQuoteArg) clean.
const gitConventionsGuidance = "Prefer warden git tools over raw git Bash: wd commit (or mcp__warden__commit) " +
	"to stage+commit, wd push (mcp__warden__push) to push your branch, wd sync (mcp__warden__sync) to rebase onto the base. " +
	"warden enforces the branch rail (never commits to main), runs pre-commit hooks and returns only failures, " +
	"and links the commit to this agent. git status/log/diff stay yours to run directly. " +
	"Run the project tests/lint/build with wd check (or mcp__warden__check) when the repo defines them in .warden/check.yml — " +
	"warden runs the configured commands and returns only the failures, not the full log."

// gitConventionsHint returns the claude flag fragment that injects
// gitConventionsGuidance as a system-prompt addendum, or "" when the
// git_conventions config setting is disabled. Applied to typed (worktree-backed)
// agents — the ones that commit — alongside collabHint/guardSettingsFlag.
func (l *Lifecycle) gitConventionsHint(b agentbackend.Backend) string {
	return systemPromptHint(b, l.cfg.GetGitConventions(), gitConventionsGuidance)
}

// memStore returns the memory reader for launch-time projection, defaulting to a
// zero-value Store (git-shelling repo-root resolution) when none is injected.
func (l *Lifecycle) memStore() *memory.Store {
	if l.MemStore != nil {
		return l.MemStore
	}
	return &memory.Store{}
}

// memoryGuidance renders the repo's curated .warden/memory.md as a projection
// string for an agent spawning in dir (#53 PR-1), or "" when memory_inject is off,
// the file is absent/empty, or resolution fails. It is ONE MORE guidance string
// threaded through the SAME systemPromptHint / injectContext assembly the
// collab/pipeline/git hints already ride — Claude via --append-system-prompt, the
// six file-drop backends via their AGENTS.md/CRUSH.md/.goosehints warden block,
// aider degrade-skips (neither seam). Read-only: it never auto-creates the file, so
// a repo with no memory.md — and memory_inject off — leaves the launch byte-identical
// to today (the regression-lock). Any failure degrades to "" and never blocks a
// spawn: memory is additive, exactly like its sibling hints.
func (l *Lifecycle) memoryGuidance(ctx context.Context, dir string) string {
	if !l.cfg.GetMemoryInject() {
		return ""
	}
	path, err := l.memStore().Locate(ctx, dir)
	if err != nil {
		slog.Debug("spawn: resolve project memory path failed; skipping projection", "dir", dir, "err", err)
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("spawn: read project memory failed; skipping projection", "path", path, "err", err)
		}
		return ""
	}
	return memory.Parse(string(raw)).RenderDefault()
}

// resolveRole applies the requested built-in role to req: it validates the role
// name and fills each unset spawn field from the role's defaults, with precedence
// explicit request value > role default > global default. type/model/
// permission_mode fill only when the request left them empty; auto_approve (a
// bool with no tri-state) is OR-ed in so an explicit true and a role default of
// true both enable it; tags are UNIONED onto the request's tags (normalized,
// de-duplicated) rather than replacing them. It returns the resolved Role so the
// caller can inject its persona, and mutates req in place. An empty role
// normalizes to role.Default ("general"); an unknown name is an error.
func resolveRole(req *SpawnRequest) (role.Role, error) {
	name := req.Role
	if name == "" {
		name = role.Default
	}
	r, ok := role.Get(name)
	if !ok {
		return role.Role{}, fmt.Errorf("unknown role %q (known: %s)", name, strings.Join(role.Names(), ", "))
	}
	// Persist the canonical name, but keep the default (general) as "" so a plain
	// agent's record stays byte-identical to today (role JSON-omitted). Non-default
	// role names are exact (no aliases), so the input is already canonical.
	if r.Name == role.Default {
		req.Role = ""
	} else {
		req.Role = r.Name
	}
	d := r.Defaults
	if req.Type == "" && d.Type != "" {
		req.Type = store.Type(d.Type)
	}
	if req.Model == "" {
		req.Model = d.Model
	}
	if req.PermissionMode == "" {
		req.PermissionMode = d.PermissionMode
	}
	req.AutoApprove = req.AutoApprove || d.AutoApprove
	if len(d.Tags) > 0 {
		req.Tags = store.NormalizeTags(append(append([]string{}, req.Tags...), d.Tags...))
	}
	return r, nil
}

// personaGuidance returns the trimmed persona text for the built-in role named
// name, or "" for the general/empty role (or an unknown name — defensive; Spawn
// already validated it). It is re-resolved from the registry at every (re)launch
// so switching a role + resuming re-injects the new persona; only the role NAME
// is stored on the session, never the persona text. The persona is always
// injected when non-empty — unlike the config-gated collab/pipeline/git hints —
// since it is the agent's defining instruction, not an optional nudge.
func personaGuidance(name string) string {
	r, ok := role.Get(name)
	if !ok {
		return ""
	}
	return strings.TrimSpace(r.Persona)
}

// launchModel resolves the model id passed to a backend's LaunchCmd. Claude
// applies warden's alias/default expansion (opus/sonnet/… → full id, empty →
// the configured default); other backends receive the raw model unchanged —
// warden's Claude aliases don't map onto a different provider's model ids, and a
// bring-your-own-model backend (Aider) supplies its own default when empty.
func (l *Lifecycle) launchModel(b agentbackend.Backend, model string) string {
	if b.ID() == agentbackend.DefaultID {
		return l.modelOrDefault(model)
	}
	return model
}

// promptArg returns the launch fragment that seeds the initial task prompt for
// backend b from promptFile, or "" when there is no prompt (interactive agent).
// The adapter decides the delivery shape (Claude: trailing positional; Aider:
// --message) and owns the shell-quoting.
func (l *Lifecycle) promptArg(b agentbackend.Backend, promptFile string) string {
	if promptFile == "" {
		return ""
	}
	return b.LaunchPromptArg(promptFile)
}

// buildLaunch returns the bare backend launch command for a spawn — the base the
// spawn paths then concatenate hint/prompt/exit suffixes onto. For a normal spawn
// (req.ForkFrom == "") it returns exactly b.LaunchCmd(...), so the assembled launch
// line is byte-identical to the pre-fork path. For a fork it builds the command via
// the backend's SessionForker instead: a backend that does not implement it (Claude,
// by construction) degrades to a clean "cannot fork" error rather than launching a
// bare agent. The source's pinned backend session id and branch are already resolved
// by the daemon adapter (lifecycle is store-free); this only shapes the command.
func (l *Lifecycle) buildLaunch(b agentbackend.Backend, req SpawnRequest, sess *store.Session, mode string) (string, error) {
	if req.ForkFrom == "" {
		return b.LaunchCmd(agentbackend.LaunchOpts{
			SessionID: sess.ClaudeSessionID, Name: sess.ID, Model: l.launchModel(b, req.Model), Mode: mode,
		}), nil
	}
	fk, ok := b.(agentbackend.SessionForker)
	if !ok {
		return "", fmt.Errorf("backend %s cannot fork a session", b.ID())
	}
	cmd, ok := fk.ForkCmd(agentbackend.ForkOpts{
		SourceSessionID: req.ForkSourceSessionID, Name: sess.ID, Model: l.launchModel(b, req.Model), Mode: mode,
		Workdir: sess.Workdir, // the fork's own worktree → codex -C, suppresses the working-dir picker
	})
	if !ok {
		return "", fmt.Errorf("backend %s cannot fork session %q", b.ID(), req.ForkSourceSessionID)
	}
	return cmd, nil
}

// carryDirtyTree seeds the SOURCE agent's UNCOMMITTED tracked changes into the fork
// worktree (PR-2 dirty-tree carry, §7), so a fork diverges from the source's EXACT
// live state rather than only its branch HEAD. It reuses the SAME non-destructive
// stash primitive the snapshot package uses (internal/snapshot Capture/Restore):
// `git stash create` in the source builds a commit object recording its working tree
// WITHOUT perturbing it (no stash entry pushed, no index touched — the source agent
// runs on untouched), and `git stash apply <sha>` re-applies that commit into the
// fork worktree. The fork is a `git worktree` sibling sharing the source's object
// database, so the stash commit is reachable from the fork with no transfer.
//
// A CLEAN source tree yields an empty stash sha — nothing to carry, so this is a
// no-op and the fork stays HEAD-only (exactly the PR-1 behavior). The apply is
// conflict-free by construction: the fork worktree's HEAD IS the source branch HEAD
// the stash was created against (ensureWorktree bases the fork off ForkSourceBranch),
// so the patch re-applies onto the same base it came from.
//
// CAVEAT (design §8.4 #3): `git stash create` captures only TRACKED changes — the
// source's untracked / .gitignore'd build artifacts are NOT carried (the same
// contract the snapshot package has). So the fork's tree is not byte-identical to the
// source's; its tracked working diff is.
func (l *Lifecycle) carryDirtyTree(ctx context.Context, srcWorkdir, forkWorkdir string) error {
	out, err := l.run.Run(ctx, srcWorkdir, "git", "stash", "create", "warden fork dirty-carry")
	if err != nil {
		return fmt.Errorf("fork dirty-carry: git stash create in source: %w: %s", err, strings.TrimSpace(out))
	}
	sha := strings.TrimSpace(out)
	if sha == "" {
		return nil // clean source tree — nothing to carry; the fork stays HEAD-only
	}
	if out, err := l.run.Run(ctx, forkWorkdir, "git", "stash", "apply", sha); err != nil {
		return fmt.Errorf("fork dirty-carry: git stash apply %s into fork: %w: %s", sha, err, strings.TrimSpace(out))
	}
	return nil
}

// guardSettings returns the Claude --settings launch fragment for the isolation/
// git/check/root guard hooks, but only for the Claude backend: the hooks are
// installed via Claude Code's settings mechanism, which other backends don't
// share. A non-Claude agent therefore runs without the PreToolUse guards (its
// adapter would wire equivalent enforcement if/when one exists).
func (l *Lifecycle) guardSettings(b agentbackend.Backend, id string) string {
	if b.ID() != agentbackend.DefaultID {
		return ""
	}
	return l.guardSettingsFlag(id)
}

// classifyInstruction is prepended to the task prompt for headless classification.
const classifyInstruction = "You are a classifier. Classify the following agent task into exactly one of these labels: development, analysis, spike, pr-review, code, docs, website, debug-ci, tests, other. Reply with ONLY the label, nothing else.\n\nTask: "

// classifyArg builds the single argument passed to `claude -p`.
func classifyArg(prompt string) string { return classifyInstruction + prompt }

const summaryInstruction = "In 8 words or fewer, summarize what this agent is currently working on. Reply with ONLY the phrase — no quotes, no preamble.\n\nRecent activity:\n"

// nameInstruction asks the local model for a short, memorable handle for an
// agent derived from its task. The reply is sanitized by parseName, so the model
// only needs to get close to the kebab-case format.
const nameInstruction = "Generate a short, memorable handle (2-3 words) for an agent doing the task below. Use lowercase kebab-case, letters/digits/hyphens only, max 24 characters. Reply with ONLY the handle — no quotes, no preamble.\n\nTask: "

// nameArg builds the single argument passed to the local model for naming,
// capping the prompt so a huge task description can't blow up local inference.
func nameArg(prompt string) string {
	const max = 2000
	if len(prompt) > max {
		prompt = prompt[:max]
	}
	return nameInstruction + prompt
}

// parseName normalizes a free-form model reply (or a prompt fragment) into a
// stored-name handle: the first line, lowercased, with runs of spaces/underscores/
// hyphens collapsed to single hyphens, every other character dropped, trimmed of
// edge hyphens, and capped at 32 runes. The result is always a valid name per
// store.ValidateName (1-32 of [a-z0-9-]) or "" when nothing usable remains.
func parseName(out string) string {
	line := strings.TrimSpace(out)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	line = strings.ToLower(line)
	var b strings.Builder
	prevHyphen := false
	for _, r := range line {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevHyphen = false
		case r == '-' || r == '_' || r == ' ' || r == '\t':
			if b.Len() > 0 && !prevHyphen {
				b.WriteByte('-')
				prevHyphen = true
			}
		}
	}
	name := strings.Trim(b.String(), "-")
	if len(name) > 32 { // name is ASCII here, so bytes == runes
		name = strings.Trim(name[:32], "-")
	}
	return name
}

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
	// backend is the default agent backend (Claude) resolved from the registry.
	// Per-session backends are resolved via backendFor; in Phase 0 every session
	// is Claude, so this is also the effective backend everywhere.
	backend agentbackend.Backend
	// ProjectsDir is the Claude Code transcript root (default empty → transcript
	// lookup disabled; the daemon sets it from config). Overridable in tests.
	ProjectsDir string
	// PromptsDir is a single shared directory (the daemon sets it from config,
	// e.g. ~/.warden/prompts) where free-form agents with a prompt drop their
	// initial prompt file, keyed by agent id. It is NOT per-agent and is never the
	// dir the agent runs in — agents launch in the caller's cwd. Interactive
	// (empty-prompt) agents write no file. Overridable in tests.
	PromptsDir string
	// HintsDir is a shared dir (the daemon sets it, e.g. ~/.warden/hints) where a
	// flag-based backend's system-prompt addendum (collab/git/pipeline guidance) is
	// written, keyed by agent id, so the launch line references it via
	// --append-system-prompt "$(cat <file>)" instead of carrying ~1.6 KB of inline
	// text. That inline text would push the typed command past the tty canonical-mode
	// line limit (1024 bytes on macOS/BSD), truncating it so the agent never starts.
	// Empty (tests / older configs) disables file-backing — the addendum then rides
	// the launch line inline exactly as before. Never the dir the agent runs in.
	HintsDir string
	// MemStore reads the repo's curated .warden/memory.md for launch-time projection
	// (#53 PR-1). The zero value (nil) uses a default memory.Store, which resolves the
	// repo root by shelling `git rev-parse`; tests inject a Store with a stub RepoRoot
	// to stay hermetic. Projection is read-only — it never auto-creates the file (that
	// is the `wd memory` verb's job), so a repo with no memory.md projects nothing.
	MemStore *memory.Store
	// ExitsDir is a shared dir (the daemon sets it, e.g. ~/.warden/exits) where
	// each agent's shell records claude's exit status, keyed by agent id. Empty
	// (tests) disables exit capture — agents then fall back to orphaned-only
	// classification. Never the dir the agent runs in.
	ExitsDir string
	// SettingsDir is a shared dir (the daemon sets it, e.g. ~/.warden/settings)
	// where each isolated agent's generated `claude --settings` file is written,
	// keyed by agent id. The file installs the PreToolUse isolation guard hook.
	// Empty (tests) disables the guard injection.
	SettingsDir string
	// WardenBin is the absolute path to the warden binary (the daemon sets it via
	// os.Executable). It is the command the generated settings file invokes as the
	// PreToolUse hook (`<WardenBin> hook guard`). Empty disables guard injection.
	WardenBin string
	// LLM is the optional local-model provider (Ollama). nil — the default — means
	// the local LLM is off, so every LLM-backed method uses its headless-Claude or
	// deterministic fallback. The daemon sets it only when config enables it.
	LLM llm.Completer
	// SavingsHook, when set, is called by the LLM-offload sites (Classify/Summarize/
	// GenerateName/commit-message) when a responsibility is served by the local
	// model instead of warden's own Claude — with the prompt tokens that never
	// reached Claude (rawTokens) and what still did (keptTokens, 0 for a full
	// offload), plus the optional provenance bytes (rawSample is the offload prompt;
	// keptSample is "" for an offload). The daemon wires it to the savings ledger;
	// nil disables recording. Called inline on the offload path, so it must be cheap
	// and must not panic.
	SavingsHook func(feature, agent string, rawTokens, keptTokens int, rawSample, keptSample string)
	// goos and readPSI are the platform seams for MemoryPressure: runtime.GOOS
	// and a /proc/pressure/memory read in production, injected by tests so both
	// kernel sources are exercised on any host.
	goos    string
	readPSI func() (string, error)
}

// recordOffload reports a fully-offloaded local-model call to the savings hook (if
// any): the whole prompt left warden's Claude spend, so rawTokens is the prompt's
// estimated size and keptTokens is 0. agent may be "" (an unattributed call).
func (l *Lifecycle) recordOffload(agent, prompt string) {
	if l.SavingsHook == nil {
		return
	}
	// The prompt is the raw provenance sample (it left Claude's spend entirely);
	// there is no kept side for a full offload. The hook truncates/gates the sample.
	l.SavingsHook(savings.FeatureLLMOffload, agent, savings.EstimateTokens([]byte(prompt)), 0, prompt, "")
}

// ConfigProvider is the subset of config.Config that lifecycle needs.
// Extracted to avoid a circular dependency and to allow test doubles.
type ConfigProvider interface {
	GetDefaultPermissionMode() string
	GetModelDefault() string
	GetPipelineHint() bool
	GetCollabHint() bool
	GetMemoryInject() bool
	GetIsolationGuard() bool
	GetGitConventions() bool
	GetGitRedirect() bool
	GetCheckRedirect() bool
	GetRootGuard() bool
}

func New(r Runner, cfg ConfigProvider) *Lifecycle {
	return &Lifecycle{run: r, cfg: cfg, backend: agentbackend.Default(), goos: runtime.GOOS, readPSI: readPSIFile}
}

// backendFor resolves the backend for a session by its Backend field, falling
// back to the default (Claude) for an empty/unknown id. Empty ⇒ Claude keeps
// existing stores back-compatible (Session.Backend is omitempty).
func (l *Lifecycle) backendFor(id string) agentbackend.Backend {
	if b, err := agentbackend.Get(id); err == nil {
		return b
	}
	return l.backend
}

// SpawnRequest is the type-aware input to Spawn (design §2 / §6).
type SpawnRequest struct {
	Type           store.Type
	Ticket         string // optional; becomes the id when present
	Name           string // optional; human-readable name for the agent
	Repo           string
	Branch         string   // optional; development branch / pr-review checkout target
	PR             string   // optional; pr-review
	Worktree       bool     // analysis/spike opt-in
	InRepo         bool     // write-agent opt-out: share the repo instead of isolating in a worktree (ignored for pr-review)
	Prompt         string   // free-form: the agent's initial prompt (no repo/worktree); empty = interactive
	Cwd            string   // free-form: dir to launch claude from (the caller's "master shell"); required
	PermissionMode string   // explicit mode override; empty = use global default
	AutoRestart    bool     // opt-in: auto-resume this agent when it errors (capped)
	AutoApprove    bool     // opt-in: auto-approve yes/no prompts (also filled by a role default)
	Model          string   // claude model (opus/sonnet/haiku or full ID); empty = default
	Backend        string   // agent backend id (claude, aider, …); empty = claude (the default)
	Tags           []string // optional free-form labels for grouping/filtering (#30)
	Role           string   // built-in role (persona + default flags); empty = "general" (no persona)
	ParentID       string   // id of the agent that spawned this one; empty = root (operator/CLI spawn)

	// Fork fields (codex fork superpower, #52). Set by the daemon adapter when a
	// spawn carries fork_from: the adapter (which owns the store) resolves the
	// source agent ONCE and hands lifecycle the already-read values, so lifecycle
	// stays store-free. ForkFrom non-empty ⇒ this is a fork: build the launch
	// command via the backend's SessionForker instead of LaunchCmd, and base the
	// worktree off ForkSourceBranch. ForkSourceSessionID is the source backend's
	// PINNED session id (codex rollout UUID) the fork branches from.
	ForkFrom            string // source agent id (provenance / "this is a fork" signal); empty = normal spawn
	ForkSourceSessionID string // source backend session id (pinned) to fork from
	ForkSourceBranch    string // source agent's branch — the fork worktree's base (§7)
	// ForkSourceWorkdir is the source agent's absolute worktree, the read-side of the
	// PR-2 dirty-tree carry (§7): the fork ALSO receives the source's UNCOMMITTED
	// tracked changes (not just its branch HEAD) so it diverges from the source's EXACT
	// live state. The adapter resolves it from the source session (lifecycle stays
	// store-free). Empty ⇒ carry nothing (HEAD-only fork, the PR-1 behavior).
	ForkSourceWorkdir string
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

// wantWorktree applies the per-type isolation policy (Phase 0a). Every
// write-agent (DefaultWorktree types) is isolated in its own worktree unless the
// caller passes --in-repo (req.InRepo) to share the repo deliberately —
// pr-review is exempt from that opt-out because it is structurally a separate
// checkout (a PR laid over your tree). The investigation types (analysis/spike)
// remain opt-in via --worktree; the free-form catch-all (other) never isolates.
func wantWorktree(req SpawnRequest) bool {
	if req.Type == store.TypePRReview {
		return true
	}
	if req.Type.DefaultWorktree() {
		return !req.InRepo
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

// cleanupPartialWorktree best-effort reverses a `git worktree add` that failed
// partway — most importantly when the daemon's request context is cancelled
// mid-checkout (e.g. a spawn that outran its budget) and git is SIGKILLed,
// leaving a registered — sometimes locked — worktree that would otherwise orphan
// (the exact "locked orphan worktree" symptom). It runs on a detached context
// (the request context may already be cancelled) and every step is best-effort,
// logged rather than returned: unlock, force-remove, prune the admin metadata,
// and delete the branch we were creating (branch == "" for a detached/adopted
// checkout, so a user's existing branch is never deleted). Only ever called for
// the path this spawn was creating — ensureWorktree returns early for an adopted,
// pre-existing worktree — so force-removal can never touch a user's tree.
func (l *Lifecycle) cleanupPartialWorktree(repo, rel, branch string) {
	ctx := context.Background()
	_, _ = l.run.Run(ctx, "", "git", "-C", repo, "worktree", "unlock", rel)
	_, _ = l.run.Run(ctx, "", "git", "-C", repo, "worktree", "remove", "--force", rel)
	if out, err := l.run.Run(ctx, "", "git", "-C", repo, "worktree", "prune"); err != nil {
		slog.Warn("spawn cleanup: prune partial worktree failed", "worktree", rel, "err", err, "out", strings.TrimSpace(out))
	}
	if branch != "" {
		_, _ = l.run.Run(ctx, "", "git", "-C", repo, "branch", "-D", branch)
	}
}

// worktreeExists checks `git worktree list --porcelain` for an absolute path.
func (l *Lifecycle) worktreeExists(ctx context.Context, repo, rel string) (bool, error) {
	entries, err := l.gitWorktrees(ctx, repo)
	if err != nil {
		return false, err
	}
	want := filepath.Join(repo, rel)
	for _, e := range entries {
		if e.Path == want {
			return true, nil
		}
	}
	return false, nil
}

// ensureWorktree creates (or adopts) the worktree and returns the branch name
// recorded on the doc (empty for a detached pr-review checkout) plus whether it
// CREATED the worktree and whether it CREATED the branch (vs. adopted a
// pre-existing worktree / checked out a user-named branch). worktreeCreated lets
// a failed spawn roll back only worktrees it made, never the user's existing
// ones; branchCreated gates later branch deletion to branches warden owns.
func (l *Lifecycle) ensureWorktree(ctx context.Context, req SpawnRequest, id, rel string) (branch string, worktreeCreated, branchCreated bool, err error) {
	// req.Branch and req.PR flow into git/gh as positional args; reject an
	// option-like value (leading '-') before it can be parsed as a flag.
	if err := safeGitRef(req.Branch); err != nil {
		return "", false, false, err
	}
	if err := safeGitRef(req.PR); err != nil {
		return "", false, false, err
	}
	// ForkSourceBranch flows into `git worktree add` as a positional start point;
	// reject an option-like value before git could parse it as a flag.
	if err := safeGitRef(req.ForkSourceBranch); err != nil {
		return "", false, false, err
	}
	exists, err := l.worktreeExists(ctx, req.Repo, rel)
	if err != nil {
		return "", false, false, err
	}
	if exists { // adopt — we did not create the worktree or its branch
		if req.Branch != "" {
			return req.Branch, false, false, nil
		}
		return id, false, false, nil
	}
	if req.Type == store.TypePRReview && req.Branch == "" {
		// Detached worktree, then let gh resolve + fetch the PR branch.
		if out, err := l.run.Run(ctx, req.Repo, "git", "worktree", "add", "--detach", rel); err != nil {
			l.cleanupPartialWorktree(req.Repo, rel, "") // detached: no branch of ours to delete
			return "", false, false, wrapWorktreeError(fmt.Errorf("git worktree add --detach: %w", err), out, rel)
		}
		abs := filepath.Join(req.Repo, rel)
		if out, err := l.run.Run(ctx, abs, "gh", "pr", "checkout", req.PR); err != nil {
			// Wrap gh command-not-found errors with install hint
			if strings.Contains(strings.ToLower(out), "command not found") ||
				strings.Contains(strings.ToLower(out), "not found") {
				hint := commandInstallHint("gh")
				return "", true, false, fmt.Errorf("gh pr checkout: %w: %s\n%s", err, out, hint)
			}
			return "", true, false, fmt.Errorf("gh pr checkout: %w: %s", err, out)
		}
		// gh leaves HEAD on the local branch it created. Capture it so warden owns
		// its deletion on cleanup (no leak). If rev-parse fails or returns HEAD
		// (still detached), fall back to branch="" and let prune sweep it by name.
		out, err := l.run.Run(ctx, abs, "git", "rev-parse", "--abbrev-ref", "HEAD")
		ghBranch := strings.TrimSpace(out)
		if err != nil || ghBranch == "" || ghBranch == "HEAD" {
			return "", true, false, nil
		}
		return ghBranch, true, true, nil
	}
	if req.Type == store.TypePRReview { // checkout the given existing branch (adopted)
		if out, err := l.run.Run(ctx, req.Repo, "git", "worktree", "add", rel, req.Branch); err != nil {
			l.cleanupPartialWorktree(req.Repo, rel, "") // adopted branch: never delete it
			return "", false, false, wrapWorktreeError(fmt.Errorf("git worktree add: %w", err), out, rel)
		}
		return req.Branch, true, false, nil
	}
	// development / opt-in analysis|spike → new branch (branch = req.Branch or id).
	branch = req.Branch
	if branch == "" {
		branch = id
	}
	// A fork bases its branch off the SOURCE agent's branch HEAD (§7), so the forked
	// conversation continues against the committed state it diverged from — a fresh
	// SIBLING worktree, not the repo default and not shared with the source. A normal
	// spawn appends no start point and branches off the repo's current HEAD as before.
	args := []string{"worktree", "add", rel, "-b", branch}
	if req.ForkFrom != "" && req.ForkSourceBranch != "" {
		args = append(args, req.ForkSourceBranch)
	}
	if out, err := l.run.Run(ctx, req.Repo, "git", args...); err != nil {
		l.cleanupPartialWorktree(req.Repo, rel, branch) // -b created this branch: delete it too
		return "", false, false, wrapWorktreeError(fmt.Errorf("git worktree add: %w", err), out, rel)
	}
	return branch, true, true, nil
}

// Classify labels a task prompt into a store.Type. When the local LLM is enabled
// it tries that first (the cheapest responsibility to move off warden's own
// Claude spend — pure classification with a safe fallback), and on any local
// error falls back to headless Claude. On a Claude error it returns TypeOther
// alongside the error so callers degrade gracefully. A successful local call is
// trusted as-is: the fallback exists for unavailability, not to second-guess the
// model's label.
func (l *Lifecycle) Classify(ctx context.Context, prompt string) (store.Type, error) {
	arg := classifyArg(prompt)
	if l.LLM != nil {
		if out, err := l.LLM.Complete(ctx, arg); err == nil {
			l.recordOffload("", arg) // the whole classify call stayed off warden's Claude spend
			return parseType(out), nil
		} else {
			slog.Warn("classify: local LLM failed, falling back to claude", "err", err)
		}
	}
	out, err := l.runClaudeP(ctx, arg)
	if err != nil {
		return store.TypeOther, fmt.Errorf("claude -p: %w: %s", err, out)
	}
	return parseType(out), nil
}

// Summarize produces a one-line subject for an agent: it reads recent activity
// (transcript, else pane) and asks for an <=8-word phrase. When the local LLM is
// enabled it tries that first (summarization is a fuzzy-but-cheap task, safe to
// move off warden's own Claude spend), and falls back to headless Claude on any
// local error or an empty reply — an empty summary carries no signal, so unlike
// Classify it is not trusted as a final answer.
func (l *Lifecycle) Summarize(ctx context.Context, sess *store.Session) (string, error) {
	text := l.recentActivity(ctx, sess)
	if strings.TrimSpace(text) == "" {
		text = sess.Prompt // last resort: the original prompt
	}
	if strings.TrimSpace(text) == "" {
		return "", nil
	}
	arg := summaryArg(text)
	if l.LLM != nil {
		if out, err := l.LLM.Complete(ctx, arg); err == nil {
			if s := parseSummary(out); s != "" {
				l.recordOffload(sess.ID, arg) // summary served locally, not by warden's Claude
				return s, nil
			}
		} else {
			slog.Warn("summarize: local LLM failed, falling back to claude", "err", err)
		}
	}
	out, err := l.runClaudeP(ctx, arg)
	if err != nil {
		return "", fmt.Errorf("claude -p: %w: %s", err, out)
	}
	return parseSummary(out), nil
}

// GenerateName derives a short human-friendly handle for an agent from its task
// prompt. When the local LLM is enabled it asks for a kebab-case handle; on any
// local error, an empty/unusable reply, or when no local model is configured it
// falls back to a deterministic slug of the prompt's first words. Naming is purely
// cosmetic, so — unlike Classify/Summarize — it never spends warden's own Claude
// budget. The returned name is sanitized to the stored-name format; "" means no
// usable name (e.g. an empty prompt). Uniqueness is the caller's concern.
func (l *Lifecycle) GenerateName(ctx context.Context, prompt string) string {
	if strings.TrimSpace(prompt) == "" {
		return ""
	}
	if l.LLM != nil {
		// No savings event here: GenerateName's fallback is a deterministic slug,
		// not Claude — the local model displaces no Claude call, so offloading it
		// saves nothing to record (unlike Classify/Summarize).
		if out, err := l.LLM.Complete(ctx, nameArg(prompt)); err == nil {
			if n := parseName(out); n != "" {
				return n
			}
		} else {
			slog.Warn("generate name: local LLM failed, using deterministic fallback", "err", err)
		}
	}
	return parseName(firstWords(prompt, 4))
}

// checkSummaryInstruction prompts the local model to distil oversized check
// failure output into the distinct failures an agent must act on. It must preserve
// error text verbatim and never speculate about fixes — warden condenses the log,
// it does not interpret the failure.
const checkSummaryInstruction = "The following is failing test, build, or lint output. List each distinct failure as a single line: the failing test or file:line followed by the exact error message, verbatim. Do not summarize away the error text and do not suggest fixes. Reply with ONLY the list.\n\nOutput:\n"

// checkSummaryMarker prefixes a model-condensed failure so the agent knows it is
// reading a distilled view rather than the raw runner output.
const checkSummaryMarker = "⚠ failures condensed by local model (raw output exceeded the line cap):\n"

// checkSummaryArg builds the prompt for condensing failure output, capping the
// model's input to the tail (where runners print the decisive failure) so a huge
// log can't blow up local inference. The tail may begin mid-rune after slicing;
// drop the leading partial rune so the model gets valid UTF-8.
func checkSummaryArg(out string) string {
	const max = 16000
	if len(out) > max {
		out = out[len(out)-max:]
		for len(out) > 0 && !utf8.RuneStart(out[0]) {
			out = out[1:]
		}
	}
	return checkSummaryInstruction + out
}

// parseCheckSummary cleans the model's condensed-failure reply and defensively
// caps it at the same line budget as the deterministic truncation, so a runaway
// model can never spill more than the tail it replaces.
func parseCheckSummary(out string) string {
	s := strings.TrimSpace(out)
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > maxCheckOutputLines {
		lines = lines[:maxCheckOutputLines]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// summarizeCheckOutput renders a failed check's captured output for the agent. The
// deterministic result is the tail-truncated log (truncateTail). When a local model
// is configured AND the raw output is oversized, it asks the model to distil the
// distinct failures and returns that instead — fewer transcript tokens spent reading
// a failure the truncation would have clipped anyway. Any model error, an empty
// reply, or no model at all falls back to the deterministic tail, so the agent never
// loses the failure to a slow or absent model.
func (l *Lifecycle) summarizeCheckOutput(ctx context.Context, name, out string) string {
	truncated := truncateTail(out, maxCheckOutputLines)
	if l.LLM == nil || !oversizedOutput(out) {
		return truncated
	}
	summary, err := l.LLM.Complete(ctx, checkSummaryArg(out))
	if err != nil {
		slog.Warn("check: local LLM summarize failed, using truncated tail", "check", name, "err", err)
		return truncated
	}
	if s := parseCheckSummary(summary); s != "" {
		return checkSummaryMarker + s
	}
	return truncated
}

// oversizedOutput reports whether s holds more than maxCheckOutputLines lines —
// the threshold at which truncateTail starts dropping lines and a condensed view
// can earn its keep.
func oversizedOutput(s string) bool {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return false
	}
	return strings.Count(s, "\n")+1 > maxCheckOutputLines
}

// transcriptPath resolves the agent's claude transcript file. With a pinned
// ClaudeSessionID the file is exactly <id>.jsonl: look under the encoded project
// dir first, then an unambiguous glob across all project dirs (the UUID is
// globally unique, so this is robust to cwd path-encoding quirks). With no
// pinned id (legacy sessions) it falls back to the newest .jsonl in the dir.
func (l *Lifecycle) transcriptPath(sess *store.Session) string {
	p, _ := l.backendFor(sess.Backend).TranscriptPath(l.ProjectsDir, sess.Workdir, sess.ClaudeSessionID)
	return p
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

// MemoryPressure reads the OS memory-pressure level: Linux's PSI memory file
// (/proc/pressure/memory) on linux, the memorystatus sysctl on macOS (and on
// any other GOOS, where the sysctl attempt simply fails). Best-effort: on any
// error (file/sysctl missing, PSI disabled via psi=0, unparseable output) it
// degrades to pressure.Normal with no error, so the spawn gate falls back to
// count-only.
func (l *Lifecycle) MemoryPressure(ctx context.Context) (pressure.Level, error) {
	if l.goos == "linux" && l.readPSI != nil {
		raw, err := l.readPSI()
		if err != nil {
			return pressure.Normal, nil
		}
		lvl, perr := pressure.ParsePSI(raw)
		if perr != nil {
			return pressure.Normal, nil
		}
		return lvl, nil
	}
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

// readPSIFile reads /proc/pressure/memory. On a kernel booted with psi=0 the
// file exists but the read fails (EOPNOTSUPP), which the caller degrades to
// Normal like any other error.
func readPSIFile() (string, error) {
	b, err := os.ReadFile("/proc/pressure/memory")
	return string(b), err
}

// RunClaudeP exposes the bounded headless `claude -p` runner (the same plumbing
// Classify/Summarize use) so the digest Narrator can reuse it.
func (l *Lifecycle) RunClaudeP(ctx context.Context, arg string) (string, error) {
	return l.runClaudeP(ctx, arg)
}

// LocalLLM exposes the optional local-model completer (nil when local_llm is off) so
// the memory-curation proposer can prefer the $0 local path before degrading to
// headless claude -p — the same offload preference Summarize/Classify use.
func (l *Lifecycle) LocalLLM() llm.Completer { return l.LLM }

// RecordOffload books a fully-offloaded local-model call to the savings ledger, so a
// curation pass served by the local LLM is credited exactly like Summarize/Classify.
func (l *Lifecycle) RecordOffload(agent, prompt string) { l.recordOffload(agent, prompt) }

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
	// Resolve the role FIRST: its defaults fill unset request fields, and a role
	// default type (e.g. implementer ⇒ development) can flip a spawn from free-form
	// to typed, so this must precede the freeMode decision below.
	if _, err := resolveRole(&req); err != nil {
		return nil, err
	}
	freeMode := req.Type == ""
	if !freeMode {
		req.Type = store.NormalizeType(string(req.Type))
	}
	// Reject an unknown backend up front (before any tmux/worktree side effects),
	// so a typo fails cleanly rather than launching the wrong agent. An empty
	// backend resolves to Claude (back-compat) inside agentbackend.Get.
	if _, err := agentbackend.Get(req.Backend); err != nil {
		return nil, err
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
		Tags:           store.NormalizeTags(req.Tags),
		Status:         store.StatusSpawning,
		PermissionMode: req.PermissionMode,
		AutoRestart:    req.AutoRestart,
		AutoApprove:    req.AutoApprove,
		Model:          req.Model,
		Backend:        req.Backend,
		Role:           req.Role,
	}
	// Record provenance, but never let an agent be its own parent (a self-id would
	// create a degenerate cycle in the sub-tree view).
	if req.ParentID != id {
		sess.ParentID = req.ParentID
	}
	// Only pinning backends (Caps.SessionIDControl) take a warden-minted session
	// id — for them the id is pinned at launch for a deterministic transcript path
	// + --resume. A non-pinning backend (codex, cursor, antigravity, …) mints its
	// own id and ignores the one we'd pass, so minting here would leave
	// ClaudeSessionID holding a UUID that matches no on-disk transcript. Instead we
	// leave it empty (empty already = the dir-scoped transcript fallback, safe even
	// before discovery lands) and let the poller discover-then-pin the agent's real
	// id post-launch (design §5.2; agentbackend.SessionIDDiscoverer).
	if l.backendFor(req.Backend).Capabilities().SessionIDControl {
		sess.ClaudeSessionID, err = store.NewSessionID()
		if err != nil {
			return nil, err
		}
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
	// A fork needs its own worktree (dir-scoped discover-then-pin, §5/§7); a free-form
	// spawn has none. Free-form fork is explicitly deferred (design §4.2) — reject it
	// rather than launch a fork that would share the caller's cwd and mis-pin.
	if req.ForkFrom != "" {
		return nil, fmt.Errorf("fork requires a typed (worktree-backed) spawn; free-form fork is not supported")
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
	promptFile := ""
	if req.Prompt != "" {
		if l.PromptsDir == "" {
			return nil, fmt.Errorf("prompt spawn requires a prompts dir")
		}
		if out, err := l.run.Run(ctx, "", "mkdir", "-m", "700", "-p", l.PromptsDir); err != nil {
			return nil, fmt.Errorf("mkdir prompts dir: %w: %s", err, out)
		}
		promptFile = filepath.Join(l.PromptsDir, sess.ID)
		// umask 077 so the prompt file is created 0600: task prompts can carry
		// sensitive context, and the 0700 PromptsDir is the only other guard.
		if out, err := l.run.Run(ctx, "", "sh", "-c", `umask 077; printf '%s' "$1" > "$2"`, "sh", req.Prompt, promptFile); err != nil {
			return nil, fmt.Errorf("write prompt file: %w: %s", err, out)
		}
	}

	if err := l.newAgentSession(ctx, "", sess.ID, req.Cwd); err != nil {
		return nil, err
	}
	mode := req.PermissionMode
	if mode == "" {
		mode = l.cfg.GetDefaultPermissionMode()
	}
	b := l.backendFor(sess.Backend)
	// For a backend with no system-prompt flag but an AGENTS.md rules file (Codex),
	// deliver the same pipeline/collab addendum by writing it into the workdir before
	// launch. A flag-based backend (Claude) skips this — its hints ride the launch
	// line below instead. A write failure degrades (no hints) but does not fail spawn.
	// The role persona is prepended ahead of the collab/pipeline hints so it reads
	// first; it is always injected when non-empty (general = "" = nothing).
	persona := personaGuidance(sess.Role)
	mem := l.memoryGuidance(ctx, sess.Workdir)
	if err := l.injectContext(b, sess.Workdir,
		persona,
		hintGuidance(l.cfg.GetPipelineHint(), pipelineHintGuidance),
		hintGuidance(l.cfg.GetCollabHint(), collabHintGuidance),
		mem,
	); err != nil {
		slog.Warn("spawn: context injection failed", "agent", sess.ID, "backend", b.ID(), "err", err)
	}
	hints := l.systemPromptHints(ctx, b, sess.ID,
		hintSpec{persona != "", persona},
		hintSpec{l.cfg.GetPipelineHint(), pipelineHintGuidance},
		hintSpec{l.cfg.GetCollabHint(), collabHintGuidance},
		hintSpec{l.cfg.GetMemoryInject(), mem})
	launch := b.LaunchCmd(agentbackend.LaunchOpts{
		SessionID: sess.ClaudeSessionID, Name: sess.ID, Model: l.launchModel(b, req.Model), Mode: mode,
	}) + hints + l.promptArg(b, promptFile) + l.exitSuffix(sess.ID)
	if out, err := l.run.Run(ctx, "", "tmux", "send-keys", "-t", sess.ID, launch, "Enter"); err != nil {
		// The session exists but launch failed — don't orphan it. No worktree here.
		l.cleanupFailedSpawn(sess, true, false)
		return nil, fmt.Errorf("tmux send-keys claude: %w: %s", err, out)
	}
	l.seedInteractivePrompt(b, sess.ID, req.Prompt)
	return sess, nil
}

// spawnTyped runs the per-type managed flow: optionally create a git worktree,
// start a tmux session in it, and auto-launch claude. On any post-resource
// failure it rolls back via cleanupFailedSpawn (a worktree only when WE made it).
func (l *Lifecycle) spawnTyped(ctx context.Context, req SpawnRequest, sess *store.Session) (*store.Session, error) {
	// A fork MUST run in its own worktree: discover-then-pin is dir-scoped, so a fork
	// sharing the source's tree (or the bare repo) would mis-pin both ends (§5/§7). The
	// fork worktree is also what the launch command is based off the SOURCE branch in
	// (ensureWorktree). Reject a fork that would not get its own worktree.
	if req.ForkFrom != "" && !wantWorktree(req) {
		return nil, fmt.Errorf("fork requires its own worktree; spawn a worktree-backed type without --in-repo")
	}
	workdir := req.Repo
	worktreeCreated := false
	if wantWorktree(req) {
		rel := worktreeRel(sess.ID)
		branch, created, branchCreated, err := l.ensureWorktree(ctx, req, sess.ID, rel)
		if err != nil {
			return nil, err
		}
		sess.Worktree = rel
		sess.Branch = branch
		sess.WorktreeCreated = created
		sess.BranchCreated = branchCreated
		worktreeCreated = created
		workdir = filepath.Join(req.Repo, rel)
	}
	sess.Workdir = workdir
	// Dirty-tree carry (PR-2, §7): a fork ALSO seeds the source agent's UNCOMMITTED
	// tracked changes into its fresh worktree, so it diverges from the source's EXACT
	// live state rather than only the source branch's committed HEAD. No-op when the
	// source tree is clean or carry is off (ForkSourceWorkdir empty = HEAD-only, the
	// PR-1 behavior). The carry is non-destructive on the source (git stash create);
	// run before launch so the files are present when the agent starts.
	if req.ForkFrom != "" && req.ForkSourceWorkdir != "" {
		if err := l.carryDirtyTree(ctx, req.ForkSourceWorkdir, workdir); err != nil {
			// Worktree (but not yet tmux) exists here; undo only a worktree we made.
			l.cleanupFailedSpawn(sess, false, worktreeCreated)
			return nil, err
		}
	}
	if err := l.newAgentSession(ctx, req.Repo, sess.ID, workdir); err != nil {
		// new-session failed, so no tmux session exists; only undo a worktree we made.
		l.cleanupFailedSpawn(sess, false, worktreeCreated)
		return nil, err
	}
	// Seed the task prompt as claude's positional argument, file-backed via
	// "$(cat …)" so a multi-line prompt types as a single physical line (same
	// mechanism as spawnFreeForm/SpawnJob). Empty prompt = an interactive managed
	// agent (open claude and wait). Without this the worktree+session come up but
	// the agent sits idle at an empty prompt.
	promptFile := ""
	if req.Prompt != "" {
		pf, err := l.writePromptFile(ctx, sess.ID, req.Prompt)
		if err != nil {
			l.cleanupFailedSpawn(sess, true, worktreeCreated)
			return nil, err
		}
		promptFile = pf
	}
	mode := req.PermissionMode
	if mode == "" {
		mode = l.cfg.GetDefaultPermissionMode()
	}
	b := l.backendFor(sess.Backend)
	// For a backend with no system-prompt flag but an AGENTS.md rules file (Codex),
	// deliver the same pipeline/collab/git addendum by writing it into the worktree
	// (created above) before launch. A flag-based backend (Claude) skips this — its
	// hints ride the launch line below instead. A write failure degrades (no hints)
	// but does not fail spawn.
	// The role persona is prepended ahead of the collab/pipeline/git hints so it
	// reads first; it is always injected when non-empty (general = "" = nothing).
	persona := personaGuidance(sess.Role)
	mem := l.memoryGuidance(ctx, sess.Workdir)
	if err := l.injectContext(b, sess.Workdir,
		persona,
		hintGuidance(l.cfg.GetPipelineHint(), pipelineHintGuidance),
		hintGuidance(l.cfg.GetCollabHint(), collabHintGuidance),
		hintGuidance(l.cfg.GetGitConventions(), gitConventionsGuidance),
		mem,
	); err != nil {
		slog.Warn("spawn: context injection failed", "agent", sess.ID, "backend", b.ID(), "err", err)
	}
	base, err := l.buildLaunch(b, req, sess, mode)
	if err != nil {
		l.cleanupFailedSpawn(sess, true, worktreeCreated)
		return nil, err
	}
	hints := l.systemPromptHints(ctx, b, sess.ID,
		hintSpec{persona != "", persona},
		hintSpec{l.cfg.GetPipelineHint(), pipelineHintGuidance},
		hintSpec{l.cfg.GetCollabHint(), collabHintGuidance},
		hintSpec{l.cfg.GetGitConventions(), gitConventionsGuidance},
		hintSpec{l.cfg.GetMemoryInject(), mem})
	launch := base + hints + l.guardSettings(b, sess.ID) + l.promptArg(b, promptFile) + l.exitSuffix(sess.ID)
	if out, err := l.run.Run(ctx, req.Repo, "tmux", "send-keys", "-t", sess.ID, launch, "Enter"); err != nil {
		l.cleanupFailedSpawn(sess, true, worktreeCreated)
		return nil, fmt.Errorf("tmux send-keys claude: %w: %s", err, out)
	}
	l.seedInteractivePrompt(b, sess.ID, req.Prompt)
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
		slog.Warn("spawn cleanup: kill tmux session failed", "agent", id, "err", err, "out", strings.TrimSpace(out))
	}
}

// rollbackWorktree best-effort force-removes a worktree (and its branch) that
// this spawn created when a later step fails. Only ever called for worktrees we
// created — never for an adopted, pre-existing one (see ensureWorktree). A
// failure is logged (not returned) so a leaked worktree is visible.
func (l *Lifecycle) rollbackWorktree(sess *store.Session) {
	if err := l.RemoveWorktree(context.Background(), CleanupTarget{
		ID: sess.ID, Repo: sess.Repo, Worktree: sess.Worktree,
		Branch: sess.Branch, BranchCreated: sess.BranchCreated,
		TmuxSession: sess.TmuxSession,
	}, true, false); err != nil {
		slog.Warn("spawn cleanup: rollback worktree failed", "worktree", sess.Worktree, "err", err)
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

// EnsureExtendedKeys configures tmux so the user can insert a newline (rather
// than submit) while typing into Claude. It installs two layers, both
// best-effort — a keyboard-protocol quirk must never block a spawn or cockpit
// launch:
//
//  1. Extended-keys passthrough (a server option). On terminals that speak the
//     CSI-u / modifyOtherKeys protocol, this lets Claude receive Shift+Enter as a
//     distinct key it treats as a newline, instead of the bare CR that tmux would
//     otherwise collapse it into (which Claude treats as submit). terminal-features
//     is appended only when extkeys is absent so repeated spawns don't accumulate
//     duplicate entries.
//
//  2. An Alt+Enter fallback (a root-table key binding). Many Linux terminals —
//     notably VTE/GNOME (Ptyxis, GNOME Terminal) — never report the Shift modifier
//     on Enter at all: they emit a bare CR for Shift+Enter, so layer 1 cannot
//     recover it. Alt+Enter, however, arrives distinctly (ESC+CR, which tmux reads
//     as M-Enter) even on those terminals, so we bind it to send a literal LF (C-j)
//     into the active pane — which Claude inserts as a newline. The binding is in
//     the root table so it works in the cockpit and in attached agent sessions
//     alike (same tmux server).
func EnsureExtendedKeys(ctx context.Context, run Runner) {
	// Terminal-independent newline key for terminals that can't report Shift+Enter.
	_, _ = run.Run(ctx, "", "tmux", "bind-key", "-n", "M-Enter", "send-keys", "C-j")
	_, _ = run.Run(ctx, "", "tmux", "set-option", "-s", "extended-keys", "on")
	if out, err := run.Run(ctx, "", "tmux", "show-options", "-s", "-v", "terminal-features"); err == nil && strings.Contains(out, "extkeys") {
		return // outer terminal already advertised; don't append a duplicate
	}
	_, _ = run.Run(ctx, "", "tmux", "set-option", "-sa", "terminal-features", "*:extkeys")
}

// resumeInTmux creates a detached tmux session named id in cwd and resumes the
// agent conversation claudeID inside it using backend b. Shared by Restore and
// Adopt. The ResumeCmd capability is checked BEFORE the tmux session is created
// so a backend without resume (Caps.Resume=false, e.g. Aider) fails cleanly
// instead of stranding an empty session (design §5: !Resume ⇒ start fresh).
func (l *Lifecycle) resumeInTmux(ctx context.Context, b agentbackend.Backend, id, cwd, claudeID, model, mode string) error {
	return l.resumeInTmuxWithHints(ctx, b, id, cwd, claudeID, model, mode, "")
}

// resumeInTmuxWithHints is resumeInTmux with an extra system-prompt hints fragment
// appended to the resume command. The plain Restore/Adopt paths pass "" (a resume
// re-injects nothing, matching pre-roles behavior); the role-switch path (SwitchRole)
// passes the flag-based backend's --append-system-prompt fragment so the freshly
// resolved persona is re-injected onto Claude. Injecting backends contribute an
// empty fragment here — their persona rides the AGENTS.md rules file rewritten by
// injectContext before this call — so hints is "" for them and the resume is
// byte-identical to the plain path.
func (l *Lifecycle) resumeInTmuxWithHints(ctx context.Context, b agentbackend.Backend, id, cwd, claudeID, model, mode, hints string) error {
	cmd, ok := b.ResumeCmd(agentbackend.ResumeOpts{
		SessionID: claudeID, Name: id, Model: l.launchModel(b, model), Mode: mode,
	})
	if !ok {
		return fmt.Errorf("backend %s does not support resume — start a fresh agent instead", b.ID())
	}
	if err := l.newAgentSession(ctx, "", id, cwd); err != nil {
		return err
	}
	resume := cmd + hints + l.exitSuffix(id)
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
	b := l.backendFor(sess.Backend)
	if !b.Capabilities().Resume {
		// The agent's backend can't resume a prior session by id (e.g. Aider
		// continues from repo history, not a pinned id). Restore is resume-only,
		// so refuse rather than silently start a fresh conversation.
		return fmt.Errorf("backend %s does not support restore/resume — start a fresh agent instead", b.ID())
	}
	// A pinning backend (Claude) resumes by exact id, so a missing id is fatal. A
	// non-pinning backend (codex) resumes dir-scoped (e.g. `codex resume --last`)
	// and never needs the id, so an empty ClaudeSessionID is fine — the
	// transcript-exists check below still guards that there is a session to resume.
	if b.Capabilities().SessionIDControl && sess.ClaudeSessionID == "" {
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
	return l.resumeInTmux(ctx, b, sess.ID, sess.Workdir, sess.ClaudeSessionID, sess.Model, mode)
}

// SwitchRole re-injects the persona for sess.Role (already persisted by the caller
// via store.UpdateRole) onto a resumable agent and relaunches it so the new persona
// takes effect immediately. A plain resume re-injects nothing (see resumeInTmux), so
// switching a role has to re-run the spawn-time injection: it rewrites the AGENTS.md
// rules file (injecting backends) AND re-appends Claude's --append-system-prompt
// (flag backends), threading the freshly resolved persona ahead of the config-gated
// collab/git/pipeline hints exactly as spawnTyped does. The in-flight turn is
// discarded (the tmux session is killed and resumed) — switching a role is an
// explicit operator action, like force-compact. Resume-only: a backend that cannot
// resume (Aider) or an agent with no pinned session / missing workdir is refused so
// the role stays persisted (it applies on the next fresh launch) rather than
// stranding the agent.
func (l *Lifecycle) SwitchRole(ctx context.Context, sess *store.Session) error {
	b := l.backendFor(sess.Backend)
	if !b.Capabilities().Resume {
		return fmt.Errorf("backend %s does not support resume — cannot re-inject a role on a running agent (the role is saved and applies on the next fresh launch)", b.ID())
	}
	if b.Capabilities().SessionIDControl && sess.ClaudeSessionID == "" {
		return ErrNoSessionID
	}
	if fi, err := os.Stat(sess.Workdir); err != nil || !fi.IsDir() {
		return ErrWorkdirMissing
	}
	if l.transcriptPath(sess) == "" {
		return ErrNoTranscript
	}
	// Kill the live tmux session (if any) so the relaunch below re-creates it. Unlike
	// Restore we do NOT refuse a running agent — switching a role deliberately
	// relaunches it.
	if _, err := l.run.Run(ctx, "", "tmux", "has-session", "-t", sess.TmuxSession); err == nil {
		l.killSession(sess.TmuxSession)
	}
	// Re-run the spawn-time injection with the freshly resolved persona prepended
	// ahead of the config-gated hints (mirrors spawnTyped's injectContext call).
	persona := personaGuidance(sess.Role)
	mem := l.memoryGuidance(ctx, sess.Workdir)
	if err := l.injectContext(b, sess.Workdir,
		persona,
		hintGuidance(l.cfg.GetPipelineHint(), pipelineHintGuidance),
		hintGuidance(l.cfg.GetCollabHint(), collabHintGuidance),
		hintGuidance(l.cfg.GetGitConventions(), gitConventionsGuidance),
		mem,
	); err != nil {
		slog.Warn("switch-role: context injection failed", "agent", sess.ID, "backend", b.ID(), "err", err)
	}
	mode := sess.PermissionMode
	if mode == "" {
		mode = l.cfg.GetDefaultPermissionMode()
	}
	hints := l.systemPromptHints(ctx, b, sess.ID,
		hintSpec{persona != "", persona},
		hintSpec{l.cfg.GetPipelineHint(), pipelineHintGuidance},
		hintSpec{l.cfg.GetCollabHint(), collabHintGuidance},
		hintSpec{l.cfg.GetGitConventions(), gitConventionsGuidance},
		hintSpec{l.cfg.GetMemoryInject(), mem})
	return l.resumeInTmuxWithHints(ctx, b, sess.ID, sess.Workdir, sess.ClaudeSessionID, sess.Model, mode, hints)
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
		// Adopt registers a Claude session warden did not spawn, so resume always
		// goes through the default (Claude) backend.
		if err := l.resumeInTmux(ctx, l.backend, id, req.Cwd, req.ClaudeSessionID, req.Model, l.cfg.GetDefaultPermissionMode()); err != nil {
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
	ErrDirtyWorktree       = errors.New("worktree has uncommitted changes (use --force)")
	ErrUnpushedCommits     = errors.New("worktree has unpushed commits (use --force)")
	ErrAlreadyRunning      = errors.New("agent is already running (use send/attach)")
	ErrNoSessionID         = errors.New("no pinned claude session id; re-spawn instead")
	ErrWorkdirMissing      = errors.New("agent workdir is gone; re-spawn instead")
	ErrNoTranscript        = errors.New("no transcript to resume")
	ErrForkSourceNotPinned = errors.New("fork source agent's session id is not yet known; let it run one turn, then retry")
	ErrNoWorktree          = errors.New("session has no worktree")
	ErrWorktreeAgentAlive  = errors.New("agent is still running; terminate it before removing its worktree")
	ErrTmuxGone            = errors.New("tmux session not found")
)

// CleanupTarget carries the fields Cleanup needs (filled from the store doc).
type CleanupTarget struct {
	ID            string
	Repo          string
	Worktree      string // relative, e.g. .worktrees/A-1
	Branch        string
	BranchCreated bool // warden/gh created Branch; gates branch -D (vs. an adopted branch)
	TmuxSession   string
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
//
// The branch is git branch -D'd only when warden created it (t.BranchCreated) —
// an adopted branch (a branch a human made and warden merely checked out) is
// left untouched even under force, unless deleteAdoptedBranch explicitly opts in.
// After a successful remove it best-effort runs git worktree prune to clear any
// stale admin metadata; a prune failure is logged, not propagated.
func (l *Lifecycle) RemoveWorktree(ctx context.Context, t CleanupTarget, force, deleteAdoptedBranch bool) error {
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
	// Delete the branch only when warden owns it (created it) — never an adopted,
	// human-made branch, unless the caller explicitly opts in. force governs the
	// dirty/unpushed guard + worktree removal, NOT branch provenance.
	if t.Branch != "" && (t.BranchCreated || deleteAdoptedBranch) {
		if out, err := l.run.Run(ctx, "", "git", "-C", t.Repo, "branch", "-D", t.Branch); err != nil {
			return fmt.Errorf("git branch -D: %w: %s", err, out)
		}
	}
	// Clear stale .git/worktrees admin metadata for anything removed here or
	// out-of-band. Best-effort: a prune failure must not fail the removal.
	if out, err := l.run.Run(ctx, "", "git", "-C", t.Repo, "worktree", "prune"); err != nil {
		slog.Warn("remove-worktree: git worktree prune failed", "agent", t.ID, "err", err, "out", strings.TrimSpace(out))
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

// Post-launch prompt-seeding tunables (overridable in tests). A backend that takes
// its first task only as typed input (agentbackend.PromptSeeder — Crush/Goose/Aider)
// has its prompt typed into the pane after its UI is ready, rather than on the
// launch line.
var (
	promptSeedTimeout      = 45 * time.Second       // overall budget to get ready + type
	promptSeedPollInterval = 400 * time.Millisecond // pane re-capture cadence while waiting
	promptSeedSettle       = 700 * time.Millisecond // extra wait after the marker appears
	promptSeedFallbackWait = 6 * time.Second        // used when the backend has no ReadyMarker
)

// seedInteractivePrompt types the task prompt into a just-launched interactive
// agent whose UI accepts the prompt only as typed input (PromptSeeder). It is a
// no-op for backends that seed on the launch line. It runs asynchronously: the
// launch keystroke must land and the agent's UI must finish booting before the
// prompt can be typed, so the goroutine waits for the backend's ReadyMarker in the
// captured pane (or a fallback settle) and then bracketed-pastes the prompt + Enter
// via Input — the same path an operator's message takes. Failures degrade to "no
// seed" (the agent simply waits at an empty prompt) rather than erroring the spawn.
func (l *Lifecycle) seedInteractivePrompt(b agentbackend.Backend, tmuxSession, prompt string) {
	ps, ok := b.(agentbackend.PromptSeeder)
	if !ok {
		return
	}
	text, ok := ps.PromptText(prompt)
	if !ok || strings.TrimSpace(text) == "" {
		return
	}
	marker := ps.ReadyMarker()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), promptSeedTimeout)
		defer cancel()
		if !l.waitPaneReady(ctx, tmuxSession, marker) {
			slog.Warn("prompt seed: agent UI not ready, skipping initial prompt", "agent", tmuxSession, "backend", b.ID())
			return
		}
		if err := l.Input(ctx, tmuxSession, text); err != nil {
			slog.Warn("prompt seed: failed to type initial prompt", "agent", tmuxSession, "backend", b.ID(), "err", err)
		}
	}()
}

// waitPaneReady blocks until marker appears in the agent's captured pane (then a
// short settle), up to ctx's deadline. With an empty marker it instead waits a
// fixed fallback delay. Returns false if the deadline passes before the marker is
// seen (caller skips seeding rather than typing into an unready UI / the shell).
func (l *Lifecycle) waitPaneReady(ctx context.Context, tmuxSession, marker string) bool {
	if marker == "" {
		select {
		case <-time.After(promptSeedFallbackWait):
			return true
		case <-ctx.Done():
			return false
		}
	}
	tick := time.NewTicker(promptSeedPollInterval)
	defer tick.Stop()
	for {
		out, err := l.run.Run(ctx, "", "tmux", "capture-pane", "-p", "-t", tmuxSession)
		if err == nil && strings.Contains(out, marker) {
			select {
			case <-time.After(promptSeedSettle):
				return true
			case <-ctx.Done():
				return false
			}
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			return false
		}
	}
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
	PermissionMode string   // explicit mode override; empty = use global default
	Model          string   // claude model (opus/sonnet/haiku or full ID); empty = default
	Tags           []string // labels stamped on the job's session (e.g. inherited autopilot ownership tags)
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
		slog.Warn("exit-capture: mkdir failed", "dir", l.ExitsDir, "err", err)
		return ""
	}
	path := filepath.Join(l.ExitsDir, id)
	_ = os.Remove(path) // clear a prior run's file so the poller can't consume it
	return " ; printf '%s' \"$?\" > " + shellQuoteArg(path)
}

// guardSettingsFlag writes a per-agent `claude --settings` file that installs the
// PreToolUse isolation-guard hook and returns the ` --settings <path>` launch
// fragment, or "" when the guard is disabled (config) or unconfigured
// (SettingsDir/WardenBin unset, e.g. tests). The file is scoped to this agent so
// the blocking hook never touches the user's own (non-warden) Claude sessions;
// --settings is additive, so the user's global status hooks still fire. Writing
// is best-effort — a failure logs and returns "" so the spawn still proceeds
// (the guard is a backstop, not a hard dependency).
func (l *Lifecycle) guardSettingsFlag(id string) string {
	if l.SettingsDir == "" || l.WardenBin == "" {
		return ""
	}
	doc := guardSettingsJSON(l.WardenBin, l.cfg.GetIsolationGuard(), l.cfg.GetGitRedirect(), l.cfg.GetCheckRedirect(), l.cfg.GetRootGuard())
	if doc == "" {
		return "" // every PreToolUse hook disabled — write nothing
	}
	if err := os.MkdirAll(l.SettingsDir, 0o700); err != nil {
		slog.Warn("agent-guard: mkdir settings dir failed", "dir", l.SettingsDir, "err", err)
		return ""
	}
	path := filepath.Join(l.SettingsDir, id+".json")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		slog.Warn("agent-guard: write settings failed", "path", path, "err", err)
		return ""
	}
	return " --settings " + shellQuoteArg(path)
}

// guardSettingsJSON builds the Claude Code settings document that registers
// warden's PreToolUse hooks for a spawned agent. Each enabled hook runs the
// warden binary itself (`<warden> hook ...`), which reads the tool call on stdin
// and emits an allow/deny verdict. Two hooks are wired independently:
//   - isolation: blocks file edits that escape the agent's worktree (asks the
//     daemon, since the verdict depends on session state);
//   - gitRedirect: denies raw `git commit/push/pull/rebase` in Bash and points
//     the agent at the warden tools (a pure, static redirect — no daemon round-trip).
//   - checkRedirect: denies a raw test/lint/build command the project's
//     .warden/check.yml registers and points the agent at `wd check` (reads the
//     per-project config itself — no daemon round-trip; no config means nothing
//     is redirected).
//   - rootGuard: denies any file edit that targets the main repo working tree,
//     for every spawned agent regardless of worktree ownership (a pure, local
//     git check — no daemon round-trip). This is the backstop that catches the
//     no-worktree (free-form / --in-repo) agents the isolation guard exempts.
//
// It returns "" when every hook is disabled, so the caller writes no file.
// wardenBin is shell-quoted because Claude runs the command string through a shell.
func guardSettingsJSON(wardenBin string, isolation, gitRedirect, checkRedirect, rootGuard bool) string {
	bin := shellQuoteArg(wardenBin)
	var pre []any
	if isolation {
		pre = append(pre, hookMatcher("Edit|Write|MultiEdit|NotebookEdit", bin+" hook guard"))
	}
	if rootGuard {
		pre = append(pre, hookMatcher("Edit|Write|MultiEdit|NotebookEdit", bin+" hook root-guard"))
	}
	if gitRedirect {
		pre = append(pre, hookMatcher("Bash", bin+" hook git-guard"))
	}
	if checkRedirect {
		pre = append(pre, hookMatcher("Bash", bin+" hook check-guard"))
	}
	if len(pre) == 0 {
		return ""
	}
	settings := map[string]any{"hooks": map[string]any{"PreToolUse": pre}}
	b, err := json.Marshal(settings)
	if err != nil { // map of strings can't fail to marshal; defensive only
		return ""
	}
	return string(b)
}

// hookMatcher builds one PreToolUse entry: a tool-name matcher plus a single
// command hook that runs cmd.
func hookMatcher(matcher, cmd string) any {
	return map[string]any{
		"matcher": matcher,
		"hooks": []any{
			map[string]any{"type": "command", "command": cmd},
		},
	}
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
	// umask 077 so the prompt file is created 0600 (see spawnFreeForm).
	if out, err := l.run.Run(ctx, "", "sh", "-c", `umask 077; printf '%s' "$1" > "$2"`, "sh", prompt, path); err != nil {
		return "", fmt.Errorf("write prompt file: %w: %s", err, out)
	}
	return path, nil
}

// writeHintsFile persists the assembled system-prompt addendum to <HintsDir>/<id>
// and returns the path, so the launch line can reference it via
// --append-system-prompt "$(cat <file>)" rather than inlining ~1.6 KB of text (see
// HintsDir / systemPromptHints). It mirrors writePromptFile: the file is created
// 0600 (umask 077) and the write goes through the runner so tests fake it.
func (l *Lifecycle) writeHintsFile(ctx context.Context, id, text string) (string, error) {
	if l.HintsDir == "" {
		return "", fmt.Errorf("hints dir not configured")
	}
	if out, err := l.run.Run(ctx, "", "mkdir", "-m", "700", "-p", l.HintsDir); err != nil {
		return "", fmt.Errorf("mkdir hints dir: %w: %s", err, out)
	}
	path := filepath.Join(l.HintsDir, id)
	if out, err := l.run.Run(ctx, "", "sh", "-c", `umask 077; printf '%s' "$1" > "$2"`, "sh", text, path); err != nil {
		return "", fmt.Errorf("write hints file: %w: %s", err, out)
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
		Model: req.Model, Tags: store.NormalizeTags(req.Tags),
	}
	cid, err := store.NewSessionID()
	if err != nil {
		return nil, err
	}
	sess.ClaudeSessionID = cid

	workdir := req.Repo
	worktreeCreated := false
	if req.Worktree {
		if err := safeGitRef(req.BaseBranch); err != nil {
			return nil, err
		}
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
		sess.WorktreeCreated = true
		sess.BranchCreated = true
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
	b := l.backendFor(sess.Backend)
	// For a backend with no system-prompt flag but an AGENTS.md rules file (Codex),
	// deliver the same collab addendum by writing it into the workdir before launch.
	// A flag-based backend (Claude) skips this — its hint rides the launch line below.
	// A write failure degrades (no hints) but does not fail spawn.
	mem := l.memoryGuidance(ctx, sess.Workdir)
	if err := l.injectContext(b, sess.Workdir,
		hintGuidance(l.cfg.GetCollabHint(), collabHintGuidance),
		mem,
	); err != nil {
		slog.Warn("spawn job: context injection failed", "agent", id, "backend", b.ID(), "err", err)
	}
	hints := l.systemPromptHints(ctx, b, id,
		hintSpec{l.cfg.GetCollabHint(), collabHintGuidance},
		hintSpec{l.cfg.GetMemoryInject(), mem})
	launch := b.LaunchCmd(agentbackend.LaunchOpts{
		SessionID: sess.ClaudeSessionID, Name: id, Model: l.launchModel(b, req.Model), Mode: mode,
	}) + hints + l.promptArg(b, promptFile) + l.exitSuffix(id)
	if out, err := l.run.Run(ctx, req.Repo, "tmux", "send-keys", "-t", id, launch, "Enter"); err != nil {
		l.cleanupFailedSpawn(sess, true, worktreeCreated)
		return nil, fmt.Errorf("tmux send-keys claude: %w: %s", err, out)
	}
	l.seedInteractivePrompt(b, id, req.Prompt)
	return sess, nil
}

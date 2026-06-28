package backends

import (
	"bufio"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/srjn45/warden/internal/agentbackend"
)

func init() { agentbackend.Register(Codex{}) }

// Codex is the **experimental** Backend adapter for OpenAI's Codex CLI (the
// `codex` binary). It is breadth-first work (#52): a thin, correct adapter that
// launches Codex and sources its transcript, with the gaps documented rather than
// papered over (docs/agent-backends/codex.md).
//
// Tier decision: Codex is shipped as **Tier A** for transcripts. Codex persists
// every session as a "rollout" JSONL file under `$CODEX_HOME/sessions/<Y>/<M>/<D>/`
// (one record per line: a `session_meta` header, `response_item` conversation
// items, `event_msg` UI events, `turn_context`). This adapter parses the canonical
// `response_item` `message` / `function_call` records into neutral Turns with good
// fidelity, so digests run on real structured data. Captured live against the
// $0-local rig (`codex exec --oss --local-provider ollama -m qwen2.5-coder:3b`).
//
// Session-id handling: Codex mints its own UUID session id at launch and exposes
// no flag to assign one up front (Caps.SessionIDControl=false). warden therefore
// cannot pin the id, so — like the Aider and OpenCode adapters — this adapter is
// **dir-scoped**: every warden agent runs in its own git worktree, and both the
// transcript locator (filter rollouts by `session_meta.cwd`) and resume
// (`codex resume --last`, which Codex scopes to the cwd) key off that working
// directory. Exact-id resume (`codex resume <uuid>`) becomes available once the
// discover-then-pin write-back lands (FUTURE_ENHANCEMENTS #52).
//
// Provider is intentionally not forced here: warden passes only `-m <model>` (when
// set) and lets Codex's `~/.codex/config.toml` select the provider, so the same
// adapter serves the $0-local Ollama rig (config default `model_provider=oss`/
// ollama) and a paid OpenAI-auth setup without warden picking sides — the
// "BYO config" stance shared with OpenCode.
type Codex struct{}

// --- Identity ---------------------------------------------------------------

func (Codex) ID() string          { return "codex" }
func (Codex) DisplayName() string { return "Codex" }
func (Codex) Binary() string      { return "codex" }
func (Codex) InstallHint() string {
	return "Install Codex: npm install -g @openai/codex\nOr visit: https://github.com/openai/codex"
}

// --- Launch / resume --------------------------------------------------------

// codexSandbox maps a warden permission mode onto Codex's sandbox vocabulary
// (`-s read-only|workspace-write|danger-full-access`) and reports whether the
// approval policy should also be pinned to `never` (so an unattended tmux agent
// never blocks on a prompt). Codex's three native sandbox modes pass through
// directly; warden's richer Claude-flavored "just do it" modes fold onto
// danger-full-access + never; "default"/"" returns "" so Codex applies its own
// default posture (workspace-write + on-request), preserving the interactive UX
// (warden adds on top, never strips it down).
func codexSandbox(mode string) (sandbox string, neverApprove bool) {
	switch mode {
	case "read-only":
		return "read-only", false
	case "workspace-write", "acceptEdits":
		return "workspace-write", false
	case "danger-full-access", "dangerously-skip-permissions", "bypassPermissions", "yes-always", "auto", "dontAsk":
		return "danger-full-access", true
	default:
		return "", false
	}
}

// LaunchCmd builds the interactive `codex` (TUI) invocation for a tmux pane. Model
// is shaped as Codex's `-m` and omitted when empty so the config-default provider
// applies (BYO config; the Claude default alias never resolves here, same call as
// Aider/OpenCode). The permission mode maps to `-s <sandbox>` (+ `-a never` for the
// auto-approve modes). SessionID and Name are ignored: Codex mints its own UUID
// session id (SessionIDControl=false) and the TUI has no session-name flag. The
// pane is already cd'd into the agent's workdir, so no `-C/--cd` is appended.
func (Codex) LaunchCmd(o agentbackend.LaunchOpts) string {
	cmd := "codex"
	if o.Model != "" {
		cmd += " -m " + shellQuoteArg(o.Model)
	}
	if sb, never := codexSandbox(o.Mode); sb != "" {
		cmd += " -s " + sb
		if never {
			cmd += " -a never"
		}
	}
	return cmd
}

// ResumeCmd builds the interactive resume invocation, run in the agent's workdir.
// warden cannot pin Codex's UUID session id (SessionIDControl=false) and the id in
// ResumeOpts is warden's own placeholder, not Codex's, so this uses
// `codex resume --last` — "continue the most recent session", which Codex scopes to
// the cwd by default (its `--all` flag exists precisely to disable that cwd
// filtering). For a per-worktree warden agent that deterministically continues that
// agent's own session. ok is always true (Caps.Resume=true). Exact-id resume
// (`codex resume <uuid>`) lands with discover-then-pin (FUTURE_ENHANCEMENTS #52).
func (Codex) ResumeCmd(agentbackend.ResumeOpts) (string, bool) {
	return "codex resume --last", true
}

// ReviewCmd implements agentbackend.Reviewer: it returns the argv for a one-shot
// `codex review` run in the agent's worktree (the CLI verb `wd review` execs it
// locally and streams the findings to the operator — the agent-native counterpart
// to `wd check`). Codex's review subcommand is diff-scoped natively:
//   - Scope "uncommitted" (the default) → `--uncommitted` (staged+unstaged+untracked),
//   - Scope "base" → `--base <Base>` (changes against a base branch).
//
// Model/provider are deliberately NOT forced here, exactly like LaunchCmd: warden
// emits only the review verb and lets Codex's config (`~/.codex/config.toml`, or the
// operator's `-c` overrides) pick the provider, so the same argv serves the
// $0-local Ollama rig (`-c model_provider=ollama`) and a paid OpenAI setup ("BYO
// config"). An optional Prompt rides as Codex's trailing positional review
// instruction. This returns argv (not a tmux-pane string), so the verb execs it
// directly with no shell — no shell-quoting needed and untrusted values never reach
// a shell.
//
// SchemaFile selects the command form (verified against codex v0.142.3): the plain
// `codex review` subcommand has NO `--output-schema`/`--json` flags — only the
// `codex exec review` sub-form does — so a non-empty SchemaFile switches to
// `codex exec review … --output-schema <file>`. PR-A's caller always passes "" (the
// prose form); the branch is wired now so PR-B's structured-result path inherits the
// correct command shape. ok is always true (Codex always offers native review).
func (Codex) ReviewCmd(opts agentbackend.ReviewOpts) ([]string, bool) {
	var argv []string
	if opts.SchemaFile != "" {
		argv = []string{"codex", "exec", "review"}
	} else {
		argv = []string{"codex", "review"}
	}
	if opts.Scope == "base" && opts.Base != "" {
		argv = append(argv, "--base", opts.Base)
	} else {
		argv = append(argv, "--uncommitted")
	}
	if opts.SchemaFile != "" {
		argv = append(argv, "--output-schema", opts.SchemaFile)
	}
	if opts.Prompt != "" {
		argv = append(argv, opts.Prompt)
	}
	return argv, true
}

// LaunchPromptArg seeds the initial task prompt as Codex's trailing positional
// argument (read back from promptFile via "$(cat …)" so a multi-line prompt types
// as one physical line). Codex's TUI accepts an optional PROMPT positional and then
// stays interactive — a persistent agent loop, like Claude's trailing positional
// prompt rather than Aider's run-once-and-exit --message.
func (Codex) LaunchPromptArg(promptFile string) string {
	return ` "$(cat ` + shellQuoteArg(promptFile) + `)"`
}

// HeadlessCmd returns the argv for a headless one-shot used by warden's own
// classify/summarize offload when Codex is the default backend. It runs a single
// `codex exec` with sandbox+approvals bypassed (the run is already externally
// sandboxed) and --skip-git-repo-check so it works outside a repo and never blocks.
// (warden's default backend is Claude, so this path is rarely exercised for Codex;
// it exists to honor Caps.Headless=true.)
func (Codex) HeadlessCmd(prompt string) ([]string, bool) {
	return []string{"codex", "exec", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox", prompt}, true
}

// --- Transcript -------------------------------------------------------------

// codexHome resolves Codex's home directory (where it persists session rollouts):
// $CODEX_HOME if set, else ~/.codex. It is a package var so tests can point it at a
// fixture tree. Returns "" when no home can be resolved (lookup disabled).
var codexHome = func() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".codex")
	}
	return ""
}

// TranscriptPath resolves the agent's Codex rollout file. Codex stores sessions as
// `$CODEX_HOME/sessions/<Y>/<M>/<D>/rollout-<ts>-<uuid>.jsonl`, keyed by a
// Codex-minted id warden cannot pin, so this resolves **dir-scoped**: it walks the
// sessions tree newest-first and returns the most recent rollout whose
// `session_meta.cwd` equals workdir (every warden agent runs in its own worktree).
// projectsDir (Claude-specific) and sessionID (warden's placeholder) are ignored.
// ok=false on any miss (no home, no sessions, no rollout for the dir), so the
// digest path degrades to "no transcript" rather than erroring — same contract as
// Aider/OpenCode.
func (Codex) TranscriptPath(_, workdir, _ string) (string, bool) {
	if workdir == "" {
		return "", false
	}
	home := codexHome()
	if home == "" {
		return "", false
	}
	sessionsDir := filepath.Join(home, "sessions")

	type rollout struct {
		path string
		mod  int64
	}
	var cands []rollout
	_ = filepath.WalkDir(sessionsDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
			return nil
		}
		if info, err := d.Info(); err == nil {
			cands = append(cands, rollout{p, info.ModTime().UnixNano()})
		}
		return nil
	})
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].mod > cands[j].mod })

	for _, c := range cands {
		if codexRolloutCwd(c.path) == workdir {
			return c.path, true
		}
	}
	return "", false
}

// codexRolloutCwd reads a rollout's `session_meta` header (its first line) and
// returns the recorded cwd, or "" if it can't be read — the key the dir-scoped
// locator filters on.
func codexRolloutCwd(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	if !sc.Scan() {
		return ""
	}
	var rec codexLine
	if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.Type != "session_meta" {
		return ""
	}
	var meta struct {
		Cwd string `json:"cwd"`
	}
	if json.Unmarshal(rec.Payload, &meta) != nil {
		return ""
	}
	return meta.Cwd
}

// DiscoverSessionID implements agentbackend.SessionIDDiscoverer: Codex mints its
// own UUID session id at launch (Caps.SessionIDControl=false), which warden cannot
// pin up front, so the poller calls this post-launch to discover and pin it. It
// reuses the dir-scoped locator (newest rollout whose `session_meta.cwd` equals
// workdir — every warden agent runs in its own worktree), then reads that
// rollout's `session_meta.session_id`. projectsDir (Claude-specific) is ignored,
// same as TranscriptPath. ok=false on any miss (no rollout for the dir yet, or no
// id in the header) so the poller keeps the empty id and retries on a later tick.
// Once pinned, exact-id resume (`codex resume <uuid>`) and an exact transcript
// path become possible even with more than one session per workdir.
func (Codex) DiscoverSessionID(projectsDir, workdir string) (string, bool) {
	path, ok := Codex{}.TranscriptPath(projectsDir, workdir, "")
	if !ok {
		return "", false
	}
	id := codexRolloutSessionID(path)
	return id, id != ""
}

// codexRolloutSessionID reads a rollout's `session_meta` header (its first line)
// and returns the recorded Codex session id, or "" if it can't be read. Codex
// writes the id under `session_id` (with a duplicate `id` field as a fallback for
// older rollouts).
func codexRolloutSessionID(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	if !sc.Scan() {
		return ""
	}
	var rec codexLine
	if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.Type != "session_meta" {
		return ""
	}
	var meta struct {
		SessionID string `json:"session_id"`
		ID        string `json:"id"`
	}
	if json.Unmarshal(rec.Payload, &meta) != nil {
		return ""
	}
	if meta.SessionID != "" {
		return meta.SessionID
	}
	return meta.ID
}

// codexLine is one rollout JSONL record: a top-level type and an opaque payload
// whose shape depends on that type (session_meta | response_item | event_msg |
// turn_context).
type codexLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// codexMessage is a `response_item` of payload-type "message": a role plus an
// ordered list of content parts (user parts are "input_text", assistant parts are
// "output_text"; both carry .text).
type codexMessage struct {
	Role    string `json:"role"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

// codexFunctionCall is a `response_item` of payload-type "function_call": a tool
// invocation carrying the tool name and its arguments as a JSON-encoded string.
type codexFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ParseTranscript normalizes a Codex rollout JSONL stream into warden's neutral
// []Turn. It reads the canonical `response_item` records:
//   - message/user — a user prompt → a user Turn (synthetic wrapper messages Codex
//     injects, like <environment_context>, are skipped).
//   - message/assistant — the model's reply → an assistant Turn.
//   - message/developer — system/permissions/skills instructions → skipped.
//   - function_call — a tool invocation: its name (and any files it touches, parsed
//     from an apply_patch envelope or a shell command) fold onto the current
//     assistant Turn, or start a new one when the model went straight to a tool.
//
// `event_msg` / `turn_context` / `session_meta` records are control/UI metadata and
// ignored. Malformed lines are skipped (best-effort, like the Claude/Aider/OpenCode
// parsers); only a reader error is returned.
func (Codex) ParseTranscript(r io.Reader) ([]agentbackend.Turn, error) {
	var turns []agentbackend.Turn
	sc := bufio.NewScanner(r)
	// Rollout lines (esp. base_instructions / tool output) can be very long; raise
	// the scanner cap well above the 64K default (same bound as the Claude parser).
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec codexLine
		if json.Unmarshal(line, &rec) != nil || rec.Type != "response_item" {
			continue
		}
		var head struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(rec.Payload, &head) != nil {
			continue
		}
		ts := parseTime(rec.Timestamp)

		switch head.Type {
		case "message":
			var m codexMessage
			if json.Unmarshal(rec.Payload, &m) != nil {
				continue
			}
			text := codexMessageText(m)
			switch m.Role {
			case "user":
				if t := strings.TrimSpace(text); t != "" && !codexSyntheticUserMsg(t) {
					turns = append(turns, agentbackend.Turn{Role: "user", Text: text, Timestamp: ts})
				}
			case "assistant":
				if strings.TrimSpace(text) != "" {
					turns = append(turns, agentbackend.Turn{Role: "assistant", Text: text, Timestamp: ts})
				}
			}
		case "function_call":
			var fc codexFunctionCall
			if json.Unmarshal(rec.Payload, &fc) != nil {
				continue
			}
			files := codexFilesFromCall(fc.Arguments)
			if n := len(turns); n > 0 && turns[n-1].Role == "assistant" && turns[n-1].ToolName == "" {
				turns[n-1].ToolName = fc.Name
				for _, f := range files {
					turns[n-1].Files = appendUnique(turns[n-1].Files, f)
				}
			} else {
				turns = append(turns, agentbackend.Turn{Role: "assistant", ToolName: fc.Name, Files: files, Timestamp: ts})
			}
		}
	}
	return turns, sc.Err()
}

// codexMessageText joins the text of every content part in a message.
func codexMessageText(m codexMessage) string {
	var b strings.Builder
	for _, c := range m.Content {
		b.WriteString(c.Text)
	}
	return b.String()
}

// codexSyntheticUserMsg reports whether a user message is one of the synthetic
// context blocks Codex injects into the conversation (not an actual human prompt),
// so the parser drops it from the neutral turns.
func codexSyntheticUserMsg(text string) bool {
	for _, p := range []string{"<environment_context>", "<user_instructions>", "<permissions instructions>", "<skills_instructions>"} {
		if strings.HasPrefix(text, p) {
			return true
		}
	}
	return false
}

// codexPatchFileRe captures each touched file from an apply_patch envelope's
// `*** Add|Update|Delete File: <path>` headers.
var codexPatchFileRe = regexp.MustCompile(`(?m)^\*\*\* (?:Add|Update|Delete) File: (.+)$`)

// codexFilesFromCall best-effort extracts the files a tool call touches. The
// arguments are a JSON-encoded string; apply_patch carries the patch under "input"
// (or "patch"), shell under a "command" array, and some weak models pass the patch
// as the raw arguments — each is scanned for apply_patch file headers.
func codexFilesFromCall(arguments string) []string {
	var src string
	var parsed struct {
		Input   string   `json:"input"`
		Patch   string   `json:"patch"`
		Command []string `json:"command"`
	}
	if json.Unmarshal([]byte(arguments), &parsed) == nil {
		switch {
		case parsed.Input != "":
			src = parsed.Input
		case parsed.Patch != "":
			src = parsed.Patch
		case len(parsed.Command) > 0:
			src = strings.Join(parsed.Command, "\n")
		}
	}
	if src == "" {
		src = arguments
	}
	var files []string
	for _, m := range codexPatchFileRe.FindAllStringSubmatch(src, -1) {
		files = appendUnique(files, strings.TrimSpace(m[1]))
	}
	return files
}

// --- State / approval -------------------------------------------------------

// DetectState maps a captured Codex TUI pane to a neutral run state, mirroring the
// structure of the Claude adapter. Codex's pane carries two stable positive
// markers, captured live against codex v0.142.3:
//   - streaming a turn ⇒ a "• Working (… • esc to interrupt)" status line, so the
//     "esc to interrupt" substring (shared with Claude) means StateWorking.
//   - blocked on a command/patch approval ⇒ its numbered "Would you like to …?"
//     prompt, detected via ParseApproval, means StateNeedsInput.
//
// Codex surfaces NO positive idle marker (the composer placeholder rotates and the
// "<model> default · <dir>" footer is present in every state), so an at-rest pane
// returns StateUnknown and warden infers idle from staleness — the same
// conservative stance the Claude/Aider/OpenCode adapters take. Returning Unknown
// (never a false StateNeedsInput/Working) keeps the auto-approve path honest.
func (Codex) DetectState(pane string) agentbackend.State {
	if strings.Contains(pane, "esc to interrupt") {
		return agentbackend.StateWorking
	}
	if _, ok := (Codex{}).ParseApproval(pane); ok {
		return agentbackend.StateNeedsInput
	}
	return agentbackend.StateUnknown
}

// codexOptionRe matches one line of Codex's numbered approval menu, tolerating the
// leading "›" selection cursor (U+203A) on the highlighted option and the plain
// indent on the rest: "› 1. Yes, proceed (y)" / "  2. Yes, and don't ask again …".
var codexOptionRe = regexp.MustCompile(`^\s*(›?)\s*(\d+)\.\s+(.+?)\s*$`)

// codexCommandRe captures the proposed command Codex echoes as "$ <command>" just
// above the option run (the Action).
var codexCommandRe = regexp.MustCompile(`^\s*\$\s+(.+?)\s*$`)

// ParseApproval normalizes Codex's interactive permission prompt into the neutral
// Approval. Captured live (codex v0.142.3) — a command-escalation prompt renders as:
//
//	  Would you like to run the following command?
//	  …Reason: …
//	  $ curl -sI https://example.com
//	› 1. Yes, proceed (y)
//	  2. Yes, and don't ask again for commands that start with `curl -sI` (p)
//	  3. No, and tell Codex what to do differently (esc)
//	  Press enter to confirm or esc to cancel
//
// It locates the contiguous 1..N option run at the bottom of the pane, then reads
// the "$ <command>" Action and the "Would you like to …?" Question just above it.
// The "Would you like to" header is required (the patch variant shares it), so a
// bare numbered list in agent prose is NOT mis-parsed — it returns (nil,false), as
// does any non-approval pane. Options are 1-indexed top-down so Fingerprint(Options)
// — which the auto-approve policy and the daemon re-verify guard both key off — is
// stable and faithful to the pane.
func (Codex) ParseApproval(pane string) (*agentbackend.Approval, bool) {
	lines := strings.Split(pane, "\n")

	// The live prompt sits at the bottom: find the last option line, then walk up
	// while lines stay options.
	end := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if codexOptionRe.MatchString(lines[i]) {
			end = i
			break
		}
	}
	if end < 0 {
		return nil, false
	}
	start := end
	for start-1 >= 0 && codexOptionRe.MatchString(lines[start-1]) {
		start--
	}

	var opts []string
	sel := 0
	for i := start; i <= end; i++ {
		m := codexOptionRe.FindStringSubmatch(lines[i])
		// Numbering must be sequential 1..N (rejects an incidental numbered list).
		if m[2] != strconv.Itoa(i-start+1) {
			return nil, false
		}
		if m[1] == "›" {
			sel = i - start + 1
		}
		opts = append(opts, strings.TrimSpace(m[3]))
	}
	if len(opts) < 2 {
		return nil, false
	}

	a := &agentbackend.Approval{Options: opts, SelectedIdx: sel}

	// Scan upward from the option run for the "$ <command>" Action and the
	// "Would you like to …?" Question (a bounded window above the options).
	for i := start - 1; i >= 0 && i >= start-12; i-- {
		t := strings.TrimSpace(lines[i])
		if t == "" {
			continue
		}
		if a.Action == "" {
			if m := codexCommandRe.FindStringSubmatch(lines[i]); m != nil {
				a.Action = strings.TrimSpace(m[1])
				continue
			}
		}
		if strings.HasPrefix(t, "Would you like to") {
			a.Question = t
			break
		}
	}
	// The header gates recognition: without it this is not a Codex approval.
	if a.Question == "" {
		return nil, false
	}
	a.AffirmativeIdx, a.AffirmativeSticky = codexAffirmative(opts)
	return a, true
}

// codexAffirmative picks the least-privilege affirmative option from a Codex
// approval menu, mirroring the approval package's policy for the neutral
// Approval.AffirmativeIdx/Sticky fields. Codex's affirmatives start with "Yes"
// ("Yes, proceed" / "Yes, and don't ask again …"); the standing grant carries a
// "don't ask again"/"always" clause. It returns the 1-based index of the first
// non-sticky "Yes" (sticky=false); failing that the first sticky "Yes"
// (sticky=true); otherwise (0,false) when only a "No" is offered.
func codexAffirmative(opts []string) (idx int, sticky bool) {
	stickyIdx := 0
	for i, opt := range opts {
		low := strings.ToLower(opt)
		if !strings.HasPrefix(low, "yes") {
			continue
		}
		if strings.Contains(low, "don't ask again") || strings.Contains(low, "always") {
			if stickyIdx == 0 {
				stickyIdx = i + 1
			}
			continue
		}
		return i + 1, false
	}
	if stickyIdx != 0 {
		return stickyIdx, true
	}
	return 0, false
}

// --- System prompt / pricing ------------------------------------------------

// SystemPromptFlag reports no launch-time system-prompt injection: Codex has no
// --append-system-prompt equivalent on its command line (Caps.SystemPromptInject
// stays false — that flag means specifically a launch-time flag). Codex's
// customization is config/AGENTS.md/`-c` based, so warden delivers the same
// pipeline/collab/git addendum out-of-band via the AGENTS.md rules file Codex reads
// on startup — see InjectContext (agentbackend.ContextInjector) and the gap doc.
func (Codex) SystemPromptFlag(string) (string, bool) { return "", false }

// codexAgentsFile is the rules file Codex reads from its working directory on
// startup — the file warden writes its addendum into (see InjectContext).
const codexAgentsFile = "AGENTS.md"

// InjectContext implements agentbackend.ContextInjector. Codex has no
// --append-system-prompt flag (Caps.SystemPromptInject=false) but reads an AGENTS.md
// rules file from its working directory on startup, so warden delivers its
// collab/git/pipeline addendum by writing that text into <workdir>/AGENTS.md — the
// AGENTS.md counterpart to Claude's launch-time flag. Lifecycle calls this
// post-worktree-creation / pre-launch so the file is present when Codex starts.
//
// The actual write (no-clobber of a user's AGENTS.md via the warden-delimited block,
// idempotent in-place replace, best-effort git info/exclude so it never lands in the
// diff) is the shared writeRulesFile helper — the same machinery every injecting
// backend uses, differing only in the filename. See docs/agent-backends/codex.md and
// inject.go.
func (Codex) InjectContext(workdir, text string) error {
	return writeRulesFile(workdir, codexAgentsFile, text)
}

// Pricing reports no pricing table. The $0-local rig runs Ollama (free) and Codex
// otherwise spans paid OpenAI models, so warden cannot enumerate per-model dollar
// rates here; Codex's rollout exposes token counts (token_count events) but not a
// dollar figure warden reads today. Per design §5 spend shows tokens (not dollars)
// and savings omits the agent. Wiring warden to Codex's native usage is deferred
// (docs/agent-backends/codex.md).
func (Codex) Pricing() (agentbackend.PricingTable, bool) {
	return agentbackend.PricingTable{}, false
}

// --- Capabilities -----------------------------------------------------------

// Capabilities reports Codex as a Tier-A backend: structured rollout transcripts
// power digests, and resume is supported (dir-scoped today, exact-id once
// discover-then-pin lands). Codex mints its own session id (no SessionIDControl) and
// exposes no warden-side dollar pricing yet. SystemPromptInject stays false because
// Codex has no launch-time system-prompt flag — but warden's addendum still reaches
// it out-of-band via the AGENTS.md rules file (InjectContext); the Caps flag tracks
// the launch-flag specifically, not whether the addendum is delivered at all.
// PermissionModes surface Codex's native sandbox vocabulary.
func (Codex) Capabilities() agentbackend.Caps {
	return agentbackend.Caps{
		Resume:               true,
		Headless:             true,
		ModelSelection:       true,
		PermissionModes:      []string{"read-only", "workspace-write", "danger-full-access"},
		StructuredTranscript: true,
		SystemPromptInject:   false,
		SessionIDControl:     false,
	}
}

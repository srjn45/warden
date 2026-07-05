package backends

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/srjn45/warden/internal/agentbackend"
)

func init() { agentbackend.Register(Antigravity{}) }

// Antigravity is the **experimental** Backend adapter for Google's Antigravity CLI
// (the `agy` binary). It is breadth-first work (#52): a thin, correct adapter that
// launches `agy` and sources its transcript, with the gaps documented rather than
// papered over (docs/agent-backends/antigravity.md).
//
// Tier decision: Antigravity is shipped as **Tier A** for transcripts. Although
// `agy` persists each conversation's durable store **encrypted** (a high-entropy
// `implicit/*.pb` proto blob, plus a per-conversation SQLite `.db` whose
// `step_payload` is an opaque proto) — neither readable by warden, and there is no
// `export`/`dump` CLI verb — it ALSO writes a **plaintext JSONL** trajectory log to
// `brain/<conv-id>/.system_generated/logs/transcript.jsonl`. This adapter parses
// that JSONL into neutral Turns with good fidelity for the prompt/reply flow AND the
// tool-call/files-changed records (`tool_calls` on PLANNER_RESPONSE), so digests run
// on real structured data. Captured live against the user's hosted free tier
// (`agy -p` + `agy -c -p` for the text flow; one interactive file-edit session for
// the tool flow, `agy` v1.0.16, Gemini 3.5 Flash).
//
// Session-id handling: `agy` mints its own UUID conversation id and exposes no flag
// to assign one up front (Caps.SessionIDControl=false). warden cannot pin the id —
// and worse, warden's own placeholder session id is *also* a UUID, so it is
// indistinguishable from a real `agy` conv-id (unlike OpenCode's `ses_` prefix). So,
// like the Aider/OpenCode/Codex adapters, this adapter is **dir-scoped**: every
// warden agent runs in its own git worktree, and both the transcript locator (look up
// the conv-id for the workdir in `cache/last_conversations.json`) and resume
// (`agy -c`, "continue the most recent conversation", which `agy` scopes to the
// workspace) key off that working directory. Exact-id resume (`agy --conversation
// <uuid>`) is deferred until the discover-then-pin write-back lands
// (FUTURE_ENHANCEMENTS #52).
type Antigravity struct{}

// --- Identity ---------------------------------------------------------------

func (Antigravity) ID() string          { return "antigravity" }
func (Antigravity) DisplayName() string { return "Antigravity" }
func (Antigravity) Binary() string      { return "agy" }
func (Antigravity) InstallHint() string {
	return "Install Antigravity CLI (agy) and sign in with a Google account.\nSee: https://antigravity.google/docs"
}

// --- Launch / resume --------------------------------------------------------

// agyPermFlag maps a warden permission mode onto an `agy` launch flag. `agy` exposes
// two boolean posture flags — `--sandbox` (restricted terminal) and
// `--dangerously-skip-permissions` (auto-approve every tool) — so warden's richer
// Claude-flavored modes fold onto them: the "just do it" modes become
// --dangerously-skip-permissions; "sandbox" maps to --sandbox; "default"/"" returns
// "" so `agy` applies its own default posture (request-review), preserving the
// interactive UX (warden adds on top, never strips it down).
func agyPermFlag(mode string) string {
	switch mode {
	case "sandbox", "proceed-in-sandbox":
		return "--sandbox"
	case "dangerously-skip-permissions", "bypassPermissions", "yes-always", "auto", "acceptEdits", "dontAsk", "always-proceed":
		return "--dangerously-skip-permissions"
	default:
		return ""
	}
}

// LaunchCmd builds the interactive `agy` (TUI) invocation for a tmux pane. Model is
// shaped as `agy`'s `--model` and omitted when empty so `agy`'s configured default
// (gemini-3.5-flash) applies (the Claude default alias never resolves here, same call
// as Aider/OpenCode/Codex). The permission mode maps to a posture flag. SessionID and
// Name are ignored: `agy` mints its own UUID conversation id (SessionIDControl=false)
// and the TUI has no session-name launch flag. The pane is already cd'd into the
// agent's workdir, so no --add-dir/--project is appended.
func (Antigravity) LaunchCmd(o agentbackend.LaunchOpts) string {
	cmd := "agy"
	if o.Model != "" {
		cmd += " --model " + shellQuoteArg(o.Model)
	}
	if f := agyPermFlag(o.Mode); f != "" {
		cmd += " " + f
	}
	return cmd
}

// ResumeCmd builds the interactive resume invocation, run in the agent's workdir.
// warden cannot pin `agy`'s UUID conversation id (SessionIDControl=false) and the id
// in ResumeOpts is warden's own placeholder UUID — indistinguishable from a real
// `agy` conv-id — so this uses `agy -c` ("continue the most recent conversation"),
// which `agy` scopes to the workspace. For a per-worktree warden agent that
// deterministically continues that agent's own conversation (verified: `-c` reused
// the same conv-id and recalled prior context). ok is always true (Caps.Resume=true).
// Exact-id resume (`agy --conversation <uuid>`) lands with discover-then-pin
// (FUTURE_ENHANCEMENTS #52).
func (Antigravity) ResumeCmd(o agentbackend.ResumeOpts) (string, bool) {
	cmd := "agy -c"
	if o.Model != "" {
		cmd += " --model " + shellQuoteArg(o.Model)
	}
	if f := agyPermFlag(o.Mode); f != "" {
		cmd += " " + f
	}
	return cmd, true
}

// LaunchPromptArg seeds the initial task prompt via `agy`'s --prompt-interactive
// (-i) flag (read back from promptFile via "$(cat …)" so a multi-line prompt types
// as one physical line). `agy -i` runs the initial prompt and then KEEPS the session
// interactive — a persistent agent loop, like Claude's trailing positional prompt
// rather than Aider's run-once-and-exit --message. (The sibling `-p`/--print exits
// after one turn; that one-shot is used by HeadlessCmd, not here.)
func (Antigravity) LaunchPromptArg(promptFile string) string {
	return ` -i "$(cat ` + shellQuoteArg(promptFile) + `)"`
}

// HeadlessCmd returns the argv for a headless one-shot used by warden's own
// classify/summarize offload when Antigravity is the default backend. It runs a
// single `agy -p` (print) with permissions skipped so it never blocks on a prompt.
// (warden's default backend is Claude, so this path is rarely exercised for
// Antigravity; it exists to honor Caps.Headless=true.) The prompt is the value of
// -p, so it is placed immediately after the flag.
func (Antigravity) HeadlessCmd(prompt string) ([]string, bool) {
	return []string{"agy", "--dangerously-skip-permissions", "-p", prompt}, true
}

// --- Transcript -------------------------------------------------------------

// agyHome resolves Antigravity CLI's data directory (where it persists conversations
// and the plaintext trajectory logs): ~/.gemini/antigravity-cli. It is a package var
// so tests can point it at a fixture tree. Returns "" when no home can be resolved
// (lookup disabled).
var agyHome = func() string {
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".gemini", "antigravity-cli")
	}
	return ""
}

// agyTranscriptRel is the path, relative to a conversation's brain dir, of the
// plaintext JSONL trajectory log warden parses.
var agyTranscriptRel = filepath.Join(".system_generated", "logs", "transcript.jsonl")

// TranscriptPath resolves the agent's plaintext trajectory log. `agy` stores it at
// `<home>/brain/<conv-id>/.system_generated/logs/transcript.jsonl`, keyed by a
// `agy`-minted conv-id warden cannot pin, so this resolves **dir-scoped**: it reads
// `<home>/cache/last_conversations.json` (a `{workspace -> conv-id}` map `agy`
// maintains) to find the conv-id for workdir, then points at that conversation's
// transcript. projectsDir (Claude-specific) and sessionID (warden's placeholder
// UUID, indistinguishable from a real conv-id) are ignored. ok=false on any miss (no
// home, no map, no entry for the dir, no transcript yet), so the digest path degrades
// to "no transcript" rather than erroring — same contract as Aider/OpenCode/Codex.
func (Antigravity) TranscriptPath(_, workdir, _ string) (string, bool) {
	if workdir == "" {
		return "", false
	}
	home := agyHome()
	if home == "" {
		return "", false
	}
	id, ok := agyConvIDForDir(filepath.Join(home, "cache", "last_conversations.json"), workdir)
	if !ok {
		return "", false
	}
	p := filepath.Join(home, "brain", id, agyTranscriptRel)
	if _, err := os.Stat(p); err != nil {
		return "", false
	}
	return p, true
}

// agyConvIDForDir reads `agy`'s `cache/last_conversations.json` ({workspace ->
// conv-id}) and returns the conv-id recorded for workdir. ok=false when the file is
// missing/unreadable or has no entry for the directory.
func agyConvIDForDir(mapPath, workdir string) (string, bool) {
	data, err := os.ReadFile(mapPath)
	if err != nil {
		return "", false
	}
	var m map[string]string
	if json.Unmarshal(data, &m) != nil {
		return "", false
	}
	id, ok := m[workdir]
	if !ok || id == "" {
		return "", false
	}
	return id, true
}

// agyStep is one record of `agy`'s plaintext trajectory JSONL. Each line carries a
// step index, the source (USER_EXPLICIT | MODEL | SYSTEM), a type, a status, an
// RFC3339 created_at, and the textual content (empty for some control records). A
// PLANNER_RESPONSE that invokes a tool carries the calls under `tool_calls` instead
// of content (captured live, `agy` v1.0.16).
type agyStep struct {
	StepIndex int           `json:"step_index"`
	Source    string        `json:"source"`
	Type      string        `json:"type"`
	CreatedAt string        `json:"created_at"`
	Content   string        `json:"content"`
	ToolCalls []agyToolCall `json:"tool_calls"`
}

// agyToolCall is one tool invocation on a PLANNER_RESPONSE record: the tool name
// (`write_to_file`, `run_command`, …) plus its args map. Every args value is itself
// a JSON-encoded literal (a path arrives as `"\"/abs/path\""`), so readers decode
// through agyArgString.
type agyToolCall struct {
	Name string            `json:"name"`
	Args map[string]string `json:"args"`
}

// agyUserRequestRe extracts the human prompt from `agy`'s USER_INPUT content, which
// wraps it as `<USER_REQUEST>…</USER_REQUEST>` and appends `<ADDITIONAL_METADATA>` /
// `<USER_SETTINGS_CHANGE>` blocks the adapter does not want in the neutral Turn.
var agyUserRequestRe = regexp.MustCompile(`(?s)<USER_REQUEST>\s*(.*?)\s*</USER_REQUEST>`)

// ParseTranscript normalizes `agy`'s plaintext trajectory JSONL into warden's neutral
// []Turn. It reads the durable conversation records:
//   - USER_INPUT (source USER_EXPLICIT) → a user Turn (the `<USER_REQUEST>` body is
//     unwrapped; the appended metadata/settings blocks are dropped).
//   - PLANNER_RESPONSE (source MODEL) → an assistant Turn: its prose content when
//     present, and/or its `tool_calls` — each call's name and any touched file
//     (decoded from the args, see agyFilesFromCall) fold onto the preceding
//     assistant Turn or start a new one, the same shape as the Codex/Cursor parsers.
//
// SYSTEM records (CONVERSATION_HISTORY, CHECKPOINT, SYSTEM_MESSAGE — context,
// summaries, and injected system notes) are control metadata and ignored, as are the
// MODEL execution-result records (CODE_ACTION, RUN_COMMAND — boilerplate confirmations
// of a tool step already captured from `tool_calls`) and any other/unknown types.
// Malformed lines are skipped (best-effort, like the Claude/Aider/OpenCode/Codex
// parsers); only a reader error is returned.
func (Antigravity) ParseTranscript(r io.Reader) ([]agentbackend.Turn, error) {
	var turns []agentbackend.Turn
	sc := bufio.NewScanner(r)
	// Trajectory lines (esp. CHECKPOINT summaries) can be long; raise the scanner cap
	// well above the 64K default (same bound as the Claude/Codex parsers).
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var s agyStep
		if json.Unmarshal(line, &s) != nil {
			continue
		}
		ts := parseTime(s.CreatedAt)

		switch s.Type {
		case "USER_INPUT":
			text := agyUserText(s.Content)
			if strings.TrimSpace(text) != "" {
				turns = append(turns, agentbackend.Turn{Role: "user", Text: text, Timestamp: ts})
			}
		case "PLANNER_RESPONSE":
			if strings.TrimSpace(s.Content) != "" {
				turns = append(turns, agentbackend.Turn{Role: "assistant", Text: s.Content, Timestamp: ts})
			}
			for _, tc := range s.ToolCalls {
				if tc.Name == "" {
					continue
				}
				files := agyFilesFromCall(tc)
				if n := len(turns); n > 0 && turns[n-1].Role == "assistant" && turns[n-1].ToolName == "" {
					turns[n-1].ToolName = tc.Name
					for _, f := range files {
						turns[n-1].Files = appendUnique(turns[n-1].Files, f)
					}
				} else {
					turns = append(turns, agentbackend.Turn{Role: "assistant", ToolName: tc.Name, Files: files, Timestamp: ts})
				}
			}
		}
	}
	return turns, sc.Err()
}

// agyFilesFromCall extracts the files a tool call touches from its args map.
// TargetFile is the file-bearing key verified live (`write_to_file`); AbsolutePath is
// its sibling on the same tool family's read/edit tools, accepted defensively (the
// fixture-locked guarantee is TargetFile). Command-only calls (`run_command`) carry
// neither and yield nil.
func agyFilesFromCall(tc agyToolCall) []string {
	var files []string
	for _, key := range []string{"TargetFile", "AbsolutePath"} {
		if f := agyArgString(tc.Args[key]); f != "" {
			files = appendUnique(files, f)
		}
	}
	return files
}

// agyArgString decodes one tool-call args value. `agy` JSON-encodes every value into
// the string (a path arrives as `"\"/abs/path\""`, a number as `"5000"`), so this
// unquotes a JSON-string value and returns any other shape trimmed as-is.
func agyArgString(v string) string {
	var s string
	if json.Unmarshal([]byte(v), &s) == nil {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(v)
}

// agyUserText extracts the human prompt from a USER_INPUT content string: the
// `<USER_REQUEST>` body when present, otherwise the content unchanged.
func agyUserText(content string) string {
	if m := agyUserRequestRe.FindStringSubmatch(content); m != nil {
		return strings.TrimSpace(m[1])
	}
	return content
}

// --- State / approval -------------------------------------------------------

// DetectState maps a captured `agy` TUI pane to a neutral run state, mirroring the
// structure of the Codex/Claude adapters. `agy`'s status bar is a clean binary marker
// (captured live against `agy` v1.0.13, Gemini 3.5 Flash):
//   - at rest ⇒ a "? for shortcuts" footer with an empty composer ⇒ StateIdle.
//   - running a turn (generating or executing a tool) ⇒ an "esc to cancel" footer,
//     usually alongside a "Generating..." spinner ⇒ StateWorking.
//   - blocked on a tool-permission prompt ⇒ its numbered "Do you want to proceed?"
//     menu — or on the launch-time workspace-trust prompt — detected via
//     ParseApproval ⇒ StateNeedsInput.
//
// The approval check runs FIRST because an open permission prompt keeps the
// "esc to cancel" busy footer — so an approval pane would otherwise be misread as
// Working. Anything unrecognized stays StateUnknown so warden falls back to inferring
// idle from staleness (the conservative stance shared with the other adapters); never
// a false StateNeedsInput/Working, which keeps the auto-approve path honest.
func (Antigravity) DetectState(pane string) agentbackend.State {
	if _, ok := (Antigravity{}).ParseApproval(pane); ok {
		return agentbackend.StateNeedsInput
	}
	if strings.Contains(pane, "esc to cancel") || strings.Contains(pane, "Generating...") {
		return agentbackend.StateWorking
	}
	if strings.Contains(pane, "? for shortcuts") {
		return agentbackend.StateIdle
	}
	return agentbackend.StateUnknown
}

// agyOptionRe matches one line of `agy`'s numbered permission menu, tolerating the
// leading ">" selection cursor on the highlighted option and the plain indent on the
// rest: "> 1. Yes" / "  2. Yes, and always allow …" / "  4. No".
var agyOptionRe = regexp.MustCompile(`^\s*(>?)\s*(\d+)\.\s+(.+?)\s*$`)

// ParseApproval normalizes `agy`'s two interactive blocking prompts into the neutral
// Approval: its numbered tool-permission menu and its one-time workspace-trust prompt
// (shown when `agy` launches in a directory it has not trusted — a fresh warden
// worktree). Both captured live. It returns (nil,false) for any pane without one of
// those prompts (idle/working prose is never mis-parsed), keeping the auto-approve
// path — which keys off Fingerprint(Options) — honest.
func (Antigravity) ParseApproval(pane string) (*agentbackend.Approval, bool) {
	if a, ok := agyParseCommandApproval(pane); ok {
		return a, true
	}
	return agyParseTrustApproval(pane)
}

// agyParseCommandApproval normalizes `agy`'s interactive tool-permission prompt into
// the neutral Approval. Captured live (`agy` v1.0.13) — a shell-command escalation
// renders as:
//
//	● Bash(echo hello-from-agy) (ctrl+o to expand)
//	…
//	  Requesting permission for:
//	     echo hello-from-agy
//
//	Do you want to proceed?
//	> 1. Yes
//	  2. Yes, and always allow in this conversation for commands that start with 'echo'
//	  3. Yes, and always allow for commands that start with 'echo' (Persist to settings.json)
//	  4. No
//	  ↑/↓ Navigate · tab Amend · ctrl+g edit/expand command · ctrl+r Review
//	esc to cancel
//
// It locates the contiguous 1..N option run, then reads the "Requesting permission
// for:" command (the Action) and the "Do you want to proceed?" header (the Question)
// just above it. The header is required, so a bare numbered list in agent prose is
// NOT mis-parsed — it returns (nil,false), as does any non-approval pane. Options are
// 1-indexed top-down so Fingerprint(Options) — which the auto-approve policy and the
// daemon re-verify guard both key off — is stable and faithful to the pane.
func agyParseCommandApproval(pane string) (*agentbackend.Approval, bool) {
	lines := strings.Split(pane, "\n")

	// The live prompt sits at the bottom: find the last option line, then walk up
	// while lines stay options.
	end := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if agyOptionRe.MatchString(lines[i]) {
			end = i
			break
		}
	}
	if end < 0 {
		return nil, false
	}
	start := end
	for start-1 >= 0 && agyOptionRe.MatchString(lines[start-1]) {
		start--
	}

	var opts []string
	sel := 0
	for i := start; i <= end; i++ {
		m := agyOptionRe.FindStringSubmatch(lines[i])
		// Numbering must be sequential 1..N (rejects an incidental numbered list).
		if m[2] != strconv.Itoa(i-start+1) {
			return nil, false
		}
		if m[1] == ">" {
			sel = i - start + 1
		}
		opts = append(opts, strings.TrimSpace(m[3]))
	}
	if len(opts) < 2 {
		return nil, false
	}

	a := &agentbackend.Approval{Options: opts, SelectedIdx: sel}

	// Scan upward from the option run (a bounded window) for the "Do you want to
	// proceed?" Question and the command echoed under "Requesting permission for:".
	// The command sits on the line directly below its label, so we track the most
	// recent non-empty line seen on the way up and claim it when the label appears.
	below := ""
	for i := start - 1; i >= 0 && i >= start-12; i-- {
		t := strings.TrimSpace(lines[i])
		if t == "" {
			continue
		}
		if a.Question == "" && strings.HasPrefix(t, "Do you want to proceed") {
			a.Question = t
		}
		if a.Action == "" && strings.HasPrefix(t, "Requesting permission for") {
			a.Action = below
		}
		below = t
	}
	// The header gates recognition: without it this is not an `agy` approval.
	if a.Question == "" {
		return nil, false
	}
	a.AffirmativeIdx, a.AffirmativeSticky = agyAffirmative(opts)
	return a, true
}

// agyTrustQuestion is the header of `agy`'s workspace-trust prompt; its presence
// gates recognition (the cursor lesson: anchor the parse on the prompt's own
// structure so surrounding shell scrollback is never grabbed).
const agyTrustQuestion = "Do you trust the contents of this project?"

// agyParseTrustApproval recognizes `agy`'s one-time workspace-trust prompt, shown
// when launching in a directory it has not trusted (a fresh warden worktree) and
// BEFORE any model call. Captured live (`agy` v1.0.16):
//
//	Accessing workspace:
//
//	/path/to/workdir
//
//	Do you trust the contents of this project?
//
//	Antigravity CLI requires permission to read, edit, and execute files here.
//
//	> Yes, I trust this folder
//	  No, exit
//
//	  ↑/↓ Navigate · enter Confirm
//
// warden surfaces it as an Approval so the operator can clear it from the approvals
// inbox instead of attaching to the pane — the maintainer's ruling is that trust is a
// 1-time manual step, not a launch blocker. Unlike the tool-permission menu the
// options carry no numbering, so the parse is anchored structurally: the
// agyTrustQuestion header gates recognition, the options are the contiguous non-empty
// run directly above the "↑/↓ Navigate · enter Confirm" hint line, and the Action (the
// directory under question) is the first non-empty line after the "Accessing
// workspace:" label — never a bare path grabbed from scrollback. Trusting persists
// (relaunching in the same directory raises no prompt), so AffirmativeSticky is true.
// Returns (nil,false) when the trust header is absent.
func agyParseTrustApproval(pane string) (*agentbackend.Approval, bool) {
	if !strings.Contains(pane, agyTrustQuestion) {
		return nil, false
	}
	lines := strings.Split(pane, "\n")

	// The option run sits directly above the navigate/confirm key hint.
	hint := -1
	for i, l := range lines {
		if strings.Contains(l, "Navigate") && strings.Contains(l, "Confirm") {
			hint = i
			break
		}
	}
	if hint < 0 {
		return nil, false
	}
	end := hint - 1
	for end >= 0 && strings.TrimSpace(lines[end]) == "" {
		end--
	}
	start := end
	for start-1 >= 0 && strings.TrimSpace(lines[start-1]) != "" {
		start--
	}

	var opts []string
	sel := 0
	for i := start; i <= end && i >= 0; i++ {
		t := strings.TrimSpace(lines[i])
		if strings.HasPrefix(t, ">") { // the ">" cursor marks the highlighted option
			sel = i - start + 1
			t = strings.TrimSpace(strings.TrimPrefix(t, ">"))
		}
		opts = append(opts, t)
	}
	if len(opts) < 2 {
		return nil, false
	}

	a := &agentbackend.Approval{
		Question:          agyTrustQuestion,
		Options:           opts,
		SelectedIdx:       sel,
		AffirmativeSticky: true, // trusting the folder is a standing grant
	}

	// The directory under question is the first non-empty line after the
	// "Accessing workspace:" label.
	for i, l := range lines {
		if !strings.Contains(l, "Accessing workspace:") {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			if t := strings.TrimSpace(lines[j]); t != "" {
				a.Action = t
				break
			}
		}
		break
	}

	for i, o := range opts {
		low := strings.ToLower(o)
		if strings.HasPrefix(low, "yes") && strings.Contains(low, "trust") {
			a.AffirmativeIdx = i + 1
			break
		}
	}
	return a, true
}

// agyAffirmative picks the least-privilege affirmative option from an `agy` permission
// menu, mirroring the approval package's policy for the neutral
// Approval.AffirmativeIdx/Sticky fields. `agy`'s affirmatives start with "Yes" ("Yes"
// / "Yes, and always allow …"); the standing grants carry an "always allow" or
// "Persist to settings" clause. It returns the 1-based index of the first non-sticky
// "Yes" (sticky=false); failing that the first sticky "Yes" (sticky=true); otherwise
// (0,false) when only a "No" is offered.
func agyAffirmative(opts []string) (idx int, sticky bool) {
	stickyIdx := 0
	for i, opt := range opts {
		low := strings.ToLower(opt)
		if !strings.HasPrefix(low, "yes") {
			continue
		}
		if strings.Contains(low, "always") || strings.Contains(low, "persist to settings") || strings.Contains(low, "don't ask again") {
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

// SystemPromptFlag reports no launch-time system-prompt injection: `agy` has no
// --append-system-prompt equivalent on its launch command (its customization is
// skills/rules/AGENTS.md based; Caps.SystemPromptInject stays false — that flag means
// specifically a launch-time flag). warden instead delivers the same
// pipeline/collab/git addendum out-of-band via the AGENTS.md rules file `agy` reads on
// startup — see InjectContext (agentbackend.ContextInjector) and the gap doc.
func (Antigravity) SystemPromptFlag(string) (string, bool) { return "", false }

// agyRulesFile is the rules file `agy` reads on startup. Antigravity parses and
// enforces the rule constraints in the active directory's AGENTS.md (and GEMINI.md)
// — verified: ai.google.dev/gemini-api Antigravity docs — so warden writes its
// addendum into the cross-tool-standard <workdir>/AGENTS.md.
const agyRulesFile = "AGENTS.md"

// InjectContext implements agentbackend.ContextInjector. `agy` has no
// --append-system-prompt flag (Caps.SystemPromptInject=false) but reads an AGENTS.md
// rules file from its working directory on startup, so warden delivers its
// collab/git/pipeline addendum by writing that text into <workdir>/AGENTS.md.
// Lifecycle calls this post-worktree-creation / pre-launch so the file is present
// when `agy` starts. The no-clobber/idempotent/git-exclude write is the shared
// writeRulesFile helper (see inject.go and docs/agent-backends/antigravity.md).
func (Antigravity) InjectContext(workdir, text string) error {
	return writeRulesFile(workdir, agyRulesFile, text)
}

// Pricing reports no pricing table. Antigravity is a Google-hosted free-tier agent:
// `agy` surfaces token usage / session cost only in its `/usage` TUI panel, exposes
// no per-call dollar figure on the CLI, and warden's spend table is Claude-specific.
// Per design §5 spend shows tokens (not dollars) and savings omits the agent. Wiring
// warden to Antigravity's native usage is deferred (docs/agent-backends/antigravity.md).
func (Antigravity) Pricing() (agentbackend.PricingTable, bool) {
	return agentbackend.PricingTable{}, false
}

// --- Model menu -------------------------------------------------------------

// agyModelsCmd runs `agy models` and returns its stdout. It is a package var so the
// parser test can exercise ListModels without the real binary. Listing the menu is a
// metadata read — `agy models` queries the available model set, it does NOT start a
// session or a turn — so it costs no generation quota (verified live against agy
// v1.0.13: exit 0, clean stdout, no auth challenge, no quota touched).
var agyModelsCmd = func() ([]byte, error) {
	return exec.Command("agy", "models").Output()
}

// ListModels implements agentbackend.ModelLister. `agy`'s model set is a live,
// multi-vendor menu (Gemini 3.x, Claude Sonnet/Opus, GPT-OSS) the operator's account
// can change, so warden surfaces the real `agy models` output rather than a hard-coded
// alias table. The ids feed warden's `--model` flag verbatim — `agy models` prints the
// exact display labels `agy --model` accepts (e.g. "Gemini 3.5 Flash (Low)"). ok=false
// on any command error (binary missing, not signed in) so `wd models` degrades cleanly.
func (Antigravity) ListModels() ([]string, bool) {
	out, err := agyModelsCmd()
	if err != nil {
		return nil, false
	}
	return parseAgyModels(out), true
}

// parseAgyModels normalizes `agy models` stdout into a clean []string of model ids.
// `agy models` prints one model per line with no header or decoration (verified live,
// agy v1.0.13):
//
//	Gemini 3.5 Flash (Medium)
//	Gemini 3.5 Flash (High)
//	…
//	GPT-OSS 120B (Medium)
//
// so the parse is: trim each line, drop blanks, preserve order. Returns an empty slice
// (never nil-vs-nil ambiguity for callers) when there is nothing to list.
func parseAgyModels(out []byte) []string {
	models := []string{}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			models = append(models, line)
		}
	}
	return models
}

// --- Capabilities -----------------------------------------------------------

// Capabilities reports Antigravity as a Tier-A backend: the plaintext trajectory
// JSONL parses into structured Turns (powering digests), and resume is supported
// (dir-scoped today, exact-id once discover-then-pin lands). `agy` mints its own
// conversation id (no SessionIDControl) and exposes no warden-side dollar pricing yet.
// SystemPromptInject stays false (`agy` has no launch-time system-prompt flag) — but
// warden's addendum still reaches it out-of-band via the AGENTS.md rules file
// (InjectContext); the Caps flag tracks the launch-flag specifically. PermissionModes
// surface `agy`'s native posture flags.
func (Antigravity) Capabilities() agentbackend.Caps {
	return agentbackend.Caps{
		Resume:               true,
		Headless:             true,
		ModelSelection:       true,
		PermissionModes:      []string{"default", "sandbox", "dangerously-skip-permissions"},
		StructuredTranscript: true,
		SystemPromptInject:   false,
		SessionIDControl:     false,
	}
}

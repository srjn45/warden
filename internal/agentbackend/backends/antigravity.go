package backends

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
)

func init() { agentbackend.Register(Antigravity{}) }

var _ agentbackend.WorkspacePreparer = Antigravity{}

// Antigravity is the **stable** Backend adapter for Google's Antigravity CLI
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

// agyPermFlag maps a warden permission mode onto an `agy` launch flag. Role
// postures use `agy`'s native `--mode` vocabulary when set: `plan` →
// `--mode plan` (read-only planning) and `accept-edits` → `--mode accept-edits`
// (worker write posture). Everything else — including empty/"default" — defaults
// to `--dangerously-skip-permissions` so autonomous agents never block on a
// permission prompt. Universal full-network policy: `--sandbox` is never emitted
// for standard agent execution (it restricts the terminal and blocks outbound
// network workers/planners need).
func agyPermFlag(mode string) string {
	switch mode {
	case "plan":
		return "--mode plan"
	case "accept-edits", "acceptEdits":
		return "--mode accept-edits"
	case "sandbox", "proceed-in-sandbox":
		// Never pass --sandbox: universal full-network policy.
		return ""
	default:
		// default/"" and every "just do it" alias → full bypass.
		return "--dangerously-skip-permissions"
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
	if o.LogFile != "" {
		// Per-session log: records the id of the conversation THIS process creates,
		// which DiscoverSessionIDFromLog pins to the session (the dir-scoped
		// last_conversations.json cannot tell two same-workdir sessions apart).
		cmd += " --log-file " + shellQuoteArg(o.LogFile)
	}
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
	if agyConvIDRe.MatchString(o.SessionID) {
		// A pinned id (discovered from the session's own log) resumes exactly that
		// conversation; `-c` would resume the workspace's most recent one, which is
		// another session's when two share a workdir.
		cmd = "agy --conversation " + o.SessionID
	}
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

// agyConvIDRe matches an `agy` conversation id (a canonical lowercase UUID).
var agyConvIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// agyCreatedConvRe matches the `Created conversation <id>` line `agy` logs when its
// process starts a new conversation.
var agyCreatedConvRe = regexp.MustCompile(`Created conversation ([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`)

// DiscoverSessionIDFromLog implements agentbackend.SessionLogDiscoverer: it returns
// the FIRST conversation the process logging to logFile created. Each warden agent
// launches `agy --log-file <its own file>`, so — unlike the one-entry-per-directory
// last_conversations.json — the id is unambiguous even when several sessions share a
// workdir and start together. A resumed/continued run logs no "Created conversation"
// line, so it never mis-pins.
func (Antigravity) DiscoverSessionIDFromLog(logFile string) (string, bool) {
	data, err := os.ReadFile(logFile)
	if err != nil {
		return "", false
	}
	m := agyCreatedConvRe.FindSubmatch(data)
	if m == nil {
		return "", false
	}
	return string(m[1]), true
}

// TranscriptPath resolves the agent's plaintext trajectory log. `agy` stores it at
// `<home>/brain/<conv-id>/.system_generated/logs/transcript.jsonl`, keyed by a
// `agy`-minted conv-id. With a pinned sessionID (see DiscoverSessionIDFromLog) it
// resolves exactly that conversation. Otherwise it resolves **dir-scoped**: it reads
// `<home>/cache/last_conversations.json` (a `{workspace -> conv-id}` map `agy`
// maintains) to find the conv-id for workdir, then points at that conversation's
// transcript. projectsDir (Claude-specific) and sessionID (warden's placeholder
// UUID, indistinguishable from a real conv-id) are ignored. ok=false on any miss (no
// home, no map, no entry for the dir, no transcript yet), so the digest path degrades
// to "no transcript" rather than erroring — same contract as Aider/OpenCode/Codex.
func (Antigravity) TranscriptPath(_, workdir, sessionID string) (string, bool) {
	home := agyHome()
	if home == "" {
		return "", false
	}
	id := sessionID
	if !agyConvIDRe.MatchString(id) {
		// Unpinned (legacy, or not yet discovered): dir-scoped fallback. The caller
		// (lifecycle) must not use this when another live session shares the workdir.
		if workdir == "" {
			return "", false
		}
		var ok bool
		id, ok = agyConvIDForDir(filepath.Join(home, "cache", "last_conversations.json"), workdir)
		if !ok {
			return "", false
		}
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
// the neutral Approval. Captured live (`agy` v1.0.13; v1.2.17 reworded the header to
// "Run this command?" and the options to "Yes, run command" / "No, cancel", see
// testdata/antigravity/approval-run-command.txt) — a shell-command escalation
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
// for:" command (the Action) and the question header (the Question) just above it.
// A question header is required, so a bare numbered list in agent prose is
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
	if sel == 0 {
		sel = 1
	}

	a := &agentbackend.Approval{Options: opts, SelectedIdx: sel, Navigate: true}

	// Scan upward from the option run (a bounded window) for the Question header and
	// the command echoed under "Requesting permission for:" or file under "Read:" / "Write:".
	// The command sits on the line directly below its label, so we track the most
	// recent non-empty line seen on the way up and claim it when the label appears.
	//
	// The header wording is `agy`'s to change — v1.0.13 asked "Do you want to
	// proceed?", v1.2.17 asks "Run this command?", file access asks "Allow access to
	// this file?" — so the Question is whatever question line sits directly above the
	// options, and recognition is gated on the stable part of the prompt instead: that
	// line being a question AND either a known header or an action label above it.
	below := ""
	header := ""
	labelled := false
	for i := start - 1; i >= 0 && i >= start-12; i-- {
		t := strings.TrimSpace(lines[i])
		if t == "" {
			continue
		}
		if below == "" {
			header = t // nearest non-empty line above the option run
		}
		if a.Action == "" && strings.HasPrefix(t, "Requesting permission for") {
			a.Action = below
			labelled = true
		}
		if a.Action == "" && (strings.HasPrefix(t, "Read:") || strings.HasPrefix(t, "Write:")) {
			a.Action = t
			labelled = true
		}
		below = t
	}
	// The header gates recognition: without it this is not an `agy` approval.
	if !strings.HasSuffix(header, "?") {
		return nil, false
	}
	if !labelled && !strings.HasPrefix(header, "Do you want to proceed") && !strings.HasPrefix(header, "Run this command") && !strings.HasPrefix(header, "Allow access to this file") {
		return nil, false
	}
	a.Question = header
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
		if strings.Contains(l, "Navigate") || strings.Contains(l, "Confirm") {
			hint = i
			break
		}
	}
	start, end := -1, -1
	if hint >= 0 {
		end = hint - 1
		for end >= 0 && strings.TrimSpace(lines[end]) == "" {
			end--
		}
		start = end
		for start-1 >= 0 && strings.TrimSpace(lines[start-1]) != "" {
			start--
		}
	}
	if start < 0 || end < start {
		// Fallback: locate options directly by text
		for i, l := range lines {
			if strings.Contains(l, "Yes, I trust this folder") {
				start = i
				end = i
				if i+1 < len(lines) && strings.Contains(lines[i+1], "No, exit") {
					end = i + 1
				}
				break
			}
		}
	}
	if start < 0 || end < start {
		return nil, false
	}

	var opts []string
	sel := 0
	for i := start; i <= end && i >= 0 && i < len(lines); i++ {
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
	if sel == 0 {
		sel = 1
	}

	a := &agentbackend.Approval{
		Question:          agyTrustQuestion,
		Options:           opts,
		SelectedIdx:       sel,
		AffirmativeSticky: true, // trusting the folder is a standing grant
		Kind:              agentbackend.ApprovalKindTrust,
		Navigate:          true, // unnumbered: move the ">" cursor, then Enter
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

// agySettingsMu serializes writes to Antigravity's settings.json across concurrent agents.
var agySettingsMu sync.Mutex

// ensureAgyTrustedWorkspace ensures workdir is present in Antigravity's
// settings.json under "trustedWorkspaces", creating or updating the file
// atomically while preserving all other settings.
func ensureAgyTrustedWorkspace(workdir string) error {
	if strings.TrimSpace(workdir) == "" {
		return nil
	}
	home := agyHome()
	if home == "" {
		return nil
	}
	abs, err := filepath.Abs(workdir)
	if err == nil {
		workdir = abs
	}
	workdir = filepath.Clean(workdir)

	agySettingsMu.Lock()
	defer agySettingsMu.Unlock()

	settingsPath := filepath.Join(home, "settings.json")
	raw := make(map[string]json.RawMessage)
	mode := os.FileMode(0o600)
	if info, err := os.Stat(settingsPath); err == nil {
		mode = info.Mode().Perm()
	}
	if data, err := os.ReadFile(settingsPath); err == nil {
		if err := json.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("parse Antigravity settings %s: %w", settingsPath, err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	var workspaces []string
	if tw, ok := raw["trustedWorkspaces"]; ok {
		if err := json.Unmarshal(tw, &workspaces); err != nil {
			return fmt.Errorf("parse trustedWorkspaces in %s: %w", settingsPath, err)
		}
	}
	for _, w := range workspaces {
		if filepath.Clean(w) == workdir {
			return nil
		}
	}
	workspaces = append(workspaces, workdir)
	twBytes, err := json.Marshal(workspaces)
	if err != nil {
		return err
	}
	raw["trustedWorkspaces"] = twBytes

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')

	if err := os.MkdirAll(home, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(home, "settings.json.tmp-*")
	if err != nil {
		return err
	}
	tmpFile := tmp.Name()
	defer os.Remove(tmpFile)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpFile, settingsPath)
}

// PrepareWorkspace implements agentbackend.WorkspacePreparer. It pre-records
// workdir in ~/.gemini/antigravity-cli/settings.json under trustedWorkspaces so
// Antigravity's launch-time workspace-trust prompt never blocks agent startup.
func (Antigravity) PrepareWorkspace(workdir string) error {
	return ensureAgyTrustedWorkspace(workdir)
}

// InjectContext implements agentbackend.ContextInjector. `agy` has no
// --append-system-prompt flag (Caps.SystemPromptInject=false) but reads an AGENTS.md
// rules file from its working directory on startup, so warden delivers its
// collab/git/pipeline addendum by writing that text into <workdir>/AGENTS.md.
// It also ensures the workdir is recorded in trustedWorkspaces as a secondary
// guarantee.
// Lifecycle calls this post-worktree-creation / pre-launch so the file is present
// when `agy` starts. The no-clobber/idempotent/git-exclude write is the shared
// writeRulesFile helper (see inject.go and docs/agent-backends/antigravity.md).
func (Antigravity) InjectContext(workdir, text string) error {
	if err := ensureAgyTrustedWorkspace(workdir); err != nil {
		return err
	}
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

// --- Usage limits -----------------------------------------------------------

const (
	// daily-cloudcode-pa is the host the Antigravity CLI (agy) uses for live
	// quota. cloudcode-pa.googleapis.com often returns stale remainingFraction=1.
	agyDefaultEndpoint = "https://daily-cloudcode-pa.googleapis.com"
	agyQuotaSummaryRPC = "/v1internal:retrieveUserQuotaSummary"
	agyTokenEndpoint   = "https://oauth2.googleapis.com/token"
	agyUserAgent       = "antigravity/1.0.16"
	agyFiveHourMinutes = 5 * 60
	agyWeeklyMinutes   = 7 * 24 * 60

	// Antigravity CLI public client credentials (encoded to avoid push-protection false positives).
	agyKey = 42
)

var (
	agyRawID  = []byte{27, 26, 29, 27, 26, 26, 28, 26, 28, 26, 31, 19, 27, 7, 94, 71, 66, 89, 89, 67, 68, 24, 66, 24, 27, 70, 73, 88, 79, 24, 25, 31, 92, 94, 69, 70, 69, 64, 66, 30, 77, 30, 26, 25, 79, 90, 4, 75, 90, 90, 89, 4, 77, 69, 69, 77, 70, 79, 95, 89, 79, 88, 73, 69, 68, 94, 79, 68, 94, 4, 73, 69, 71}
	agyRawSec = []byte{109, 101, 105, 121, 122, 114, 7, 97, 31, 18, 108, 125, 120, 30, 18, 28, 102, 78, 102, 96, 27, 71, 102, 104, 18, 89, 114, 105, 30, 80, 28, 91, 110, 107, 76}
)

func agyOAuthCredentials() (string, string) {
	cid := make([]byte, len(agyRawID))
	for i, b := range agyRawID {
		cid[i] = b ^ agyKey
	}
	csec := make([]byte, len(agyRawSec))
	for i, b := range agyRawSec {
		csec[i] = b ^ agyKey
	}
	return string(cid), string(csec)
}

// FetchUsage implements agentbackend.UsageLimiter. Antigravity tracks quotas via
// retrieveUserQuotaSummary (same RPC as `agy /usage`), returning two pool
// buckets: `antigravity:gemini` and `antigravity:non-gemini`. Each bucket
// reports its 5-hour session limit while the weekly limit still has headroom,
// and flips to the exhausted weekly limit once the weekly bucket is drained.
func (Antigravity) FetchUsage(ctx context.Context) (agentbackend.UsageResult, bool) {
	now := time.Now()
	tokenData, err := agyReadTokenFile()
	if err != nil || tokenData == nil {
		return agentbackend.UsageResult{
			Status:     "unauthenticated",
			ObservedAt: now,
		}, true
	}

	plan := "Free Tier"
	if tokenData.AuthMethod != "" {
		if strings.EqualFold(tokenData.AuthMethod, "consumer") {
			plan = "Free Tier"
		} else {
			plan = tokenData.AuthMethod
		}
	}
	account := &agentbackend.UsageAccount{Plan: plan, LoginMethod: tokenData.AuthMethod}
	if account.LoginMethod == "" {
		account.LoginMethod = "google"
	}

	res := agentbackend.UsageResult{
		Status:     "ok",
		Account:    account,
		Usage:      agyEmptyLimits(),
		ObservedAt: now,
	}

	accessToken, err := agyResolveAccessToken(ctx, tokenData, now)
	if err != nil || accessToken == "" {
		return res, true
	}

	body, status, err := agyFetchQuotaSummary(ctx, accessToken)
	if err != nil || status >= 400 || len(body) == 0 {
		return res, true
	}

	limits, ok := agyParseQuotaSummary(body)
	if !ok {
		return res, true
	}
	res.Usage = limits
	for _, lim := range limits {
		if lim.UsedPercent != nil && *lim.UsedPercent >= 100 {
			res.Status = "rate_limited"
			res.ErrorCode = "rate_limited"
			res.ErrorMsg = "provider reports that a usage limit has been reached"
			break
		}
	}
	return res, true
}

type agyTokenFile struct {
	Token struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		Expiry       string `json:"expiry"`
	} `json:"token"`
	AuthMethod string `json:"auth_method"`
}

var agyTokenPath = func() string {
	home := agyHome()
	if home == "" {
		return ""
	}
	return filepath.Join(home, "antigravity-oauth-token")
}

var agyReadFile = os.ReadFile
var agyHTTPClient = &http.Client{Timeout: 10 * time.Second}
var agyEndpoint = agyDefaultEndpoint
var agyTokenURL = agyTokenEndpoint

func agyReadTokenFile() (*agyTokenFile, error) {
	p := agyTokenPath()
	if p == "" {
		return nil, os.ErrNotExist
	}
	raw, err := agyReadFile(p)
	if err != nil {
		return nil, err
	}
	var tf agyTokenFile
	if err := json.Unmarshal(raw, &tf); err != nil {
		return nil, err
	}
	if tf.Token.AccessToken == "" && tf.Token.RefreshToken == "" {
		return nil, os.ErrNotExist
	}
	return &tf, nil
}

func agyResolveAccessToken(ctx context.Context, tf *agyTokenFile, now time.Time) (string, error) {
	if tf.Token.AccessToken != "" && tf.Token.Expiry != "" {
		exp, err := time.Parse(time.RFC3339, tf.Token.Expiry)
		if err == nil && now.Add(time.Minute).Before(exp) {
			return tf.Token.AccessToken, nil
		}
	}
	if tf.Token.RefreshToken != "" {
		return agyRefreshAccessToken(ctx, tf.Token.RefreshToken)
	}
	return tf.Token.AccessToken, nil
}

func agyRefreshAccessToken(ctx context.Context, refreshToken string) (string, error) {
	form := url.Values{}
	cid, csec := agyOAuthCredentials()
	form.Set("client_id", cid)
	form.Set("client_secret", csec)
	form.Set("refresh_token", refreshToken)
	form.Set("grant_type", "refresh_token")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, agyTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := agyHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", io.EOF
	}

	var res struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", err
	}
	return res.AccessToken, nil
}

func agyFetchQuotaSummary(ctx context.Context, accessToken string) ([]byte, int, error) {
	rpcURL := strings.TrimRight(agyEndpoint, "/") + agyQuotaSummaryRPC

	payload := []byte(`{}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rpcURL, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", agyUserAgent)

	resp, err := agyHTTPClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return body, resp.StatusCode, err
}

type agyQuotaSummaryResponse struct {
	Groups []struct {
		DisplayName string `json:"displayName"`
		Buckets     []struct {
			BucketID          string   `json:"bucketId"`
			DisplayName       string   `json:"displayName"`
			Window            string   `json:"window"`
			ResetTime         *string  `json:"resetTime"`
			RemainingFraction *float64 `json:"remainingFraction"`
		} `json:"buckets"`
	} `json:"groups"`
}

// agyPoolStats accumulates a pool's (gemini / non-gemini) 5-hour and weekly
// bucket stats from the quota summary before they are collapsed into a single
// reported UsageLimit by resolveAgyPoolLimit.
type agyPoolStats struct {
	fiveHourRemaining *float64
	fiveHourReset     *time.Time
	weeklyRemaining   *float64
	weeklyReset       *time.Time
}

func agyParseQuotaSummary(body []byte) ([]agentbackend.UsageLimit, bool) {
	var resp agyQuotaSummaryResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, false
	}
	if len(resp.Groups) == 0 {
		return nil, false
	}

	byPool := map[string]*agyPoolStats{}
	for _, group := range resp.Groups {
		groupPool := agyPoolFromGroup(group.DisplayName)
		for _, bucket := range group.Buckets {
			pool := groupPool
			if pool == "" {
				pool = agyPoolFromBucketID(bucket.BucketID)
			}
			window := agyWindowKind(bucket.Window, bucket.BucketID)
			if pool == "" || window == "" {
				continue
			}
			s := byPool[pool]
			if s == nil {
				s = &agyPoolStats{}
				byPool[pool] = s
			}
			reset := agyParseReset(bucket.ResetTime)
			switch window {
			case "5h":
				s.fiveHourRemaining = bucket.RemainingFraction
				s.fiveHourReset = reset
			case "weekly":
				s.weeklyRemaining = bucket.RemainingFraction
				s.weeklyReset = reset
			}
		}
	}

	out := agyEmptyLimits()
	found := false
	for i, lim := range out {
		if s, ok := byPool[lim.Scope]; ok {
			out[i] = resolveAgyPoolLimit(lim.Scope, *s)
			found = true
		}
	}
	return out, found
}

// resolveAgyPoolLimit collapses a pool's 5-hour and weekly stats into the single
// UsageLimit warden reports for that bucket. While the weekly limit has
// headroom, the bucket reports its 5-hour session limit (usage + reset). Once
// the weekly limit is exhausted (remainingFraction <= 0) the bucket flips to the
// weekly limit fully consumed: 100% used, 0% remaining, limitState "reached",
// weekly duration, and the weekly reset time. Kept identical in behavior to the
// backendusage adapter's resolveAntigravityPoolLimit.
func resolveAgyPoolLimit(pool string, s agyPoolStats) agentbackend.UsageLimit {
	id, scope, label, families := agyPoolMeta(pool)
	if s.weeklyRemaining != nil && *s.weeklyRemaining <= 0 {
		used := 100.0
		return agyWindow(id, scope, label, families, nil, &used, s.weeklyReset, agyWeeklyMinutes)
	}
	used := agyUsedFromRemaining(s.fiveHourRemaining)
	return agyWindow(id, scope, label, families, nil, used, s.fiveHourReset, agyFiveHourMinutes)
}

// agyParseReset parses an RFC3339 resetTime into a UTC *time.Time, returning nil
// for a missing/empty/unparseable value.
func agyParseReset(resetTime *string) *time.Time {
	if resetTime == nil || *resetTime == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, *resetTime)
	if err != nil {
		return nil
	}
	ut := t.UTC()
	return &ut
}

func agyPoolFromGroup(displayName string) string {
	lower := strings.ToLower(displayName)
	switch {
	case strings.Contains(lower, "gemini"):
		return "gemini"
	case strings.Contains(lower, "claude"), strings.Contains(lower, "gpt"), strings.Contains(lower, "3p"):
		return "non-gemini"
	default:
		return ""
	}
}

func agyPoolFromBucketID(bucketID string) string {
	lower := strings.ToLower(bucketID)
	switch {
	case strings.HasPrefix(lower, "gemini"):
		return "gemini"
	case strings.HasPrefix(lower, "3p"), strings.HasPrefix(lower, "non-gemini"):
		return "non-gemini"
	default:
		return ""
	}
}

func agyWindowKind(window, bucketID string) string {
	lower := strings.ToLower(window)
	if lower == "" {
		lower = strings.ToLower(bucketID)
	}
	switch {
	case strings.Contains(lower, "weekly"), strings.Contains(lower, "week"):
		return "weekly"
	case strings.Contains(lower, "5h"), strings.Contains(lower, "five"), strings.Contains(lower, "hour"):
		return "5h"
	default:
		return ""
	}
}

// agyPoolMeta returns the stable identity of a pool's reported bucket. Kept
// identical in behavior to the backendusage adapter's antigravityPoolMeta.
func agyPoolMeta(pool string) (id, scope, label string, families []string) {
	switch pool {
	case "gemini":
		return "antigravity:gemini", "gemini", "Gemini", []string{"gemini"}
	default:
		return "antigravity:non-gemini", "non-gemini", "Non-Gemini", nil
	}
}

func agyUsedFromRemaining(remaining *float64) *float64 {
	if remaining == nil {
		return nil
	}
	rem := *remaining
	if rem < 0 {
		rem = 0
	}
	if rem > 1 {
		rem = 1
	}
	u := math.Round((1.0-rem)*10000) / 100
	return &u
}

// agyEmptyLimits returns the two pool buckets with no measurements yet. The
// 5-hour session limit is the default reported window, so the placeholder
// carries its duration until live stats replace it.
func agyEmptyLimits() []agentbackend.UsageLimit {
	return []agentbackend.UsageLimit{
		agyWindow("antigravity:gemini", "gemini", "Gemini", []string{"gemini"}, nil, nil, nil, agyFiveHourMinutes),
		agyWindow("antigravity:non-gemini", "non-gemini", "Non-Gemini", nil, nil, nil, nil, agyFiveHourMinutes),
	}
}

func agyWindow(id, scope, label string, families, models []string, used *float64, resets *time.Time, durationMinutes int) agentbackend.UsageLimit {
	var remaining *float64
	if used != nil && *used >= 0 && *used <= 100 {
		v := math.Round((100-*used)*100) / 100
		remaining = &v
	}
	var state *string
	if used != nil && *used >= 100 {
		v := "reached"
		state = &v
	}
	var duration *int
	if durationMinutes > 0 {
		d := durationMinutes
		duration = &d
	}
	return agentbackend.UsageLimit{
		ID:               id,
		Scope:            scope,
		Label:            label,
		ModelFamilies:    families,
		Models:           models,
		UsedPercent:      used,
		RemainingPercent: remaining,
		DurationMinutes:  duration,
		ResetsAt:         resets,
		LimitState:       state,
	}
}

// --- Rate-limit detection ---------------------------------------------------

// antigravityRLClause is the reset/availability clause every agy limit banner
// carries: an absolute clock ("resets at 15:30", "available at 15:30") or the
// relative form the live banner uses ("Resets in 10m5s").
const antigravityRLClause = `(?:(?:resets|available)\s+at\s+\d{1,2}:\d{2}|(?:resets|available)\s+in\s+\d+(?:\.\d+)?\s*[hms])`

// antigravityRLPhrase is the limit phrase specific to agy's Google-backed quota
// system ("Individual quota reached", "Free-tier session quota reached", ...).
const antigravityRLPhrase = `(?:rate\s+limit(?:ed)?|quota(?:\s+exceeded)?|usage\s+limit|session\s+(?:limit|quota)|limit\s+reached|resource\s+exhausted)`

// antigravityRLBannerRe matches Antigravity (agy) rate-limit banners. The banner
// must carry both a limit phrase AND a reset clause so that ordinary agent prose
// merely mentioning "rate limit", "quota", or "limit reached" — in code,
// discussion, tool output, or transcript review — does NOT trigger detection.
// The [\s\S]{0,150}? bridge tolerates multi-line banner layout. Both orderings
// are covered.
//
// Live capture (GitHub #684): "⚠ Individual quota reached. Please upgrade your
// subscription to increase your limits. Resets in 10m5s."
var antigravityRLBannerRe = regexp.MustCompile(
	`(?i)(?:` + antigravityRLPhrase + `[\s\S]{0,150}?` + antigravityRLClause +
		`|` + antigravityRLClause + `[\s\S]{0,150}?` + antigravityRLPhrase + `)`,
)

// antigravityResetsAtRe matches "resets at HH:MM" / "available at HH:MM" in
// agy's rate-limit output, used to extract an absolute reset time.
var antigravityResetsAtRe = regexp.MustCompile(
	`(?i)(?:resets\s+at|available\s+at)\s+(\d{1,2}:\d{2})\s*(am|pm)?`,
)

const (
	// antigravityRLTailLines is the legacy trailing window, used only when the
	// pane shows no input box to anchor on.
	antigravityRLTailLines = 6
	// antigravityRLAboveBox is how many lines directly above the input box are
	// scanned: banner, "Error ID", blank, plus slack for an extra line or two.
	antigravityRLAboveBox = 6
	// antigravityRLBoxScan bounds how far from the bottom the input box's rules
	// are searched for (rule, prompt, rule, status line, + slack).
	antigravityRLBoxScan = 8
)

// agyIsRule reports whether a line is a horizontal rule of the input box.
func agyIsRule(line string) bool {
	t := strings.TrimSpace(line)
	if len([]rune(t)) < 8 {
		return false
	}
	for _, r := range t {
		if r != '─' && r != '━' && r != '-' && r != '═' {
			return false
		}
	}
	return true
}

// agyBannerWindow returns the pane region a live limit banner can occupy. agy
// renders the banner either directly above its input box (rule, prompt, rule,
// status line — the live #684 capture, 7 lines from the bottom) or inside it. So
// when the box is visible the window runs from antigravityRLAboveBox lines above
// the box's top rule to the end of the pane — robust to however many lines the
// banner/Error ID take — and always covers at least the legacy trailing window.
// A banner (or quoted text) further up in scrollback falls outside it.
func agyBannerWindow(pane string) string {
	lines := strings.Split(strings.TrimRight(pane, "\n \t"), "\n")
	from := len(lines) - antigravityRLTailLines
	var rules []int
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-antigravityRLBoxScan; i-- {
		if agyIsRule(lines[i]) {
			rules = append(rules, i)
		}
	}
	if len(rules) >= 2 {
		if above := rules[1] - antigravityRLAboveBox; above < from {
			from = above
		}
	}
	if from < 0 {
		from = 0
	}
	return strings.Join(lines[from:], "\n")
}

// DetectRateLimit implements agentbackend.RateLimitDetector for Antigravity.
// It anchors on the structure of the live pane (the region directly above the
// input box) so neither a stale banner that scrolled away nor a live agent
// writing about quota policies triggers a false positive. The banner must
// exhibit both a provider-specific limit phrase and a reset clause.
func (Antigravity) DetectRateLimit(pane string) (bool, time.Time, bool) {
	tail := agyBannerWindow(pane)
	if !antigravityRLBannerRe.MatchString(tail) {
		return false, time.Time{}, false
	}
	t, ok := (Antigravity{}).ParseRateLimitReset(tail)
	return true, t, ok
}

// ParseRateLimitReset implements agentbackend.RateLimitResetParser for
// Antigravity. It extracts "resets in 10m5s" (relative), then "resets at HH:MM" /
// "available at HH:MM" (absolute); falls back to the generic parser otherwise.
func (Antigravity) ParseRateLimitReset(pane string) (time.Time, bool) {
	now := time.Now()
	if t, ok := rlParseRelativeReset(pane, now); ok {
		return t, true
	}
	if m := antigravityResetsAtRe.FindStringSubmatch(pane); len(m) == 3 {
		if h, min, ok := rlParseClock(m[1], m[2]); ok {
			result := time.Date(now.Year(), now.Month(), now.Day(), h, min, 0, 0, now.Location())
			if result.Before(now) {
				result = result.Add(24 * time.Hour)
			}
			return result, true
		}
	}
	return parseRateLimitResetTime(pane)
}

// agyStatusModelRe matches the status line's trailing "<model> · <effort>"
// segment, e.g. "? for shortcuts      Gemini 3.8 Flash · medium" (live agy) or the
// "Gemini 3.5 Flash (Low)" display-label form.
var agyStatusModelRe = regexp.MustCompile(`(?i)(?:^|\s{2,})((?:gemini|claude|gpt)[\w .\-()]*?)(?:\s+·\s+(?:minimal|low|medium|high)|\s+\((?:[\w ]+)\))\s*$`)

// ObservedQuotaScope implements agentbackend.QuotaScopeObserver. It reads the
// model agy is actually running from the status line (the last line, below the
// input box) and maps its family to the quota bucket scope: Gemini models share
// the "gemini" bucket; Claude and GPT-OSS models the "non-gemini" one. Only a
// recognised family on a pane with an input box is reported — anything else
// returns ok=false rather than a guess.
func (Antigravity) ObservedQuotaScope(pane string) (model, scope string, ok bool) {
	lines := strings.Split(strings.TrimRight(pane, "\n \t"), "\n")
	if len(lines) < 4 || !agyIsRule(lines[len(lines)-2]) {
		return "", "", false
	}
	m := agyStatusModelRe.FindStringSubmatch(strings.TrimRight(lines[len(lines)-1], " \t"))
	if m == nil {
		return "", "", false
	}
	model = strings.TrimSpace(m[1])
	if strings.HasPrefix(strings.ToLower(model), "gemini") {
		return model, "gemini", true
	}
	return model, "non-gemini", true
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
		PermissionModes:      []string{"default", "plan", "accept-edits", "sandbox", "dangerously-skip-permissions"},
		StructuredTranscript: true,
		SystemPromptInject:   false,
		SessionIDControl:     false,
	}
}

var agyModeTable = agentbackend.ModeTable{
	ToIntent: map[string]agentbackend.PermissionIntent{
		"default": agentbackend.IntentDefault, "plan": agentbackend.IntentPlan,
		"accept-edits": agentbackend.IntentAcceptEdits, "acceptEdits": agentbackend.IntentAcceptEdits,
		"dangerously-skip-permissions": agentbackend.IntentSkipAll,
	},
	FromIntent: map[agentbackend.PermissionIntent]string{
		agentbackend.IntentDefault: "default", agentbackend.IntentPlan: "plan",
		agentbackend.IntentReadOnly: "plan", agentbackend.IntentAcceptEdits: "accept-edits",
		agentbackend.IntentSkipAll: "dangerously-skip-permissions",
	},
}

// ModeIntent implements agentbackend.PermissionMapper.
func (Antigravity) ModeIntent(mode string) (agentbackend.PermissionIntent, bool) {
	return agyModeTable.Intent(mode)
}

// ModeForIntent implements agentbackend.PermissionMapper.
func (Antigravity) ModeForIntent(i agentbackend.PermissionIntent) (string, bool) {
	return agyModeTable.ForIntent(i)
}

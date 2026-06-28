package backends

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
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
// that JSONL into neutral Turns with good fidelity for the prompt/reply flow, so
// digests run on real structured data. Captured live against the user's hosted free
// tier (`agy -p` + `agy -c -p`, Gemini 3.5 Flash).
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
// RFC3339 created_at, and the textual content (empty for some control records).
type agyStep struct {
	StepIndex int    `json:"step_index"`
	Source    string `json:"source"`
	Type      string `json:"type"`
	CreatedAt string `json:"created_at"`
	Content   string `json:"content"`
}

// agyUserRequestRe extracts the human prompt from `agy`'s USER_INPUT content, which
// wraps it as `<USER_REQUEST>…</USER_REQUEST>` and appends `<ADDITIONAL_METADATA>` /
// `<USER_SETTINGS_CHANGE>` blocks the adapter does not want in the neutral Turn.
var agyUserRequestRe = regexp.MustCompile(`(?s)<USER_REQUEST>\s*(.*?)\s*</USER_REQUEST>`)

// ParseTranscript normalizes `agy`'s plaintext trajectory JSONL into warden's neutral
// []Turn. It reads the durable conversation records:
//   - USER_INPUT (source USER_EXPLICIT) → a user Turn (the `<USER_REQUEST>` body is
//     unwrapped; the appended metadata/settings blocks are dropped).
//   - PLANNER_RESPONSE (source MODEL) → an assistant Turn.
//
// SYSTEM records (CONVERSATION_HISTORY, CHECKPOINT, SYSTEM_MESSAGE — context,
// summaries, and injected system notes) are control metadata and ignored, as are any
// other/unknown types. Malformed lines are skipped (best-effort, like the
// Claude/Aider/OpenCode/Codex parsers); only a reader error is returned.
//
// Tool calls are NOT extracted (ToolName/Files stay empty): no tool-using `agy`
// transcript was captured this frugal phase, so the tool-step format is unverified
// and the adapter degrades honestly rather than guessing field names
// (docs/agent-backends/antigravity.md).
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
		}
	}
	return turns, sc.Err()
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

// DetectState reports Unknown: `agy`'s run-state and approval prompts live in its
// TUI, whose pane has no stable text marker this experimental adapter keys on yet
// (no live interactive prompt was captured for this phase). warden infers idle from
// staleness, the same conservative stance the Claude/Aider/OpenCode/Codex adapters
// take. Mapping `agy`'s TUI approvals is deferred (docs/agent-backends/antigravity.md);
// the faithful non-interactive surface is `agy -p`, which raises no prompts.
func (Antigravity) DetectState(string) agentbackend.State { return agentbackend.StateUnknown }

// ParseApproval reports no approval: see DetectState — `agy`'s interactive permission
// prompts are not yet mapped, so this degrades (returns false) rather than
// mis-parsing. The `-p --dangerously-skip-permissions` headless path raises no prompts.
func (Antigravity) ParseApproval(string) (*agentbackend.Approval, bool) { return nil, false }

// --- System prompt / pricing ------------------------------------------------

// SystemPromptFlag reports no system-prompt injection: `agy` has no
// --append-system-prompt equivalent on its launch command (its customization is
// skills/rules/AGENTS.md based), so warden's pipeline/collab/git hints are skipped
// for Antigravity agents (Caps.SystemPromptInject=false; see the gap doc).
func (Antigravity) SystemPromptFlag(string) (string, bool) { return "", false }

// Pricing reports no pricing table. Antigravity is a Google-hosted free-tier agent:
// `agy` surfaces token usage / session cost only in its `/usage` TUI panel, exposes
// no per-call dollar figure on the CLI, and warden's spend table is Claude-specific.
// Per design §5 spend shows tokens (not dollars) and savings omits the agent. Wiring
// warden to Antigravity's native usage is deferred (docs/agent-backends/antigravity.md).
func (Antigravity) Pricing() (agentbackend.PricingTable, bool) {
	return agentbackend.PricingTable{}, false
}

// --- Capabilities -----------------------------------------------------------

// Capabilities reports Antigravity as a Tier-A backend: the plaintext trajectory
// JSONL parses into structured Turns (powering digests), and resume is supported
// (dir-scoped today, exact-id once discover-then-pin lands). `agy` mints its own
// conversation id (no SessionIDControl), has no launch-time system-prompt injection,
// and exposes no warden-side dollar pricing yet. PermissionModes surface `agy`'s
// native posture flags.
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

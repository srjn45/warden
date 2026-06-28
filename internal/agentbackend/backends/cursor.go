package backends

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
)

func init() { agentbackend.Register(Cursor{}) }

// Cursor is the **experimental** Backend adapter for Cursor's CLI agent (the
// `cursor-agent` binary). It is breadth-first work (#52): a thin, honest adapter
// that opens cursor-agent in a warden-managed tmux session and documents the gaps
// rather than pretending Cursor is a drop-in for Claude Code. warden adds capability
// *on top of* Cursor; it never strips Cursor's features to a lowest common
// denominator. The full gap analysis lives in docs/agent-backends/cursor.md.
//
// Tier decision: Cursor is shipped as **Tier C** for transcripts (degraded).
// Cursor's interactive launch (the path warden uses) persists each session to an
// **undocumented per-chat SQLite `store.db`** under
// `~/.cursor/chats/<md5(workspacePath)>/<chatId>/store.db` — there is no `export`
// command, no `sqlite3` on the rig, and warden carries no SQLite dependency, so
// that store cannot be sourced minimally. The richest *structured* surface is the
// headless `--print --output-format stream-json` NDJSON event stream; this adapter
// implements a real, tested parser for it (ParseTranscript, exercised against a
// captured fixture), but `StructuredTranscript` is **off** and `TranscriptPath`
// returns false because the interactive TUI never writes that NDJSON to disk — the
// parser is forward-compat for a future store.db reader or a headless-capture path
// (the chatId is observable in the stream's `system`/init event and as the on-disk
// chat dir name; discover-then-pin is deferred, #52).
//
// Session-id handling: Cursor mints its own UUID chatId at launch and exposes no
// flag to assign one up front (Caps.SessionIDControl=false). The minted id is a bare
// UUID — indistinguishable in shape from warden's own placeholder — so this adapter
// does not branch on it: resume is **dir/workspace-scoped** via `--continue`, which
// Cursor scopes to the current workspace (verified: it resumed the same chatId and
// recalled prior context). Exact-id resume (`--resume <chatId>`) becomes available
// once discover-then-pin captures the minted id (#52).
//
// ⚠️ Double-worktree hazard: cursor-agent has its OWN `-w/--worktree` feature that
// creates a git worktree under `~/.cursor/worktrees/`. warden already manages the
// git worktree, so this adapter NEVER passes `-w`; it launches cursor-agent directly
// in warden's worktree dir (the tmux pane is already cd'd there). See the gap doc.
type Cursor struct{}

// --- Identity ---------------------------------------------------------------

func (Cursor) ID() string          { return "cursor" }
func (Cursor) DisplayName() string { return "Cursor" }
func (Cursor) Binary() string      { return "cursor-agent" }
func (Cursor) InstallHint() string {
	return "Install Cursor CLI: curl https://cursor.com/install -fsS | bash\nThen authenticate: cursor-agent login (https://docs.cursor.com/cli)"
}

// --- Launch / resume --------------------------------------------------------

// cursorModeFlag maps a warden permission mode onto Cursor's native execution /
// approval vocabulary. Cursor exposes distinct surfaces that warden surfaces
// honestly rather than collapsing: `--mode plan` (read-only planning),
// `--mode ask` (read-only Q&A), `--auto-review` (a server classifier auto-runs
// safe tool calls and prompts for the rest), and `-f/--force/--yolo` (run
// everything). warden's richer Claude-flavored "just do it" aliases fold onto
// `-f`; "default"/"" returns "" so Cursor keeps its own interactive approval
// posture (warden adds on top, never strips it down).
func cursorModeFlag(mode string) string {
	switch mode {
	case "plan":
		return " --mode plan"
	case "ask":
		return " --mode ask"
	case "auto-review":
		return " --auto-review"
	case "force", "yolo", "dangerously-skip-permissions", "bypassPermissions", "yes-always", "auto", "acceptEdits", "dontAsk":
		return " -f"
	default:
		return ""
	}
}

// LaunchCmd builds the interactive `cursor-agent` (TUI) invocation for a tmux pane.
// Model is shaped as Cursor's `--model` and omitted when empty so Cursor's own
// configured default applies (Cursor is hosted with its own model catalog —
// composer-2.5-fast by default — and the Claude default alias never resolves here,
// same call as Codex/OpenCode). The permission mode maps via cursorModeFlag.
// SessionID and Name are ignored: Cursor mints its own UUID chatId
// (SessionIDControl=false) and the TUI has no session-name flag. The pane is already
// cd'd into the agent's worktree, so neither `--workspace` nor (critically) Cursor's
// own `-w/--worktree` is passed — warden owns the worktree.
func (Cursor) LaunchCmd(o agentbackend.LaunchOpts) string {
	cmd := "cursor-agent"
	if o.Model != "" {
		cmd += " --model " + shellQuoteArg(o.Model)
	}
	cmd += cursorModeFlag(o.Mode)
	return cmd
}

// ResumeCmd builds the interactive resume invocation, run in the agent's worktree.
// warden cannot pin Cursor's UUID chatId (SessionIDControl=false) and that id is the
// same shape as warden's own placeholder, so this adapter does not branch on the id:
// it uses `cursor-agent --continue`, "continue the previous session", which Cursor
// scopes to the current workspace (verified dir-scoped). For a per-worktree warden
// agent that deterministically continues that agent's own session. ok is always true
// (Caps.Resume=true). Exact-id resume (`--resume <chatId>`) lands with
// discover-then-pin (#52).
func (Cursor) ResumeCmd(o agentbackend.ResumeOpts) (string, bool) {
	cmd := "cursor-agent --continue"
	if o.Model != "" {
		cmd += " --model " + shellQuoteArg(o.Model)
	}
	return cmd, true
}

// LaunchPromptArg seeds the initial task prompt as Cursor's trailing positional
// argument (read back from promptFile via "$(cat …)" so a multi-line prompt types as
// one physical line). cursor-agent accepts an optional `[prompt...]` positional and
// then stays interactive — a persistent agent loop, like Claude's/Codex's trailing
// positional prompt rather than Aider's run-once-and-exit --message.
func (Cursor) LaunchPromptArg(promptFile string) string {
	return ` "$(cat ` + shellQuoteArg(promptFile) + `)"`
}

// HeadlessCmd returns the argv for a headless one-shot used by warden's own
// classify/summarize offload when Cursor is the default backend. It runs a single
// `-p` (print) turn with `--force` (so it never blocks on an approval) and `--trust`
// (so it never blocks on Cursor's workspace-trust prompt in a fresh worktree).
// (warden's default backend is Claude, so this path is rarely exercised for Cursor;
// it exists to honor Caps.Headless=true.)
func (Cursor) HeadlessCmd(prompt string) ([]string, bool) {
	return []string{"cursor-agent", "-p", "--force", "--trust", prompt}, true
}

// --- Transcript -------------------------------------------------------------

// TranscriptPath reports no on-disk transcript (degrade, Tier C). Cursor's
// interactive sessions — the path warden launches — persist only to an undocumented
// per-chat SQLite `store.db` (`~/.cursor/chats/<md5(workspacePath)>/<chatId>/store.db`)
// with no `export` command and no SQLite dependency in warden to read it, so there is
// nothing this minimal adapter can point ParseTranscript at. ok=false ⇒ the digest
// path degrades to "no transcript" rather than erroring (same contract as the other
// adapters on a miss). The forward path (a store.db reader, or capturing the headless
// stream-json the parser below already handles) is documented in
// docs/agent-backends/cursor.md.
func (Cursor) TranscriptPath(_, _, _ string) (string, bool) { return "", false }

// cursorEvent is one record of Cursor's `--print --output-format stream-json` NDJSON
// stream. Each line carries a top-level `type` (system | user | assistant | tool_call
// | result); message events carry `message`, tool events carry `tool_call` + a
// `subtype` (started | completed) and `timestamp_ms`.
type cursorEvent struct {
	Type        string          `json:"type"`
	Subtype     string          `json:"subtype"`
	Message     *cursorMsg      `json:"message"`
	ToolCall    json.RawMessage `json:"tool_call"`
	TimestampMs int64           `json:"timestamp_ms"`
}

// cursorMsg is a user/assistant message: a role plus ordered content parts (each a
// {type,text}); warden joins the "text" parts.
type cursorMsg struct {
	Role    string `json:"role"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

// ParseTranscript normalizes Cursor's stream-json NDJSON into warden's neutral
// []Turn. NOTE: this is **not wired into the live digest path today** —
// StructuredTranscript is off and TranscriptPath returns false because Cursor's
// interactive launch writes only the SQLite store.db, not this NDJSON (see the type
// doc). The parser is kept real and tested (against a captured fixture) so a future
// store.db reader or headless-capture path can switch the backend to Tier A with no
// further parsing work. It reads:
//   - user / assistant message events → role-keyed Turns (text joined from parts).
//   - tool_call/"completed" events → the tool name (derived from the `<name>ToolCall`
//     key) and any touched file (args.path) fold onto the preceding assistant Turn,
//     or start a new one when the model went straight to a tool.
//
// system / result records (init metadata and the final usage summary) are ignored.
// Malformed lines are skipped (best-effort, like the Claude/Codex parsers); only a
// reader error is returned.
func (Cursor) ParseTranscript(r io.Reader) ([]agentbackend.Turn, error) {
	var turns []agentbackend.Turn
	sc := bufio.NewScanner(r)
	// stream-json lines (esp. an edit tool's full-file content) can be very long;
	// raise the scanner cap well above the 64K default (same bound as Claude/Codex).
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev cursorEvent
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		switch ev.Type {
		case "user":
			if ev.Message != nil {
				if text := cursorMsgText(ev.Message); strings.TrimSpace(text) != "" {
					turns = append(turns, agentbackend.Turn{Role: "user", Text: text})
				}
			}
		case "assistant":
			if ev.Message != nil {
				if text := cursorMsgText(ev.Message); strings.TrimSpace(text) != "" {
					turns = append(turns, agentbackend.Turn{Role: "assistant", Text: text})
				}
			}
		case "tool_call":
			// Only "completed" calls are counted, so a started/completed pair is not
			// double-recorded.
			if ev.Subtype != "completed" {
				continue
			}
			name, files := cursorToolNameAndFiles(ev.ToolCall)
			if name == "" {
				continue
			}
			var ts time.Time
			if ev.TimestampMs > 0 {
				ts = time.UnixMilli(ev.TimestampMs)
			}
			if n := len(turns); n > 0 && turns[n-1].Role == "assistant" && turns[n-1].ToolName == "" {
				turns[n-1].ToolName = name
				for _, f := range files {
					turns[n-1].Files = appendUnique(turns[n-1].Files, f)
				}
				if turns[n-1].Timestamp.IsZero() {
					turns[n-1].Timestamp = ts
				}
			} else {
				turns = append(turns, agentbackend.Turn{Role: "assistant", ToolName: name, Files: files, Timestamp: ts})
			}
		}
	}
	return turns, sc.Err()
}

// cursorMsgText joins the text of every "text" content part in a message.
func cursorMsgText(m *cursorMsg) string {
	var b strings.Builder
	for _, c := range m.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// cursorToolNameAndFiles derives a tool name + touched files from a stream-json
// `tool_call` object. Cursor keys the call by a single `<name>ToolCall` field (e.g.
// `editToolCall`, `shellToolCall`); the name is that key with the "ToolCall" suffix
// trimmed, and a file (when the tool touches one) is read from the nested
// `.args.path` (variants: filePath / relativePath). Returns "" when no tool field is
// present.
func cursorToolNameAndFiles(raw json.RawMessage) (string, []string) {
	if len(raw) == 0 {
		return "", nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return "", nil
	}
	for k, v := range m {
		if k == "toolCallId" || !strings.HasSuffix(k, "ToolCall") {
			continue
		}
		name := strings.TrimSuffix(k, "ToolCall")
		var inner struct {
			Args struct {
				Path         string `json:"path"`
				FilePath     string `json:"filePath"`
				RelativePath string `json:"relativePath"`
			} `json:"args"`
		}
		var files []string
		if json.Unmarshal(v, &inner) == nil {
			if f := firstNonEmpty(inner.Args.Path, inner.Args.FilePath, inner.Args.RelativePath); f != "" {
				files = appendUnique(files, f)
			}
		}
		return name, files
	}
	return "", nil
}

// --- State / approval -------------------------------------------------------

// DetectState reports Unknown: Cursor's run-state and approval prompts (including its
// workspace-trust prompt and the interactive approval UI) live in its TUI, whose pane
// has no stable text marker this experimental adapter keys on yet. warden infers idle
// from staleness, the same conservative stance the Claude/Codex/OpenCode adapters
// take. Mapping Cursor's TUI approvals is deferred (docs/agent-backends/cursor.md);
// the faithful non-interactive surface is `-p --force`, which raises no prompts.
func (Cursor) DetectState(string) agentbackend.State { return agentbackend.StateUnknown }

// ParseApproval reports no approval: see DetectState — Cursor's interactive
// permission prompts are not yet mapped, so this degrades (returns false) rather than
// mis-parsing. The `-p --force --trust` headless path raises no prompts.
func (Cursor) ParseApproval(string) (*agentbackend.Approval, bool) { return nil, false }

// --- System prompt / pricing ------------------------------------------------

// SystemPromptFlag reports no system-prompt injection: cursor-agent has no
// --append-system-prompt equivalent on its launch command (its customization is
// rules / AGENTS.md based), so warden's pipeline/collab/git hints are skipped for
// Cursor agents (Caps.SystemPromptInject=false; see the gap doc).
func (Cursor) SystemPromptFlag(string) (string, bool) { return "", false }

// Pricing reports no pricing table. Cursor is a hosted plan: it surfaces token counts
// (the stream-json `result.usage` carries input/output/cache tokens) but never a
// per-call dollar figure — billing is against the user's Cursor subscription — so
// warden cannot enumerate per-model dollar rates here. Per design §5 spend shows
// tokens (not dollars) and savings omits the agent. Wiring warden to Cursor's native
// token usage is deferred (docs/agent-backends/cursor.md).
func (Cursor) Pricing() (agentbackend.PricingTable, bool) {
	return agentbackend.PricingTable{}, false
}

// --- Capabilities -----------------------------------------------------------

// Capabilities reports Cursor as an experimental **Tier-C** backend: resume works
// (dir/workspace-scoped `--continue`; exact-id once discover-then-pin lands) and
// there is a headless one-shot, but the interactive transcript is an unreadable
// SQLite store so StructuredTranscript is off (digests degrade). Cursor mints its own
// chatId (no SessionIDControl), has no launch-time system-prompt injection, and
// exposes no warden-side dollar pricing (hosted plan). PermissionModes surface
// Cursor's native execution/approval vocabulary.
func (Cursor) Capabilities() agentbackend.Caps {
	return agentbackend.Caps{
		Resume:               true,
		Headless:             true,
		ModelSelection:       true,
		PermissionModes:      []string{"default", "plan", "ask", "auto-review", "force"},
		StructuredTranscript: false,
		SystemPromptInject:   false,
		SessionIDControl:     false,
	}
}

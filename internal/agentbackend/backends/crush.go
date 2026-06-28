package backends

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
)

func init() { agentbackend.Register(Crush{}) }

// Crush is the Backend adapter for Crush (Charm's `crush` CLI) — warden's second
// SQLite-backed backend (after OpenCode), shipped as **experimental** (#52,
// breadth-first). Crush is a glamorous terminal-first, multi-provider AI coding
// agent with LSP and MCP support; warden launches it and adds its
// orchestration/digest layer ON TOP, preserving Crush's own features.
//
// Tier decision: Crush is shipped as **Tier A** for transcripts. Its session
// store is a per-project SQLite db (.crush/crush.db), and Crush exposes a clean,
// agent-oriented read path — `crush session show <id> --json` emits the whole
// session as `{meta, messages[{role, parts[]}]}` JSON. This adapter sources the
// transcript through that command (exactly the OpenCode pattern: query the agent,
// don't read the DB schema) and parses it into neutral Turns with good fidelity.
//
// Session-id handling: Crush mints its own 16-hex session id (e.g.
// 9c76cb01dc3e5252); warden cannot assign it up front (Caps.SessionIDControl=
// false). The adapter is **dir-scoped**, like Aider/OpenCode: Crush's session
// store lives in the agent's own working directory (.crush/crush.db), so
// `crush session list --json` run there is inherently scoped to that worktree —
// no global-list directory filter is needed (simpler than OpenCode). `crush
// --continue` resumes the most recent session for that directory, and the
// transcript is the newest row from the cwd-scoped list. When a real 16-hex id
// is pinned (future discover-then-pin, FUTURE_ENHANCEMENTS #52), this adapter
// automatically prefers exact-id `session show <id>` / `--session <id>` resume.
//
// Known gaps (documented in docs/agent-backends/crush.md): Crush's interactive
// TUI takes neither a `-m` model flag nor an initial positional prompt (only the
// headless `crush run` does), so warden cannot seed the first task into the TUI
// from the launch line (LaunchPromptArg returns "") and the TUI model is
// config-driven (LaunchCmd omits it). Its permission prompts live in the Bubble
// Tea TUI with no captured pane marker, so DetectState/ParseApproval degrade.
type Crush struct{}

// --- Identity ---------------------------------------------------------------

func (Crush) ID() string          { return "crush" }
func (Crush) DisplayName() string { return "Crush" }
func (Crush) Binary() string      { return "crush" }
func (Crush) InstallHint() string {
	return "Install Crush: go install github.com/charmbracelet/crush@latest\nOr: npm install -g @charmland/crush (https://github.com/charmbracelet/crush)"
}

// --- Launch / resume --------------------------------------------------------

// crushYolo reports whether a warden permission mode should map to Crush's
// --yolo (auto-accept all permissions) flag. Crush's TUI has two effective modes
// — interactive prompting (default) and yolo — so warden's richer Claude modes
// are folded onto them, mirroring Aider/OpenCode: any "just do it" mode becomes
// --yolo; the cautious modes stay interactive.
func crushYolo(mode string) bool {
	switch mode {
	case "yolo", "yes-always", "auto", "acceptEdits", "bypassPermissions", "dangerously-skip-permissions", "dontAsk":
		return true
	default:
		return false
	}
}

// LaunchCmd builds the interactive `crush` (TUI) invocation for a tmux pane.
// Crush's TUI takes no `-m` model flag (only the headless `crush run` does), so
// Model is intentionally not shaped onto the launch — the model is config-driven
// for the TUI (crush.json models.large/small, or the in-TUI switcher); see the
// gap doc. SessionID and Name are ignored: Crush mints its own session id
// (SessionIDControl=false) and has no session-name flag. The pane is already cd'd
// into the agent's workdir, whose .crush/ store Crush uses automatically.
func (Crush) LaunchCmd(o agentbackend.LaunchOpts) string {
	cmd := "crush"
	if crushYolo(o.Mode) {
		cmd += " --yolo"
	}
	return cmd
}

// ResumeCmd builds the interactive resume invocation, run in the agent's workdir.
// Crush resumes by id when a real 16-hex id is pinned (--session <id>); otherwise
// it falls back to --continue, "continue the most recent session" — which, because
// Crush's session store is per-cwd (.crush/crush.db), deterministically continues
// this worktree's own session. ok is always true (Caps.Resume=true), like OpenCode.
func (Crush) ResumeCmd(o agentbackend.ResumeOpts) (string, bool) {
	cmd := "crush"
	if looksLikeCrushSessionID(o.SessionID) {
		cmd += " --session " + shellQuoteArg(o.SessionID)
	} else {
		cmd += " --continue"
	}
	if crushYolo(o.Mode) {
		cmd += " --yolo"
	}
	return cmd, true
}

// LaunchPromptArg reports no launch-line prompt seeding for Crush. The interactive
// `crush` TUI accepts neither a positional prompt nor a prompt flag (it rejects a
// positional as an unknown command); only the headless `crush run "<prompt>"` takes
// one. warden therefore cannot seed the first task onto the TUI launch line — the
// operator types it after attaching, or uses the headless path (HeadlessCmd). This
// is a documented experimental gap (docs/agent-backends/crush.md); returning ""
// makes lifecycle launch the bare interactive agent (promptArg short-circuits ""),
// exactly as for an interactive agent spawned with no prompt.
func (Crush) LaunchPromptArg(string) string { return "" }

// HeadlessCmd returns the argv for a headless one-shot used by warden's own
// classify/summarize offload when Crush is the default backend. `crush run` is
// non-interactive and raises no permission prompts, so no auto-accept flag is
// needed; --quiet hides the spinner so stdout is the model's answer. (warden's
// default backend is Claude, so this path is rarely exercised for Crush; it
// exists to honor Caps.Headless=true.)
func (Crush) HeadlessCmd(prompt string) ([]string, bool) {
	return []string{"crush", "run", "--quiet", prompt}, true
}

// --- Transcript -------------------------------------------------------------

// crushCmdTimeout bounds each crush subprocess TranscriptPath spawns (session
// list / show). TranscriptPath is called without a caller context, so the bound is
// internal — generous for a local SQLite read, tight enough not to wedge a poll
// tick if the binary hangs. Mirrors OpenCode's ocCmdTimeout.
var crushCmdTimeout = 10 * time.Second

// crushExec runs a crush subcommand in dir and returns its stdout. It is a package
// var so tests can stub the subprocess and exercise TranscriptPath's orchestration
// without the real binary. Mirrors OpenCode's ocExec.
var crushExec = func(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "crush", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	return cmd.Output()
}

// crushSessionIDRe matches a Crush-minted session id: 16 lowercase hex chars,
// no dashes (e.g. 9c76cb01dc3e5252). warden's own pinned placeholder is a UUID
// (dashed, 36 chars), so this cleanly distinguishes a real captured id from the
// placeholder and selects the exact-id vs dir-scoped path.
var crushSessionIDRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

// looksLikeCrushSessionID reports whether s is a Crush-minted session id.
func looksLikeCrushSessionID(s string) bool { return crushSessionIDRe.MatchString(s) }

// TranscriptPath materializes the session transcript to a file and returns its
// path — the "DB query, not file read" variant of the interface (design §5):
// Crush stores transcripts in SQLite (.crush/crush.db), so there is no on-disk
// JSONL/markdown file to point at. It resolves the session id (a pinned 16-hex id,
// else the newest session for workdir via the cwd-scoped `crush session list
// --json`), runs `crush session show <id> --json` to get the clean session JSON,
// writes it to a deterministic temp file keyed by id, and returns that path for the
// caller to open and feed to ParseTranscript. ok=false on any failure (binary
// missing, no session yet, show error), so the digest path degrades to "no
// transcript" rather than erroring — same contract as Aider/OpenCode.
func (Crush) TranscriptPath(_, workdir, sessionID string) (string, bool) {
	id := ""
	switch {
	case looksLikeCrushSessionID(sessionID):
		id = sessionID
	case workdir != "":
		ctx, cancel := context.WithTimeout(context.Background(), crushCmdTimeout)
		out, err := crushExec(ctx, workdir, "session", "list", "--json")
		cancel()
		if err != nil {
			return "", false
		}
		if rid, ok := newestCrushSession(out); ok {
			id = rid
		}
	}
	if id == "" {
		return "", false
	}

	ctx, cancel := context.WithTimeout(context.Background(), crushCmdTimeout)
	out, err := crushExec(ctx, workdir, "session", "show", id, "--json")
	cancel()
	if err != nil || len(out) == 0 {
		return "", false
	}
	p := filepath.Join(os.TempDir(), "warden-crush-"+id+".json")
	if err := os.WriteFile(p, out, 0o600); err != nil {
		return "", false
	}
	return p, true
}

// crushSessionRow is the subset of a `crush session list --json` row we key on:
// the id and its last-modified timestamp. The list is already cwd-scoped (Crush's
// store is per-project), so no directory field is needed for filtering.
type crushSessionRow struct {
	ID       string `json:"id"`
	Modified string `json:"modified"` // RFC3339
}

// newestCrushSession picks the most-recently-modified session id from a `crush
// session list --json` payload. The list is cwd-scoped by Crush itself, so this is
// just "the newest session in this worktree". ok=false when the list is empty.
func newestCrushSession(listJSON []byte) (string, bool) {
	var rows []crushSessionRow
	if err := json.Unmarshal(listJSON, &rows); err != nil {
		return "", false
	}
	var withID []crushSessionRow
	for _, r := range rows {
		if r.ID != "" {
			withID = append(withID, r)
		}
	}
	if len(withID) == 0 {
		return "", false
	}
	sort.SliceStable(withID, func(i, j int) bool {
		return crushParseTime(withID[i].Modified).After(crushParseTime(withID[j].Modified))
	})
	return withID[0].ID, true
}

// crushShow mirrors the shape of `crush session show <id> --json`: a meta header
// (unused by the parser) and the ordered messages, each with its parts.
type crushShow struct {
	Messages []crushMessage `json:"messages"`
}

type crushMessage struct {
	Role    string      `json:"role"` // "user" | "assistant" | "tool"
	Created string      `json:"created"`
	Parts   []crushPart `json:"parts"`
}

// crushPart is one message part. Crush emits several part types; we read "text"
// (message body), "tool_call" (a tool invocation carrying its name and a JSON
// *string* input from which file paths are extracted). "reasoning", "tool_result",
// "finish", "binary", and "image_url" are not surfaced as neutral Turn fields.
type crushPart struct {
	Type  string `json:"type"`
	Text  string `json:"text"`  // on a "text" part
	Name  string `json:"name"`  // tool name, on a "tool_call" part
	Input string `json:"input"` // JSON-encoded tool args (a string), on a "tool_call" part
}

// ParseTranscript normalizes `crush session show --json` into warden's neutral
// []Turn. The show output is a single JSON document (not NDJSON), so it is read
// whole. Each user/assistant message becomes one Turn keyed by role; assistant
// turns carry the first tool name and any edited files (extracted from each
// tool_call's JSON-string input). Standalone "tool" (tool_result) messages are
// not emitted as Turns — the assistant turn already records the tool name/files,
// matching the Claude/OpenCode adapters. A reader/JSON error is returned; an
// empty/odd message is tolerated (best-effort, like the other parsers).
func (Crush) ParseTranscript(r io.Reader) ([]agentbackend.Turn, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var show crushShow
	if err := json.Unmarshal(data, &show); err != nil {
		return nil, err
	}

	var turns []agentbackend.Turn
	for _, m := range show.Messages {
		ts := crushParseTime(m.Created)
		switch m.Role {
		case "user":
			if text := crushConcatText(m.Parts); strings.TrimSpace(text) != "" {
				turns = append(turns, agentbackend.Turn{Role: "user", Text: text, Timestamp: ts})
			}
		case "assistant":
			tool, files := crushToolAndFiles(m.Parts)
			turns = append(turns, agentbackend.Turn{
				Role: "assistant", Text: crushConcatText(m.Parts), ToolName: tool, Files: files, Timestamp: ts,
			})
		}
	}
	return turns, nil
}

// crushConcatText joins the text of every "text" part in a message.
func crushConcatText(parts []crushPart) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// crushToolAndFiles returns the first tool name and the unique files referenced by
// the message's tool_call parts. Each tool_call's `input` is a JSON-encoded string
// of the tool's arguments; the file path is read from the common field names Crush's
// file tools use (file_path / path / filename).
func crushToolAndFiles(parts []crushPart) (tool string, files []string) {
	for _, p := range parts {
		if p.Type != "tool_call" {
			continue
		}
		if tool == "" {
			tool = p.Name
		}
		if f := crushFileFromInput(p.Input); f != "" {
			files = appendUnique(files, f)
		}
	}
	return tool, files
}

// crushToolInput is the subset of a tool_call's JSON-string input we read: the
// target file path under the field names Crush's file tools (view/write/edit/…)
// use.
type crushToolInput struct {
	FilePath string `json:"file_path"`
	Path     string `json:"path"`
	Filename string `json:"filename"`
}

// crushFileFromInput parses a tool_call's JSON-string input and returns the first
// file path it carries, or "" if the input is empty, not JSON, or carries no path.
func crushFileFromInput(input string) string {
	if strings.TrimSpace(input) == "" {
		return ""
	}
	var in crushToolInput
	if err := json.Unmarshal([]byte(input), &in); err != nil {
		return ""
	}
	return firstNonEmpty(in.FilePath, in.Path, in.Filename)
}

// crushParseTime parses Crush's RFC3339 timestamps (e.g. 2026-06-28T11:28:47+02:00).
// A zero time is returned for an empty/unparseable value (best-effort, like the
// other adapters).
func crushParseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// --- State / approval -------------------------------------------------------

// DetectState reports Unknown: Crush's run-state and approval prompts live in its
// Bubble Tea TUI, whose captured pane has no stable text marker this adapter can
// key on (no live interactive prompt was captured for this experimental phase).
// warden infers idle from staleness, the same conservative stance the Claude/
// Aider/OpenCode adapters take. The faithful non-interactive surface is `crush
// run`, which prompts for nothing. Mapping interactive Crush approvals is deferred
// (docs/agent-backends/crush.md).
func (Crush) DetectState(string) agentbackend.State { return agentbackend.StateUnknown }

// ParseApproval reports no approval: see DetectState — Crush's TUI permission
// prompts are not yet mapped, so this degrades (returns false) rather than
// mis-parsing. Headless `crush run` and `--yolo` raise no prompts.
func (Crush) ParseApproval(string) (*agentbackend.Approval, bool) { return nil, false }

// --- System prompt / pricing ------------------------------------------------

// SystemPromptFlag reports no system-prompt injection: Crush has no
// --append-system-prompt equivalent on its launch command (its customization is
// config / CRUSH.md context-file based), so warden's pipeline/collab/git hints are
// skipped for Crush agents (Caps.SystemPromptInject=false).
func (Crush) SystemPromptFlag(string) (string, bool) { return "", false }

// Pricing reports no pricing table. Crush is multi-provider bring-your-own-model
// (local Ollama at $0, or any paid provider), so warden cannot enumerate per-model
// rates, and the per-tick usage reader is Claude-JSONL-specific and does not yet
// read Crush's first-class cost/tokens (which Crush DOES track natively in its
// session meta: cost, prompt_tokens, completion_tokens, total_tokens). Per design
// §5 spend shows tokens (heuristic) but not dollars and savings omits the agent.
// Wiring warden to Crush's native cost is deferred (docs/agent-backends/crush.md).
func (Crush) Pricing() (agentbackend.PricingTable, bool) {
	return agentbackend.PricingTable{}, false
}

// --- Capabilities -----------------------------------------------------------

// Capabilities reports Crush as a Tier-A backend (structured SQLite-sourced
// transcript powers digests) that, like OpenCode, supports resume (dir-scoped
// today via --continue, exact-id once discover-then-pin lands). No assignable
// session id, no launch-time system-prompt injection, no warden-side pricing yet.
// ModelSelection is true — Crush accepts a model flag on the headless `run` path
// and via config — though the interactive TUI launch is config-driven (gap doc).
func (Crush) Capabilities() agentbackend.Caps {
	return agentbackend.Caps{
		Resume:               true,
		Headless:             true,
		ModelSelection:       true,
		PermissionModes:      []string{"default", "yolo"},
		StructuredTranscript: true,
		SystemPromptInject:   false,
		SessionIDControl:     false,
	}
}

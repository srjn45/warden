// Package backends holds the per-agent Backend adapters. Each adapter registers
// itself with the agentbackend registry from init(), so importing this package
// (directly or for its side effects) makes its backends resolvable.
//
// claude.go is the reference adapter: a mechanical extraction of the Claude Code
// command/transcript logic that previously lived inline in internal/lifecycle.
// The command-string builders (LaunchCmd / ResumeCmd / HeadlessCmd) and the
// transcript locator (TranscriptPath) are byte-for-byte equivalent to the old
// lifecycle code so the string typed into tmux is unchanged (Phase 0 exit gate).
package backends

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/spend"
)

func init() { agentbackend.Register(Claude{}) }

// Claude is the Backend adapter for Claude Code (the `claude` CLI).
type Claude struct{}

// --- Identity ---------------------------------------------------------------

func (Claude) ID() string          { return "claude" }
func (Claude) DisplayName() string { return "Claude Code" }
func (Claude) Binary() string      { return "claude" }
func (Claude) InstallHint() string {
	return "Install Claude Code: curl -fsSL https://claude.ai/install.sh | bash (macOS/Linux)\nOr visit: https://claude.ai/download"
}

// --- Launch / resume --------------------------------------------------------

// shellQuoteArg single-quotes s for safe inclusion in a shell command line typed
// into a tmux pane (preserves spaces, quotes, and newlines). Mirrors
// lifecycle.shellQuoteArg — both quote values that reach the same tmux pane.
func shellQuoteArg(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// permissionFlag selects the claude permission-mode flag. The mode is
// shell-quoted because the result is typed into a tmux pane and run by a shell.
func permissionFlag(mode string) string {
	return "--permission-mode " + shellQuoteArg(mode)
}

// base is the claude command + model + permission flag every session starts
// from. model is the already-resolved id (lifecycle expands aliases/default
// before calling); it is shell-quoted because it may be an arbitrary
// caller-supplied value and the result is executed by a shell.
func base(model, mode string) string {
	return "claude --model " + shellQuoteArg(model) + " " + permissionFlag(mode)
}

// LaunchCmd builds the claude invocation for a fresh session: base + a pinned
// --session-id (deterministic transcript + --resume) and a --name display
// label. The whole line is typed into the agent's tmux pane (a shell), so every
// interpolated value is shell-quoted. A fresh SessionID is a generated UUID, but
// the resume/adopt/import paths carry a *stored* id, so it is quoted defensively
// alongside Name (a UUID is byte-identical once single-quoted).
func (Claude) LaunchCmd(o agentbackend.LaunchOpts) string {
	return base(o.Model, o.Mode) + " --session-id " + shellQuoteArg(o.SessionID) + " --name " + shellQuoteArg(o.Name)
}

// ResumeCmd builds the invocation that resumes a session by its pinned id. Claude
// always supports resume, so ok is always true. SessionID is shell-quoted: on the
// resume/adopt/import paths it originates from stored data, not a fresh mint.
func (Claude) ResumeCmd(o agentbackend.ResumeOpts) (string, bool) {
	return base(o.Model, o.Mode) + " --resume " + shellQuoteArg(o.SessionID) + " --name " + shellQuoteArg(o.Name), true
}

// LaunchPromptArg seeds the initial task prompt as Claude's trailing positional
// argument, read back from promptFile via "$(cat …)" so a multi-line prompt types
// as a single physical line. Byte-identical to the fragment lifecycle appended
// before backend selection landed (the Phase-0 exit gate).
func (Claude) LaunchPromptArg(promptFile string) string {
	return ` "$(cat ` + shellQuoteArg(promptFile) + `)"`
}

// HeadlessCmd returns the argv for a headless one-shot (`claude -p <prompt>`),
// used for classify/summarize. The prompt is the warden-built instruction; this
// adapter only wraps it in the binary invocation.
func (Claude) HeadlessCmd(prompt string) ([]string, bool) {
	return []string{"claude", "-p", prompt}, true
}

// --- Transcript -------------------------------------------------------------

// nonAlnum matches every character Claude Code replaces with '-' when it encodes
// a workdir into a transcript project-dir name. Mirrors lifecycle's copy used by
// NewestClaudeSession (kept in sync; consolidated once that moves behind the
// interface in a later phase).
var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]`)

// claudeProjectDir maps an absolute workdir to its Claude Code transcript project
// directory under root. Returns "" when root or workdir is empty (lookup
// disabled).
func claudeProjectDir(root, workdir string) string {
	if root == "" || workdir == "" {
		return ""
	}
	return filepath.Join(root, nonAlnum.ReplaceAllString(workdir, "-"))
}

// newestTranscriptPath returns the path of the most recently modified *.jsonl in
// dir, or "" if none.
func newestTranscriptPath(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var newest string
	var newestMod int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if m := info.ModTime().UnixNano(); newest == "" || m > newestMod {
			newest, newestMod = filepath.Join(dir, e.Name()), m
		}
	}
	return newest
}

// TranscriptPath resolves the agent's claude transcript file. With a pinned
// sessionID the file is exactly <sessionID>.jsonl: look under the encoded project
// dir first, then an unambiguous glob across all project dirs (the UUID is
// globally unique, so this is robust to cwd path-encoding quirks). With no pinned
// id (legacy sessions) it falls back to the newest .jsonl in the dir. ok=false
// when nothing resolves.
func (Claude) TranscriptPath(projectsDir, workdir, sessionID string) (string, bool) {
	if sessionID != "" {
		if dir := claudeProjectDir(projectsDir, workdir); dir != "" {
			p := filepath.Join(dir, sessionID+".jsonl")
			if _, err := os.Stat(p); err == nil {
				return p, true
			}
		}
		if projectsDir != "" {
			if m, _ := filepath.Glob(filepath.Join(projectsDir, "*", sessionID+".jsonl")); len(m) == 1 {
				return m[0], true
			}
		}
		return "", false // pinned but not written yet -> caller falls back to the pane
	}
	if dir := claudeProjectDir(projectsDir, workdir); dir != "" {
		if p := newestTranscriptPath(dir); p != "" {
			return p, true
		}
	}
	return "", false
}

// transcript JSONL schema (subset). Mirrors internal/digest/parse.go; kept here
// so the adapter owns its own format knowledge (a future non-JSONL backend has
// its own ParseTranscript).
type tRecord struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Message   struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"` // string OR []block
	} `json:"message"`
}

type tBlock struct {
	Type  string          `json:"type"` // "text" | "tool_use" | "tool_result"
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type tToolInput struct {
	FilePath     string `json:"file_path"`
	NotebookPath string `json:"notebook_path"`
}

var editTools = map[string]bool{"Write": true, "Edit": true, "MultiEdit": true, "NotebookEdit": true}

// ParseTranscript reads a Claude Code transcript JSONL stream and normalizes it
// into warden's neutral []Turn. Malformed lines are skipped (not fatal); only a
// reader error is returned. Note: warden's own digest path still parses via
// internal/digest.ParseTranscript today — this neutral path is exercised once a
// non-JSONL backend forces it (Phase 1+).
func (Claude) ParseTranscript(r io.Reader) ([]agentbackend.Turn, error) {
	var turns []agentbackend.Turn
	sc := bufio.NewScanner(r)
	// Transcript lines (esp. tool_result payloads) can be very long; raise the
	// scanner cap well above the 64K default (same bound as digest.ParseTranscript).
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec tRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue // malformed line — skip
		}
		switch rec.Type {
		case "assistant":
			text, tool, files := assistantParts(rec.Message.Content)
			turns = append(turns, agentbackend.Turn{
				Role:      "assistant",
				Text:      text,
				ToolName:  tool,
				Files:     files,
				Timestamp: parseTime(rec.Timestamp),
			})
		case "user":
			if t := userText(rec.Message.Content); t != "" {
				turns = append(turns, agentbackend.Turn{
					Role:      "user",
					Text:      t,
					Timestamp: parseTime(rec.Timestamp),
				})
			}
		}
	}
	if err := sc.Err(); err != nil {
		return turns, err
	}
	return turns, nil
}

// assistantParts returns the concatenated text, the first tool name, and any
// edit-tool file targets in an assistant message.
func assistantParts(content json.RawMessage) (text, tool string, files []string) {
	if s, ok := asString(content); ok {
		return s, "", nil
	}
	var blocks []tBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
		return "", "", nil
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			text += b.Text
		case "tool_use":
			if tool == "" {
				tool = b.Name
			}
			if editTools[b.Name] {
				var in tToolInput
				_ = json.Unmarshal(b.Input, &in)
				p := in.FilePath
				if p == "" {
					p = in.NotebookPath
				}
				if p != "" {
					files = append(files, p)
				}
			}
		}
	}
	return text, tool, files
}

// userText returns the text of a user message, or "" if it is only tool_result
// blocks (not an actual prompt).
func userText(content json.RawMessage) string {
	if s, ok := asString(content); ok {
		return s
	}
	var blocks []tBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
		return ""
	}
	var text string
	for _, b := range blocks {
		if b.Type == "text" {
			text += b.Text
		}
	}
	return text
}

// asString reports whether raw is a JSON string and returns its value.
func asString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// parseTime best-effort parses an ISO8601 transcript timestamp; the zero time on
// any failure (the field is optional in the neutral Turn).
func parseTime(s string) time.Time {
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

// DetectState maps a captured tmux pane to a neutral run state, mirroring the
// pane signals warden's poller keys on: "esc to interrupt" ⇒ streaming, a prompt
// box ("❯" / "Do you want") ⇒ awaiting input. Anything else is Unknown — Claude
// surfaces no positive idle marker, so idle is inferred elsewhere from staleness.
func (Claude) DetectState(pane string) agentbackend.State {
	if strings.Contains(pane, "esc to interrupt") {
		return agentbackend.StateWorking
	}
	if strings.Contains(pane, "❯") || strings.Contains(pane, "Do you want") {
		return agentbackend.StateNeedsInput
	}
	return agentbackend.StateUnknown
}

// ParseApproval normalizes Claude's box-drawing approval prompt into the neutral
// Approval, delegating to the existing approval detector.
func (Claude) ParseApproval(pane string) (*agentbackend.Approval, bool) {
	a, ok := approval.Parse(pane)
	if !ok {
		return nil, false
	}
	return &agentbackend.Approval{
		Action:            a.Action,
		Question:          a.Question,
		Options:           a.Options,
		SelectedIdx:       a.SelectedIdx,
		AffirmativeIdx:    a.AffirmativeIdx,
		AffirmativeSticky: a.AffirmativeSticky,
	}, true
}

// --- System prompt / pricing ------------------------------------------------

// SystemPromptFlag returns the flag fragment that injects text as a system-prompt
// addendum (leading space so it concatenates onto LaunchCmd). Claude uses
// --append-system-prompt.
func (Claude) SystemPromptFlag(text string) (string, bool) {
	return " --append-system-prompt " + shellQuoteArg(text), true
}

// SystemPromptFileFlag returns the same --append-system-prompt flag, but with the
// value read from path at launch via a "$(cat …)" command substitution. This keeps
// a large addendum off the typed command line (see SystemPromptFiler: a >1024-byte
// launch line is truncated by the tty canonical-mode buffer on macOS/BSD, so the
// agent never starts). The path is single-quoted for the shell; the surrounding
// double quotes pass the file's contents to Claude as one argument.
func (Claude) SystemPromptFileFlag(path string) (string, bool) {
	return ` --append-system-prompt "$(cat ` + shellQuoteArg(path) + `)"`, true
}

// Pricing returns Claude's per-model rates, delegating to internal/spend so the
// spend report and this table can never disagree.
func (Claude) Pricing() (agentbackend.PricingTable, bool) {
	models := map[string]agentbackend.Price{}
	for _, fam := range []string{"opus", "sonnet", "haiku", "fable"} {
		p := spend.PriceFor(fam)
		models[fam] = agentbackend.Price{InputPerMTok: p.InputPerMTok, OutputPerMTok: p.OutputPerMTok}
	}
	def := spend.PriceFor("") // unrecognized ⇒ conservative default tier
	return agentbackend.PricingTable{
		Models:  models,
		Default: agentbackend.Price{InputPerMTok: def.InputPerMTok, OutputPerMTok: def.OutputPerMTok},
	}, true
}

// --- Capabilities -----------------------------------------------------------

// claudePermissionModes is the canonical set of Claude permission modes. It
// mirrors lifecycle.PermissionModes (the spawn-time validation gate); the two
// converge when permission validation becomes backend-aware in a later phase.
var claudePermissionModes = []string{"acceptEdits", "auto", "bypassPermissions", "default", "dontAsk", "plan"}

// Capabilities reports Claude as a full-fidelity (Tier A) backend.
func (Claude) Capabilities() agentbackend.Caps {
	return agentbackend.Caps{
		Resume:               true,
		Headless:             true,
		ModelSelection:       true,
		PermissionModes:      claudePermissionModes,
		StructuredTranscript: true,
		SystemPromptInject:   true,
		SessionIDControl:     true,
	}
}

package backends

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
)

func init() { agentbackend.Register(OpenCode{}) }

// OpenCode is the Backend adapter for OpenCode (the `opencode` CLI) — warden's
// first SQLite-backed, agent-minted-session-id backend.
//
// Tier decision: OpenCode is shipped as **Tier A**. Its transcript lives in a
// SQLite store (not a flat file like Claude's JSONL or Aider's markdown), so this
// adapter sources it through `opencode export <id>` — one command that emits the
// whole session as clean `{info, messages[]}` JSON — and parses that into neutral
// Turns with good fidelity. Sourcing via the export command (rather than the raw
// `opencode db "<SQL>"` rows) keeps the adapter decoupled from the DB schema.
//
// Session-id handling: OpenCode mints its own `ses_…` id; warden cannot assign it
// up front (Caps.SessionIDControl=false). This adapter is **dir-scoped**, the same
// model Aider uses: it keys the transcript and resume off the agent's working
// directory (every warden agent runs in its own git worktree), so it never needs
// to capture or persist the agent-generated id. `opencode -c` continues the last
// session *for that directory* (verified dir-scoped), and the transcript is
// resolved by filtering `opencode session list` to the workdir.
//
// Forward-compatible: when a real `ses_…` id IS pinned into the session (the
// future discover-then-pin write-back; FUTURE_ENHANCEMENTS #52), this adapter
// automatically prefers it — exact-id `export <id>` and `-s <id>` resume — with no
// further changes. Until then the dir-scoped fallback is the live path.
type OpenCode struct{}

// --- Identity ---------------------------------------------------------------

func (OpenCode) ID() string          { return "opencode" }
func (OpenCode) DisplayName() string { return "OpenCode" }
func (OpenCode) Binary() string      { return "opencode" }
func (OpenCode) InstallHint() string {
	return "Install OpenCode: npm install -g opencode-ai\nOr visit: https://opencode.ai"
}

// --- Launch / resume --------------------------------------------------------

// opencodeSkipPerms reports whether a warden permission mode should map to
// OpenCode's auto-approve behavior. OpenCode has two effective modes — prompt
// (default) and skip — so warden's richer Claude modes are folded onto them,
// mirroring the Aider adapter: any "just do it" mode auto-approves; the cautious
// modes stay interactive.
func opencodeSkipPerms(mode string) bool {
	switch mode {
	case "dangerously-skip-permissions", "yes-always", "auto", "acceptEdits", "bypassPermissions", "dontAsk":
		return true
	default:
		return false
	}
}

// opencodeAutoApproveEnv is the env-var prefix that puts an interactive OpenCode
// session into auto-approve. The interactive TUI in current OpenCode (verified on
// v1.17.11) has NO --dangerously-skip-permissions flag — that flag exists only on
// the headless `opencode run` subcommand, so passing it to the TUI makes OpenCode
// print help and exit. Auto-approve for the TUI is config-driven, so warden injects
// a permissive `permission` block via OPENCODE_CONFIG_CONTENT, which OpenCode MERGES
// over the user's config (their provider/model/auth are preserved — verified). The
// JSON is a fixed trusted literal containing no single quotes, so the surrounding
// single-quotes are a safe shell quote.
const opencodeAutoApproveEnv = `OPENCODE_CONFIG_CONTENT='{"permission":{"edit":"allow","bash":"allow","webfetch":"allow"}}'`

// LaunchCmd builds the interactive `opencode` (TUI) invocation for a tmux pane.
// Model is shaped as OpenCode's `-m provider/model` and omitted when empty so a
// BYO/Ollama config default applies (OpenCode is bring-your-own-model; the Claude
// default alias never resolves here, same call as Aider). SessionID and Name are
// ignored: OpenCode mints its own session id (SessionIDControl=false) and the bare
// TUI has no session-name flag. The pane is already cd'd into the agent's workdir,
// so no project-dir argument is appended. Skip ("just do it") modes prepend the
// OPENCODE_CONFIG_CONTENT auto-approve env (the TUI has no skip-permissions flag —
// see opencodeAutoApproveEnv).
func (OpenCode) LaunchCmd(o agentbackend.LaunchOpts) string {
	cmd := "opencode"
	if o.Model != "" {
		cmd += " -m " + shellQuoteArg(o.Model)
	}
	if opencodeSkipPerms(o.Mode) {
		cmd = opencodeAutoApproveEnv + " " + cmd
	}
	return cmd
}

// ResumeCmd builds the interactive resume invocation, run in the agent's workdir.
// OpenCode resumes by id when one is pinned (-s <ses_…>); otherwise it falls back
// to -c, "continue the last session in this directory" — verified dir-scoped, so
// for a per-worktree warden agent it deterministically continues that agent's own
// session. ok is always true (Caps.Resume=true) — richer than Aider, which has no
// resume at all.
func (OpenCode) ResumeCmd(o agentbackend.ResumeOpts) (string, bool) {
	cmd := "opencode"
	if looksLikeSessionID(o.SessionID) {
		cmd += " -s " + shellQuoteArg(o.SessionID)
	} else {
		cmd += " -c"
	}
	if o.Model != "" {
		cmd += " -m " + shellQuoteArg(o.Model)
	}
	if opencodeSkipPerms(o.Mode) {
		cmd = opencodeAutoApproveEnv + " " + cmd
	}
	return cmd, true
}

// LaunchPromptArg seeds the initial task prompt via OpenCode's --prompt flag (read
// back from promptFile via "$(cat …)" so a multi-line prompt types as one physical
// line). OpenCode's TUI runs the seeded prompt on launch and stays interactive — a
// persistent agent loop, like Claude's trailing positional prompt, rather than
// Aider's run-once-and-exit --message.
func (OpenCode) LaunchPromptArg(promptFile string) string {
	return ` --prompt "$(cat ` + shellQuoteArg(promptFile) + `)"`
}

// HeadlessCmd returns the argv for a headless one-shot used by warden's own
// classify/summarize offload when OpenCode is the default backend. It runs a
// single message with auto-approve so it never blocks on a permission prompt.
// (warden's default backend is Claude, so this path is rarely exercised for
// OpenCode; it exists to honor Caps.Headless=true.)
func (OpenCode) HeadlessCmd(prompt string) ([]string, bool) {
	return []string{"opencode", "run", "--dangerously-skip-permissions", prompt}, true
}

// --- Transcript -------------------------------------------------------------

// ocCmdTimeout bounds each opencode subprocess TranscriptPath spawns (session
// list / export). TranscriptPath is called without a caller context, so the bound
// is internal — generous enough for a local SQLite read, tight enough not to wedge
// a poll tick if the binary hangs.
var ocCmdTimeout = 10 * time.Second

// ocExec runs an opencode subcommand in dir and returns its stdout. It is a
// package var so tests can stub the subprocess and exercise TranscriptPath's
// orchestration without the real binary.
var ocExec = func(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "opencode", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	return cmd.Output()
}

// looksLikeSessionID reports whether s is an OpenCode-minted session id (ses_…).
// warden's own pinned id is a UUID (no such prefix), so this cleanly distinguishes
// a real captured id from the placeholder and selects the exact-id vs dir-scoped
// path in ResumeCmd / TranscriptPath.
func looksLikeSessionID(s string) bool {
	return strings.HasPrefix(s, "ses_") && len(s) > len("ses_")
}

// TranscriptPath materializes the session transcript to a file and returns its
// path — the "DB query, not file read" variant of the interface (design §5):
// OpenCode stores transcripts in SQLite, so there is no on-disk file to point at.
// It resolves the session id (a pinned ses_… id, else the newest session for
// workdir via `opencode session list`), runs `opencode export <id>` to get the
// clean session JSON, writes it to a deterministic temp file keyed by id, and
// returns that path for the caller to open and feed to ParseTranscript. ok=false
// on any failure (binary missing, no session yet, export error), so the digest
// path degrades to "no transcript" rather than erroring — same contract as Aider.
func (OpenCode) TranscriptPath(_, workdir, sessionID string) (string, bool) {
	id := ""
	switch {
	case looksLikeSessionID(sessionID):
		id = sessionID
	case workdir != "":
		ctx, cancel := context.WithTimeout(context.Background(), ocCmdTimeout)
		out, err := ocExec(ctx, workdir, "session", "list", "--format", "json")
		cancel()
		if err != nil {
			return "", false
		}
		if rid, ok := newestSessionForDir(out, workdir); ok {
			id = rid
		}
	}
	if id == "" {
		return "", false
	}

	ctx, cancel := context.WithTimeout(context.Background(), ocCmdTimeout)
	out, err := ocExec(ctx, workdir, "export", id)
	cancel()
	if err != nil || len(out) == 0 {
		return "", false
	}
	p := filepath.Join(os.TempDir(), "warden-opencode-"+id+".json")
	if err := os.WriteFile(p, out, 0o600); err != nil {
		return "", false
	}
	return p, true
}

// ocSessionRow is the subset of an `opencode session list --format json` row we
// key on: the id, its directory, and its last-updated timestamp.
type ocSessionRow struct {
	ID        string `json:"id"`
	Directory string `json:"directory"`
	Updated   int64  `json:"updated"`
}

// newestSessionForDir picks the most-recently-updated session whose directory
// equals workdir from an `opencode session list --format json` payload (the list
// is global; warden scopes it to the agent's worktree). ok=false when no session
// matches the directory.
func newestSessionForDir(listJSON []byte, workdir string) (string, bool) {
	var rows []ocSessionRow
	if err := json.Unmarshal(listJSON, &rows); err != nil {
		return "", false
	}
	var matches []ocSessionRow
	for _, r := range rows {
		if r.ID != "" && r.Directory == workdir {
			matches = append(matches, r)
		}
	}
	if len(matches) == 0 {
		return "", false
	}
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].Updated > matches[j].Updated })
	return matches[0].ID, true
}

// ocExport mirrors the shape of `opencode export <id>` output: a session info
// header (unused here) and the ordered messages, each with its parts.
type ocExport struct {
	Messages []ocMessage `json:"messages"`
}

type ocMessage struct {
	Info  ocMsgInfo `json:"info"`
	Parts []ocPart  `json:"parts"`
}

type ocMsgInfo struct {
	Role string `json:"role"` // "user" | "assistant"
	Time struct {
		Created int64 `json:"created"` // epoch milliseconds
	} `json:"time"`
}

// ocPart is one message part. OpenCode emits several part types; we read "text"
// (message body), "tool" (a tool invocation, carrying its name + input), and
// "patch" (an applied file edit, carrying the touched files). "step-start" /
// "step-finish" are control parts and ignored.
type ocPart struct {
	Type  string   `json:"type"`
	Text  string   `json:"text"`
	Tool  string   `json:"tool"`  // tool name, on a "tool" part
	Files []string `json:"files"` // edited files, on a "patch" part
	State *struct {
		Input struct {
			FilePath string `json:"filePath"`
			Path     string `json:"path"`
			Filename string `json:"filename"`
		} `json:"input"`
	} `json:"state"`
}

// ParseTranscript normalizes `opencode export` JSON into warden's neutral []Turn.
// The export is a single JSON document (not NDJSON), so it is read whole. Each
// message becomes one Turn keyed by role; assistant turns carry the first tool
// name and any edited files (from tool inputs and patch parts). A reader/JSON
// error is returned; an empty/odd message is tolerated (best-effort, like the
// Claude and Aider parsers).
func (OpenCode) ParseTranscript(r io.Reader) ([]agentbackend.Turn, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var exp ocExport
	if err := json.Unmarshal(data, &exp); err != nil {
		return nil, err
	}

	var turns []agentbackend.Turn
	for _, m := range exp.Messages {
		var ts time.Time
		if m.Info.Time.Created > 0 {
			ts = time.UnixMilli(m.Info.Time.Created)
		}
		switch m.Info.Role {
		case "user":
			if text := ocConcatText(m.Parts); strings.TrimSpace(text) != "" {
				turns = append(turns, agentbackend.Turn{Role: "user", Text: text, Timestamp: ts})
			}
		case "assistant":
			tool, files := ocToolAndFiles(m.Parts)
			turns = append(turns, agentbackend.Turn{
				Role: "assistant", Text: ocConcatText(m.Parts), ToolName: tool, Files: files, Timestamp: ts,
			})
		}
	}
	return turns, nil
}

// ocConcatText joins the text of every "text" part in a message.
func ocConcatText(parts []ocPart) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// ocToolAndFiles returns the first tool name and the unique edited files in a
// message: a "tool" part contributes its name and its input file (write/edit
// tools), a "patch" part contributes its files[] (and implies an "edit" when no
// explicit tool was named).
func ocToolAndFiles(parts []ocPart) (tool string, files []string) {
	for _, p := range parts {
		switch p.Type {
		case "tool":
			if tool == "" {
				tool = p.Tool
			}
			if p.State != nil {
				if f := firstNonEmpty(p.State.Input.FilePath, p.State.Input.Path, p.State.Input.Filename); f != "" {
					files = appendUnique(files, f)
				}
			}
		case "patch":
			for _, f := range p.Files {
				if f != "" {
					files = appendUnique(files, f)
				}
			}
			if tool == "" {
				tool = "edit"
			}
		}
	}
	return tool, files
}

// firstNonEmpty returns the first non-empty string in vals, or "".
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// --- State / approval -------------------------------------------------------

// DetectState reports Unknown: OpenCode's run-state and approval prompts live in
// its TUI, whose pane has no stable text marker this adapter can key on (no live
// interactive prompt was captured for Phase 2). warden infers idle from staleness,
// the same conservative stance the Claude/Aider adapters take for idle. Mapping
// interactive OpenCode approvals is deferred (FUTURE_ENHANCEMENTS #52); the
// faithful Phase-2 surface is the autonomous `run`, which prompts for nothing.
func (OpenCode) DetectState(string) agentbackend.State { return agentbackend.StateUnknown }

// ParseApproval reports no approval: see DetectState — OpenCode's interactive
// permission prompts are not yet mapped, so this degrades (returns false) rather
// than mis-parsing. Headless `--dangerously-skip-permissions` raises no prompts.
func (OpenCode) ParseApproval(string) (*agentbackend.Approval, bool) { return nil, false }

// --- System prompt / pricing ------------------------------------------------

// SystemPromptFlag reports no launch-time system-prompt injection: OpenCode has no
// --append-system-prompt equivalent on its launch command (its customization is
// config/agent-file based; Caps.SystemPromptInject stays false — that flag means
// specifically a launch-time flag). warden instead delivers the same
// pipeline/collab/git addendum out-of-band via the AGENTS.md rules file OpenCode
// reads on startup — see InjectContext (agentbackend.ContextInjector).
func (OpenCode) SystemPromptFlag(string) (string, bool) { return "", false }

// opencodeAgentsFile is the rules file OpenCode reads on startup. OpenCode follows
// the AGENTS.md standard: it loads the nearest AGENTS.md by traversing up from the
// working directory (verified: opencode.ai/docs/rules), so warden writes its addendum
// into <workdir>/AGENTS.md.
const opencodeAgentsFile = "AGENTS.md"

// InjectContext implements agentbackend.ContextInjector. OpenCode has no
// --append-system-prompt flag (Caps.SystemPromptInject=false) but reads an AGENTS.md
// rules file from its working directory on startup, so warden delivers its
// collab/git/pipeline addendum by writing that text into <workdir>/AGENTS.md.
// Lifecycle calls this post-worktree-creation / pre-launch so the file is present
// when OpenCode starts. The no-clobber/idempotent/git-exclude write is the shared
// writeRulesFile helper (the AGENTS.md counterpart to Claude's launch-time flag; see
// inject.go and docs/agent-backends/opencode.md).
func (OpenCode) InjectContext(workdir, text string) error {
	return writeRulesFile(workdir, opencodeAgentsFile, text)
}

// Pricing reports no pricing table. OpenCode is multi-provider bring-your-own-model
// (local Ollama at $0, or any paid provider), so warden cannot enumerate per-model
// rates, and the per-tick usage reader is Claude-JSONL-specific and does not yet
// read OpenCode's first-class cost/tokens. Per design §5 spend shows tokens (not
// dollars) and savings omits the agent. Wiring warden to OpenCode's native cost is
// tracked alongside discover-then-pin (FUTURE_ENHANCEMENTS #52).
func (OpenCode) Pricing() (agentbackend.PricingTable, bool) {
	return agentbackend.PricingTable{}, false
}

// --- Capabilities -----------------------------------------------------------

// Capabilities reports OpenCode as a Tier-A backend that, unlike Aider, supports
// resume (dir-scoped today, exact-id once discover-then-pin lands). Structured
// transcript powers digests; no assignable session id, no warden-side pricing yet.
// SystemPromptInject stays false (OpenCode has no launch-time system-prompt flag) —
// but warden's addendum still reaches it out-of-band via the AGENTS.md rules file
// (InjectContext); the Caps flag tracks the launch-flag specifically.
func (OpenCode) Capabilities() agentbackend.Caps {
	return agentbackend.Caps{
		Resume:               true,
		Headless:             true,
		ModelSelection:       true,
		PermissionModes:      []string{"default", "dangerously-skip-permissions"},
		StructuredTranscript: true,
		SystemPromptInject:   false,
		SessionIDControl:     false,
	}
}

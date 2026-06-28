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

func init() { agentbackend.Register(Goose{}) }

// Goose is the Backend adapter for Goose (Block's open-source `goose` CLI —
// https://github.com/block/goose). It is warden's first **experimental,
// breadth-first** adapter (design §5, #52): a thin, correct seam that launches
// Goose in a tmux pane and sources its transcript, with the gaps honestly
// declared in Caps and documented in docs/agent-backends/goose.md rather than
// papered over.
//
// Tier decision: Goose is shipped as **Tier A** for transcripts. Like OpenCode
// it stores sessions in a SQLite DB (~/.local/share/goose/sessions/sessions.db),
// so this adapter sources the transcript through `goose session export
// --format json` — one command that emits the whole session as clean
// `{conversation: [...]}` JSON — and parses that into neutral Turns with good
// fidelity (text bodies, tool names + edited file paths from `toolRequest`
// parts). Sourcing via the export command keeps the adapter decoupled from the
// DB schema.
//
// Session-id handling: Goose mints its own date-stamped id (e.g. `20260628_1`)
// that warden cannot assign up front (Caps.SessionIDControl=false). But Goose
// also takes a **`--name`** that warden pins to its own agent id (opts.Name =
// sess.ID), so resume is **name-deterministic** (`goose session -r --name <id>`)
// rather than dir-scoped guessing — strictly richer than OpenCode's `-c`. The
// transcript lookup, however, is **dir-scoped** (the same model OpenCode uses):
// TranscriptPath is handed sess.ClaudeSessionID, not the name, so it resolves
// the session by filtering `goose session list` to the agent's working
// directory (every warden agent runs in its own git worktree).
type Goose struct{}

// --- Identity ---------------------------------------------------------------

func (Goose) ID() string          { return "goose" }
func (Goose) DisplayName() string { return "Goose" }
func (Goose) Binary() string      { return "goose" }
func (Goose) InstallHint() string {
	return "Install Goose: curl -fsSL https://raw.githubusercontent.com/block/goose/main/download_cli.sh | CONFIGURE=false bash\nThen configure a provider (Ollama for $0-local): https://block.github.io/goose/"
}

// --- Launch / resume --------------------------------------------------------

// LaunchCmd builds the interactive `goose session` invocation for a tmux pane.
// warden pins its own agent id as the session **name** (--name) so the session
// has a stable, warden-owned handle for deterministic resume.
//
// Honest gaps (declared in Caps, detailed in docs/agent-backends/goose.md):
//   - Model: `goose session` has no --model/--provider flag, so opts.Model is NOT
//     applied here — Goose resolves its model from GOOSE_PROVIDER/GOOSE_MODEL
//     (env or ~/.config/goose/config.yaml). `goose run` (headless) does take
//     --model/--provider; the interactive launch does not.
//   - Mode: `goose session` has no permission-mode flag either — Goose's approval
//     mode is the GOOSE_MODE env/config (auto|approve|chat|smart_approve), so
//     opts.Mode is not applied on the launch command.
//
// opts.SessionID (warden's ClaudeSessionID placeholder) is ignored: Goose mints
// its own id and warden keys off the --name instead.
func (Goose) LaunchCmd(o agentbackend.LaunchOpts) string {
	cmd := "goose session"
	if o.Name != "" {
		cmd += " --name " + shellQuoteArg(o.Name)
	}
	return cmd
}

// ResumeCmd builds the interactive resume invocation, run in the agent's workdir.
// Goose resumes by the warden-pinned name (`goose session -r --name <id>`), so a
// per-worktree warden agent deterministically continues its own session by
// warden's own id — no discovery step (richer than Aider, which can't resume at
// all, and than OpenCode's dir-scoped `-c`). ok is always true (Caps.Resume=true).
// Falls back to a bare `goose session -r` (resume most-recent) when no name is
// pinned, which still resumes the newest session for the pane's directory.
func (Goose) ResumeCmd(o agentbackend.ResumeOpts) (string, bool) {
	cmd := "goose session -r"
	if o.Name != "" {
		cmd += " --name " + shellQuoteArg(o.Name)
	}
	return cmd, true
}

// LaunchPromptArg reports no initial-prompt seeding for the interactive launch.
// Goose's `goose session` accepts no initial-prompt argument (a positional is
// parsed as a subcommand, and it has no -t/--message), so warden cannot seed a
// managed agent's task into the interactive session via the launch command. The
// task prompt is still written to warden's prompt file; an operator pastes it
// into the pane. The only prompt-capable Goose entry point is the headless
// `goose run -t` (see HeadlessCmd), which is one-shot rather than a persistent
// loop. Returning "" leaves LaunchCmd a valid standalone interactive launch.
// (Documented gap, docs/agent-backends/goose.md — Caps cannot express this, so
// it lives in the gap doc; it is the honest breadth-first limitation.)
func (Goose) LaunchPromptArg(string) string { return "" }

// PromptText / ReadyMarker implement agentbackend.PromptSeeder: `goose session`
// takes no launch-line prompt, so warden types the task into the REPL once Goose
// has finished booting (provider/model resolved, banner drawn). ReadyMarker keys
// on Goose's "goose is ready" banner line, which prints right before the prompt
// becomes interactive.
func (Goose) PromptText(prompt string) (string, bool) { return prompt, prompt != "" }
func (Goose) ReadyMarker() string                     { return "goose is ready" }

// HeadlessCmd returns the argv for a headless one-shot used by warden's own
// classify/summarize offload when Goose is the default backend. `goose run -t`
// runs a single instruction non-interactively; --no-session keeps it out of the
// session store and --quiet prints only the model response to stdout. (warden's
// default backend is Claude, so this path is rarely exercised for Goose; it
// honors Caps.Headless=true.) Provider/model come from GOOSE_PROVIDER/GOOSE_MODEL.
func (Goose) HeadlessCmd(prompt string) ([]string, bool) {
	return []string{"goose", "run", "--no-session", "--quiet", "-t", prompt}, true
}

// --- Transcript -------------------------------------------------------------

// gooseCmdTimeout bounds each goose subprocess TranscriptPath spawns (session
// list / export). TranscriptPath is called without a caller context, so the
// bound is internal — generous for a local SQLite read, tight enough not to
// wedge a poll tick if the binary hangs. (Mirrors OpenCode's ocCmdTimeout.)
var gooseCmdTimeout = 10 * time.Second

// gooseExec runs a goose subcommand in dir and returns its stdout. It is a
// package var so tests can stub the subprocess and exercise TranscriptPath's
// orchestration without the real binary. (Mirrors OpenCode's ocExec.)
var gooseExec = func(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "goose", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	return cmd.Output()
}

// TranscriptPath materializes the session transcript to a file and returns its
// path — the "DB query, not file read" variant of the interface (design §5):
// Goose stores transcripts in SQLite, so there is no on-disk file to point at.
// It is dir-scoped (like OpenCode): it lists sessions for the agent's working
// directory (`goose session list --format json --working_dir <dir>`), picks the
// newest, exports it (`goose session export --session-id <id> --format json`),
// writes the JSON to a deterministic temp file keyed by id, and returns that
// path for the caller to open and feed to ParseTranscript. ok=false on any
// failure (binary missing, no session yet, export error), so the digest path
// degrades to "no transcript" rather than erroring — same contract as OpenCode.
//
// sessionID (warden's ClaudeSessionID placeholder) is unused: the name warden
// pinned at launch is not plumbed to TranscriptPath, so resolution is by
// directory, which is unambiguous because every warden agent has its own worktree.
func (Goose) TranscriptPath(_, workdir, _ string) (string, bool) {
	if workdir == "" {
		return "", false
	}

	ctx, cancel := context.WithTimeout(context.Background(), gooseCmdTimeout)
	out, err := gooseExec(ctx, workdir, "session", "list", "--format", "json", "--working_dir", workdir)
	cancel()
	if err != nil {
		return "", false
	}
	id, ok := newestGooseSessionForDir(out, workdir)
	if !ok {
		return "", false
	}

	ctx, cancel = context.WithTimeout(context.Background(), gooseCmdTimeout)
	out, err = gooseExec(ctx, workdir, "session", "export", "--session-id", id, "--format", "json")
	cancel()
	if err != nil || len(out) == 0 {
		return "", false
	}
	p := filepath.Join(os.TempDir(), "warden-goose-"+id+".json")
	if err := os.WriteFile(p, out, 0o600); err != nil {
		return "", false
	}
	return p, true
}

// gooseSessionRow is the subset of a `goose session list --format json` row we
// key on: the id, its working directory, and its last-updated timestamp (an
// RFC3339 string, unlike OpenCode's epoch-millis int).
type gooseSessionRow struct {
	ID         string `json:"id"`
	WorkingDir string `json:"working_dir"`
	UpdatedAt  string `json:"updated_at"`
}

// newestGooseSessionForDir picks the most-recently-updated session whose
// working_dir equals workdir from a `goose session list --format json` payload.
// `goose session list --working_dir` already filters server-side and sorts
// newest-first, but this re-filters and sorts defensively so the result is
// correct regardless of the flag's behavior. ok=false when no session matches.
func newestGooseSessionForDir(listJSON []byte, workdir string) (string, bool) {
	var rows []gooseSessionRow
	if err := json.Unmarshal(listJSON, &rows); err != nil {
		return "", false
	}
	var matches []gooseSessionRow
	for _, r := range rows {
		if r.ID != "" && r.WorkingDir == workdir {
			matches = append(matches, r)
		}
	}
	if len(matches) == 0 {
		return "", false
	}
	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i].UpdatedAt > matches[j].UpdatedAt // RFC3339 sorts lexically by time
	})
	return matches[0].ID, true
}

// gooseExport mirrors the shape of `goose session export --format json` output:
// a session header (unused here) and the ordered conversation, each message
// carrying its content parts.
type gooseExport struct {
	Conversation []gooseMessage `json:"conversation"`
}

type gooseMessage struct {
	Role    string      `json:"role"`    // "user" | "assistant"
	Created int64       `json:"created"` // epoch seconds
	Content []goosePart `json:"content"`
}

// goosePart is one message content part. Goose emits "text" (message body),
// "toolRequest" (a tool invocation, carrying the tool name + arguments), and
// "toolResponse" (a tool result, which Goose attaches to a `role: user`
// message). Other part types (image, thinking, …) are ignored.
type goosePart struct {
	Type     string         `json:"type"`
	Text     string         `json:"text"`     // "text" part
	ToolCall *gooseToolCall `json:"toolCall"` // "toolRequest" part
}

// gooseToolCall is the toolRequest's call, a Result-shaped envelope whose
// `value` carries the tool name and arguments.
type gooseToolCall struct {
	Value struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"value"`
}

// gooseToolArgs is the subset of a tool's arguments we mine for edited files:
// the common file-path keys across Goose's developer tools (write/edit/view).
type gooseToolArgs struct {
	Path     string `json:"path"`
	FilePath string `json:"filePath"`
	Filename string `json:"filename"`
}

// ParseTranscript normalizes `goose session export --format json` into warden's
// neutral []Turn. The export is a single JSON document (not NDJSON), so it is
// read whole. Each conversation message becomes one Turn keyed by role:
//   - a user message with text → a "user" Turn (a user message that is only a
//     toolResponse — Goose's tool results arrive as user messages — carries no
//     prompt text and is skipped, matching the Claude/OpenCode parsers).
//   - an assistant message → an "assistant" Turn carrying its text, the first
//     toolRequest's tool name, and any edited file paths from tool arguments.
//
// A reader/JSON error is returned; an empty/odd message is tolerated
// (best-effort, like the Claude, Aider, and OpenCode parsers).
func (Goose) ParseTranscript(r io.Reader) ([]agentbackend.Turn, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var exp gooseExport
	if err := json.Unmarshal(data, &exp); err != nil {
		return nil, err
	}

	var turns []agentbackend.Turn
	for _, m := range exp.Conversation {
		var ts time.Time
		if m.Created > 0 {
			ts = time.Unix(m.Created, 0)
		}
		switch m.Role {
		case "user":
			if text := gooseText(m.Content); strings.TrimSpace(text) != "" {
				turns = append(turns, agentbackend.Turn{Role: "user", Text: text, Timestamp: ts})
			}
		case "assistant":
			tool, files := gooseToolAndFiles(m.Content)
			turns = append(turns, agentbackend.Turn{
				Role: "assistant", Text: gooseText(m.Content), ToolName: tool, Files: files, Timestamp: ts,
			})
		}
	}
	return turns, nil
}

// gooseText joins the text of every "text" part in a message.
func gooseText(parts []goosePart) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// gooseToolAndFiles returns the first tool name and the unique edited files in a
// message: each "toolRequest" part contributes its tool name (first wins) and
// any file path found in its arguments (path / filePath / filename).
func gooseToolAndFiles(parts []goosePart) (tool string, files []string) {
	for _, p := range parts {
		if p.Type != "toolRequest" || p.ToolCall == nil {
			continue
		}
		if tool == "" {
			tool = p.ToolCall.Value.Name
		}
		var a gooseToolArgs
		_ = json.Unmarshal(p.ToolCall.Value.Arguments, &a)
		if f := firstNonEmpty(a.FilePath, a.Path, a.Filename); f != "" {
			files = appendUnique(files, f)
		}
	}
	return tool, files
}

// --- State / approval -------------------------------------------------------

// DetectState reports Unknown: Goose's run-state and approval prompts live in
// its interactive TUI, and no stable tool-approval pane marker was captured for
// this experimental Phase (the default GOOSE_MODE=auto prompts for nothing).
// warden infers idle from staleness, the same conservative stance the
// Aider/OpenCode adapters take. Mapping Goose's `approve`-mode prompts is
// deferred (docs/agent-backends/goose.md); degrade rather than mis-detect.
func (Goose) DetectState(string) agentbackend.State { return agentbackend.StateUnknown }

// ParseApproval reports no approval: see DetectState — Goose's interactive
// permission prompts are not yet mapped, so this degrades (returns false) rather
// than mis-parsing. Headless `goose run` in the default auto mode raises none.
func (Goose) ParseApproval(string) (*agentbackend.Approval, bool) { return nil, false }

// --- System prompt / pricing ------------------------------------------------

// SystemPromptFlag reports no launch-time system-prompt injection for the interactive
// launch: `goose session` has no system-prompt flag (only headless `goose run
// --system` does; Caps.SystemPromptInject stays false — that flag means specifically a
// launch-time flag). warden instead delivers the same pipeline/collab/git addendum
// out-of-band via the .goosehints file Goose reads on startup — see InjectContext
// (agentbackend.ContextInjector). Goose's other first-class customization (recipes,
// the "Top Of Mind" extension) is worth wiring later (docs/agent-backends/goose.md).
func (Goose) SystemPromptFlag(string) (string, bool) { return "", false }

// gooseHintsFile is the hints file Goose reads on startup. Goose loads .goosehints
// (and AGENTS.md) from the working directory up to the repo root and adds them to the
// system prompt for every request — verified: block.github.io/goose using-goosehints
// — so warden writes its addendum into the Goose-native <workdir>/.goosehints.
const gooseHintsFile = ".goosehints"

// InjectContext implements agentbackend.ContextInjector. The interactive `goose
// session` has no system-prompt flag (Caps.SystemPromptInject=false) but Goose reads
// a .goosehints file from its working directory on startup, so warden delivers its
// collab/git/pipeline addendum by writing that text into <workdir>/.goosehints.
// Lifecycle calls this post-worktree-creation / pre-launch so the file is present
// when Goose starts. The no-clobber/idempotent/git-exclude write is the shared
// writeRulesFile helper (see inject.go and docs/agent-backends/goose.md).
func (Goose) InjectContext(workdir, text string) error {
	return writeRulesFile(workdir, gooseHintsFile, text)
}

// Pricing reports no pricing table. Goose is multi-provider bring-your-own-model
// (local Ollama at $0, or any paid provider), so warden cannot enumerate per-model
// rates, and the per-tick usage reader is Claude-JSONL-specific. Goose DOES track
// tokens and cost natively (the export carries `usage`/`accumulated_cost`); wiring
// warden's spend/savings to it is deferred (docs/agent-backends/goose.md). Per
// design §5 spend shows tokens (not dollars) and savings omits the agent.
func (Goose) Pricing() (agentbackend.PricingTable, bool) {
	return agentbackend.PricingTable{}, false
}

// --- Capabilities -----------------------------------------------------------

// Capabilities reports Goose as an experimental Tier-A backend: structured
// (JSON-export) transcript powers digests, and resume is supported and
// name-deterministic (warden pins --name). Honest degradations: warden cannot
// assign Goose's session id (it mints its own; warden pins a name instead), the
// interactive launch takes no model or launch-time system-prompt flag (config/env-
// driven), and there is no warden-side pricing yet. SystemPromptInject stays false
// (no launch-time flag) — but warden's addendum still reaches Goose out-of-band via
// the .goosehints file (InjectContext). PermissionModes lists Goose's native
// approval modes for reference even though the interactive launch can't select
// one (GOOSE_MODE env/config only) — see docs/agent-backends/goose.md.
func (Goose) Capabilities() agentbackend.Caps {
	return agentbackend.Caps{
		Resume:               true,
		Headless:             true,
		ModelSelection:       false,
		PermissionModes:      []string{"auto", "approve", "chat", "smart_approve"},
		StructuredTranscript: true,
		SystemPromptInject:   false,
		SessionIDControl:     false,
	}
}

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

// DetectState reports Unknown: Codex's run-state and approval prompts live in its
// TUI, whose pane has no stable text marker this experimental adapter keys on yet
// (no live interactive prompt was captured for this phase). warden infers idle from
// staleness, the same conservative stance the Claude/Aider/OpenCode adapters take.
// Mapping Codex's TUI approvals is deferred (docs/agent-backends/codex.md); the
// faithful headless surface is `codex exec`, which raises no prompts.
func (Codex) DetectState(string) agentbackend.State { return agentbackend.StateUnknown }

// ParseApproval reports no approval: see DetectState — Codex's interactive
// permission prompts are not yet mapped, so this degrades (returns false) rather
// than mis-parsing. The bypassed `codex exec` headless path raises no prompts.
func (Codex) ParseApproval(string) (*agentbackend.Approval, bool) { return nil, false }

// --- System prompt / pricing ------------------------------------------------

// SystemPromptFlag reports no system-prompt injection: Codex has no
// --append-system-prompt equivalent on its launch command (its customization is
// config/AGENTS.md/`-c` based), so warden's pipeline/collab/git hints are skipped
// for Codex agents (Caps.SystemPromptInject=false; see the gap doc for the `-c`
// override route that could carry this later).
func (Codex) SystemPromptFlag(string) (string, bool) { return "", false }

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
// discover-then-pin lands). Codex mints its own session id (no SessionIDControl),
// has no launch-time system-prompt injection, and exposes no warden-side dollar
// pricing yet. PermissionModes surface Codex's native sandbox vocabulary.
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

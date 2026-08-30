package backends

import (
	"bufio"
	"encoding/json"
	"io"
	"os/exec"
	"regexp"
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

// LaunchPromptArg returns "" — Cursor is seeded after launch (PromptSeeder), not on
// the launch line. cursor-agent accepts an optional `[prompt...]` positional and
// stays interactive, but passing the prompt that way only *populates* the composer:
// cursor-agent does not auto-submit it, so a managed agent would sit forever with the
// task typed but never sent. To make Cursor actually start on its task, warden
// launches the bare interactive TUI and types the prompt in once the composer is
// ready, pressing Enter to submit it (PromptText/ReadyMarker, the same seam as
// Aider/Goose). The headless one-shot path (HeadlessCmd) still passes the prompt as a
// positional to `-p`.
func (Cursor) LaunchPromptArg(string) string { return "" }

// PromptText / ReadyMarker implement agentbackend.PromptSeeder: warden types the task
// into cursor-agent's interactive composer once its UI is ready and presses Enter to
// submit it (the launch-line positional populates the composer but never auto-submits,
// leaving the agent stuck). ReadyMarker keys on cursor-agent's fresh-launch composer
// placeholder, drawn right when the composer becomes interactive; if it never appears
// the lifecycle falls back to a settle delay.
func (Cursor) PromptText(prompt string) (string, bool) { return prompt, prompt != "" }
func (Cursor) ReadyMarker() string                     { return cursorIdlePlaceholders[0] }

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

// cursorWorkingMarker is the stable footer hint cursor-agent right-aligns on the
// composer's prompt line for the whole duration of a streaming turn
// ("→ Add a follow-up … ctrl+c to stop"). The spinner status line above it varies
// ("⠘⠤ Composing" / "⠠⠛ Running  N tokens"), so this hint — not the spinner — is the
// reliable working marker. Captured live (cursor-agent 2026.06.26-7079533).
const cursorWorkingMarker = "ctrl+c to stop"

// cursorIdlePlaceholders are cursor-agent's at-rest composer placeholders: the
// fresh-launch tagline and the post-turn follow-up prompt. They mark an empty
// composer awaiting input. ("Add a follow-up" is ALSO shown mid-turn, but DetectState
// gates StateWorking on cursorWorkingMarker first, so by the time idle is tested the
// pane is genuinely at rest.) Both captured live (the fresh and after-a-turn panes).
var cursorIdlePlaceholders = []string{
	"Plan, search, build anything",
	"Add a follow-up",
}

// DetectState maps a captured cursor-agent TUI pane to a neutral run state, mirroring
// the Codex/Claude adapters. cursor-agent carries stable positive markers captured
// live (2026.06.26-7079533):
//   - streaming a turn ⇒ a right-aligned "ctrl+c to stop" hint on the composer prompt
//     line, so that substring ⇒ StateWorking.
//   - blocked on a command-allowlist approval or the workspace-trust prompt ⇒ a menu
//     detected by ParseApproval ⇒ StateNeedsInput.
//   - at rest with an empty composer ⇒ one of cursor's composer placeholders ⇒
//     StateIdle.
//
// Anything else returns StateUnknown (never a false NeedsInput/Working) so warden
// falls back to staleness — the same conservative stance as Codex. Order matters:
// working is tested before idle because the at-rest follow-up placeholder is also
// present mid-turn.
func (Cursor) DetectState(pane string) agentbackend.State {
	if strings.Contains(pane, cursorWorkingMarker) {
		return agentbackend.StateWorking
	}
	if _, ok := (Cursor{}).ParseApproval(pane); ok {
		return agentbackend.StateNeedsInput
	}
	for _, p := range cursorIdlePlaceholders {
		if strings.Contains(pane, p) {
			return agentbackend.StateIdle
		}
	}
	return agentbackend.StateUnknown
}

// cursorMenuOptionRe matches one line of cursor-agent's interactive permission menu,
// tolerating the leading "→" selection cursor (U+2192) on the highlighted option and
// the plain indent on the rest, and anchoring on the trailing "(<key hint>)" every
// option carries: "→ Run (once) (y)", "Add Shell(echo) to allowlist? (tab)",
// "Run Everything (shift+tab)", "Skip (esc or n)". The label group is non-greedy and
// the hint is anchored at end-of-line, so a label with its own inner parens (e.g.
// "Shell(echo)") still binds the final key-hint paren.
var cursorMenuOptionRe = regexp.MustCompile(`^\s*(\x{2192}\s+)?(.+?)\s+\(([^()]*)\)\s*$`)

// cursorCommandRe captures the proposed command cursor-agent echoes as "$ <command>"
// just above the menu (the Action); cursorLocationRe strips the trailing " in <cwd>"
// annotation cursor appends to it.
var cursorCommandRe = regexp.MustCompile(`^\s*\$\s+(.+?)\s*$`)
var cursorLocationRe = regexp.MustCompile(`\s+in\s+[./]\S*$`)

// cursorTrustOptionRe matches a workspace-trust menu entry once its box border is
// stripped: a "[<key>] <label>" line, e.g. "[a] Trust this workspace" / "[q] Quit".
var cursorTrustOptionRe = regexp.MustCompile(`^\[[^\]]+\]\s+(.+?)\s*$`)

// ParseApproval normalizes cursor-agent's two interactive blocking prompts into the
// neutral Approval: its command-allowlist approval and its one-time workspace-trust
// prompt. Captured live (cursor-agent 2026.06.26-7079533). It returns (nil,false) for
// any pane without one of those menus (idle/working prose is never mis-parsed),
// keeping the auto-approve path — which keys off Fingerprint(Options) — honest.
// Options are 1-indexed top-down and faithful to the pane.
func (Cursor) ParseApproval(pane string) (*agentbackend.Approval, bool) {
	if a, ok := cursorParseCommandApproval(pane); ok {
		return a, true
	}
	return cursorParseTrustApproval(pane)
}

// cursorParseCommandApproval recognizes cursor's command-allowlist prompt, captured
// live:
//
//	$  echo hello-from-cursor in .
//
//	Run this command?
//	Not in allowlist: echo
//	 → Run (once) (y)
//	   Add Shell(echo) to allowlist? (tab)
//	   Run Everything (shift+tab)
//	   Skip (esc or n)
//
// It finds the contiguous menu run at the bottom of the pane (≥2 option lines, each
// carrying a trailing key hint), then reads the "?"-terminated Question and the
// "$ <command>" Action just above it. The Question gate is required, so a stray line
// with trailing parens (e.g. the single "Reason for rejection (…)" composer prompt)
// is not mis-read as a menu. Options keep their key hint so they stay faithful to the
// pane (the auto-approve policy and daemon re-verify guard key off Fingerprint).
func cursorParseCommandApproval(pane string) (*agentbackend.Approval, bool) {
	lines := strings.Split(pane, "\n")

	// The live prompt sits at the bottom: find the last option line, then walk up
	// while lines stay options.
	end := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if cursorMenuOptionRe.MatchString(lines[i]) {
			end = i
			break
		}
	}
	if end < 0 {
		return nil, false
	}
	start := end
	for start-1 >= 0 && cursorMenuOptionRe.MatchString(lines[start-1]) {
		start--
	}

	var opts []string
	sel := 0
	for i := start; i <= end; i++ {
		m := cursorMenuOptionRe.FindStringSubmatch(lines[i])
		if m[1] != "" { // the "→ " selection cursor sits on the highlighted option
			sel = i - start + 1
		}
		opts = append(opts, strings.TrimSpace(m[2])+" ("+m[3]+")")
	}
	if len(opts) < 2 {
		return nil, false
	}

	a := &agentbackend.Approval{Options: opts, SelectedIdx: sel}

	// Scan upward from the menu for the "$ <command>" Action and the "?"-terminated
	// Question (a bounded window above the options).
	for i := start - 1; i >= 0 && i >= start-12; i-- {
		t := strings.TrimSpace(lines[i])
		if t == "" {
			continue
		}
		if a.Action == "" {
			if m := cursorCommandRe.FindStringSubmatch(lines[i]); m != nil {
				a.Action = strings.TrimSpace(cursorLocationRe.ReplaceAllString(m[1], ""))
				continue
			}
		}
		if a.Question == "" && strings.HasSuffix(t, "?") {
			a.Question = t
		}
	}
	// The question header gates recognition: without it this is not a cursor approval.
	if a.Question == "" {
		return nil, false
	}
	a.AffirmativeIdx, a.AffirmativeSticky = cursorAffirmative(opts)
	return a, true
}

// cursorParseTrustApproval recognizes cursor-agent's one-time workspace-trust prompt,
// shown when launching interactively in an untrusted directory (a fresh warden
// worktree). Captured live:
//
//	╭───────────────────────────────────────────────╮
//	│  ⚠ Workspace Trust Required                     │
//	│  Cursor Agent can execute code and access …     │
//	│  Do you trust the contents of this directory?   │
//	│    /path/to/workdir                             │
//	│  ▶ [a] Trust this workspace                      │
//	│    [q] Quit                                     │
//	╰───────────────────────────────────────────────╯
//
// warden surfaces it as an Approval so the operator can clear it from the approvals
// inbox instead of attaching to the pane — the maintainer's ruling is that trust is a
// 1-time manual step, not a launch blocker. Only the bordered box interior is scanned
// (so the shell scrollback / tmux titlebar above it is ignored). The affirmative
// ("Trust this workspace") is a standing grant — cursor persists it to
// ~/.cursor/projects/<…>/.workspace-trusted — so AffirmativeSticky is true. Returns
// (nil,false) when the trust banner is absent.
func cursorParseTrustApproval(pane string) (*agentbackend.Approval, bool) {
	if !strings.Contains(pane, "Workspace Trust Required") &&
		!strings.Contains(pane, "Do you trust the contents of this directory?") {
		return nil, false
	}

	var opts []string
	sel, action := 0, ""
	for _, raw := range strings.Split(pane, "\n") {
		if !strings.Contains(raw, "│") {
			continue // only the bordered box interior carries the prompt
		}
		t := cursorStripBox(raw)
		cursored := strings.HasPrefix(t, "▶") // "▶" marks the highlighted entry
		t = strings.TrimSpace(strings.TrimPrefix(t, "▶"))
		if m := cursorTrustOptionRe.FindStringSubmatch(t); m != nil {
			opts = append(opts, strings.TrimSpace(m[1]))
			if cursored {
				sel = len(opts)
			}
			continue
		}
		// The directory under question is the first path line inside the box.
		if action == "" && strings.HasPrefix(t, "/") {
			action = t
		}
	}
	if len(opts) < 2 {
		return nil, false
	}

	a := &agentbackend.Approval{
		Action:            action,
		Question:          "Do you trust the contents of this directory?",
		Options:           opts,
		SelectedIdx:       sel,
		AffirmativeSticky: true, // trusting persists to .workspace-trusted (a standing grant)
	}
	for i, o := range opts {
		if strings.Contains(strings.ToLower(o), "trust") {
			a.AffirmativeIdx = i + 1
			break
		}
	}
	return a, true
}

// cursorStripBox trims the box-drawing border cursor-agent draws around the
// workspace-trust prompt (the vertical bars "│" U+2502 and surrounding whitespace),
// leaving the inner text.
func cursorStripBox(line string) string {
	return strings.TrimSpace(strings.Trim(strings.TrimSpace(line), "│"))
}

// cursorAffirmative picks the least-privilege affirmative option from a cursor
// command-approval menu for the neutral Approval.AffirmativeIdx/Sticky fields.
// cursor's negative is "Skip" ("Skip (esc or n)"); among the affirmatives, a standing
// grant carries an "allowlist"/"everything"/"always"/"don't ask" clause (sticky). It
// returns the 1-based index of the first non-sticky affirmative ("Run (once)");
// failing that the first sticky affirmative; otherwise (0,false) when only a negative
// is offered.
func cursorAffirmative(opts []string) (idx int, sticky bool) {
	stickyIdx := 0
	for i, opt := range opts {
		low := strings.ToLower(opt)
		if cursorNegativeOption(low) {
			continue
		}
		if strings.Contains(low, "allowlist") || strings.Contains(low, "everything") ||
			strings.Contains(low, "always") || strings.Contains(low, "don't ask") {
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

// cursorNegativeOption reports whether a menu label is a decline ("Skip", "No",
// "Reject", "Cancel", "Deny") rather than an affirmative.
func cursorNegativeOption(low string) bool {
	for _, n := range []string{"skip", "no,", "no ", "reject", "cancel", "deny", "abort"} {
		if strings.HasPrefix(low, n) {
			return true
		}
	}
	return low == "no"
}

// --- System prompt / pricing ------------------------------------------------

// SystemPromptFlag reports no launch-time system-prompt injection: cursor-agent has
// no --append-system-prompt equivalent on its launch command (its customization is
// rules / AGENTS.md based; Caps.SystemPromptInject stays false — that flag means
// specifically a launch-time flag). warden instead delivers the same
// pipeline/collab/git addendum out-of-band via the AGENTS.md rules file cursor-agent
// reads on startup — see InjectContext (agentbackend.ContextInjector) and the gap doc.
func (Cursor) SystemPromptFlag(string) (string, bool) { return "", false }

// cursorRulesFile is the rules file cursor-agent reads on startup. The Cursor CLI
// reads AGENTS.md (and CLAUDE.md) at the project root and applies it as rules
// alongside .cursor/rules — verified: cursor.com/docs/cli — so warden writes its
// addendum into the cross-tool-standard <workdir>/AGENTS.md rather than the
// .mdc-formatted .cursor/rules tree.
const cursorRulesFile = "AGENTS.md"

// InjectContext implements agentbackend.ContextInjector. cursor-agent has no
// --append-system-prompt flag (Caps.SystemPromptInject=false) but reads an AGENTS.md
// rules file from its working directory on startup, so warden delivers its
// collab/git/pipeline addendum by writing that text into <workdir>/AGENTS.md.
// Lifecycle calls this post-worktree-creation / pre-launch so the file is present
// when cursor-agent starts. The no-clobber/idempotent/git-exclude write is the shared
// writeRulesFile helper (see inject.go and docs/agent-backends/cursor.md).
func (Cursor) InjectContext(workdir, text string) error {
	return writeRulesFile(workdir, cursorRulesFile, text)
}

// Pricing reports no pricing table. Cursor is a hosted plan: it surfaces token counts
// (the stream-json `result.usage` carries input/output/cache tokens) but never a
// per-call dollar figure — billing is against the user's Cursor subscription — so
// warden cannot enumerate per-model dollar rates here. Per design §5 spend shows
// tokens (not dollars) and savings omits the agent. Wiring warden to Cursor's native
// token usage is deferred (docs/agent-backends/cursor.md).
func (Cursor) Pricing() (agentbackend.PricingTable, bool) {
	return agentbackend.PricingTable{}, false
}

// --- Model menu -------------------------------------------------------------

// cursorListModelsCmd runs `cursor-agent --list-models` and returns its stdout. It is
// a package var so the parser test can exercise ListModels without the real binary.
// Listing the menu is a metadata read — `--list-models` is documented as "List
// available models and exit", it does NOT start a chat or a turn — so it costs no
// generation allowance against the hosted plan (verified live, cursor-agent
// 2026.06.26-7079533: exit 0, clean stdout, no turn started, plan untouched).
var cursorListModelsCmd = func() ([]byte, error) {
	return exec.Command("cursor-agent", "--list-models").Output()
}

// ListModels implements agentbackend.ModelLister. Cursor is hosted with a live,
// multi-vendor menu (Composer, Claude, GPT/Codex, Gemini, Grok, …) the operator's
// account/plan can change, so warden surfaces the real `cursor-agent --list-models`
// output rather than a hard-coded alias table. The returned ids feed warden's
// `--model` flag verbatim — they are the exact ids `--model` accepts (and that the
// tool's own "use --model <id>" tip points at). ok=false on any command error (binary
// missing, not signed in) so `wd models` degrades cleanly.
func (Cursor) ListModels() ([]string, bool) {
	out, err := cursorListModelsCmd()
	if err != nil {
		return nil, false
	}
	return parseCursorModels(out), true
}

// parseCursorModels normalizes `cursor-agent --list-models` stdout into a clean
// []string of model ids. The command prints an "Available models" header, a blank
// line, then one model per line as "<id> - <Display Name>", and a trailing blank line
// + "Tip: …" footer (verified live, cursor-agent 2026.06.26-7079533):
//
//	Available models
//
//	auto - Auto
//	gpt-5.3-codex-low - Codex 5.3 Low
//	…
//	glm-5.2-max - GLM 5.2 Max
//
//	Tip: use --model <id> (or /model <id> …) to switch. …
//
// so the parse keeps only lines carrying the " - " id/name separator (the header and
// the Tip footer have none and drop out), takes the id to the left of the first
// separator, trims it, and preserves order. Returns an empty (never nil) slice when
// there is nothing to list, so JSON callers emit [] not null.
func parseCursorModels(out []byte) []string {
	models := []string{}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		// Model lines are "<id> - <Display Name>"; the header/tip lines carry no
		// " - " separator and are skipped.
		idx := strings.Index(line, " - ")
		if idx < 0 {
			continue
		}
		if id := strings.TrimSpace(line[:idx]); id != "" {
			models = append(models, id)
		}
	}
	return models
}

// --- Capabilities -----------------------------------------------------------

// Capabilities reports Cursor as an experimental **Tier-C** backend: resume works
// (dir/workspace-scoped `--continue`; exact-id once discover-then-pin lands) and
// there is a headless one-shot, but the interactive transcript is an unreadable
// SQLite store so StructuredTranscript is off (digests degrade). Cursor mints its own
// chatId (no SessionIDControl) and exposes no warden-side dollar pricing (hosted
// plan). SystemPromptInject stays false (cursor-agent has no launch-time
// system-prompt flag) — but warden's addendum still reaches it out-of-band via the
// AGENTS.md rules file (InjectContext); the Caps flag tracks the launch-flag
// specifically. PermissionModes surface Cursor's native execution/approval vocabulary.
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

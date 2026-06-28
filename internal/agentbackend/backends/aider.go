package backends

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
)

func init() { agentbackend.Register(Aider{}) }

// Aider is the Backend adapter for Aider (the `aider` CLI) — warden's Tier-C
// proof backend. It validates the interface shape against a non-Claude agent:
// a markdown transcript (not JSONL), no assignable/resumable session id, a
// simple y/n approval UI, and a bring-your-own-model pricing story.
//
// Tier decision: Aider is shipped as **Tier A** for transcripts. Its
// .aider.chat.history.md is regular enough to parse into neutral Turns with good
// fidelity (session headers, `#### ` user prompts, fenced edit blocks, and
// `> Applied edit to <file>` lines), so digests run on real structured data.
// Pricing is off (BYO model), so spend/savings degrade per design §5.
type Aider struct{}

// --- Identity ---------------------------------------------------------------

func (Aider) ID() string          { return "aider" }
func (Aider) DisplayName() string { return "Aider" }
func (Aider) Binary() string      { return "aider" }
func (Aider) InstallHint() string {
	return "Install Aider: python -m pip install aider-install && aider-install\nOr: uv tool install aider (https://aider.chat/docs/install.html)"
}

// --- Launch / resume --------------------------------------------------------

// aiderAuto reports whether a warden permission mode should map to Aider's
// --yes-always (auto-approve) flag. Aider has only two real modes — prompt
// (default) and auto — so warden's richer Claude modes are folded onto them:
// any "just do it" mode becomes --yes-always; the cautious modes stay
// interactive. This lets `--permission-mode` flow through for Aider without a
// backend-aware validation gate.
func aiderAuto(mode string) bool {
	switch mode {
	case "yes-always", "auto", "acceptEdits", "bypassPermissions":
		return true
	default:
		return false
	}
}

// LaunchCmd builds the interactive `aider` invocation for a tmux pane.
// --no-show-model-warnings suppresses the blocking "unknown model" confirmation
// so a BYO/Ollama model id doesn't strand the agent at a y/n prompt on startup.
// Model is omitted when empty: Aider is bring-your-own-model, so an empty model
// (the Claude default never applies here — lifecycle passes it through) lets
// Aider fall back to its own configured default rather than a Claude alias it
// can't resolve. SessionID/Name are ignored: Aider has no assignable session id
// (SessionIDControl=false) and no session-name flag.
func (Aider) LaunchCmd(o agentbackend.LaunchOpts) string {
	cmd := "aider --no-show-model-warnings"
	if o.Model != "" {
		cmd += " --model " + shellQuoteArg(o.Model)
	}
	if aiderAuto(o.Mode) {
		cmd += " --yes-always"
	}
	return cmd
}

// ResumeCmd reports no resume support: Aider continues from the repo's chat
// history rather than a session id warden can pin, so rotate/handoff re-spawn
// fresh instead (design §5, Caps.Resume=false).
func (Aider) ResumeCmd(agentbackend.ResumeOpts) (string, bool) { return "", false }

// LaunchPromptArg returns "" — Aider is seeded after launch (PromptSeeder), not on
// the launch line. Aider treats trailing positionals as files to add to the chat,
// and its only launch-line prompt path (--message) runs the task and then EXITS
// (a one-shot, which surfaced as the agent dropping straight to "done"). To make
// Aider a persistent interactive agent like the other backends, warden launches the
// bare REPL and types the prompt in once it's ready (PromptText/ReadyMarker). The
// headless one-shot path (HeadlessCmd) still uses --message for classify/summarize.
func (Aider) LaunchPromptArg(string) string { return "" }

// PromptText / ReadyMarker implement agentbackend.PromptSeeder: warden types the
// task into Aider's interactive REPL once startup has finished. ReadyMarker keys on
// the "Repo-map:" startup line Aider prints just before the input prompt becomes
// interactive; if it never appears (e.g. repo-map disabled) the lifecycle falls
// back to a settle delay.
func (Aider) PromptText(prompt string) (string, bool) { return prompt, prompt != "" }
func (Aider) ReadyMarker() string                     { return "Repo-map:" }

// HeadlessCmd returns the argv for a headless one-shot used by warden's own
// classify/summarize offload when Aider is the default backend. It runs a single
// message with auto-approve and no auto-commit so it never blocks or mutates the
// repo. (warden's default backend is Claude, so this path is rarely exercised
// for Aider; it exists for completeness and to honor Caps.Headless=true.)
func (Aider) HeadlessCmd(prompt string) ([]string, bool) {
	return []string{"aider", "--no-show-model-warnings", "--yes-always", "--no-auto-commits", "--message", prompt}, true
}

// --- Transcript -------------------------------------------------------------

// aiderTranscriptName is the markdown chat log Aider writes in the repo root.
const aiderTranscriptName = ".aider.chat.history.md"

// TranscriptPath resolves Aider's markdown transcript. It lives in the working
// directory (not under a projects dir keyed by a session id), so projectsDir and
// sessionID are ignored — Aider has no assignable session id. ok=false until the
// file exists, so the digest path shows "no transcript" rather than erroring.
func (Aider) TranscriptPath(_, workdir, _ string) (string, bool) {
	if workdir == "" {
		return "", false
	}
	p := filepath.Join(workdir, aiderTranscriptName)
	if _, err := os.Stat(p); err != nil {
		return "", false
	}
	return p, true
}

var (
	// aiderSessionHeaderRe matches a chat-session boundary line.
	aiderSessionHeaderRe = regexp.MustCompile(`^# aider chat started at (.+)$`)
	// aiderAppliedRe captures the file an edit was applied to (the reliable
	// edited-file signal — more robust than the bare filename before a fence).
	aiderAppliedRe = regexp.MustCompile(`^Applied edit to (.+)$`)
)

// aiderTimeLayout is how Aider stamps a session header.
const aiderTimeLayout = "2006-01-02 15:04:05"

// ParseTranscript normalizes Aider's .aider.chat.history.md markdown into
// warden's neutral []Turn. The format is line-oriented:
//   - `# aider chat started at <ts>` — a session boundary (timestamps the turns
//     that follow).
//   - `#### <text>` — a user prompt → a user Turn.
//   - `> <text>` — Aider's own narration/banner; mostly skipped, except
//     `> Applied edit to <file>` which attributes an edited file to the current
//     assistant turn.
//   - everything else (the bare filename line + fenced code of an edit, or model
//     prose) — the assistant's response body.
//
// A reader error is returned; malformed structure is tolerated (best-effort, as
// with the Claude JSONL parser).
func (Aider) ParseTranscript(r io.Reader) ([]agentbackend.Turn, error) {
	var turns []agentbackend.Turn
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	var sessTime time.Time
	var body strings.Builder
	var files []string
	inAssistant := false
	hasEdit := false

	flush := func() {
		if !inAssistant {
			return
		}
		text := strings.TrimSpace(body.String())
		if text != "" || hasEdit || len(files) > 0 {
			tool := ""
			if hasEdit {
				tool = "edit"
			}
			turns = append(turns, agentbackend.Turn{
				Role: "assistant", Text: text, ToolName: tool, Files: files, Timestamp: sessTime,
			})
		}
		body.Reset()
		files = nil
		hasEdit = false
		inAssistant = false
	}

	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), " \t") // strip markdown hard-break spaces
		switch {
		case aiderSessionHeaderRe.MatchString(line):
			flush()
			m := aiderSessionHeaderRe.FindStringSubmatch(line)
			if t, err := time.Parse(aiderTimeLayout, strings.TrimSpace(m[1])); err == nil {
				sessTime = t
			}
		case strings.HasPrefix(line, "#### "):
			flush()
			turns = append(turns, agentbackend.Turn{
				Role: "user", Text: strings.TrimSpace(line[len("#### "):]), Timestamp: sessTime,
			})
			inAssistant = true // the model's response follows
		case strings.HasPrefix(line, "> "):
			content := strings.TrimSpace(line[len("> "):])
			if m := aiderAppliedRe.FindStringSubmatch(content); m != nil {
				files = appendUnique(files, strings.TrimSpace(m[1]))
				hasEdit = true
				inAssistant = true // an applied edit implies an assistant response
			}
			// other "> " lines are Aider's banner/narration — skipped from Text
		case line == "```" || strings.HasPrefix(line, "```"):
			// fence markers delimit an edit block; the code inside is captured by
			// the default branch below.
		default:
			if inAssistant && line != "" {
				body.WriteString(line)
				body.WriteByte('\n')
			}
		}
	}
	flush()
	return turns, sc.Err()
}

// appendUnique appends s to dst unless already present (first-seen order).
func appendUnique(dst []string, s string) []string {
	for _, e := range dst {
		if e == s {
			return dst
		}
	}
	return append(dst, s)
}

// --- State / approval -------------------------------------------------------

// aiderApprovalRe matches Aider's y/n confirmation prompts, e.g.
// "Add .aider* to .gitignore (recommended)? (Y)es/(N)o [Yes]:" or the
// multi-option "(Y)es/(N)o/(A)ll/(S)kip all [Yes]:". Far simpler than Claude's
// box-drawing UI: a "(Y)es" token followed by a "(N)o" token on the same line.
var aiderApprovalRe = regexp.MustCompile(`(?i)\(y\)es/\(n\)o`)

// aiderOptionRe captures each "(X)label" option token in an approval line.
var aiderOptionRe = regexp.MustCompile(`\(([A-Za-z])\)([A-Za-z][\w' ]*?)(?:/|\s*\[|\s*:|\s*$)`)

// aiderDefaultRe captures the bracketed default option, e.g. "[Yes]".
var aiderDefaultRe = regexp.MustCompile(`\[([A-Za-z][\w ]*)\]`)

// DetectState maps a captured tmux pane to a neutral run state. Aider exposes a
// clear "needs input" signal — its y/n prompt — which is the state warden most
// needs to act on. It has no reliable static "working" marker (the spinner is
// drawn by prompt_toolkit and leaves no stable text), so anything that is not an
// open approval is reported Unknown and warden infers idle from staleness, the
// same conservative stance the Claude adapter takes for idle.
func (Aider) DetectState(pane string) agentbackend.State {
	if aiderApprovalRe.MatchString(pane) {
		return agentbackend.StateNeedsInput
	}
	return agentbackend.StateUnknown
}

// ParseApproval normalizes Aider's y/n prompt into the neutral Approval. It scans
// for the prompt line, parses its "(X)label" options in order, and resolves the
// bracketed default ([Yes]) to the selected/affirmative index.
func (Aider) ParseApproval(pane string) (*agentbackend.Approval, bool) {
	line := aiderApprovalLine(pane)
	if line == "" {
		return nil, false
	}
	a := &agentbackend.Approval{}

	// Question = the text before the first "(Y)es" option.
	if idx := aiderApprovalRe.FindStringIndex(line); idx != nil {
		a.Question = strings.TrimSpace(strings.TrimRight(line[:idx[0]], " ?"))
	}

	for _, m := range aiderOptionRe.FindAllStringSubmatch(line, -1) {
		label := strings.TrimSpace(m[1] + m[2]) // letter + remainder, e.g. "Y"+"es"
		a.Options = append(a.Options, label)
		if strings.EqualFold(label, "Yes") {
			a.AffirmativeIdx = len(a.Options) // 1-based; least-privilege "yes"
		}
	}

	if dm := aiderDefaultRe.FindStringSubmatch(line); dm != nil {
		for i, opt := range a.Options {
			if strings.EqualFold(opt, strings.TrimSpace(dm[1])) {
				a.SelectedIdx = i + 1
				break
			}
		}
	}
	return a, true
}

// aiderApprovalLine returns the first pane line that carries a y/n prompt, or "".
func aiderApprovalLine(pane string) string {
	for _, l := range strings.Split(pane, "\n") {
		if aiderApprovalRe.MatchString(l) {
			return strings.TrimSpace(l)
		}
	}
	return ""
}

// --- System prompt / pricing ------------------------------------------------

// SystemPromptFlag reports no system-prompt injection: Aider has no
// --append-system-prompt equivalent (its convention mechanism is a --read file),
// so warden's pipeline/collab/git hints are skipped for Aider agents
// (Caps.SystemPromptInject=false).
func (Aider) SystemPromptFlag(string) (string, bool) { return "", false }

// Pricing reports no pricing table: Aider is bring-your-own-model (any of dozens
// of providers / local Ollama), so warden cannot price its tokens. Per design
// §5, `wd spend` shows tokens (heuristic) but not dollars and `wd savings` omits
// the agent (Caps via Pricing=false).
func (Aider) Pricing() (agentbackend.PricingTable, bool) {
	return agentbackend.PricingTable{}, false
}

// --- Capabilities -----------------------------------------------------------

// Capabilities reports Aider as a Tier-C backend: headless-friendly and
// structured-transcript (Tier A digests), but with no assignable/resumable
// session id, no system-prompt injection, and no known pricing.
func (Aider) Capabilities() agentbackend.Caps {
	return agentbackend.Caps{
		Resume:               false,
		Headless:             true,
		ModelSelection:       true,
		PermissionModes:      []string{"default", "yes-always"},
		StructuredTranscript: true,
		SystemPromptInject:   false,
		SessionIDControl:     false,
	}
}

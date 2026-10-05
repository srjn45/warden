package fastbrain

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Compact zero-shot prompts for the arbiter. Each demands a single JSON object.
const (
	toolPermissionSystem = `You are a security gate for a coding agent. Judge whether the requested tool/command is safe, non-destructive and in scope for the stated goal. Reply with ONLY this JSON, no prose: {"approve": true|false, "confidence": 0.0-1.0, "reason": "<short>"}`

	strategicQuestionSystem = `You are a decision assistant for a coding agent. Pick exactly one numbered option that best serves the goal within the constraints. Reply with ONLY this JSON, no prose: {"selected_option": <number>, "confidence": 0.0-1.0, "reason": "<short>"}`
)

func renderContext(in ArbiterInput) string {
	var b strings.Builder
	if in.Goal != "" {
		fmt.Fprintf(&b, "Goal: %s\n", in.Goal)
	}
	if in.Constraints != "" {
		fmt.Fprintf(&b, "Constraints: %s\n", in.Constraints)
	}
	return b.String()
}

func toolPermissionPrompt(in ArbiterInput) string {
	a := in.Approval
	return fmt.Sprintf("%s\n\n%sTool/command: %s\nPrompt: %s\n", toolPermissionSystem, renderContext(in), a.Action, a.Question)
}

func strategicQuestionPrompt(in ArbiterInput) string {
	a := in.Approval
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n%s", strategicQuestionSystem, renderContext(in))
	if a.Action != "" {
		fmt.Fprintf(&b, "Context: %s\n", a.Action)
	}
	fmt.Fprintf(&b, "Question: %s\nOptions:\n", a.Question)
	for i, o := range a.Options {
		fmt.Fprintf(&b, "%d. %s\n", i+1, o)
	}
	return b.String()
}

// Prompts for the universal micro-cognition kinds. Each demands one JSON
// object and has a deterministic fallback parser (never errors).
const (
	classifyTaskSystem   = `Classify the coding task. Reply with ONLY this JSON, no prose: {"type":"test|implementation|review|refactor|docs|other","confidence":0.0-1.0}`
	summarizeActivitySys = `Summarize what the agent is doing in one short sentence (max 12 words). Reply with ONLY this JSON, no prose: {"summary":"<text>"}`
	summarizeCheckSystem = `Condense this test/linter output into the actionable failure lines (file:line + cause), max 15 lines. Reply with ONLY this JSON, no prose: {"summary":"<text>"}`
	commitMessageSystem  = `Write a conventional commit message for this diff: "type: subject" (<=72 chars, imperative). Reply with ONLY this JSON, no prose: {"message":"type: subject"}`
	prSummarySystem      = `Write a pull-request title and description for this change. "title": Conventional Commits style "type(scope): subject", imperative, <=72 chars. "body": short markdown with a "## What changed" section and a "## Why" section. Reply with ONLY this JSON, no prose: {"title":"type: subject","body":"## What changed\n- ...\n\n## Why\n..."}`
	curateExtractSystem  = `Extract durable, reusable project facts (decisions, conventions, gotchas) from the text as short bullets. Skip transient chatter. Reply with ONLY this JSON, no prose: {"entries":["<fact>"]}`
	replTurnSystem       = `You are a tool-using assistant. Answer briefly, or call tools from the list. Reply with ONLY this JSON, no prose outside it: {"text":"<reply or empty>","tool_calls":[{"name":"<tool>","args":{}}]}`
	classifyTaskFallback = "other"
	maxPromptInputBytes  = 8000
)

var taskTypes = map[string]bool{"test": true, "implementation": true, "review": true, "refactor": true, "docs": true, "other": true}

func clip(s string) string {
	if len(s) > maxPromptInputBytes {
		return s[:maxPromptInputBytes]
	}
	return s
}

// ClassifyTaskPrompt builds the KindClassifyTask prompt.
func ClassifyTaskPrompt(prompt string) string {
	return classifyTaskSystem + "\n\nTask: " + clip(prompt) + "\n"
}

// SummarizeActivityPrompt builds the KindSummarizeActivity prompt.
func SummarizeActivityPrompt(activity string) string {
	return summarizeActivitySys + "\n\nActivity:\n" + clip(activity) + "\n"
}

// SummarizeCheckPrompt builds the KindSummarizeCheck prompt.
func SummarizeCheckPrompt(output string) string {
	return summarizeCheckSystem + "\n\nOutput:\n" + clip(output) + "\n"
}

// CommitMessagePrompt builds the KindCommitMessage prompt.
func CommitMessagePrompt(diff string) string {
	return commitMessageSystem + "\n\nDiff:\n" + clip(diff) + "\n"
}

// PRSummaryPrompt builds the KindPRSummary prompt from the agent's task, the
// `git diff --stat` against the PR base and the commit subjects on the branch.
// The combined input is capped (head kept) so a huge branch cannot blow the
// prompt budget.
func PRSummaryPrompt(task, diffStat, commits string) string {
	cap := func(s string, n int) string {
		s = strings.TrimSpace(s)
		if len(s) > n {
			s = strings.ToValidUTF8(s[:n], "")
		}
		return s
	}
	return prSummarySystem + "\n\nTask:\n" + cap(task, 2000) + "\n\nDiff stat:\n" + cap(diffStat, 4000) +
		"\n\nCommits:\n" + cap(commits, 2000) + "\n"
}

// PRSummaryMaxTitle is the hard cap on a drafted PR title.
const PRSummaryMaxTitle = 72

// ParsePRSummary parses {"title":"...","body":"..."}. It never errors: a
// non-OK response yields ("",""), and each field is "" when absent or blank so
// the caller can fall back per field. The title is reduced to its first line
// and capped at PRSummaryMaxTitle bytes (with an ellipsis).
func ParsePRSummary(r Response) (title, body string) {
	title = stringField(r, "title")
	if i := strings.IndexByte(title, '\n'); i >= 0 {
		title = strings.TrimSpace(title[:i])
	}
	if len(title) > PRSummaryMaxTitle {
		title = strings.TrimSpace(strings.ToValidUTF8(title[:PRSummaryMaxTitle-1], "")) + "…"
	}
	return title, stringField(r, "body")
}

// CurateExtractPrompt builds the KindCurateExtract prompt.
func CurateExtractPrompt(text string) string {
	return curateExtractSystem + "\n\nText:\n" + clip(text) + "\n"
}

func stringField(r Response, key string) string {
	if !r.OK() {
		return ""
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(r.Output.Parsed, &m) != nil {
		return ""
	}
	var s string
	if json.Unmarshal(m[key], &s) != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

// ParseClassifyTask returns the task type and confidence; any non-OK response
// or unknown type yields the "other" fallback.
func ParseClassifyTask(r Response) (string, float64) {
	t := strings.ToLower(stringField(r, "type"))
	if !taskTypes[t] {
		return classifyTaskFallback, 0
	}
	c, _ := extractMeta(r.Output.Parsed)
	return t, c
}

// ParseSummary parses {"summary":"..."} (activity and check kinds); "" on failure.
func ParseSummary(r Response) string { return stringField(r, "summary") }

// ParseCommitMessage parses {"message":"..."}; "" means use the deterministic
// commit message.
func ParseCommitMessage(r Response) string {
	m := stringField(r, "message")
	if i := strings.IndexByte(m, '\n'); i >= 0 {
		m = strings.TrimSpace(m[:i])
	}
	return m
}

// ParseCurateEntries parses {"entries":[...]}; empty on failure. Blank entries
// are dropped.
func ParseCurateEntries(r Response) []string {
	if !r.OK() {
		return nil
	}
	var m struct {
		Entries []string `json:"entries"`
	}
	if json.Unmarshal(r.Output.Parsed, &m) != nil {
		return nil
	}
	var out []string
	for _, e := range m.Entries {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}

// ReplToolCall is one parsed tool call from a REPL turn.
type ReplToolCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

// ReplTurn is the parsed KindReplTurn reply.
type ReplTurn struct {
	Text      string         `json:"text"`
	ToolCalls []ReplToolCall `json:"tool_calls"`
}

// ParseReplTurn parses a REPL turn; the zero ReplTurn (empty reply) on failure.
// Calls with an empty name are dropped.
func ParseReplTurn(r Response) ReplTurn {
	if !r.OK() {
		return ReplTurn{}
	}
	var t ReplTurn
	if json.Unmarshal(r.Output.Parsed, &t) != nil {
		return ReplTurn{}
	}
	t.Text = strings.TrimSpace(t.Text)
	calls := t.ToolCalls[:0]
	for _, c := range t.ToolCalls {
		if strings.TrimSpace(c.Name) == "" {
			continue
		}
		if c.Args == nil {
			c.Args = map[string]any{}
		}
		calls = append(calls, c)
	}
	t.ToolCalls = calls
	return t
}

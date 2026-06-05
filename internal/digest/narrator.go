package digest

import (
	"context"
	"fmt"
	"strings"
)

// ClaudeNarrator is the real Narrator: it shells `claude -p` through the Run
// func (wired to lifecycle's bounded claude -p plumbing). Run is the only seam,
// so tests inject a canned function and stay offline.
type ClaudeNarrator struct {
	Run func(ctx context.Context, arg string) (string, error)
}

// Summarize asks the model for a 1–2 sentence "what this agent did" line.
func (n ClaudeNarrator) Summarize(ctx context.Context, f Facts) (string, error) {
	out, err := n.Run(ctx, NarratorPrompt(f))
	if err != nil {
		return "", err
	}
	return stripPreamble(cleanLine(out)), nil
}

// NarratorPrompt builds the compact prompt from deterministic facts. The
// instruction is deliberately blunt about omitting preamble: the narrator's
// `claude -p` inherits the project's skill/CLAUDE.md context and otherwise
// tends to narrate its meta-reasoning ("No skill applies.", "This is a
// summarization task.") before the actual summary.
func NarratorPrompt(f Facts) string {
	var b strings.Builder
	b.WriteString("Summarize, in 1-2 sentences, what a coding agent accomplished. ")
	b.WriteString("Output ONLY the summary itself — start with the first word of the summary. ")
	b.WriteString("Do NOT restate this request, do NOT mention skills or instructions, ")
	b.WriteString("do NOT add any preamble, label, or quotes.\n\n")
	if f.Task != "" {
		fmt.Fprintf(&b, "Task: %s\n", f.Task)
	}
	if len(f.EditedFiles) > 0 {
		fmt.Fprintf(&b, "Files edited: %s\n", strings.Join(f.EditedFiles, ", "))
	}
	if f.LastMessage != "" {
		fmt.Fprintf(&b, "Agent's last message: %s\n", f.LastMessage)
	}
	return b.String()
}

// cleanLine collapses a model reply to a trimmed single paragraph.
func cleanLine(out string) string {
	s := strings.TrimSpace(out)
	return strings.Join(strings.Fields(s), " ")
}

// metaMarkers are self-referential phrases that mark a fragment as model
// preamble (narration about the summarization task / skills / instructions)
// rather than a description of what the agent did. They are deliberately
// specific so they don't fire on a genuine summary that merely mentions a
// "task" or a "summary" the agent produced.
var metaMarkers = []string{
	"no skill", "skill applies", "no relevant skill", "skill is needed", "skills apply",
	"the instruction is", "the instructions are", "per the instruction", "instruction is clear",
	"based on the agent", "based on the last", "based on the transcript", "based on what",
	"here is the summary", "here's the summary", "here is a summary", "here's a summary",
	"1-2 sentence", "1 to 2 sentence", "one to two sentence", "in two sentences",
	"what the agent did", "what this agent did", "what the agent accomplished",
	"this is a summarization", "this is just a summarization", "summarization task",
	"plain summary", "plain-language summary", "a plain summary",
}

func isMetaFragment(seg string) bool {
	l := strings.ToLower(seg)
	for _, m := range metaMarkers {
		if strings.Contains(l, m) {
			return true
		}
	}
	return false
}

// sentenceEnd returns the index just past the first sentence terminator that is
// followed by a space (i.e. an internal sentence boundary), or -1 if there is
// none — so the final/only sentence is never treated as a leading fragment.
func sentenceEnd(s string) int {
	for i := 0; i+1 < len(s); i++ {
		if (s[i] == '.' || s[i] == '!' || s[i] == '?') && s[i+1] == ' ' {
			return i + 1
		}
	}
	return -1
}

// stripPreamble peels leading model preamble off a summary. The narrator's
// `claude -p` sometimes prepends meta-narration in free-form phrasings, as a
// colon lead-in ("Based on the agent's last message: <summary>") or as whole
// meta sentences ("No skill applies. <summary>"). We drop a leading fragment
// only when it is clearly meta (see metaMarkers), stopping at the first real
// sentence. Conservative: if peeling would leave nothing, the original is kept.
func stripPreamble(s string) string {
	out := strings.TrimSpace(s)
	for {
		// Colon lead-in: a meta phrase before a ':' that precedes the first '.'.
		if ci := strings.IndexByte(out, ':'); ci > 0 {
			pi := strings.IndexByte(out, '.')
			if (pi == -1 || ci < pi) && isMetaFragment(out[:ci]) {
				out = strings.TrimSpace(out[ci+1:])
				continue
			}
		}
		// Leading meta sentence.
		if si := sentenceEnd(out); si > 0 && isMetaFragment(out[:si]) {
			out = strings.TrimSpace(out[si:])
			continue
		}
		break
	}
	if out == "" {
		return strings.TrimSpace(s)
	}
	return out
}

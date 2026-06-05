package digest

import (
	"context"
	"fmt"
	"regexp"
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

// NarratorPrompt builds the compact prompt from deterministic facts.
func NarratorPrompt(f Facts) string {
	var b strings.Builder
	b.WriteString("You are summarizing what a coding agent accomplished. ")
	b.WriteString("In 1-2 sentences, plainly describe what it did. Reply with ONLY the summary.\n\n")
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

// preamblePrefixes are leading meta-fragments the model sometimes prepends
// despite being told to reply with ONLY the summary (e.g. "This is a
// summarization task.", "No skill applies.", "Based on the agent's last
// message:"). Each matches only at the very start of the (whitespace-collapsed)
// reply; stripPreamble peels them off until the real summary remains.
var preamblePrefixes = []*regexp.Regexp{
	regexp.MustCompile(`(?is)^this is (?:just |simply |really )?an?\b[^.:]*\btask\b[^.:]*[.:]\s*`),
	regexp.MustCompile(`(?is)^no skills? (?:applies?|apply|are needed|is needed|needed)[^.]*\.\s*`),
	regexp.MustCompile(`(?is)^based on [^.:]*:\s*`),
	regexp.MustCompile(`(?is)^here(?:'s| is)[^.:]*:\s*`),
}

// stripPreamble removes leading model preamble. It is conservative: if peeling
// would leave nothing, it returns the original text unchanged.
func stripPreamble(s string) string {
	out := strings.TrimSpace(s)
	for {
		matched := false
		for _, re := range preamblePrefixes {
			if loc := re.FindStringIndex(out); loc != nil {
				out = strings.TrimSpace(out[loc[1]:])
				matched = true
				break
			}
		}
		if !matched {
			break
		}
	}
	if out == "" {
		return strings.TrimSpace(s)
	}
	return out
}

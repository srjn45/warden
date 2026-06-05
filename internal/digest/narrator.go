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
	return cleanLine(out), nil
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
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.Join(strings.Fields(s), " ")
}

package fastbrain

import (
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

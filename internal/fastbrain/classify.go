package fastbrain

import (
	"regexp"
	"strings"

	"github.com/srjn45/warden/internal/agentbackend"
)

// PromptCategory is the deterministic class of an agent prompt.
type PromptCategory string

const (
	// CategoryToolPermission is a yes/no grant for a tool or command.
	CategoryToolPermission PromptCategory = "tool_permission"
	// CategoryStrategicQuestion is anything else: trust/workspace prompts,
	// which-option/architecture questions, menus without a safe "yes".
	CategoryStrategicQuestion PromptCategory = "strategic_question"
)

var (
	toolShape = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]*\(.*\)\s*$`)
	yesWords  = []string{"yes", "allow", "approve", "run", "proceed", "accept", "ok", "continue"}
	noWords   = []string{"no", "skip", "deny", "reject", "decline", "cancel", "don't", "do not"}
)

func startsWithWord(label string, words []string) bool {
	l := strings.ToLower(strings.TrimSpace(label))
	for _, w := range words {
		if l == w || strings.HasPrefix(l, w+" ") || strings.HasPrefix(l, w+",") || strings.HasPrefix(l, w+"(") {
			return true
		}
	}
	return false
}

// ClassifyPrompt deterministically classifies an approval prompt without AI.
// nil or empty prompts classify as CategoryStrategicQuestion: the safe default
// is to never treat an unrecognized prompt as an auto-grantable tool permission.
func ClassifyPrompt(a *agentbackend.Approval) PromptCategory {
	if a == nil || (a.Action == "" && a.Question == "" && len(a.Options) == 0) {
		return CategoryStrategicQuestion
	}
	// Workspace/folder trust is a standing, strategic grant, not a tool call.
	if strings.Contains(strings.ToLower(a.Question), "trust") {
		return CategoryStrategicQuestion
	}
	if a.AffirmativeIdx <= 0 || a.AffirmativeIdx > len(a.Options) {
		return CategoryStrategicQuestion
	}
	if toolShape.MatchString(strings.TrimSpace(a.Action)) {
		return CategoryToolPermission
	}
	if !startsWithWord(a.Options[a.AffirmativeIdx-1], yesWords) {
		return CategoryStrategicQuestion
	}
	for i, o := range a.Options {
		if i != a.AffirmativeIdx-1 && startsWithWord(o, noWords) {
			return CategoryToolPermission
		}
	}
	return CategoryStrategicQuestion
}

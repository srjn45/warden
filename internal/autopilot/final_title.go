package autopilot

import (
	"strings"

	"github.com/srjn45/warden/internal/release"
)

// maxFinalTitle is the longest final-PR title FinalPRTitle returns.
const maxFinalTitle = 72

// CommitMsg is one commit on the integration branch (not on the default branch).
type CommitMsg struct {
	Subject, Body string
}

var finalTitleRank = map[string]int{
	"feat": 0, "fix": 1, "perf": 2, "revert": 3, "refactor": 4,
	"docs": 5, "test": 6, "build": 7, "ci": 8, "chore": 9,
}

// FinalPRTitle renders the conventional-commit title of the final PR from the
// plan name and the integration branch's commits. Pure and deterministic.
func FinalPRTitle(name, runID string, commits []CommitMsg) string {
	desc := strings.Join(strings.Fields(strings.NewReplacer("-", " ", "_", " ").Replace(firstNonEmpty(name, runID))), " ")

	var conv []release.Commit
	breaking := false
	for _, cm := range commits {
		if strings.HasPrefix(strings.TrimSpace(cm.Subject), "Merge ") {
			continue
		}
		c := release.ParseConventional(cm.Subject, cm.Body)
		if c.Type == "" {
			continue
		}
		if _, ok := finalTitleRank[c.Type]; !ok {
			continue
		}
		conv = append(conv, c)
		breaking = breaking || c.Breaking
	}

	typ := "chore"
	best := len(finalTitleRank)
	for _, c := range conv {
		if r := finalTitleRank[c.Type]; r < best {
			best, typ = r, c.Type
		}
	}

	var ofType []release.Commit
	for _, c := range conv {
		if c.Type == typ {
			ofType = append(ofType, c)
		}
	}
	scope := sharedScope(ofType)
	if scope == "" {
		scope = sharedScope(conv)
	}

	prefix := typ
	if scope != "" {
		prefix += "(" + scope + ")"
	}
	if breaking {
		prefix += "!"
	}
	prefix += ": "
	if room := maxFinalTitle - len(prefix); len(desc) > room {
		if room < 0 {
			room = 0
		}
		desc = strings.TrimSpace(desc[:room])
	}
	return prefix + desc
}

// sharedScope is the scope every commit carries, or "" if none or mixed.
func sharedScope(cs []release.Commit) string {
	if len(cs) == 0 {
		return ""
	}
	s := cs[0].Scope
	for _, c := range cs[1:] {
		if c.Scope != s {
			return ""
		}
	}
	return s
}

package daemon

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/srjn45/warden/internal/groupstore"
	"github.com/srjn45/warden/internal/store"
)

// summaryAskPromptFmt is the pane-level question typed into a joining agent's
// session when no declared summary exists. It is one cheap, one-time user-turn
// (the agent already has project context loaded), so the agent can answer in
// its current session. The summary is then cached via `warden collaborate group
// <name> summary --set '<text>'` (B8 CLI) so this prompt never fires again for
// the same project key.
const summaryAskPromptFmt = "warden needs a one-line summary of this project (%s) for the collaboration group %q roster. Please reply with just one line describing what this project does. Use `warden collaborate group %s summary --set '<text>'` to save it (one-time; cached forever after)."

// summaryFromFiles resolves a one-line project summary by reading CLAUDE.md
// and README.md in dir. Resolution order (cheapest/most-specific first):
//  1. A `## Summary`, `## Project Summary`, or `## About` heading in CLAUDE.md —
//     first content line beneath the heading.
//  2. Same in README.md.
//  3. First non-blank, non-heading line in CLAUDE.md.
//  4. First non-blank, non-heading line in README.md.
//
// Returns "" when no usable text is found so the caller can move to the next
// resolution tier (agent-generated-once or cache).
func summaryFromFiles(dir string) string {
	for _, name := range []string{"CLAUDE.md", "README.md"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if s := extractSummarySection(string(data)); s != "" {
			return s
		}
	}
	for _, name := range []string{"CLAUDE.md", "README.md"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if s := firstContentLine(string(data)); s != "" {
			return s
		}
	}
	return ""
}

// extractSummarySection finds a `## Summary` / `## Project Summary` / `## About`
// heading and returns the first non-blank content line beneath it.
func extractSummarySection(text string) string {
	sc := bufio.NewScanner(strings.NewReader(text))
	inSection := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !inSection {
			if isSummaryHeading(line) {
				inSection = true
			}
			continue
		}
		if isMarkdownHeading(line) {
			return "" // next heading with nothing between
		}
		if line == "" {
			continue // skip blanks between heading and content
		}
		return line
	}
	return ""
}

// firstContentLine returns the first non-blank, non-heading line in text.
func firstContentLine(text string) string {
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || isMarkdownHeading(line) {
			continue
		}
		return line
	}
	return ""
}

func isSummaryHeading(line string) bool {
	l := strings.ToLower(line)
	return l == "## summary" || l == "## project summary" || l == "## about"
}

func isMarkdownHeading(line string) bool {
	return strings.HasPrefix(line, "#")
}

// groupSummaryAskPrompt composes the one-time pane question typed into the
// joining agent's session when no declared summary exists (design §4.2 tier 2).
func groupSummaryAskPrompt(groupName, projectKey string) string {
	return fmt.Sprintf(summaryAskPromptFmt, projectKey, groupName, groupName)
}

// resolveGroupSummary resolves a one-line project summary for a joining member,
// following the design §4.2 precedence:
//  1. Declared blurb: `## Summary` / first line of CLAUDE.md or README.md.
//     Zero tokens; purely file-based.
//  2. Cached in the group record from a previous member with the same project
//     key (survives leave/rejoin — the durable SummaryCache persists even when
//     the roster seat is removed on leave).
//  3. Ask the agent once via pane input (one cheap, one-time user-turn): the
//     agent already has project context loaded and can answer immediately.
//     Returns "" so the join proceeds with a "summary pending" placeholder
//     while the agent formulates and caches its reply (via B8 CLI/MCP).
//
// The returned summary (or "") should be set on the member before seating, and
// stored in the group's SummaryCache via CacheSummary whenever it is non-empty.
func resolveGroupSummary(ctx context.Context, s *Server, sess *store.Session, grp *groupstore.Group, groupName, projectKey string) string {
	// 1. Declared blurb — zero tokens, sync.
	if declared := summaryFromFiles(sessionRepoDir(sess)); declared != "" {
		return declared
	}

	// 2. Cached from a prior seat for this project key.
	if grp != nil && grp.SummaryCache != nil {
		if cached := grp.SummaryCache[projectKey]; cached != "" {
			return cached
		}
	}

	// 3. Ask the agent once via pane input (best-effort, non-blocking). The
	// question is typed directly into the agent's active session so it becomes
	// the agent's next user turn — no inbox message, no extra round-trip.
	if s.life != nil {
		_ = s.life.Input(ctx, sess.TmuxSession, groupSummaryAskPrompt(groupName, projectKey))
	}
	return ""
}

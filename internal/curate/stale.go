package curate

import (
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/memory"
)

// repoPathRe matches a repo-relative path token — one or more slash-joined segments
// of word/dot/dash chars (e.g. internal/memory/parse.go, site/src). It deliberately
// does NOT match absolute paths, "~/…" home paths, URLs, or `backtick commands`,
// which are filtered out before the check so a fact like "state lives in ~/.warden"
// or "run `wd check`" is never falsely flagged stale.
var repoPathRe = regexp.MustCompile(`\b[\w.-]+(?:/[\w.-]+)+\b`)

// CheckStale flags LIVE entries whose named repo-relative path no longer exists on
// the tree at root — the cheap DETERMINISTIC freshness check of §4.2. exists reports
// whether an absolute path is present (inject a stub in tests; production passes an
// os.Stat wrapper). An entry with no qualifying path token is never touched; an entry
// naming a path that IS present is left alone; only a fact pointing at a vanished path
// is flagged (not deleted), so the diff reviewer sees what went stale and why.
func CheckStale(m *memory.Memory, root string, exists func(abs string) bool, now time.Time) int {
	if exists == nil {
		return 0
	}
	flagged := 0
	for i := range m.Entries {
		e := &m.Entries[i]
		if !e.Live() {
			continue
		}
		missing := firstMissingPath(e.Text, root, exists)
		if missing == "" {
			continue
		}
		e.Stale = true
		e.Note = missing + " no longer exists (checked " + now.Format("2006-01-02") + ")"
		flagged++
	}
	return flagged
}

// firstMissingPath returns the first repo-relative path token in text that does not
// exist under root, or "" if the text names no qualifying path (or all of them still
// exist). Tokens inside `backticks` (commands, not paths) are stripped first.
func firstMissingPath(text, root string, exists func(abs string) bool) string {
	text = stripBackticks(text)
	for _, tok := range repoPathRe.FindAllString(text, -1) {
		if !looksLikeRepoPath(tok) {
			continue
		}
		if !exists(filepath.Join(root, tok)) {
			return tok
		}
	}
	return ""
}

// looksLikeRepoPath filters path-shaped tokens down to plausibly-in-repo relatives:
// it rejects home ("~/…"), absolute ("/…"), URL-ish ("http://", "a.b/c" hosts), and
// version-ish ("v1.2/…") tokens to keep the deterministic check conservative — a
// false stale-flag is worse than a missed one.
func looksLikeRepoPath(tok string) bool {
	if tok == "" || strings.HasPrefix(tok, "/") || strings.HasPrefix(tok, "~") {
		return false
	}
	if strings.Contains(tok, "://") {
		return false
	}
	first := tok
	if i := strings.IndexByte(tok, '/'); i >= 0 {
		first = tok[:i]
	}
	// A dotted first segment (example.com, api.v2) reads as a host/version, not a
	// repo dir — skip it. A leading dir like "internal" or "site" has no dot.
	return !strings.Contains(first, ".")
}

func stripBackticks(s string) string {
	var b strings.Builder
	inTick := false
	for _, r := range s {
		if r == '`' {
			inTick = !inTick
			continue
		}
		if !inTick {
			b.WriteRune(r)
		}
	}
	return b.String()
}

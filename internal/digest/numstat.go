package digest

import (
	"sort"
	"strconv"
	"strings"
)

// ParseNumstat parses `git diff --numstat` output into a path -> LineDelta map.
// Rows are "added\tremoved\tpath"; binary files use "-" which maps to 0. Rows
// that don't have three tab fields are skipped.
func ParseNumstat(out string) map[string]LineDelta {
	res := map[string]LineDelta{}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 {
			continue
		}
		res[strings.TrimRight(fields[2], "\r")] = LineDelta{Added: atoiDash(fields[0]), Removed: atoiDash(fields[1])}
	}
	return res
}

func atoiDash(s string) int {
	if s == "-" {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

// MergeFiles unions transcript-edited files (authoritative for WHICH files,
// kept in first-seen order) with git-changed files (annotated +/-). Edited files
// come first in transcript order; git-only files follow, sorted for determinism.
// Returns a nil slice when there are no files; JSON consumers should treat null as "no files".
func MergeFiles(edited []string, stats map[string]LineDelta) []FileChange {
	editedSet := map[string]bool{}
	var out []FileChange
	for _, p := range edited {
		editedSet[p] = true
		d := stats[p] // zero value when absent (reverted / non-repo)
		out = append(out, FileChange{Path: p, Added: d.Added, Removed: d.Removed, Edited: true})
	}
	var gitOnly []string
	for p := range stats {
		if !editedSet[p] {
			gitOnly = append(gitOnly, p)
		}
	}
	sort.Strings(gitOnly)
	for _, p := range gitOnly {
		d := stats[p]
		out = append(out, FileChange{Path: p, Added: d.Added, Removed: d.Removed, Edited: false})
	}
	return out
}

// Package release recommends the next SemVer release for a repository by
// analyzing the commits (and merged pull requests) since the latest vX.Y.Z
// tag. It only recommends: it never creates or pushes tags. Git and gh are
// reached through small interfaces so callers and tests can inject fakes.
package release

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Bump is the SemVer increment a set of commits calls for.
type Bump int

const (
	BumpNone Bump = iota
	BumpPatch
	BumpMinor
	BumpMajor
)

func (b Bump) String() string {
	switch b {
	case BumpPatch:
		return "patch"
	case BumpMinor:
		return "minor"
	case BumpMajor:
		return "major"
	}
	return "none"
}

// Version is a SemVer MAJOR.MINOR.PATCH triple.
type Version struct{ Major, Minor, Patch int }

// String renders the version as a tag: vMAJOR.MINOR.PATCH.
func (v Version) String() string { return fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, v.Patch) }

// Less reports whether v sorts before o.
func (v Version) Less(o Version) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	return v.Patch < o.Patch
}

var versionRe = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)$`)

// ParseVersion parses "1.2.3" or "v1.2.3". Pre-release/build suffixes are rejected.
func ParseVersion(tag string) (Version, error) {
	m := versionRe.FindStringSubmatch(strings.TrimSpace(tag))
	if m == nil {
		return Version{}, fmt.Errorf("release: %q is not a MAJOR.MINOR.PATCH version", tag)
	}
	n := func(s string) int { i, _ := strconv.Atoi(s); return i }
	return Version{n(m[1]), n(m[2]), n(m[3])}, nil
}

// BumpVersion applies b to v. BumpNone returns v unchanged.
func BumpVersion(v Version, b Bump) Version {
	switch b {
	case BumpMajor:
		return Version{v.Major + 1, 0, 0}
	case BumpMinor:
		return Version{v.Major, v.Minor + 1, 0}
	case BumpPatch:
		return Version{v.Major, v.Minor, v.Patch + 1}
	}
	return v
}

// Commit is a parsed commit.
type Commit struct {
	SHA, Subject, Body, Type, Scope string
	// Description is the subject without the "type(scope)!:" prefix.
	Description string
	Breaking    bool
	PR          int // 0 if none
}

// PullRequest is a merged PR.
type PullRequest struct {
	Number                   int
	Title, Body, URL, Author string
}

// Changelog holds rendered entries (markdown bullet text) per category.
type Changelog struct {
	Breaking, Features, Fixes, Performance, Docs, Other []string
}

// Advice is the full recommendation.
type Advice struct {
	Target    string
	TargetSHA string
	LatestTag string
	Current   Version
	Next      Version
	Bump      Bump
	Commits   []Commit
	PRs       []PullRequest
	Changelog Changelog
}

var (
	convRe     = regexp.MustCompile(`^(\w+)(?:\(([^)]*)\))?(!)?:\s*(.*)$`)
	breakingRe = regexp.MustCompile(`(?m)^BREAKING[ -]CHANGE:`)
	mergePRRe  = regexp.MustCompile(`^Merge pull request #(\d+)\b`)
	squashRe   = regexp.MustCompile(`\(#(\d+)\)\s*$`)
	trailerRe  = regexp.MustCompile(`(?mi)^(?:PR|gh)-(\d+)\s*$`)
)

// ParseConventional parses a Conventional Commit. A subject that doesn't
// follow the format yields an empty Type; PR numbers are extracted regardless.
func ParseConventional(subject, body string) Commit {
	subject = strings.TrimSpace(subject)
	c := Commit{Subject: subject, Body: body, Description: subject}
	if m := convRe.FindStringSubmatch(subject); m != nil {
		c.Type = strings.ToLower(m[1])
		c.Scope = m[2]
		c.Breaking = m[3] == "!"
		c.Description = strings.TrimSpace(m[4])
	}
	if breakingRe.MatchString(body) {
		c.Breaking = true
	}
	c.PR = findPR(subject, body)
	return c
}

func findPR(subject, body string) int {
	for _, re := range []*regexp.Regexp{mergePRRe, squashRe} {
		if m := re.FindStringSubmatch(subject); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n
		}
	}
	if m := trailerRe.FindStringSubmatch(body); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

// DecideBump returns the highest bump the commits call for.
func DecideBump(commits []Commit) Bump {
	b := BumpNone
	for _, c := range commits {
		var cb Bump
		switch {
		case c.Breaking:
			cb = BumpMajor
		case c.Type == "feat":
			cb = BumpMinor
		case c.Type == "fix", c.Type == "perf", c.Type == "revert":
			cb = BumpPatch
		}
		if cb > b {
			b = cb
		}
	}
	return b
}

// category ranks changelog sections; lower wins when merging commits of one PR.
func category(c Commit) int {
	switch {
	case c.Breaking:
		return 0
	case c.Type == "feat":
		return 1
	case c.Type == "fix", c.Type == "revert":
		return 2
	case c.Type == "perf":
		return 3
	case c.Type == "docs":
		return 4
	}
	return 5
}

func entryText(scope, desc string, pr int) string {
	s := desc
	if scope != "" {
		s = "**" + scope + ":** " + s
	}
	if pr > 0 {
		s += " (#" + strconv.Itoa(pr) + ")"
	}
	return s
}

// BuildChangelog categorizes commits and PRs. Commits sharing a PR collapse to
// one entry that prefers the PR title; PR-less commits get their own entry.
func BuildChangelog(commits []Commit, prs []PullRequest) Changelog {
	prTitle := map[int]string{}
	for _, p := range prs {
		if p.Title != "" {
			prTitle[p.Number] = p.Title
		}
	}
	type entry struct {
		cat  int
		text string
	}
	var entries []entry
	byPR := map[int]int{} // PR number -> index in entries
	for _, c := range commits {
		desc := c.Description
		if desc == "" {
			desc = c.Subject
		}
		if c.PR > 0 {
			if i, ok := byPR[c.PR]; ok {
				if cat := category(c); cat < entries[i].cat {
					entries[i].cat = cat
				}
				continue
			}
			if t, ok := prTitle[c.PR]; ok {
				desc = ParseConventional(t, "").Description
			}
			byPR[c.PR] = len(entries)
		}
		entries = append(entries, entry{category(c), entryText(c.Scope, desc, c.PR)})
	}
	var cl Changelog
	for _, e := range entries {
		switch e.cat {
		case 0:
			cl.Breaking = append(cl.Breaking, e.text)
		case 1:
			cl.Features = append(cl.Features, e.text)
		case 2:
			cl.Fixes = append(cl.Fixes, e.text)
		case 3:
			cl.Performance = append(cl.Performance, e.text)
		case 4:
			cl.Docs = append(cl.Docs, e.text)
		default:
			cl.Other = append(cl.Other, e.text)
		}
	}
	return cl
}

// RenderChangelog renders the advice's changelog as markdown.
func RenderChangelog(a Advice) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "## %s\n", a.Next)
	sections := []struct {
		title   string
		entries []string
	}{
		{"Breaking Changes", a.Changelog.Breaking},
		{"Features", a.Changelog.Features},
		{"Bug Fixes", a.Changelog.Fixes},
		{"Performance", a.Changelog.Performance},
		{"Documentation", a.Changelog.Docs},
		{"Other", a.Changelog.Other},
	}
	empty := true
	for _, s := range sections {
		if len(s.entries) == 0 {
			continue
		}
		empty = false
		fmt.Fprintf(&sb, "\n### %s\n\n", s.title)
		for _, e := range s.entries {
			sb.WriteString("- " + e + "\n")
		}
	}
	if empty {
		sb.WriteString("\nNo changes.\n")
	}
	return sb.String()
}

// sortPRs orders PRs by number.
func sortPRs(prs []PullRequest) {
	sort.Slice(prs, func(i, j int) bool { return prs[i].Number < prs[j].Number })
}

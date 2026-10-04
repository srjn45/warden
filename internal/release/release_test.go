package release

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseVersion(t *testing.T) {
	for in, want := range map[string]Version{"v1.2.3": {1, 2, 3}, "1.2.3": {1, 2, 3}, " v0.10.0 ": {0, 10, 0}} {
		v, err := ParseVersion(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, v)
	}
	for _, bad := range []string{"", "v1.2", "v1.2.3-rc1", "release-1", "v1.2.3.4"} {
		_, err := ParseVersion(bad)
		assert.Error(t, err, bad)
	}
	assert.Equal(t, "v1.2.3", Version{1, 2, 3}.String())
}

func TestBumpVersion(t *testing.T) {
	v := Version{1, 2, 3}
	assert.Equal(t, v, BumpVersion(v, BumpNone))
	assert.Equal(t, Version{1, 2, 4}, BumpVersion(v, BumpPatch))
	assert.Equal(t, Version{1, 3, 0}, BumpVersion(v, BumpMinor))
	assert.Equal(t, Version{2, 0, 0}, BumpVersion(v, BumpMajor))
	assert.Equal(t, "minor", BumpMinor.String())
	assert.Equal(t, "none", BumpNone.String())
}

func TestParseConventional(t *testing.T) {
	tests := []struct {
		name, subject, body string
		typ, scope, desc    string
		breaking            bool
		pr                  int
	}{
		{"plain feat", "feat: add x", "", "feat", "", "add x", false, 0},
		{"scope", "fix(cli): crash", "", "fix", "cli", "crash", false, 0},
		{"bang", "feat(api)!: drop v1", "", "feat", "api", "drop v1", true, 0},
		{"missing type", "Update readme", "", "", "", "Update readme", false, 0},
		{"uppercase type", "Fix: thing", "", "fix", "", "thing", false, 0},
		{"footer only breaking", "refactor: tidy", "details\n\nBREAKING CHANGE: removed flag", "refactor", "", "tidy", true, 0},
		{"hyphen footer", "chore: x", "BREAKING-CHANGE: y", "chore", "", "x", true, 0},
		{"multiline body no breaking", "docs: a", "line1\nline2\n\nmore", "docs", "", "a", false, 0},
		{"breaking mid-line ignored", "fix: a", "this is not a BREAKING CHANGE: footer", "fix", "", "a", false, 0},
		{"squash pr", "feat: thing (#42)", "", "feat", "", "thing (#42)", false, 42},
		{"merge pr", "Merge pull request #7 from a/b", "", "", "", "Merge pull request #7 from a/b", false, 7},
		{"trailer PR", "fix: a", "body\n\nPR-12", "fix", "", "a", false, 12},
		{"trailer gh", "fix: a", "gh-13\n", "fix", "", "a", false, 13},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := ParseConventional(tt.subject, tt.body)
			assert.Equal(t, tt.typ, c.Type)
			assert.Equal(t, tt.scope, c.Scope)
			assert.Equal(t, tt.desc, c.Description)
			assert.Equal(t, tt.breaking, c.Breaking)
			assert.Equal(t, tt.pr, c.PR)
		})
	}
}

func TestDecideBump(t *testing.T) {
	mk := func(subjects ...string) []Commit {
		var cs []Commit
		for _, s := range subjects {
			cs = append(cs, ParseConventional(s, ""))
		}
		return cs
	}
	tests := []struct {
		name string
		in   []Commit
		want Bump
	}{
		{"empty", nil, BumpNone},
		{"docs only", mk("docs: a", "chore: b", "test: c", "ci: d", "refactor: e", "style: f", "build: g"), BumpNone},
		{"untyped", mk("random stuff"), BumpNone},
		{"fix", mk("fix: a"), BumpPatch},
		{"perf", mk("perf: a"), BumpPatch},
		{"revert", mk("revert: a"), BumpPatch},
		{"feat", mk("feat: a"), BumpMinor},
		{"feat beats fix", mk("fix: a", "feat: b", "fix: c"), BumpMinor},
		{"bang", mk("chore!: a", "feat: b"), BumpMajor},
		{"footer", []Commit{ParseConventional("docs: a", "BREAKING CHANGE: x"), ParseConventional("feat: b", "")}, BumpMajor},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { assert.Equal(t, tt.want, DecideBump(tt.in)) })
	}
}

func TestBuildChangelogAndRender(t *testing.T) {
	commits := []Commit{
		ParseConventional("feat(ui): shiny (#5)", ""),
		ParseConventional("fix: follow-up to shiny (#5)", ""),
		ParseConventional("fix: lone bug", ""),
		ParseConventional("feat!: new api", ""),
		ParseConventional("perf: faster", ""),
		ParseConventional("docs: words", ""),
		ParseConventional("chore: misc", ""),
	}
	prs := []PullRequest{{Number: 5, Title: "feat(ui): Shiny new thing"}}
	cl := BuildChangelog(commits, prs)
	assert.Equal(t, []string{"new api"}, cl.Breaking)
	assert.Equal(t, []string{"**ui:** Shiny new thing (#5)"}, cl.Features) // deduped, PR title wins
	assert.Equal(t, []string{"lone bug"}, cl.Fixes)
	assert.Equal(t, []string{"faster"}, cl.Performance)
	assert.Equal(t, []string{"words"}, cl.Docs)
	assert.Equal(t, []string{"misc"}, cl.Other)

	md := RenderChangelog(Advice{Next: Version{2, 0, 0}, Changelog: cl})
	assert.True(t, strings.HasPrefix(md, "## v2.0.0\n"))
	for _, h := range []string{"### Breaking Changes", "### Features", "### Bug Fixes", "### Performance", "### Documentation", "### Other"} {
		assert.Contains(t, md, h)
	}
	assert.Contains(t, md, "- lone bug\n")
	assert.Contains(t, RenderChangelog(Advice{Next: Version{1, 0, 0}}), "No changes.")
}

type fakeGit struct {
	tags         []string
	commits      []RawCommit
	tagErr       error
	gotBase, got string
}

func (f *fakeGit) Tags(context.Context) ([]string, error) { return f.tags, f.tagErr }
func (f *fakeGit) Log(_ context.Context, base, head string) ([]RawCommit, error) {
	f.gotBase, f.got = base, head
	return f.commits, nil
}

type fakeGH struct {
	prs   map[int]PullRequest
	calls int
}

func (f *fakeGH) PullRequest(_ context.Context, n int) (PullRequest, error) {
	f.calls++
	p, ok := f.prs[n]
	if !ok {
		return PullRequest{}, errors.New("not found")
	}
	return p, nil
}

func TestAnalyze(t *testing.T) {
	g := &fakeGit{
		tags: []string{"v1.2.0", "v1.10.0", "v1.9.9", "nightly", "v2.0.0-rc1", "1.99.0"},
		commits: []RawCommit{
			{"a1", "Merge pull request #7 from x/feat", "feat(cli): add widget\n\nbody"},
			{"a2", "fix: tidy thing (#8)", ""},
			{"a3", "docs: readme", "PR-9"},
			{"a4", "feat: add widget (#7)", ""},
		},
	}
	gh := &fakeGH{prs: map[int]PullRequest{
		7: {Number: 7, Title: "feat(cli): Add widget command", URL: "u7", Author: "me"},
		8: {Title: "fix: Tidy the thing"},
	}}
	a, err := Analyze(context.Background(), Options{Git: g, GH: gh})
	require.NoError(t, err)
	assert.Equal(t, "v1.10.0", a.LatestTag)
	assert.Equal(t, Version{1, 10, 0}, a.Current)
	assert.Equal(t, BumpMinor, a.Bump)
	assert.Equal(t, Version{1, 11, 0}, a.Next)
	assert.Equal(t, "v1.10.0", g.gotBase)
	assert.Equal(t, "HEAD", g.got)
	require.Len(t, a.Commits, 4)
	assert.Equal(t, 7, a.Commits[0].PR)
	assert.Equal(t, "a1", a.Commits[0].SHA)
	require.Len(t, a.PRs, 3)
	assert.Equal(t, []int{7, 8, 9}, []int{a.PRs[0].Number, a.PRs[1].Number, a.PRs[2].Number})
	assert.Equal(t, "u7", a.PRs[0].URL)
	assert.Equal(t, 8, a.PRs[1].Number)             // gh result missing number is filled in
	assert.Equal(t, "docs: readme", a.PRs[2].Title) // gh miss degrades to commit title
	assert.Equal(t, 3, gh.calls)                    // PR 7 looked up once
	assert.Equal(t, []string{"**cli:** Add widget command (#7)"}, a.Changelog.Features)
	assert.Equal(t, []string{"Tidy the thing (#8)"}, a.Changelog.Fixes)
	assert.Equal(t, []string{"readme (#9)"}, a.Changelog.Docs)
}

func TestAnalyzeNoGH(t *testing.T) {
	g := &fakeGit{tags: []string{"v0.1.0"}, commits: []RawCommit{{"a", "fix: x (#3)", ""}}}
	a, err := Analyze(context.Background(), Options{Git: g, GH: &fakeGH{}, Head: "feature"})
	require.NoError(t, err)
	assert.Equal(t, "feature", g.got)
	assert.Equal(t, Version{0, 1, 1}, a.Next)
	require.Len(t, a.PRs, 1)
	assert.Equal(t, "fix: x (#3)", a.PRs[0].Title)
}

func TestAnalyzeNoReleasableCommits(t *testing.T) {
	g := &fakeGit{tags: []string{"v1.0.0"}, commits: []RawCommit{{"a", "chore: x", ""}}}
	a, err := Analyze(context.Background(), Options{Git: g})
	require.NoError(t, err)
	assert.Equal(t, BumpNone, a.Bump)
	assert.Equal(t, a.Current, a.Next)
}

func TestAnalyzeNoTagsAndPin(t *testing.T) {
	g := &fakeGit{commits: []RawCommit{{"a", "feat: first", ""}}}
	a, err := Analyze(context.Background(), Options{Git: g})
	require.NoError(t, err)
	assert.Equal(t, "", a.LatestTag)
	assert.Equal(t, "", g.gotBase)
	assert.Equal(t, Version{0, 1, 0}, a.Next)

	g2 := &fakeGit{tags: []string{"v3.0.0"}}
	a, err = Analyze(context.Background(), Options{Git: g2, BaseTag: "v1.5.0"})
	require.NoError(t, err)
	assert.Equal(t, "v1.5.0", g2.gotBase)
	assert.Equal(t, Version{1, 5, 0}, a.Next)

	_, err = Analyze(context.Background(), Options{Git: g2, BaseTag: "bogus"})
	assert.Error(t, err)
}

func TestAnalyzeTagError(t *testing.T) {
	_, err := Analyze(context.Background(), Options{Git: &fakeGit{tagErr: errors.New("boom")}})
	assert.ErrorContains(t, err, "boom")
}

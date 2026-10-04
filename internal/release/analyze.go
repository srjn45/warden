package release

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// RawCommit is an unparsed commit as returned by Git.
type RawCommit struct{ SHA, Subject, Body string }

// Git is the slice of git that Analyze needs.
type Git interface {
	// Tags lists all tag names.
	Tags(ctx context.Context) ([]string, error)
	// Log lists commits reachable from head but not from base (all of head when
	// base is empty), newest first.
	Log(ctx context.Context, base, head string) ([]RawCommit, error)
}

// GH looks up pull requests. It is optional; errors are tolerated.
type GH interface {
	PullRequest(ctx context.Context, number int) (PullRequest, error)
}

// Options configures Analyze.
type Options struct {
	Repo      string
	Head      string // default HEAD, or target ref like origin/main
	TargetSHA string // optional resolved commit SHA of Head
	BaseTag   string // optional pin; empty = latest SemVer tag
	Git       Git    // default: the git binary in Repo
	GH        GH     // default: the gh binary if on PATH, else none
}

// Analyze inspects commits since the base tag and recommends the next version.
func Analyze(ctx context.Context, opts Options) (Advice, error) {
	if opts.Head == "" {
		opts.Head = "HEAD"
	}
	g := opts.Git
	if g == nil {
		g = execGit{repo: opts.Repo}
	}
	gh := opts.GH
	if gh == nil {
		if _, err := exec.LookPath("gh"); err == nil {
			gh = execGH{repo: opts.Repo}
		}
	}

	tags, err := g.Tags(ctx)
	if err != nil {
		return Advice{}, fmt.Errorf("release: list tags: %w", err)
	}
	var a Advice
	a.Target = opts.Head
	a.TargetSHA = opts.TargetSHA
	if opts.BaseTag != "" {
		v, err := ParseVersion(opts.BaseTag)
		if err != nil {
			return Advice{}, err
		}
		a.LatestTag, a.Current = opts.BaseTag, v
	} else {
		found := false
		for _, t := range tags {
			if !strings.HasPrefix(t, "v") {
				continue
			}
			v, err := ParseVersion(t)
			if err != nil {
				continue
			}
			if !found || a.Current.Less(v) {
				a.LatestTag, a.Current, found = t, v, true
			}
		}
	}

	raw, err := g.Log(ctx, a.LatestTag, opts.Head)
	if err != nil {
		return Advice{}, fmt.Errorf("release: list commits: %w", err)
	}
	prs := map[int]PullRequest{}
	for _, r := range raw {
		c := parseRaw(r)
		a.Commits = append(a.Commits, c)
		if c.PR == 0 {
			continue
		}
		if _, ok := prs[c.PR]; ok {
			continue
		}
		pr := PullRequest{Number: c.PR, Title: c.Subject}
		if gh != nil {
			if got, err := gh.PullRequest(ctx, c.PR); err == nil {
				if got.Number == 0 {
					got.Number = c.PR
				}
				pr = got
			}
		}
		prs[c.PR] = pr
	}
	for _, p := range prs {
		a.PRs = append(a.PRs, p)
	}
	sortPRs(a.PRs)

	a.Bump = DecideBump(a.Commits)
	a.Next = BumpVersion(a.Current, a.Bump)
	a.Changelog = BuildChangelog(a.Commits, a.PRs)
	return a, nil
}

// parseRaw parses a raw commit. For "Merge pull request #N" commits the
// conventional message is the PR title carried in the body's first line.
func parseRaw(r RawCommit) Commit {
	if mergePRRe.MatchString(r.Subject) {
		title, rest, _ := strings.Cut(strings.TrimSpace(r.Body), "\n")
		if title = strings.TrimSpace(title); title != "" {
			c := ParseConventional(title, rest)
			c.PR = findPR(r.Subject, "")
			c.SHA = r.SHA
			return c
		}
	}
	c := ParseConventional(r.Subject, r.Body)
	c.SHA = r.SHA
	return c
}

type execGit struct{ repo string }

func (e execGit) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = e.repo
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

func (e execGit) Tags(ctx context.Context) ([]string, error) {
	out, err := e.run(ctx, "tag", "--list")
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

func (e execGit) Log(ctx context.Context, base, head string) ([]RawCommit, error) {
	rng := head
	if base != "" {
		rng = base + ".." + head
	}
	out, err := e.run(ctx, "log", "--format=%H%x1f%s%x1f%b%x1e", rng)
	if err != nil {
		return nil, err
	}
	var cs []RawCommit
	for _, rec := range strings.Split(out, "\x1e") {
		rec = strings.TrimLeft(rec, "\n")
		if rec == "" {
			continue
		}
		f := strings.SplitN(rec, "\x1f", 3)
		if len(f) < 3 {
			continue
		}
		cs = append(cs, RawCommit{SHA: f[0], Subject: f[1], Body: strings.TrimSpace(f[2])})
	}
	return cs, nil
}

type execGH struct{ repo string }

func (e execGH) PullRequest(ctx context.Context, n int) (PullRequest, error) {
	cmd := exec.CommandContext(ctx, "gh", "pr", "view", fmt.Sprint(n), "--json", "number,title,body,url,author")
	cmd.Dir = e.repo
	out, err := cmd.Output()
	if err != nil {
		return PullRequest{}, err
	}
	var v struct {
		Number int
		Title  string
		Body   string
		URL    string
		Author struct{ Login string }
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return PullRequest{}, err
	}
	return PullRequest{v.Number, v.Title, v.Body, v.URL, v.Author.Login}, nil
}

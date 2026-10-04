package fastbrain

import (
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
)

// BugReportRepo is the GitHub repository bug reports are filed against.
const BugReportRepo = "srjn45/warden"

// BugReportIssuesURL is the human-facing issues page shown in the consent prompt.
const BugReportIssuesURL = "https://github.com/" + BugReportRepo + "/issues"

// maxPrefillURL caps the pre-filled new-issue URL so browsers/servers accept it.
const maxPrefillURL = 7000

// GHRunner abstracts the `gh` CLI so tests never shell out. Run returns the
// combined output. Authenticated reports whether `gh auth status` succeeds.
type GHRunner interface {
	Authenticated(ctx context.Context) bool
	Run(ctx context.Context, args ...string) (string, error)
}

type execGH struct{}

func (execGH) Authenticated(ctx context.Context) bool {
	if _, err := exec.LookPath("gh"); err != nil {
		return false
	}
	return exec.CommandContext(ctx, "gh", "auth", "status").Run() == nil
}

func (execGH) Run(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "gh", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// DefaultGH is the real `gh` CLI runner.
func DefaultGH() GHRunner { return execGH{} }

// PrefillIssueURL builds the clickable new-issue URL for d.
func PrefillIssueURL(d *IssueDraft) string {
	body := d.Body()
	build := func(b string) string {
		q := url.Values{}
		q.Set("title", d.Title)
		q.Set("body", b)
		return BugReportIssuesURL + "/new?" + strings.ReplaceAll(q.Encode(), "+", "%20")
	}
	u := build(body)
	for len(u) > maxPrefillURL && len(body) > 200 {
		body = body[:len(body)/2] + "\n…(truncated; full draft is staged locally)\n```\n"
		u = build(body)
	}
	return u
}

// SubmitBugReport files d only when the caller has already obtained explicit
// user approval. It uses an authenticated gh to create the issue and returns
// its URL; otherwise it returns a pre-filled new-issue URL (viaGH=false) and
// submits nothing. gh may be nil (treated as unauthenticated).
func SubmitBugReport(ctx context.Context, gh GHRunner, d *IssueDraft) (link string, viaGH bool, err error) {
	if d == nil {
		return "", false, fmt.Errorf("%w: nil draft", ErrInvalidRequest)
	}
	if gh != nil && gh.Authenticated(ctx) {
		out, err := gh.Run(ctx, "issue", "create", "--repo", BugReportRepo, "--title", d.Title, "--body", d.Body())
		if err == nil {
			return lastURLLine(out), true, nil
		}
		// gh failed (network, perms): fall back to the manual link.
	}
	return PrefillIssueURL(d), false, nil
}

func lastURLLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); strings.HasPrefix(l, "http") {
			return l
		}
	}
	return strings.TrimSpace(out)
}

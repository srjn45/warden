package lifecycle

import (
	"context"
	"fmt"
	"strings"
)

// PRResult is the compact struct `warden done --create-pr` / its daemon endpoint
// returns — the opened (or already-existing) pull request for an agent's branch.
type PRResult struct {
	Branch  string `json:"branch"`
	Base    string `json:"base"`
	URL     string `json:"url"`
	Created bool   `json:"created"`          // false = a PR already existed for this branch
	Output  string `json:"output,omitempty"` // raw gh output (trimmed)
}

// CreatePR opens a GitHub pull request for dir's current branch via `gh pr
// create`, with the supplied title and body, onto base (default main). It
// enforces the protected-branch rail — an agent's own branch is the head, never
// main/master — and treats an already-existing PR for the branch as a non-error
// result (Created=false with the URL gh reports) so `done --create-pr` is
// idempotent rather than failing on a re-run. The branch must already be pushed.
func (l *Lifecycle) CreatePR(ctx context.Context, dir, title, body, base string) (PRResult, error) {
	branch := l.GitBranch(ctx, dir)
	if branch == "" {
		return PRResult{}, fmt.Errorf("not a git repository: %s", dir)
	}
	if protectedBranches[branch] {
		return PRResult{}, fmt.Errorf("refusing to open a PR from protected branch %q — an agent works on its own branch and a PR integrates it", branch)
	}
	if base == "" {
		base = "main"
	}
	out, err := l.run.Run(ctx, dir, "gh", "pr", "create",
		"--base", base, "--head", branch, "--title", title, "--body", body)
	out = strings.TrimSpace(out)
	url := firstURL(out)
	if err != nil {
		// gh exits non-zero when a PR already exists for the branch; surface that
		// as a non-error result with the URL it prints rather than failing done.
		if strings.Contains(out, "already exists") && url != "" {
			return PRResult{Branch: branch, Base: base, URL: url, Created: false, Output: out}, nil
		}
		return PRResult{}, fmt.Errorf("gh pr create: %w: %s", err, out)
	}
	return PRResult{Branch: branch, Base: base, URL: url, Created: true, Output: out}, nil
}

// PRContext returns the `git diff --stat` and the commit subjects (one per line)
// of dir's branch against base (default main), for drafting a PR summary. It is
// best-effort: it tries origin/<base> then <base>, and returns empty strings
// when neither resolves, so callers fall back to their deterministic content.
func (l *Lifecycle) PRContext(ctx context.Context, dir, base string) (stat, commits string) {
	if base == "" {
		base = "main"
	}
	if safeGitRef(base) != nil {
		return "", ""
	}
	for _, ref := range []string{"origin/" + base, base} {
		st, err := l.run.Run(ctx, dir, "git", "diff", "--stat", ref+"...HEAD")
		if err != nil {
			continue
		}
		cm, _ := l.run.Run(ctx, dir, "git", "log", "--format=%s", ref+"..HEAD")
		return strings.TrimSpace(st), strings.TrimSpace(cm)
	}
	return "", ""
}

// firstURL returns the first whitespace-delimited token that looks like an http
// URL in s, or "" — gh prints the PR URL on its own line both on success and in
// the "already exists" message.
func firstURL(s string) string {
	for _, f := range strings.Fields(s) {
		if strings.HasPrefix(f, "http://") || strings.HasPrefix(f, "https://") {
			return f
		}
	}
	return ""
}

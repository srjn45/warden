package daemon

import (
	"context"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
)

// LocalKeyPrefix tags project keys derived from a local filesystem path rather
// than a git remote. A local key identifies a remoteless repo on THIS machine
// only — it is not portable/cross-machine, so the future hub/cluster work must
// treat local keys distinctly from remote ones (design §4.1, decision 4).
const LocalKeyPrefix = "local:"

// ProjectKey returns a stable identity key for a repository. When remoteURL
// normalizes to a canonical remote key that key is returned (so two worktrees
// of the same repo collapse to one key); otherwise a `local:` fallback derived
// from repoRoot is returned — never an empty/rejected result. This is the
// single project-key normalizer referenced by the design (§4.1) and the group
// store (Track B).
func ProjectKey(remoteURL, repoRoot string) string {
	if key, ok := NormalizeRemoteURL(remoteURL); ok {
		return key
	}
	return localKey(repoRoot)
}

// NormalizeRemoteURL canonicalizes a git remote URL into a comparable project
// key of the form "host/path" (e.g. "github.com/org/repo"). It folds away the
// differences that never change repo identity: scheme, credentials/user, port,
// a `.git` suffix, trailing slashes, and letter case. So
// "git@github.com:org/repo.git" and "https://github.com/org/repo" both yield
// "github.com/org/repo". The bool is false when raw is empty or has no host to
// key on, signalling the caller to fall back to a local key.
func NormalizeRemoteURL(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", false
	}

	var host, path string
	if h, p, ok := parseSCPLike(s); ok {
		host, path = h, p
	} else {
		u, err := url.Parse(s)
		if err != nil || u.Host == "" {
			return "", false
		}
		// Hostname() drops any userinfo and :port, leaving just the host.
		host, path = u.Hostname(), u.Path
	}

	host = strings.ToLower(strings.TrimSpace(host))
	path = normalizeRepoPath(path)
	if host == "" || path == "" {
		return "", false
	}
	return host + "/" + path, true
}

// parseSCPLike recognizes git's scp-style remote syntax "[user@]host:path"
// (e.g. "git@github.com:org/repo.git"), which is not a valid URL. It returns
// ok=false for anything containing "://" (a real URL) or whose pre-colon
// segment contains a slash (a plain path that merely has a colon in it).
func parseSCPLike(s string) (host, path string, ok bool) {
	if strings.Contains(s, "://") {
		return "", "", false
	}
	hostPart, path, found := strings.Cut(s, ":")
	if !found {
		return "", "", false
	}
	if strings.Contains(hostPart, "/") {
		return "", "", false
	}
	if at := strings.LastIndex(hostPart, "@"); at >= 0 {
		hostPart = hostPart[at+1:]
	}
	if hostPart == "" {
		return "", "", false
	}
	return hostPart, path, true
}

// normalizeRepoPath strips leading/trailing slashes, a single ".git" suffix,
// and lowercases the org/repo path so casing never splits one repo's identity.
func normalizeRepoPath(p string) string {
	p = strings.Trim(strings.TrimSpace(p), "/")
	p = strings.TrimSuffix(p, ".git")
	p = strings.Trim(p, "/")
	return strings.ToLower(p)
}

// localKey builds the `local:`-tagged fallback key for a remoteless repo from
// its (cleaned, absolute where resolvable) root path.
func localKey(repoRoot string) string {
	p := strings.TrimSpace(repoRoot)
	if p == "" {
		return LocalKeyPrefix
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return LocalKeyPrefix + filepath.Clean(p)
}

// ProjectKeyForDir resolves the project key for a working directory by reading
// its canonical git remote (origin), falling back to a `local:` key rooted at
// the repo's top level (or dir itself when that cannot be determined). It is
// the git-plumbing entry point that pairs with the pure normalizer above.
func ProjectKeyForDir(ctx context.Context, dir string) string {
	remote := gitOutput(ctx, dir, "remote", "get-url", "origin")
	if key, ok := NormalizeRemoteURL(remote); ok {
		return key
	}
	root := gitOutput(ctx, dir, "rev-parse", "--show-toplevel")
	if root == "" {
		root = dir
	}
	return localKey(root)
}

// gitOutput runs `git -C dir <args...>` and returns trimmed stdout, or "" on
// any error (missing remote, not a repo, git absent).
func gitOutput(ctx context.Context, dir string, args ...string) string {
	full := append([]string{"-C", dir}, args...)
	out, err := exec.CommandContext(ctx, "git", full...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
